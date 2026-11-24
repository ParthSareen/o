package chat

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/sessionstore"
	"github.com/ollama/ollama/api"
)

var chatSpinnerFrames = []string{".", "..", "..."}

const (
	maxPickerItems            = 8
	maxInlineModelPickerItems = 5
	maxSlashCompletions       = 5
	maxPromptHistory          = 50
	idleWorkingDelayTicks     = 4
)

var chatEmptyPrompts = []string{
	`read this repo and tell me where to start`,
	`what changed on this branch?`,
	`run the tests and summarize failures`,
	`find the riskiest code path in this folder`,
	`search the web and compare the latest docs with this implementation`,
	`summarize this file and suggest edits`,
}

type ModelOption struct {
	Name              string
	Description       string
	Recommended       bool
	RequiredPlan      string
	Cloud             bool
	AvailabilityBadge string
	SignInURL         string
}

type Options struct {
	Model                       string
	Name                        string
	OpenModelPicker             bool
	ChatID                      string
	Messages                    []api.Message
	Client                      coreagent.ChatClient
	Tools                       *coreagent.Registry
	Skills                      *coreagent.SkillCatalog
	ImportSkills                func(string) (coreagent.SkillImportResult, error)
	ReloadSkills                func() (*coreagent.SkillCatalog, error)
	ToolRegistryForModel        func(context.Context, string) *coreagent.Registry
	ToolsDisabled               bool
	MultiModalForModel          func(context.Context, string) bool
	ModelOptions                func(context.Context) ([]ModelOption, error)
	OnModelSelected             func(context.Context, string) error
	SystemPromptForModel        func(context.Context, string, *coreagent.Registry, bool) string
	ApprovalPrompter            coreagent.ApprovalPrompter
	AutoReview                  bool
	ReviewModel                 string
	EventSinks                  []coreagent.EventSink
	AllowAllTools               bool
	WorkingDir                  string
	RootDir                     string
	Format                      string
	Options                     map[string]any
	Think                       *api.ThinkValue
	KeepAlive                   *api.Duration
	Images                      []api.ImageData
	MultiModal                  bool
	Compactor                   coreagent.Compactor
	ContextWindowTokens         int
	ContextWindowTokensForModel func(context.Context, string, int) int
	PreloadModel                func(context.Context, string, *api.ThinkValue) (int, error)
	CheckCloudModel             func(context.Context, string, string) error
	OpenBrowser                 func(string)
	PollCloudAuth               func(context.Context) (string, bool, error)
	Store                       *sessionstore.Store
	CompactionThreshold         float64
	SystemPrompt                string
}

type Result struct {
	ChatID   string
	Messages []api.Message
}

// maxUnattendedBackgroundRuns caps how many runs in a row may be triggered
// by background completions with no user message in between: a runaway poll
// (one printing fresh output every tick, say) must not burn tokens forever.
// Any user-submitted run resets the count.
const maxUnattendedBackgroundRuns = 6

