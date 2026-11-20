package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/internal/modelref"
)

type ChatClient interface {
	Chat(context.Context, *api.ChatRequest, api.ChatResponseFunc) error
}

type Session struct {
	Client           ChatClient
	EventSinks       []EventSink
	Tools            *Registry
	Skills           *SkillCatalog
	DisableTools     bool
	ApprovalPrompter ApprovalPrompter
	ApprovalState    *ApprovalState
	WorkingDir       string
	Compactor        Compactor
	// SupportsImages reports whether the active model accepts image input.
	// It is forwarded to tools via ToolContext so tools like read can return
	// image files as image data on the tool result message.
	SupportsImages bool
	// Background, when set, is drained for finished background tasks at run
	// boundaries: completions are injected into the conversation so the
	// model sees (and can react to) them.
	Background BackgroundSource
	// Journal, when set, durably admits runs before work begins, journals
	// tool intent and outcome, and commits terminal state before terminal
	// events publish. A nil Journal keeps the pre-journal behavior unchanged.
	Journal RunJournal
}

type RunOptions struct {
	ChatID       string
	Model        string
	SystemPrompt string
	Messages     []api.Message
	NewMessages  []api.Message
	Format       string
	Options      map[string]any
	Think        *api.ThinkValue
	KeepAlive    *api.Duration
	// SkillName loads a catalog skill as an ordered synthetic tool call/result
	// before the first model request for this run.
	SkillName string
	// RequestID lets pipe/headless callers deduplicate retried prompts: the
	// same request ID with the same input replays the committed run, a
	// changed input conflicts. Empty for legacy callers, which get no
	// retry-deduplication guarantee.
	RequestID string
	// MaxToolRounds limits consecutive model/tool cycles. A positive value is
	// an explicit limit. Zero selects the model-specific default: local models
	// use the default guard and cloud models are unlimited. A negative value
	// disables the guard for tests or special callers.
	MaxToolRounds int
}

type RunResult struct {
	Messages   []api.Message
	Latest     api.ChatResponse
	WorkingDir string
	// Replayed reports a deduplicated request: a committed run with the same
	// request ID and input already exists, so no model or tool work ran.
	Replayed bool
	// Committed reports that the journal durable-committed the terminal run
	// state and history; callers can skip their own message sync.
	Committed bool
}

const (
	defaultMaxToolRounds              = 300
	maxToolResultRunes                = 60000
	smallContextToolResultRunes       = 6000
	tinyContextToolResultRunes        = 3200
	smallContextToolResultTokenWindow = 8192
	tinyContextToolResultTokenWindow  = 4096
	toolTruncationMarkerReserveTokens = 64
	toolOutputFullOmissionPrefix      = "[tool output truncated: output omitted because the context is full]"
)

type toolOutputOverflow struct {
	toolName   string
	toolCallID string
	content    string
}

type toolBatchResult struct {
	messages  []api.Message
	stop      toolExecutionStop
	overflows []toolOutputOverflow
}

// toolExecutionStop is the batch-level outcome for a group of tool calls,
// distinct from per-call Event.Status values. The values overlap with
// runFinish.status ("denied", "canceled") because a denied or canceled
// batch also terminates the run with the matching status.
type toolExecutionStop string

const (
	toolExecutionDenied   toolExecutionStop = "denied"
	toolExecutionCanceled toolExecutionStop = "canceled"
)

// maxBackgroundContinuations caps how many times a single run can extend
// itself to report background task completions. Each completion is reported
// exactly once so continuations terminate on their own; the cap only guards
// against pathological crash-loops, and anything still buffered past the
// cap surfaces at the next run's start drain.
const maxBackgroundContinuations = 8

const toolExecutionDisabledMessage = "Tool execution disabled."

type runPhase int

const (
	runPhaseModel runPhase = iota
	runPhaseTools
	runPhaseCompact
	runPhaseDone
)

// run is the first-class per-run object that owns the model/tool/compact
// loop. It borrows a *Session for configuration and collaborators (client,
// tools, compactor, event sinks, approval) while holding all mutable per-run
// state. Keeping the loop on run (rather than on Session methods passed a
// state struct) clarifies ownership and makes "one Session, many runs"
// straightforward: each run carries its own phase, messages, and budget.
type run struct {
	session *Session

	runID string
	opts  RunOptions

	phase runPhase

	messages []api.Message
	latest   api.ChatResponse

	budget PromptBudget

	assistant        api.Message
	pendingToolCalls []api.ToolCall
	canceled         bool

	toolBatch *toolBatchResult

	consecutiveModelErrors  int
	modelRetries            int
	toolRounds              int
	maxToolRounds           int
	backgroundContinuations int
	compactionSkipNotified  bool

	finish runFinish

	// admitted is set when the journal durably admitted this run.
	admitted *RunAdmissionResult
	// blockedTools refuses re-execution of a prior attempt's
	// unknown-outcome tool calls, keyed by ToolArgsKey.
	blockedTools map[string]BlockedTool
	// deniedTools refuses re-asking about calls a human already denied in
	// this run, keyed by ToolArgsKey; the value is the model-facing denial
	// text. Denials keep the run alive, so a model that retries the same
	// call must get the refusal back instead of another prompt.
	deniedTools map[string]string
	// rawBaseline is the index into r.messages where the current unflushed raw
	// segment starts; compaction and run end flush each segment through the
	// journal's append-only record.
	rawBaseline int
}

type runFinish struct {
	status         RunStatus
	ignoreCanceled bool
	err            error
}

func (r *run) finishDone() {
	r.finish = runFinish{status: RunStatusDone}
	r.phase = runPhaseDone
}

func (r *run) finishDenied() {
	r.finish = runFinish{status: RunStatusDenied}
	r.phase = runPhaseDone
}

func (r *run) finishCanceled() {
	r.finish = runFinish{status: RunStatusCanceled, ignoreCanceled: true}
	r.phase = runPhaseDone
}

func (r *run) finishError(err error) {
	r.finish = runFinish{err: err}
	r.phase = runPhaseDone
}

func (s *Session) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if err := s.validateRun(opts); err != nil {
		return nil, err
	}
	if s.ApprovalState == nil {
		s.ApprovalState = &ApprovalState{}
	}
	runID := uuid.NewString()

	// Durable admission: commit the run identity (and dedupe by request ID)
	// before any model or tool work begins. A failed admission commit never
	// starts work.
	var admitted *RunAdmissionResult
	if s.Journal != nil && opts.ChatID != "" {
		res, err := s.Journal.AdmitRun(RunAdmission{
			ChatID:       opts.ChatID,
			RequestID:    opts.RequestID,
			Model:        opts.Model,
			SystemPrompt: opts.SystemPrompt,
			Skill:        opts.SkillName,
			Format:       opts.Format,
			Options:      opts.Options,
			Think:        opts.Think,
			Prompt:       runAdmissionPrompt(opts),
		})
		if err != nil {
			s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
			return nil, err
		}
		if res.Replayed {
			meta := newEventMetadata(res.RunID, opts)
			_ = s.emit(newRunReplayed(meta, res.Status))
			_ = s.emitIgnoringCanceled(ctx, newRunFinished(meta, RunStatus(res.Status)))
			return &RunResult{Messages: opts.Messages, Replayed: true}, nil
		}
		if res.RunID != "" {
			runID = res.RunID
		}
		admitted = &res
	}

	budget := s.runBudget(opts)
	messages, err := s.buildRunMessages(ctx, runID, opts, budget)
	if err != nil {
		s.finishUnstartedRun(admitted, runID, opts.ChatID, err)
		return nil, err
	}
	// The raw record for this run starts at the boundary between prior
	// history and this run's additions (new prompt, skill activation,
	// notices, and everything the run adds afterward).
	rawBaseline := len(opts.Messages)
	if admitted == nil {
		rawBaseline = 0
	}
	activatedSkill, err := s.activateSkill(ctx, runID, opts)
	if err != nil {
		s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
		s.finishUnstartedRun(admitted, runID, opts.ChatID, err)
		return nil, err
	}
	if len(activatedSkill) > 0 {
		messages = append(messages, activatedSkill...)
		if err := s.checkPreflightPromptBudget(budget, opts, messages); err != nil {
			s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
			s.finishUnstartedRun(admitted, runID, opts.ChatID, err)
			return nil, err
		}
	}

	// Idle drain: report background tasks that finished while no run was
	// active, ahead of the new turn so the model sees them in context.
	if notice := s.backgroundNotice(); notice != "" {
		messages = append(messages, api.Message{Role: "user", Content: notice})
		// Best-effort: a dropped render must not fail the run.
		_ = s.emit(newBackgroundTasks(newEventMetadata(runID, opts), notice))
	}

	r := &run{
		session:       s,
		runID:         runID,
		opts:          opts,
		phase:         runPhaseModel,
		messages:      messages,
		budget:        budget,
		maxToolRounds: resolvedMaxToolRounds(opts.Model, opts.MaxToolRounds),
		rawBaseline:   rawBaseline,
		deniedTools:   make(map[string]string),
	}
	if admitted != nil {
		r.admitted = admitted
		r.blockedTools = make(map[string]BlockedTool, len(admitted.BlockedTools))
		for _, blocked := range admitted.BlockedTools {
			r.blockedTools[blocked.ArgsKey] = blocked
		}
		// Admission is committed; publishing acceptance now is safe.
		_ = s.emit(newRunAdmitted(newEventMetadata(runID, opts), admitted.Recovered))
	}
	// Composing tools (codemode) get a nested executor for this run, whether
	// or not this run is journaled.
	defer s.uninstallNestedExecutor()
	s.installNestedExecutor(nestedRunIDs{
		runID:   runID,
		meta:    newEventMetadata(runID, opts),
		blocked: r.blockedTools,
		denied:  r.deniedTools,
	})
	return r.run(ctx)
}

// run is the canonical loop driver: it dispatches the model -> tools ->
// compact phases until the run reaches a terminal state, then returns the
// accumulated result. The loop and phase orchestration live here rather than
// on Session so that per-run state is owned by the Run itself.
func (r *run) run(ctx context.Context) (*RunResult, error) {
	for {
		r.injectPendingBackgroundEvents()
		var err error
		switch r.phase {
		case runPhaseModel:
			err = r.runModelStep(ctx)
		case runPhaseTools:
			err = r.runToolStep(ctx)
		case runPhaseCompact:
			err = r.runCompactionStep(ctx)
		case runPhaseDone:
			return r.finishRun(ctx)
		}
		if err != nil {
			r.abort(ctx, err)
		}
	}
}