//nolint:containedctx // chatModel is a Bubble Tea session model; the context is the run-scoped cancellation root.
type chatModel struct {
	ctx          context.Context
	opts         Options
	chatID       string
	chatName     string
	messages     []api.Message
	liveMessages []api.Message
	entries      []chatEntry
	workingDir   string

	// LLM session naming (see session_title.go): titleNamed once the one-shot
	// naming call has been fired (or found unnecessary) for this chat.
	titleNamed bool

	input            []rune
	inputCursor      int
	inputCursorSet   bool
	inputAttachments []chatInputAttachment
	inputPastedTexts []chatInputPastedText
	nextImageID      int
	nextAudioID      int
	nextPastedTextID int
	promptHistory    []string
	promptCursor     int
	promptDraft      []rune
	promptActive     bool
	running          bool
	awaitingModel    bool
	compacting       bool
	// unattendedBackgroundRuns counts consecutive runs started by background
	// completions (chatBackgroundWakeMsg) with no user message in between,
	// capped by maxUnattendedBackgroundRuns.
	unattendedBackgroundRuns int
	cancel                   context.CancelFunc
	events                   <-chan tea.Msg
	compactEvents            <-chan tea.Msg
	detectedToolCalls        []chatEntry
	scroll                   int
	toolOutputMode           bool
	toolOutputOpen           bool
	flowPrintedLines         int
	thinking                 bool
	thinkingTokens           int
	compactingTokens         int
	contextTokens            int
	contextEstimate          bool
	// lastTurnMetrics carries the most recent chat POST's usage from
	// metrics events: input (full prompt) tokens, output tokens, and the
	// cached portion of the input when the server reported it.
	lastTurnMetrics      *api.Metrics
	modelPicker          *chatModelPicker
	sessionPicker        *chatModelPicker
	modelPickerModels    []ModelOption
	sessionPickerEntries []sessionListEntry
	thinkPicker          *chatThinkPicker
	promptDebug          *chatPromptDebug
	approvalPrompt       *chatApprovalPrompt
	approvalController   *chatApprovalController
	approvalState        *coreagent.ApprovalState
	cloudAuthPrompt      *cloudAuthPrompt
	pendingModel         string
	defaultAllowAll      bool
	defaultAutoReview    bool
	autoReviewer         *coreagent.AutoReviewer
	permissionNotice     string
	selection            chatSelection

	systemPromptDisabled bool

	width              int
	height             int
	status             string
	spinner            int
	tickActive         bool
	preloadingModel    string
	complete           int
	quitting           bool
	openModelOnInit    bool
	quitArmed          bool
	quitArmedKey       string
	escArmed           bool
	eventErrorRendered bool
	err                error
	store              *sessionstore.Store
	// side is the forked conversation shown in the split pane, and sideFocus
	// routes the keyboard to it. fork is a pane's inherited snapshot, used by
	// /clear to reset it.
	side      *chatModel
	sideFocus bool
	fork      []api.Message
	// pane marks a chatModel rendered as the side pane instead of owning the
	// terminal scrollback. route wraps a pane's async messages so the root
	// update loop can tell them apart.
	pane    bool
	focused bool
	route   func(tea.Msg) tea.Msg
	// mouseCaptured mirrors the terminal's mouse reporting state so the update
	// loop only emits mouse-mode commands when the UI's needs change.
	mouseCaptured bool
}

type chatInputAttachment struct {
	placeholder string
	kind        string
	data        api.ImageData
}

func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.RootDir == "" {
		opts.RootDir = opts.WorkingDir
	}

	// Kill background tasks when the UI exits so their processes cannot
	// outlive the session.
	defer func() { _ = opts.Tools.Close() }()

	approvalState := &coreagent.ApprovalState{}
	approvalState.SetMode(defaultPermissionMode(opts.AllowAllTools, opts.AutoReview))

	m := chatModel{
		ctx:               ctx,
		opts:              opts,
		chatID:            opts.ChatID,
		messages:          slices.Clone(opts.Messages),
		workingDir:        opts.WorkingDir,
		approvalState:     approvalState,
		defaultAllowAll:   opts.AllowAllTools,
		defaultAutoReview: opts.AutoReview,
		promptHistory:     initialPromptHistory(ctx, opts),
		store:             opts.Store,
		status:            "ready",
		openModelOnInit:   opts.OpenModelPicker || (strings.TrimSpace(opts.Model) == "" && opts.ModelOptions != nil),
	}
	m.nextImageID, m.nextAudioID = nextInputAttachmentIDsFromMessages(m.messages)
	m.nextPastedTextID = nextInputPastedTextIDFromMessages(m.messages)
	m.entries = entriesFromMessages(m.messages)
	// Context window is resolved post-load (chatModelPreloadDoneMsg) rather than
	// here: for local models /api/ps only reports the running num_ctx after the
	// model loads, and opts.ContextWindowTokens already holds Show's max as a
	// pre-load fallback. Refreshing now would just re-derive that same value
	// (and block construction on a network call).
	m.contextTokens = m.estimatePromptTokens(m.messages, "")
	m.initSession()
	m.contextEstimate = true
	if m.openModelOnInit {
		updated, cmd := m.openModelPicker("")
		if cmd != nil {
			return nil, errors.New("initial model picker returned an unexpected command")
		}
		m = updated.(chatModel)
	}
	if opts.PreloadModel != nil && strings.TrimSpace(opts.Model) != "" && !m.openModelOnInit {
		m.preloadingModel = strings.TrimSpace(opts.Model)
	}

	p := tea.NewProgram(m, tea.WithReportFocus())
	finalModel, err := p.Run()
	if err != nil {
		return nil, err
	}

	fm := finalModel.(chatModel)
	fm.dropSidePane()
	if fm.err != nil {
		return nil, fm.err
	}
	return &Result{ChatID: fm.chatID, Messages: fm.messages}, nil
}

func (m chatModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.preloadingModel != "" && m.opts.PreloadModel != nil {
		cmds = append(cmds, preloadModelCmd(m.ctx, m.opts.PreloadModel, m.preloadingModel, m.opts.Think), chatTickCmd())
	}
	if cmd := cloudModelPreflightCmd(m.ctx, m.opts, m.opts.Model, ""); cmd != nil && !m.openModelOnInit {
		cmds = append(cmds, cmd)
	}
	// Watch for background completions so a poll or task that finishes while
	// the chat idles can wake the model on its own.
	cmds = append(cmds, m.armBackgroundWatcher())
	return tea.Batch(cmds...)
}

// Update routes one message into the model and keeps mouse reporting in step
// with the UI. Capture is only needed for the split pane and the prompt debug
// screen, and holding it takes clicks away from the terminal, so it is off
// whenever neither is showing.
func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	switch model := next.(type) {
	case chatModel:
		return model.withMouseCapture(cmd)
	case *chatModel:
		return model.withMouseCapture(cmd)
	}
	return next, cmd
}

func (m chatModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.canEditInput() && isShiftEnterCSI(msg) {
		m.insertInputNewline()
		return m, nil
	}

	switch msg := msg.(type) {
	case chatSideMsg:
		return m.updateSide(msg.msg)

	case chatSideCloseMsg:
		return m.closeSidePane()

	case tea.WindowSizeMsg:
		wasSet := m.width > 0 || m.height > 0
		resized := wasSet && (m.width != msg.Width || m.height != msg.Height)
		m.width = msg.Width
		m.height = msg.Height
		if m.side != nil {
			_, sideWidth := splitPaneWidths(msg.Width)
			m.side.width = sideWidth
			m.side.height = msg.Height
			return m, nil
		}
		if resized {
			return m.withFlowTranscriptRepaint(nil)
		}
		return m.withFlowTranscriptFlush(nil)

	case tea.FocusMsg:
		return m, nil

	case chatTickMsg:
		m.tickActive = false
		if !m.running && !m.compacting && m.preloadingModel == "" {
			return m, nil
		}
		m.spinner++
		cmd := m.scheduleTick()
		return m, cmd

	case chatModelPreloadDoneMsg:
		if msg.model != "" && msg.model != m.preloadingModel {
			return m, nil
		}
		m.preloadingModel = ""
		if msg.err != nil {
			if isUnsupportedThinkingError(msg.err) && thinkRequestsThinking(m.opts.Think) {
				m.opts.Think = &api.ThinkValue{Value: false}
				m.status = fmt.Sprintf("Thinking disabled for %s", msg.model)
				if msg.model != "" && m.opts.PreloadModel != nil {
					m.preloadingModel = msg.model
					return m, tea.Batch(preloadModelCmd(m.ctx, m.opts.PreloadModel, msg.model, m.opts.Think), m.scheduleTick())
				}
				return m, nil
			}
			if !errors.Is(msg.err, context.Canceled) {
				m.status = "error"
				m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: fmt.Sprintf("Could not load model: %v", msg.err), err: msg.err.Error()}))
			}
			return m, nil
		}
		if msg.contextWindowTokens > 0 {
			m.updateContextWindowTokens(msg.contextWindowTokens)
		} else {
			m.refreshContextWindowTokens(msg.model)
		}
		return m, nil

	case chatAgentMsg:
		anchorBefore := m.scrollAnchorSnapshot()
		printedLines := m.flowPrintedLines
		var printedTranscript []string
		if printedLines > 0 {
			printedTranscript = slices.Clone(m.transcriptLines(m.viewWidth()))
		}
		m.applyAgentEvent(msg.event)
		m.anchorScrollToAppendedLines(anchorBefore)
		if m.scroll > 0 {
			return m, m.waitEvents(m.events)
		}
		return m.withFlowTranscriptRefreshAfter(printedTranscript, printedLines, m.waitEvents(m.events))

	case chatClipboardErrorMsg:
		if msg.err != nil {
			m.status = "copy failed: " + msg.err.Error()
		}
		return m, nil

	case chatClipboardCopiedMsg:
		m.status = fmt.Sprintf("copied last response (%d chars)", msg.chars)
		return m, nil

	case chatDiffViewerClosedMsg:
		// tea.ExecProcess restored the terminal without mouse reporting, so
		// declare it off and let the update loop put it back if a pane still
		// needs it.
		m.mouseCaptured = false
		if msg.err != nil {
			m.status = "diffview failed: " + msg.err.Error()
			return m, nil
		}
		m.status = "diffview closed"
		return m, nil

	case chatEditorClosedMsg:
		m.mouseCaptured = false
		if msg.err != nil {
			m.status = "editor failed: " + msg.err.Error()
			return m, nil
		}
		m.status = "editor closed"
		return m, nil

	case chatApprovalPromptMsg:
		// Full access may have been enabled while this request was in flight
		// (the user toggled it on after the agent sent the approval request
		// but before this buffered message was handled). In that window
		// togglePermissionMode's auto-resolve sees no open prompt yet, so
		// short-circuit here rather than surfacing a stale approval prompt.
		if m.allowAllToolsEnabled() {
			m.approvalPrompt = &chatApprovalPrompt{request: msg.request, reply: msg.reply}
			return m.resolveApprovalPrompt(chatApprovalChoice{allow: true, allowAll: true})
		}
		printedLines := m.flowPrintedLines
		var printedTranscript []string
		if printedLines > 0 {
			printedTranscript = slices.Clone(m.transcriptLines(m.viewWidth()))
		}
		m.modelPicker = nil
		m.modelPickerModels = nil
		m.openApprovalPrompt(msg)
		return m.withFlowTranscriptRefreshAfter(printedTranscript, printedLines, nil)

	case chatRunDoneMsg:
		wasCanceling := m.status == "canceling"
		m.finishThinkingEntry()
		m.running = false
		m.awaitingModel = false
		m.compacting = false
		m.compactingTokens = 0
		m.cancel = nil
		m.events = nil
		m.approvalController = nil
		m.thinking = false
		m.thinkingTokens = 0
		m.approvalPrompt = nil
		if msg.result != nil {
			m.messages = msg.result.Messages
			m.liveMessages = nil
			// A journaled run committed its projection already; a replayed
			// request changed nothing.
			if !msg.result.Committed && !msg.result.Replayed {
				m.persistRunResult(msg.result.Messages)
			}
			// Naming runs off the completed history whether or not this process
			// was the one that persisted it.
			m.maybeNameSession(msg.result.Messages)
			if msg.result.WorkingDir != "" {
				m.workingDir = msg.result.WorkingDir
			}
			// Context window is settled by preload (local num_ctx) or is
			// static (cloud); no refresh needed post-run.
			m.contextTokens = m.estimatePromptTokens(m.messages, "")
			m.contextEstimate = true
			if !messagesEndWithCompactionResult(m.messages) {
				m.applyResponseMetrics(&msg.result.Latest)
			}
		}
		if msg.result == nil {
			m.finishLiveMessagesForStoppedRun(msg.runEmitted, msg.persistedMessages)
		}
		if wasCanceling || isChatContextCanceledError(msg.err) {
			m.status = "Tell the model what to do instead."
			return m.flushAndArmBackground(nil)
		}
		if msg.err != nil {
			if !m.eventErrorRendered {
				m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: msg.err.Error(), err: msg.err.Error()}))
			}
			m.status = "error"
			return m.flushAndArmBackground(nil)
		}
		m.status = "ready"
		return m.flushAndArmBackground(nil)

	case chatBackgroundWakeMsg:
		// Background work produced new output while the chat was idle: start
		// a run so the model reacts now. The run's start drain renders the
		// notice (agent.Session.Run); the chat only decides whether to wake.
		if m.running || m.compacting {
			// The active run injects mid-run and re-arms the watcher when it
			// finishes (chatRunDoneMsg/finishManualCompaction).
			return m, nil
		}
		if m.modelPicker != nil || m.sessionPicker != nil || m.cloudAuthPrompt != nil || m.preloadingModel != "" || m.quitting {
			// Modal UI owns the chat right now; wait for the next change rather
			// than starting a run underneath it. Block on the *next* event
			// instead of arming generally: pending work left buffered would
			// re-fire immediately and busy-loop until the modal closes.
			return m, m.waitNextBackgroundChange()
		}
		watcher := m.backgroundWatcher()
		if watcher == nil {
			return m, nil
		}
		if !watcher.PendingBackground() {
			// Stale wake: a finishing run's end-of-run drain already reported
			// the completion. Keep watching.
			return m, m.armBackgroundWatcher()
		}
		if m.unattendedBackgroundRuns >= maxUnattendedBackgroundRuns {
			// Same blocking wait as the modal case above: pending work stays
			// buffered, and an immediate re-arm would busy-loop on it.
			m.status = fmt.Sprintf("background auto-runs paused after %d — send a message to resume", maxUnattendedBackgroundRuns)
			return m, m.waitNextBackgroundChange()
		}
		m.unattendedBackgroundRuns++
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "system", content: "Background task update — starting a run."}))
		return m.startRunWithMessages("", "", nil, "", "")

	case chatCompactDoneMsg:
		return m.finishManualCompaction(msg)

	case chatCompactProgressMsg:
		if msg.tokens > m.compactingTokens {
			m.compactingTokens = msg.tokens
		}
		return m, m.waitEvents(m.compactEvents)

	case chatEventsClosedMsg:
		if m.compacting {
			return m.Update(chatCompactDoneMsg{err: context.Canceled})
		}
		if m.running {
			return m.Update(chatRunDoneMsg{err: context.Canceled, runEmitted: true})
		}
		return m, nil

	case cloudAuthTickMsg:
		if m.cloudAuthPrompt != nil {
			return m.updateCloudAuthPrompt(msg)
		}
		return m, nil

	case cloudAuthCheckMsg:
		if m.cloudAuthPrompt != nil {
			return m.updateCloudAuthPrompt(msg)
		}
		return m, nil

	case cloudModelPreflightMsg:
		return m.updateCloudModelPreflight(msg)

	case cloudAuthPollMsg:
		if m.cloudAuthPrompt != nil {
			return m.updateCloudAuthPrompt(msg)
		}
		return m, nil

	case tea.MouseMsg:
		return m.updateMouse(msg)

	case tea.KeyMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m chatModel) View() string {
	if m.quitting {
		return ""
	}

	width, height := m.viewSize()

	if m.promptDebug != nil {
		return m.renderPromptDebug(width, height)
	}
	if m.modelPicker != nil && m.openModelOnInit {
		return m.renderModelPicker(width)
	}
	if m.sessionPicker != nil {
		return m.renderSessionPicker(width)
	}
	if m.cloudAuthPrompt != nil {
		return m.renderCloudAuthPrompt(width)
	}
	if m.thinkPicker != nil {
		return m.renderThinkPicker(width)
	}
	if m.side != nil {
		return m.splitView(width, height)
	}
	if m.scroll > 0 {
		return m.scrolledView(width, height)
	}
	return m.flowView(width)
}