// abort settles a run whose step failed so it still reaches finishRun: an
// admitted run must commit a terminal state, or its run lock stays held by
// this process and every later prompt in the session is rejected as busy.
// Tool calls left unanswered by the failed step get synthetic results so the
// committed history stays protocol-valid.
func (r *run) abort(ctx context.Context, err error) {
	r.messages = append(r.messages, unansweredToolResults(r.messages, "Tool execution aborted: "+err.Error())...)
	if isContextCanceledError(ctx, err) {
		r.finishCanceled()
		return
	}
	r.finishError(err)
}

// unansweredToolResults returns a tool result for every tool call in the
// trailing assistant turn that has no result yet.
func unansweredToolResults(messages []api.Message, content string) []api.Message {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			last = i
			break
		}
	}
	if last < 0 || len(messages[last].ToolCalls) == 0 {
		return nil
	}
	answered := make(map[string]bool)
	for _, msg := range messages[last+1:] {
		if msg.Role == "tool" {
			answered[msg.ToolCallID] = true
		}
	}
	var out []api.Message
	for _, call := range messages[last].ToolCalls {
		if !answered[call.ID] {
			out = append(out, toolMessage(call.Function.Name, call.ID, content))
		}
	}
	return out
}

// validateRun checks the preconditions for a run.
func (s *Session) validateRun(opts RunOptions) error {
	if s == nil {
		return errors.New("nil session")
	}
	if s.Client == nil {
		return errors.New("agent session requires a chat client")
	}
	if opts.Model == "" {
		return errors.New("agent session requires a model")
	}
	return nil
}

// buildRunMessages sanitizes the provided message history, runs the preflight
// prompt-budget check, and returns the initial message list for the run. It
// emits an EventError and returns it if the preflight check fails.
func (s *Session) buildRunMessages(ctx context.Context, runID string, opts RunOptions, budget PromptBudget) ([]api.Message, error) {
	messages := make([]api.Message, 0, len(opts.Messages)+len(opts.NewMessages))
	for _, msg := range opts.Messages {
		messages = append(messages, sanitizeMessageForRun(msg))
	}
	for _, msg := range opts.NewMessages {
		msg = sanitizeMessageForRun(msg)
		messages = append(messages, msg)
	}

	if err := s.checkPreflightPromptBudget(budget, opts, messages); err != nil {
		s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
		return nil, err
	}
	return messages, nil
}

// runBudget builds the per-run PromptBudget from the compactor's configured
// capacity (resolved against runtime options) and binds the run's prompt
// shape (system prompt, tools, format) so estimation needs only messages. A
// nil compactor yields a budget with the prompt shape bound but a zero context
// window, which disables sizing, matching the previous behavior.
// ResolveCompactionThreshold is applied so a zero configured threshold falls
// back to the default fraction, preserving the former compactionThresholdTokens
// behavior relied on by compactor implementations that report 0.
func (s *Session) runBudget(opts RunOptions) PromptBudget {
	if s.Compactor == nil {
		return NewPromptBudget(0, 0, opts.SystemPrompt, s.availableTools(), opts.Format)
	}
	return NewPromptBudget(s.Compactor.ContextWindowTokens(opts.Options), ResolveCompactionThreshold(s.Compactor.Threshold()), opts.SystemPrompt, s.availableTools(), opts.Format)
}

func (r *run) runModelStep(ctx context.Context) error {
	opts := r.opts
	meta := newEventMetadata(r.runID, opts)

	assistant, pendingToolCalls, canceled, err := r.session.chatRound(ctx, r.runID, opts, r.messages, &r.latest)
	if err != nil {
		var statusErr api.StatusError
		switch {
		case errors.As(err, &statusErr) && isToolCallParseError(statusErr) && r.consecutiveModelErrors < 2:
			// The model produced output the server could not parse; tell it so
			// it can correct itself, rather than resending the same request.
			r.consecutiveModelErrors++
			r.messages = append(r.messages, api.Message{
				Role:    "user",
				Content: fmt.Sprintf("Your previous response caused an error: %s\n\nPlease try again with a valid response.", statusErr.ErrorMessage),
			})
			return nil
		case messageEmpty(assistant) && isTransientModelError(err) && r.modelRetries < len(modelRetryDelays):
			// Nothing streamed, so the same request can be resent without
			// leaving partial output behind in any UI.
			delay := modelRetryDelays[r.modelRetries]
			r.modelRetries++
			_ = r.session.emit(newModelRetry(meta, r.modelRetries, delay, err.Error()))
			select {
			case <-time.After(delay):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r.session.emit(newErrorEvent(meta, err.Error()))
		return err
	}
	r.consecutiveModelErrors = 0
	r.modelRetries = 0
	r.assistant = assistant
	r.pendingToolCalls = pendingToolCalls
	r.canceled = canceled

	if !messageEmpty(assistant) {
		r.messages = append(r.messages, assistant)
	}

	if len(pendingToolCalls) == 0 {
		r.toolBatch = nil
		r.phase = runPhaseCompact
		return nil
	}

	if canceled {
		skipped, skipErr := r.session.skipToolCalls(ctx, r.runID, opts, r.budget, r.messages, pendingToolCalls, toolSkippedCanceled)
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, skipped...)
		r.finishCanceled()
		return nil
	}

	if r.session.DisableTools {
		batch, skipErr := r.session.disabledToolCalls(ctx, r.runID, opts, r.budget, r.messages, pendingToolCalls)
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, batch.messages...)
		r.toolBatch = &batch
		r.phase = runPhaseCompact
		return nil
	}

	if r.session.Tools == nil {
		r.finishDone()
		return nil
	}

	if r.maxToolRounds >= 0 && r.toolRounds >= r.maxToolRounds {
		content := fmt.Sprintf("Tool execution skipped because the max tool-round limit of %d was reached. Send another message to continue.", r.maxToolRounds)
		toolMessages, skipErr := r.session.skipToolCalls(ctx, r.runID, opts, r.budget, r.messages, pendingToolCalls, content)
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, toolMessages...)
		err := fmt.Errorf("tool round limit reached after %d rounds; send another message to continue", r.maxToolRounds)
		r.session.emit(newErrorEvent(meta, err.Error()))
		r.finishError(err)
		return nil
	}

	r.phase = runPhaseTools
	return nil
}