// flushAndArmBackground bundles the transcript flush with re-arming the idle
// background watcher whenever the chat returns to idle (after a run or a
// compaction): the next buffered completion then wakes the model on its own.
func (m chatModel) flushAndArmBackground(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	next, flush := m.withFlowTranscriptFlush(cmd)
	return next, tea.Batch(flush, m.armBackgroundWatcher())
}

// backgroundWatcher returns the tool registry's BackgroundSource when it
// supports idle wake-ups (agent.BackgroundWatcher), else nil.
func (m *chatModel) backgroundWatcher() coreagent.BackgroundWatcher {
	watcher, ok := m.opts.Tools.BackgroundSource().(coreagent.BackgroundWatcher)
	if !ok {
		return nil
	}
	return watcher
}

// armBackgroundWatcher waits for the next buffered background completion.
// Work buffered between the last drain and now already missed its signal, so
// it resolves immediately instead.
func (m *chatModel) armBackgroundWatcher() tea.Cmd {
	if m.pane {
		return nil
	}
	watcher := m.backgroundWatcher()
	if watcher == nil {
		return nil
	}
	if watcher.PendingBackground() {
		return func() tea.Msg { return chatBackgroundWakeMsg{} }
	}
	return m.waitNextBackgroundChange()
}

// waitNextBackgroundChange blocks until a background task or poll buffers a
// completion after now. Unlike armBackgroundWatcher it never fires for work
// already buffered — callers on the defer/pause paths use it to avoid a
// busy-loop of immediate wake messages.
func (m *chatModel) waitNextBackgroundChange() tea.Cmd {
	watcher := m.backgroundWatcher()
	if watcher == nil {
		return nil
	}
	since := watcher.BackgroundVersion()
	ctx := m.ctx
	return func() tea.Msg {
		if _, err := watcher.WaitBackgroundChange(ctx, since); err != nil {
			// Chat is shutting down; no message to deliver.
			return nil
		}
		return chatBackgroundWakeMsg{}
	}
}