// modelRetryDelays is the backoff between resends of a model request that
// failed transiently; its length caps the retries per model step.
var modelRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// isToolCallParseError reports a server-side failure to parse the model's
// tool-call output. Resending the same request would likely fail the same
// way, so the model is asked to correct itself instead.
func isToolCallParseError(err api.StatusError) bool {
	if err.StatusCode < 500 {
		return false
	}
	msg := strings.ToLower(err.ErrorMessage)
	return strings.Contains(msg, "tool call") && (strings.Contains(msg, "pars") || strings.Contains(msg, "invalid"))
}

// isTransientModelError reports failures worth resending unchanged: server
// errors, rate limits, timeouts, and dropped connections.
func isTransientModelError(err error) bool {
	var statusErr api.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return statusErr.StatusCode >= 500
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (r *run) runToolStep(ctx context.Context) error {
	batch, err := r.session.executeToolCalls(ctx, r.runID, r.opts, r.budget, r.messages, r.pendingToolCalls, r.blockedTools, r.deniedTools)
	if err != nil {
		r.session.emit(newErrorEvent(newEventMetadata(r.runID, r.opts), err.Error()))
		return err
	}

	r.messages = append(r.messages, batch.messages...)
	r.toolBatch = &batch
	r.phase = runPhaseCompact
	return nil
}

func (r *run) runCompactionStep(ctx context.Context) error {
	opts := r.opts
	meta := newEventMetadata(r.runID, opts)
	var err error
	var compacted bool
	var compactedMessages []api.Message
	if r.toolBatch != nil && len(r.toolBatch.overflows) > 0 {
		compactedMessages, compacted, r.compactionSkipNotified, err = r.session.compactForToolOutputOverflow(ctx, r.runID, opts, r.budget, r.messages, r.latest, r.assistant, r.toolBatch.messages, r.toolBatch.overflows, r.compactionSkipNotified)
	} else {
		compactedMessages, compacted, r.compactionSkipNotified, err = r.session.maybeCompact(ctx, r.runID, opts, r.budget, r.messages, r.latest, r.compactionSkipNotified)
	}
	if compacted {
		// The pre-compaction span of this run becomes its own raw segment;
		// compaction rewrote the message list but not the append-only record.
		if err := r.flushRawSegment(); err != nil {
			r.session.emit(newErrorEvent(meta, err.Error()))
			r.finishError(err)
			return nil
		}
	}
	r.messages = compactedMessages
	if compacted {
		// The compacted projection is not raw history; the next segment
		// starts after it.
		r.rawBaseline = len(r.messages)
	}
	if err != nil {
		r.session.emit(newErrorEvent(meta, err.Error()))
		r.finishError(err)
		return nil
	}

	if r.toolBatch == nil {
		if r.canceled {
			r.finishCanceled()
			return nil
		}
		// End-of-run drain: report tasks that completed during the run and
		// take one more model step so the model can react, rather than
		// leaving the completion invisible until the next turn. The drain
		// reports each completion once and continuations are capped, so this
		// terminates; anything past the cap stays buffered for the next run's
		// start drain.
		if r.backgroundContinuations < maxBackgroundContinuations {
			if notice := r.session.backgroundNotice(); notice != "" {
				r.messages = append(r.messages, api.Message{Role: "user", Content: notice})
				_ = r.session.emit(newBackgroundTasks(newEventMetadata(r.runID, r.opts), notice))
				r.backgroundContinuations++
				r.assistant = api.Message{}
				r.phase = runPhaseModel
				return nil
			}
		}
		r.finishDone()
		return nil
	}

	switch r.toolBatch.stop {
	case toolExecutionDenied:
		r.finishDenied()
	case toolExecutionCanceled:
		r.finishCanceled()
	default:
		r.toolRounds++
		r.assistant = api.Message{}
		r.pendingToolCalls = nil
		r.toolBatch = nil
		r.phase = runPhaseModel
	}
	return nil
}

func (r *run) finishRun(ctx context.Context) (*RunResult, error) {
	result := &RunResult{Messages: r.messages, Latest: r.latest, WorkingDir: r.session.WorkingDir}
	var commitSeq int64
	if r.admitted != nil && r.session.Journal != nil {
		if err := r.flushRawSegment(); err != nil {
			// The raw record is incomplete, so the run cannot finish cleanly,
			// but it must still reach a terminal state to release its lock.
			_, _ = r.session.Journal.CommitRun(RunTerminal{
				RunID:   r.runID,
				ChatID:  r.opts.ChatID,
				Status:  RunStatusFailed,
				Err:     err.Error(),
				History: r.messages,
			})
			return result, err
		}
		commit, err := r.session.Journal.CommitRun(RunTerminal{
			RunID:   r.runID,
			ChatID:  r.opts.ChatID,
			Status:  journalRunStatus(r.finish.status),
			Err:     finishErrString(r.finish.err),
			History: r.messages,
		})
		if err != nil {
			// Terminal state is not committed; no terminal event publishes,
			// so a client cannot mistake a failed commit for a finished run.
			return result, fmt.Errorf("commit run: %w", err)
		}
		result.Committed = true
		commitSeq = commit.Seq
	}
	if r.finish.status != "" {
		event := newRunFinished(newEventMetadata(r.runID, r.opts), r.finish.status)
		var err error
		if r.finish.ignoreCanceled {
			err = r.session.emitIgnoringCanceled(ctx, event)
		} else {
			err = r.session.emit(event)
		}
		if err != nil {
			return result, err
		}
	}
	if result.Committed {
		// Settled notification, published only after the terminal commit.
		_ = r.session.emitIgnoringCanceled(ctx, newRunCommitted(newEventMetadata(r.runID, r.opts), commitSeq))
	}
	return result, r.finish.err
}

// finishErrString renders a run's terminal error for the journal row.
func finishErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runAdmissionPrompt extracts the admitted user-facing prompt: the last
// non-empty user message in NewMessages, or the skill invocation form.
func runAdmissionPrompt(opts RunOptions) string {
	for i := len(opts.NewMessages) - 1; i >= 0; i-- {
		if opts.NewMessages[i].Role == "user" && strings.TrimSpace(opts.NewMessages[i].Content) != "" {
			return strings.TrimSpace(opts.NewMessages[i].Content)
		}
	}
	if opts.SkillName != "" {
		return "/" + opts.SkillName
	}
	return ""
}

// finishUnstartedRun commits a terminal-failed state for an admission whose
// preflight checks failed before any model or tool work began.
func (s *Session) finishUnstartedRun(admitted *RunAdmissionResult, runID, chatID string, err error) {
	if admitted == nil || s == nil || s.Journal == nil {
		return
	}
	_, _ = s.Journal.CommitRun(RunTerminal{RunID: runID, ChatID: chatID, Status: RunStatusFailed, Err: err.Error()})
}

// injectPendingBackgroundEvents delivers background work that finished (or
// polls that published) while the run was in flight: when the source pushes
// a signal, buffered completions are drained into a synthetic user notice
// ahead of the next model request, so the agent reacts mid-run instead of
// only at run boundaries. Injection happens only before a model
// step: at that point the message tail is turn-complete and appending a user
// notice is protocol-safe — during tool execution the tail is an assistant
// message with unanswered tool calls. Signals are capacity-1 coalesced and
// never consumed without draining, so a signal observed during a non-model
// phase waits for the next model step.
func (r *run) injectPendingBackgroundEvents() {
	if r.phase != runPhaseModel || r.session.Background == nil {
		return
	}
	signaler, ok := r.session.Background.(BackgroundSignaler)
	if !ok {
		return
	}
	select {
	case <-signaler.BackgroundSignals():
	default:
		return
	}
	if notice := r.session.backgroundNotice(); notice != "" {
		r.messages = append(r.messages, api.Message{Role: "user", Content: notice})
		// Best-effort like the boundary drains: a dropped render must not
		// fail the run.
		_ = r.session.emit(newBackgroundTasks(newEventMetadata(r.runID, r.opts), notice))
	}
}

// backgroundNotice drains pending background task completions and renders
// them as one synthetic user message. Empty when no source is set or nothing
// finished. Completions are removed from the source by the drain, so each is
// reported exactly once.
func (s *Session) backgroundNotice() string {
	if s == nil || s.Background == nil {
		return ""
	}
	completions := s.Background.DrainCompletions()
	if len(completions) == 0 {
		return ""
	}
	return formatBackgroundNotice(completions)
}

// formatBackgroundNotice renders completion entries plus bounded failure
// tails as a system-attributed notice.
func formatBackgroundNotice(completions []BackgroundCompletion) string {
	var sb strings.Builder
	sb.WriteString("[background task update — system notice, not a user message]\n")
	for _, c := range completions {
		if c.Poll {
			formatPollNotice(&sb, c)
			continue
		}
		var status string
		switch {
		case c.Killed:
			status = "killed"
		case c.Failure != "":
			status = "failed to run: " + c.Failure
		case c.ExitCode == 0:
			status = "finished: exit 0"
		default:
			status = fmt.Sprintf("failed: exit %d", c.ExitCode)
		}
		fmt.Fprintf(&sb, "\n%s: %s after %s — %q\n  log: %s\n",
			c.ID, status, formatBackgroundTaskDuration(c.Duration), backgroundCommandSummary(c.Command), c.LogPath)
		if c.SessionID != "" {
			fmt.Fprintf(&sb, "  o session %s: after the child exits, follow up with o --resume-id %s --headless --allow-all-tools \"follow-up\"\n", c.SessionID, c.SessionID)
		}
		if tail := strings.TrimRight(c.Tail, "\n"); tail != "" {
			sb.WriteString("  log tail:\n")
			for _, line := range strings.Split(tail, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatPollNotice(sb *strings.Builder, c BackgroundCompletion) {
	var status string
	switch {
	case c.Failure != "":
		status = "tick failed: " + c.Failure
	case c.ExitCode != 0:
		status = fmt.Sprintf("tick command exited %d", c.ExitCode)
	default:
		status = "new output"
	}
	fmt.Fprintf(sb, "\n%s tick %d: %s after %s — %q\n  log: %s\n",
		c.ID, c.Tick, status, formatBackgroundTaskDuration(c.Duration), backgroundCommandSummary(c.Command), c.LogPath)
	if output := strings.TrimRight(c.Tail, "\n"); output != "" {
		sb.WriteString("  output:\n")
		for _, line := range strings.Split(output, "\n") {
			sb.WriteString("    " + line + "\n")
		}
	}
}

func backgroundCommandSummary(command string) string {
	summary := strings.Join(strings.Fields(command), " ")
	runes := []rune(summary)
	if len(runes) > 60 {
		summary = string(runes[:60]) + "…"
	}
	return summary
}

func formatBackgroundTaskDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func (s *Session) chatRound(ctx context.Context, runID string, opts RunOptions, messages []api.Message, latest *api.ChatResponse) (api.Message, []api.ToolCall, bool, error) {
	meta := newEventMetadata(runID, opts)
	var tools api.Tools
	if !s.DisableTools {
		tools = s.availableTools()
	}
	req := buildChatRequest(opts, messages, tools)

	assistant := api.Message{Role: "assistant"}
	var pendingToolCalls []api.ToolCall

	err := s.Client.Chat(ctx, &req, func(response api.ChatResponse) error {
		if response.Message.Role != "" {
			assistant.Role = response.Message.Role
		}

		if messageEmpty(response.Message) {
			*latest = response
			return nil
		}

		if response.Message.Thinking != "" {
			assistant.Thinking += response.Message.Thinking
			if err := s.emit(newThinkingDelta(meta, response.Message.Thinking)); err != nil {
				return err
			}
		}

		if response.Message.Content != "" {
			assistant.Content += response.Message.Content
			if err := s.emit(newMessageDelta(meta, response.Message.Content)); err != nil {
				return err
			}
		}

		if len(response.Message.ToolCalls) > 0 {
			assistant.ToolCalls = append(assistant.ToolCalls, response.Message.ToolCalls...)
			pendingToolCalls = append(pendingToolCalls, response.Message.ToolCalls...)
			if err := s.emit(newToolCallDetected(meta, response.Message.ToolCalls)); err != nil {
				return err
			}
		}

		*latest = response
		return nil
	})
	if err != nil {
		if isContextCanceledError(ctx, err) {
			return assistant, pendingToolCalls, true, nil
		}
		return assistant, pendingToolCalls, false, err
	}

	// Report the round's usage to UIs: the final streaming chunk carries the
	// Ollama metrics (input tokens, output tokens, and cached prompt tokens
	// when the server reports them). Skip rounds with no usage at all —
	// e.g. cloud models that report nothing — so silent UIs stay silent.
	if metrics := latest.Metrics; hasUsageMetrics(metrics) {
		_ = s.emit(newMetricsEvent(meta, &metrics))
	}

	return assistant, pendingToolCalls, false, nil
}

// hasUsageMetrics reports whether the final streaming chunk carried any
// token counts. Metrics that only measure wall-clock durations still count:
// input/output token display is the point, so require at least one count.
func hasUsageMetrics(metrics api.Metrics) bool {
	return metrics.PromptEvalCount > 0 || metrics.EvalCount > 0 ||
		(metrics.PromptEvalCachedCount != nil && *metrics.PromptEvalCachedCount > 0)
}

func buildChatRequest(opts RunOptions, messages []api.Message, tools api.Tools) api.ChatRequest {
	requestMessages := sanitizeMessagesForRequest(messages)
	if strings.TrimSpace(opts.SystemPrompt) != "" {
		withSystem := make([]api.Message, 0, len(requestMessages)+1)
		withSystem = append(withSystem, api.Message{Role: "system", Content: opts.SystemPrompt})
		requestMessages = append(withSystem, requestMessages...)
	}

	format := opts.Format
	if format == "json" {
		format = `"` + format + `"`
	}

	req := api.ChatRequest{
		Model:    opts.Model,
		Messages: requestMessages,
		Format:   json.RawMessage(format),
		Options:  opts.Options,
		Think:    opts.Think,
	}
	if opts.KeepAlive != nil {
		req.KeepAlive = opts.KeepAlive
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	return req
}

func (s *Session) currentWorkingDir() string {
	if s.WorkingDir != "" {
		return s.WorkingDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	s.WorkingDir = wd
	return s.WorkingDir
}

func (s *Session) applyToolWorkingDir(next string) bool {
	next = strings.TrimSpace(next)
	if next == "" {
		return false
	}
	current := s.currentWorkingDir()
	nextAbs, err := canonicalSessionPath(next)
	if err != nil {
		return false
	}
	if current == nextAbs {
		return false
	}
	s.WorkingDir = nextAbs
	return true
}

func canonicalSessionPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return abs, nil
}

func isContextCanceledError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled) && strings.Contains(err.Error(), "context canceled")
}

func (s *Session) maybeCompact(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, skipNotified bool) ([]api.Message, bool, bool, error) {
	if s.Compactor == nil {
		return messages, false, skipNotified, nil
	}
	req := s.compactionRequest(runID, opts, budget, messages, latest)
	trigger := s.autoCompactionTrigger(req)
	if trigger != "" {
		s.emitCompactionStarted(runID, opts, trigger)
	}
	result, err := s.Compactor.MaybeCompact(ctx, req)
	if err != nil {
		if result.Due && !skipNotified {
			if trigger == "" {
				trigger = CompactionTriggerError
			}
			s.emitCompactionSkipped(runID, opts, trigger, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			if trigger == "" {
				trigger = CompactionTriggerDue
			}
			s.emitCompactionSkipped(runID, opts, trigger, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
	}
	s.emitCompacted(runID, opts, result.Messages, trigger, result.Summary)
	if err := s.recordCompaction(runID, opts.ChatID, trigger, result.Summary); err != nil {
		return result.Messages, true, skipNotified, err
	}
	if err := s.checkPostCompactionPromptBudget(budget, opts, result.Messages); err != nil {
		return result.Messages, true, skipNotified, err
	}
	return result.Messages, true, skipNotified, nil
}

func (s *Session) compactForToolOutputOverflow(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, assistant api.Message, toolMessages []api.Message, overflows []toolOutputOverflow, skipNotified bool) ([]api.Message, bool, bool, error) {
	if s.Compactor == nil {
		return messages, false, skipNotified, nil
	}

	keepUserTurns := 0
	req := s.compactionRequest(runID, opts, budget, messages, latest)
	req.Force = true
	req.KeepUserTurns = &keepUserTurns
	s.emitCompactionStarted(runID, opts, CompactionTriggerToolOutput)

	result, err := s.Compactor.MaybeCompact(ctx, req)
	if err != nil {
		if result.Due && !skipNotified {
			s.emitCompactionSkipped(runID, opts, CompactionTriggerToolOutput, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			s.emitCompactionSkipped(runID, opts, CompactionTriggerToolOutput, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
	}

	overflowByID := make(map[string]toolOutputOverflow, len(overflows))
	for _, overflow := range overflows {
		overflowByID[overflow.toolCallID] = overflow
	}

	compacted := append([]api.Message(nil), result.Messages...)
	if !messageEmpty(assistant) {
		compacted = append(compacted, assistant)
	}

	historyTokens := budget.Estimate(compacted)
	batchTokens := 0
	for _, msg := range toolMessages {
		content := msg.Content
		toolName := msg.ToolName
		if overflow, ok := overflowByID[msg.ToolCallID]; ok {
			content = overflow.content
			if overflow.toolName != "" {
				toolName = overflow.toolName
			}
		}
		refit := budget.FitToolResult(toolName, msg.ToolCallID, content, historyTokens+batchTokens, CeilingContextWindow)
		refit.Images = msg.Images
		compacted = append(compacted, refit)
		batchTokens += estimateMessagesTokens([]api.Message{refit})
	}

	s.emitCompacted(runID, opts, compacted, CompactionTriggerToolOutput, result.Summary)
	if err := s.recordCompaction(runID, opts.ChatID, CompactionTriggerToolOutput, result.Summary); err != nil {
		return compacted, true, skipNotified, err
	}
	if err := s.checkPostCompactionPromptBudget(budget, opts, compacted); err != nil {
		return compacted, true, skipNotified, err
	}
	return compacted, true, skipNotified, nil
}

func (s *Session) compactionRequest(runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse) CompactionRequest {
	meta := newEventMetadata(runID, opts)
	return CompactionRequest{
		ChatID:       opts.ChatID,
		Model:        opts.Model,
		Messages:     messages,
		Latest:       latest,
		Options:      opts.Options,
		KeepAlive:    opts.KeepAlive,
		Think:        opts.Think,
		ContinueTask: true,
		Budget:       budget,
		Progress: func(progress CompactionProgress) {
			_ = s.emit(newCompactionProgress(meta, progress.Tokens))
		},
	}
}

func (s *Session) emitCompactionStarted(runID string, opts RunOptions, trigger CompactionTrigger) {
	_ = s.emit(newCompactionStarted(newEventMetadata(runID, opts), trigger))
}

func (s *Session) emitCompactionSkipped(runID string, opts RunOptions, trigger CompactionTrigger, reason string) {
	_ = s.emit(newCompactionSkipped(newEventMetadata(runID, opts), trigger, CompactionSkippedMessage(reason)))
}

func (s *Session) emitCompacted(runID string, opts RunOptions, messages []api.Message, trigger CompactionTrigger, summary string) {
	_ = s.emit(newCompacted(newEventMetadata(runID, opts), messages, trigger, summary))
}

func (s *Session) autoCompactionTrigger(req CompactionRequest) CompactionTrigger {
	if s.Compactor == nil {
		return ""
	}
	trigger, should := s.Compactor.ShouldCompact(req)
	if should {
		return trigger
	}
	return ""
}

func CompactionSkippedMessage(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "compaction could not run"
	}
	return reason
}

func resolvedMaxToolRounds(model string, value int) int {
	if value != 0 {
		return value
	}
	if modelref.HasExplicitCloudSource(model) {
		return -1
	}
	return defaultMaxToolRounds
}

func toolMessageWithLimit(toolName, toolCallID, content string, maxRunes int) api.Message {
	return api.Message{
		Role:       "tool",
		Content:    truncateToolResultContentTo(content, maxRunes),
		ToolName:   toolName,
		ToolCallID: toolCallID,
	}
}

func smallContextToolResultLimitRunes(contextWindow int) int {
	switch {
	case contextWindow > 0 && contextWindow <= tinyContextToolResultTokenWindow:
		return tinyContextToolResultRunes
	case contextWindow > 0 && contextWindow <= smallContextToolResultTokenWindow:
		return smallContextToolResultRunes
	default:
		return 0
	}
}

func (s *Session) availableTools() api.Tools {
	if s == nil || s.Tools == nil {
		return nil
	}
	return s.Tools.Tools()
}

func toolMessage(toolName, toolCallID, content string) api.Message {
	return toolMessageWithLimit(toolName, toolCallID, content, maxToolResultRunes)
}

func sanitizeMessageForRun(msg api.Message) api.Message {
	if msg.Role == "tool" {
		msg.Content = truncateToolResultContent(msg.Content)
	}
	return msg
}

func sanitizeMessagesForRequest(messages []api.Message) []api.Message {
	if len(messages) == 0 {
		return nil
	}
	sanitized := make([]api.Message, len(messages))
	for i, msg := range messages {
		sanitized[i] = sanitizeMessageForRun(msg)
	}
	return sanitized
}

func truncateToolResultContent(content string) string {
	return truncateToolResultContentTo(content, maxToolResultRunes)
}

func truncateToolResultContentTo(content string, maxRunes int) string {
	return Truncate(content, TruncateConfig{
		MaxRunes:           maxRunes,
		HeadTail:           true,
		HeadPct:            75,
		Label:              "tool output",
		Hint:               "Use a narrower command, line range, or search query if more detail is needed.",
		FullOmissionPrefix: toolOutputFullOmissionPrefix,
	})
}

// TruncateConfig configures content truncation via Truncate.
type TruncateConfig struct {
	MaxRunes           int    // rune limit; <= 0 means full omission
	HeadTail           bool   // true = head + tail split; false = head only
	HeadPct            int    // percentage of MaxRunes for head (e.g. 75); tail gets the rest
	Label              string // e.g. "tool output", "summary", "stdout"
	Hint               string // guidance text appended to marker (optional)
	FullOmissionPrefix string // marker prefix when MaxRunes <= 0
}

// Truncate truncates content to at most cfg.MaxRunes runes. When HeadTail is
// true, it preserves the first HeadPct% and last (100-HeadPct)% of the budget
// with a marker between; otherwise it keeps only the head. MaxRunes <= 0
// triggers full omission using FullOmissionPrefix. All token counts in
// markers use ApproximateTokens.
func Truncate(content string, cfg TruncateConfig) string {
	runes := []rune(content)
	total := len(runes)

	if cfg.MaxRunes <= 0 {
		return fmt.Sprintf("%s omitted ~%d tokens.%s]", cfg.FullOmissionPrefix, ApproximateTokens(total), truncHint(cfg.Hint))
	}
	if total <= cfg.MaxRunes {
		return content
	}

	if !cfg.HeadTail {
		head := cfg.MaxRunes
		omitted := total - head
		return string(runes[:head]) + TruncMarker(cfg.Label, head, 0, omitted, false, cfg.Hint)
	}

	head := cfg.MaxRunes * cfg.HeadPct / 100
	tail := cfg.MaxRunes - head
	omitted := total - head - tail
	return string(runes[:head]) + TruncMarker(cfg.Label, head, tail, omitted, true, cfg.Hint) + string(runes[len(runes)-tail:])
}

func truncHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	if !strings.HasSuffix(hint, ".") {
		hint += "."
	}

	return " " + hint
}

// TruncMarker formats a truncation marker with consistent wording. head and
// tail are rune counts; omitted is the count of runes removed. headTail
// selects the head+tail vs head-only format. hint is optional guidance text.
func TruncMarker(label string, head, tail, omitted int, headTail bool, hint string) string {
	var b strings.Builder
	b.WriteString("\n\n[")
	b.WriteString(label)
	b.WriteString(" truncated: ")
	if headTail {
		fmt.Fprintf(&b, "showing first ~%d tokens and last ~%d tokens; ", ApproximateTokens(head), ApproximateTokens(tail))
	} else {
		fmt.Fprintf(&b, "showing first ~%d tokens; ", ApproximateTokens(head))
	}
	fmt.Fprintf(&b, "omitted ~%d tokens.%s]", ApproximateTokens(omitted), truncHint(hint))
	if headTail {
		b.WriteString("\n\n")
	}
	return b.String()
}

func toolOutputFullyOmitted(content string) bool {
	return strings.HasPrefix(content, toolOutputFullOmissionPrefix)
}

// ApproximateTokens estimates token count from a character/byte count using
// the standard ~4 chars-per-token heuristic. It is intentionally rough; all
// callers use it only for sizing/truncation decisions, not billing.
func ApproximateTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return max(1, (n+3)/4)
}

func messageEmpty(msg api.Message) bool {
	return msg.Content == "" && msg.Thinking == "" && len(msg.ToolCalls) == 0
}

// recordCompaction journals one compaction fact; raw history stays append-
// only around it.
func (s *Session) recordCompaction(runID, chatID string, trigger CompactionTrigger, summary string) error {
	if s == nil || s.Journal == nil {
		return nil
	}
	return s.Journal.RecordCompaction(CompactionRecord{RunID: runID, SessionID: chatID, Trigger: string(trigger), Summary: summary})
}

// flushRawSegment journals the run's messages added since the last flush
// point (run end, and around each compaction) and reseats the baseline.
func (r *run) flushRawSegment() error {
	if r.admitted == nil || r.session.Journal == nil {
		r.rawBaseline = len(r.messages)
		return nil
	}
	if r.rawBaseline >= len(r.messages) {
		r.rawBaseline = len(r.messages)
		return nil
	}
	segment := append([]api.Message(nil), r.messages[r.rawBaseline:]...)
	if err := r.session.Journal.RecordRawSegment(r.runID, segment); err != nil {
		return fmt.Errorf("record raw segment: %w", err)
	}
	r.rawBaseline = len(r.messages)
	return nil
}