func (m chatModel) emptyChatHint() string {
	if m.pane {
		return "ask a side question; this chat inherited the conversation"
	}
	if len(chatEmptyPrompts) == 0 {
		return "Try asking the agent to inspect files, run tools, or explain a repo."
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(m.chatID))
	prompt := chatEmptyPrompts[int(h.Sum32())%len(chatEmptyPrompts)]
	return prompt
}

func (m chatModel) headerLines() []string {
	if m.pane {
		return m.paneHeaderLines(m.viewWidth())
	}
	return nil
}

func (m *chatModel) resetChat(status string) (tea.Model, tea.Cmd) {
	m.dropSidePane()
	m.messages = nil
	m.resetSession()
	m.liveMessages = nil
	m.entries = nil
	m.inputAttachments = nil
	m.inputPastedTexts = nil
	m.modelPicker = nil
	m.sessionPicker = nil
	m.sessionPickerEntries = nil
	m.modelPickerModels = nil
	m.approvalController = nil
	m.resetApprovalState()
	m.nextImageID = 0
	m.nextAudioID = 0
	m.nextPastedTextID = 1
	m.resetPromptHistoryCursor()
	m.resetWorkingDir()
	m.opts.AllowAllTools = m.defaultAllowAll
	m.opts.AutoReview = m.defaultAutoReview
	m.autoReviewer = nil
	m.permissionNotice = ""
	m.thinking = false
	m.thinkingTokens = 0
	m.contextTokens = 0
	m.contextEstimate = true
	m.lastTurnMetrics = nil
	m.scroll = 0
	m.flowPrintedLines = 0
	// A new chat starts unnamed even if the launcher passed --name for the
	// previous session; /new should not inherit it.
	m.opts.Name = ""
	m.status = status
	return *m, tea.ClearScreen
}

func (m *chatModel) resetWorkingDir() {
	if m.opts.RootDir != "" {
		m.workingDir = m.opts.RootDir
		return
	}
	m.workingDir = m.opts.WorkingDir
}

// copyLastResponse copies the most recent assistant message to the clipboard.
func (m *chatModel) copyLastResponse() (tea.Model, tea.Cmd) {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].role == "assistant" && strings.TrimSpace(m.entries[i].content) != "" {
			return *m, copyCountedCmd(m.ctx, m.entries[i].content)
		}
	}
	m.status = "nothing to copy"
	return *m, nil
}
