package chat

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// A side chat is a fork of the conversation: it inherits a snapshot of the
// root chat's messages and runs its own agent loop in a second pane. The
// fork's turns never touch the root history, and it is ephemeral (no store
// row, no journal), so closing the pane discards it.
const (
	// minSideSplitWidth is the narrowest terminal a split view is offered in.
	minSideSplitWidth = 90
	minSidePaneWidth  = 34
	minMainPaneWidth  = 44
	// sidePaneSharePercent is how much of the width the side pane takes.
	sidePaneSharePercent = 40
	sideDividerGlyph     = "│"
	// sideInheritedBudgetShare is the fraction of the side model's context
	// the inherited history may occupy; the rest is left for the system
	// prompt, tools, and the reply.
	sideInheritedBudgetShare = 0.6
)

var sideDividerStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#c8c8c8", Dark: "#3a3a3a"})

// paneSlashCommands are the commands the side pane accepts. Everything else
// stays in the root chat.
var paneSlashCommands = []chatSlashCommand{
	{name: "/model", description: "switch the side chat model"},
	{name: "/clear", description: "reset the side chat to its inherited context"},
	{name: "/close", description: "close the side chat"},
	{name: "/help", description: "show side chat commands"},
}

// chatSideMsg carries a message produced by the side pane's own async work
// back to the root update loop, which owns the terminal.
type chatSideMsg struct {
	msg tea.Msg
}

// chatSideCloseMsg asks the root loop to close the side pane.
type chatSideCloseMsg struct{}

func sideMessageRoute(msg tea.Msg) tea.Msg {
	if msg == nil {
		return nil
	}
	return chatSideMsg{msg: msg}
}

// splitPaneWidths returns the main and side column widths, excluding the
// divider column.
func splitPaneWidths(width int) (int, int) {
	sideWidth := width * sidePaneSharePercent / 100
	sideWidth = clamp(sideWidth, minSidePaneWidth, width-minMainPaneWidth-1)
	if sideWidth < minSidePaneWidth {
		sideWidth = minSidePaneWidth
	}
	mainWidth := width - sideWidth - 1
	if mainWidth < 1 {
		mainWidth = 1
	}
	return mainWidth, sideWidth
}

// openSidePane forks the current conversation into a side pane and focuses it.
func (m *chatModel) openSidePane() (tea.Model, tea.Cmd) {
	if m.side != nil {
		m.sideFocus = true
		m.status = "side"
		return *m, nil
	}
	if strings.TrimSpace(m.opts.Model) == "" {
		m.entries = append(m.entries, newSlashEntry("Pick a model before opening a side chat."))
		return *m, nil
	}
	width, _ := m.viewSize()
	if width < minSideSplitWidth {
		m.entries = append(m.entries, newSlashEntry("Terminal too narrow for a side chat (needs at least 90 columns)."))
		return *m, nil
	}
	side := m.newSidePane()
	m.side = &side
	m.sideFocus = true
	return *m, nil
}

// newSidePane builds the fork: cloned history, its own tool registry (the
// root registry cannot be shared because background completions are drained
// exactly once), the root's system prompt regenerated for that registry, and
// an in-memory-only chat state.
func (m *chatModel) newSidePane() chatModel {
	opts := m.opts
	opts.ChatID = ""
	opts.Name = ""
	opts.Messages = nil
	opts.Store = nil
	opts.OpenModelPicker = false
	opts.PreloadModel = nil
	opts.EventSinks = nil
	opts.Images = nil

	registry := m.sideToolRegistry(opts.Model)
	opts.Tools = registry
	if m.opts.ToolsDisabled || registry == nil {
		opts.ToolsDisabled = true
	}
	if opts.SystemPromptForModel != nil {
		opts.SystemPrompt = opts.SystemPromptForModel(m.ctx, opts.Model, registry, opts.ToolsDisabled)
	}

	messages, trimmed := m.inheritedSideMessages()
	side := chatModel{
		ctx:                  m.ctx,
		opts:                 opts,
		workingDir:           m.workingDir,
		messages:             messages,
		entries:              entriesFromMessages(messages),
		approvalState:        m.ensureApprovalState(),
		defaultAllowAll:      m.defaultAllowAll,
		defaultAutoReview:    m.defaultAutoReview,
		permissionNotice:     m.permissionNotice,
		promptHistory:        initialPromptHistory(m.ctx, Options{Messages: messages}),
		systemPromptDisabled: m.systemPromptDisabled,
		pane:                 true,
		route:                sideMessageRoute,
		status:               "ready",
		width:                m.width,
		height:               m.height,
	}
	side.fork = messages
	// The root learns its window asynchronously (model preload); a fork made
	// before that would otherwise render without a context estimate.
	if side.opts.ContextWindowTokens <= 0 {
		side.refreshContextWindowTokens(side.opts.Model)
	}
	if trimmed > 0 {
		side.entries = append([]chatEntry{newSlashEntry("Inherited the most recent part of the conversation; older messages did not fit the side chat's context.")}, side.entries...)
	}
	side.contextTokens = side.estimatePromptTokens(side.messages, side.opts.SystemPrompt)
	side.contextEstimate = true
	return side
}

// sideToolRegistry builds an isolated registry for the fork. Sharing the
// root registry would let the two runs steal each other's background
// completions, so each pane owns its tools.
func (m *chatModel) sideToolRegistry(model string) *coreagent.Registry {
	if m.opts.ToolsDisabled || m.opts.ToolRegistryForModel == nil {
		return nil
	}
	return m.opts.ToolRegistryForModel(m.ctx, model)
}

// inheritedSideMessages snapshots the root conversation for the fork. When
// the history is too large for the side model's window it keeps the newest
// suffix that fits, starting on a clean user turn so tool calls and results
// stay paired.
func (m *chatModel) inheritedSideMessages() ([]api.Message, int) {
	messages := slices.Clone(m.messages)
	window := m.opts.ContextWindowTokens
	if m.opts.ContextWindowTokensForModel != nil && m.opts.Model != "" {
		window = m.opts.ContextWindowTokensForModel(m.ctx, m.opts.Model, window)
	}
	if window <= 0 || len(messages) == 0 {
		return messages, 0
	}
	budget := int(float64(window) * sideInheritedBudgetShare)
	used := 0
	start := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		used += approximateMessageTokens(messages[i])
		if used > budget {
			break
		}
		start = i
	}
	// Don't start on a tool result: its assistant call was cut off.
	for start < len(messages) && messages[start].Role == "tool" {
		start++
	}
	// Keep the very latest exchange even when it alone exceeds the budget.
	// A tool result is useless without the call that produced it, so back up
	// past tool results to their assistant message.
	if start >= len(messages) {
		start = max(0, len(messages)-1)
		for start > 0 && messages[start].Role == "tool" {
			start--
		}
	}
	return messages[start:], len(messages) - start
}

func approximateMessageTokens(msg api.Message) int {
	total := approximateTokenCount(msg.Content) + approximateTokenCount(msg.Thinking) + 8
	for _, call := range msg.ToolCalls {
		total += approximateTokenCount(call.Function.Name) + approximateTokenCount(call.Function.Arguments.String())
	}
	return total
}

// closeSidePane stops the fork's run, releases its tools (killing any
// background processes it started) and restores the root's scrollback view.
func (m *chatModel) closeSidePane() (tea.Model, tea.Cmd) {
	if m.side == nil {
		return *m, nil
	}
	m.dropSidePane()
	m.status = "ready"
	return m.withFlowTranscriptRepaint(nil)
}

// dropSidePane tears the fork down without repainting; callers that already
// redraw (reset/resume/quit) use this.
func (m *chatModel) dropSidePane() {
	if m.side == nil {
		return
	}
	if m.side.cancel != nil {
		m.side.cancel()
	}
	if m.side.opts.Tools != nil && m.side.opts.Tools != m.opts.Tools {
		_ = m.side.opts.Tools.Close()
	}
	m.side = nil
	m.sideFocus = false
}

// updateSide forwards a message produced by the fork's async work into the
// fork's own update loop.
func (m chatModel) updateSide(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.side == nil {
		return m, nil
	}
	switch msg.(type) {
	case tea.WindowSizeMsg:
		// The root owns terminal size; the pane is laid out from it.
		return m, nil
	}
	next, cmd := m.side.Update(msg)
	if updated, ok := next.(chatModel); ok {
		*m.side = updated
	}
	return m, cmd
}

// paneModalOpen reports whether the side pane has a modal that owns the
// keyboard (an open picker), so focus follows it even after opt+[.
func (m chatModel) paneModalOpen() bool {
	return m.modelPicker != nil || m.thinkPicker != nil || m.sessionPicker != nil ||
		m.cloudAuthPrompt != nil || m.promptDebug != nil
}

// rootModalOpen reports whether a root-level modal owns the keyboard.
func (m chatModel) rootModalOpen() bool {
	return m.promptDebug != nil || m.modelPicker != nil || m.sessionPicker != nil ||
		m.thinkPicker != nil || m.cloudAuthPrompt != nil
}

// handleSideShortcut handles the pane focus keys. Option+] and ctrl+] both
// work: Option+[ cannot, because ESC [ is the CSI introducer and the input
// parser would swallow the following keypress waiting for a sequence.
func (m *chatModel) handleSideShortcut(msg tea.KeyMsg) (bool, tea.Cmd) {
	if msg.Type == tea.KeyCtrlCloseBracket {
		return true, m.toggleSidePane()
	}
	if msg.Type != tea.KeyRunes || !msg.Alt || len(msg.Runes) != 1 || msg.Runes[0] != ']' {
		return false, nil
	}
	return true, m.toggleSidePane()
}

// toggleSidePane opens the fork, or switches focus between the two panes when
// it is already open.
func (m *chatModel) toggleSidePane() tea.Cmd {
	if m.side == nil {
		next, _ := m.openSidePane()
		*m = next.(chatModel)
		return nil
	}
	m.sideFocus = !m.sideFocus
	return nil
}

// paneCommand resolves an input line against the side pane's own command set.
func paneCommand(input string) (string, string) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", ""
	}
	token, args, _ := strings.Cut(input, " ")
	token = strings.ToLower(token)
	for _, command := range paneSlashCommands {
		if command.name == token {
			return command.name, strings.TrimSpace(args)
		}
	}
	return "", ""
}

// submitPaneInput handles a submitted line inside the side pane.
func (m *chatModel) submitPaneInput(input string) (tea.Model, tea.Cmd) {
	command, args := paneCommand(input)
	skillName, skillPrompt, skillOK := m.skillSlashInvocation(input)

	switch {
	case command == "/close":
		return *m, func() tea.Msg { return chatSideCloseMsg{} }
	case command == "/clear":
		if m.running || m.compacting {
			m.entries = append(m.entries, newSlashEntry("Wait for the current run to finish before clearing the side chat."))
			return *m, nil
		}
		m.messages = slices.Clone(m.fork)
		m.liveMessages = nil
		m.entries = entriesFromMessages(m.messages)
		m.scroll = 0
		m.contextTokens = m.estimatePromptTokens(m.messages, m.opts.SystemPrompt)
		m.contextEstimate = true
		m.status = "ready"
		return *m, nil
	case command == "/help":
		m.entries = append(m.entries, newSlashEntry(m.paneHelpSummary()))
		return *m, nil
	case command == "/model":
		if m.running || m.compacting {
			m.entries = append(m.entries, newSlashEntry("Wait for the current run to finish before switching the side chat model."))
			return *m, nil
		}
		return m.openModelPicker(args)
	case skillOK:
		return m.startSkillRun(skillName, skillPrompt)
	}
	if strings.HasPrefix(input, "/") {
		if m.slashInputIsMultimodalFile(input) {
			return m.startRun(input)
		}
		m.entries = append(m.entries, newSlashEntry("Not available in the side chat. Use /model, /clear, /close, /help, or a skill."))
		return *m, nil
	}
	return m.startRun(input)
}

func (m chatModel) paneHelpSummary() string {
	lines := []string{"Side chat commands:"}
	for _, command := range paneSlashCommands {
		usage := command.name
		if command.usage != "" {
			usage = command.usage
		}
		lines = append(lines, "- `"+usage+"`: "+command.description)
	}
	lines = append(lines, "", "Side chats inherit the conversation at the moment they open and run on their own. opt+] (or ctrl+]) switches focus between the panes; esc closes the side chat when it is idle.")
	return strings.Join(lines, "\n")
}

// paneSlashCompletions lists the commands available inside the side pane,
// including the skills the pane can run.
func (m chatModel) paneSlashCompletions(input string) []chatCompletion {
	lower := strings.ToLower(input)
	var completions []chatCompletion
	for _, command := range paneSlashCommands {
		if strings.HasPrefix(command.name, lower) {
			completions = append(completions, chatCompletion{
				value:       command.name,
				label:       command.name,
				description: command.description,
			})
		}
	}
	if m.opts.Skills != nil {
		for _, skill := range m.opts.Skills.List() {
			name := "/" + skill.Name
			if !strings.HasPrefix(strings.ToLower(name), lower) || isPaneCommand(name) {
				continue // built-in pane command wins; don't shadow it
			}
			description := skill.Description
			if description == "" {
				description = "No description provided."
			}
			completions = append(completions, chatCompletion{
				value:       name,
				label:       name,
				description: description,
			})
		}
	}
	if len(completions) == 0 {
		return []chatCompletion{{label: "No matching commands"}}
	}
	return completions
}

func isPaneCommand(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, command := range paneSlashCommands {
		if command.name == name {
			return true
		}
	}
	return false
}

// selectPaneModel applies a model choice to the fork only: it rebuilds the
// fork's tools and system prompt for that model and leaves the root chat
// untouched (and the last-used model config unmodified).
func (m chatModel) selectPaneModel() (tea.Model, tea.Cmd) {
	if m.modelPicker == nil {
		return m, nil
	}
	selectedItem, ok := m.modelPicker.SelectedItem()
	if !ok {
		return m, nil
	}
	selected, ok := m.modelOptionForSelection(selectedItem.Name)
	if !ok {
		return m, nil
	}
	m.opts.Model = selected.Name
	m.modelPicker = nil
	m.modelPickerModels = nil
	m.openModelOnInit = false
	m.status = "ready"

	previous := m.opts.Tools
	registry := (&m).sideToolRegistry(selected.Name)
	m.opts.Tools = registry
	if previous != nil && previous != registry {
		_ = previous.Close()
	}
	if m.opts.ToolsDisabled || registry == nil {
		m.opts.ToolsDisabled = true
	}
	if m.opts.SystemPromptForModel != nil {
		m.opts.SystemPrompt = m.opts.SystemPromptForModel(m.ctx, selected.Name, registry, m.opts.ToolsDisabled)
	}
	if m.opts.MultiModalForModel != nil {
		m.opts.MultiModal = m.opts.MultiModalForModel(m.ctx, selected.Name)
	}
	if m.opts.ContextWindowTokensForModel != nil {
		m.opts.ContextWindowTokens = m.opts.ContextWindowTokensForModel(m.ctx, selected.Name, m.opts.ContextWindowTokens)
	}
	m.contextTokens = m.estimatePromptTokens(m.messages, m.opts.SystemPrompt)
	m.contextEstimate = true
	return m, nil
}

// paneHeaderLines is the side pane's title row. It reports the model, the
// context estimate, and whether an approval is waiting in an unfocused pane.
func (m chatModel) paneHeaderLines(width int) []string {
	label := "side"
	if m.running || m.compacting {
		label = "side " + chatSpinnerFrames[m.spinner%len(chatSpinnerFrames)]
	}
	parts := []string{label}
	if model := strings.TrimSpace(m.opts.Model); model != "" {
		parts = append(parts, model)
	}
	if contextStatus := m.contextStatus(); contextStatus != "" {
		parts = append(parts, contextStatus)
	}
	if m.approvalPrompt != nil {
		parts = append(parts, "approval needed")
	}
	titleStyle := chatMetaStyle
	if m.focused {
		titleStyle = chatHeaderStyle
	}
	line := titleStyle.Render(parts[0])
	if len(parts) > 1 {
		line += " " + chatMetaStyle.Render(strings.Join(parts[1:], " · "))
	}
	return []string{truncateRenderedLine(line, width)}
}

// paneLines renders the side pane as exactly height lines of width.
func (m chatModel) paneLines(width, height int) []string {
	if width < 1 || height < 1 {
		return nil
	}
	m.width = width
	m.height = height
	header := m.headerLines()
	if len(header) >= height {
		return header[:height]
	}
	bottom := m.bottomLines(width, height-len(header))
	if len(bottom) > height-len(header) {
		bottom = bottom[len(bottom)-(height-len(header)):]
	}
	all := m.transcriptLines(width)
	gap := transcriptInputGap(height-len(header), len(bottom), len(all))
	available := max(0, height-len(header)-len(bottom)-gap)
	start := m.visibleTranscriptStartLineForLines(len(all), available)
	end := min(len(all), start+available)

	lines := make([]string, 0, height)
	lines = append(lines, header...)
	for i := start; i < end; i++ {
		lines = append(lines, truncateRenderedLine(all[i], width))
	}
	for len(lines) < height-len(bottom)-gap {
		lines = append(lines, "")
	}
	for range gap {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

// mainPaneLines renders the root conversation as the left column of the
// split view: a tail of the transcript above the input box, honouring the
// root's scroll position.
func (m chatModel) mainPaneLines(width, height int) []string {
	if width < 1 || height < 1 {
		return nil
	}
	bottom := m.bottomLines(width, height)
	if len(bottom) > height {
		bottom = bottom[len(bottom)-height:]
	}
	all := m.transcriptLines(width)
	gap := transcriptInputGap(height, len(bottom), len(all))
	available := max(0, height-len(bottom)-gap)
	start := m.visibleTranscriptStartLineForLines(len(all), available)
	end := min(len(all), start+available)

	lines := make([]string, 0, height)
	for i := start; i < end; i++ {
		lines = append(lines, truncateRenderedLine(all[i], width))
	}
	for len(lines) < height-len(bottom)-gap {
		lines = append(lines, "")
	}
	for range gap {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

// splitView paints the root and fork conversations side by side.
func (m chatModel) splitView(width, height int) string {
	if m.side == nil {
		return m.flowView(width)
	}
	if width < minSideSplitWidth || height < 8 {
		return m.tooNarrowView(width)
	}
	mainWidth, sideWidth := splitPaneWidths(width)
	m.side.width = sideWidth
	m.side.height = height
	m.side.focused = m.sideFocus

	left := m.mainPaneLines(mainWidth, height)
	right := m.side.paneLines(sideWidth, height)

	lines := make([]string, 0, height)
	divider := sideDividerStyle.Render(sideDividerGlyph)
	for i := 0; i < height; i++ {
		var line string
		if i < len(left) {
			line = padRenderedLine(truncateRenderedLine(left[i], mainWidth), mainWidth)
		} else {
			line = strings.Repeat(" ", mainWidth)
		}
		line += divider
		if i < len(right) {
			line += padRenderedLine(truncateRenderedLine(right[i], sideWidth), sideWidth)
		} else {
			line += strings.Repeat(" ", sideWidth)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m chatModel) tooNarrowView(width int) string {
	lines := []string{
		"",
		chatMetaStyle.Render("  The side chat needs a terminal at least 90 columns wide."),
		chatMetaStyle.Render("  Widen the window, or close the side chat with /close."),
	}
	return strings.Join(lines, "\n")
}

// updateSideKey routes a key press to the fork while it has focus.
func (m chatModel) updateSideKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.side == nil {
		return m, nil
	}
	// The nvim integrations suspend the whole TUI, which a pane cannot do.
	if msg.Type == tea.KeyCtrlT || msg.Type == tea.KeyCtrlG {
		return m, nil
	}
	next, cmd := m.side.Update(msg)
	if updated, ok := next.(chatModel); ok {
		*m.side = updated
	}
	return m, cmd
}

// updateSideMouse routes mouse input landing in the side column.
func (m chatModel) updateSideMouse(msg tea.MouseMsg, localX int) (tea.Model, tea.Cmd) {
	if m.side == nil {
		return m, nil
	}
	msg.X = localX
	switch msg.Type {
	case tea.MouseWheelUp:
		m.side.scrollBy(3)
	case tea.MouseWheelDown:
		m.side.scrollBy(-3)
	case tea.MouseLeft:
		m.sideFocus = true
		switch msg.Action {
		case tea.MouseActionPress:
			if m.side.mouseInTranscript(msg) {
				m.side.startTranscriptSelection(msg)
			}
		case tea.MouseActionMotion:
			m.side.dragTranscriptSelection(msg)
		default:
			if msg.Action == 0 && m.side.mouseInTranscript(msg) {
				m.side.startTranscriptSelection(msg)
			}
		}
	case tea.MouseMotion:
		m.side.dragTranscriptSelection(msg)
	case tea.MouseRelease:
		return m, m.side.finishTranscriptSelection(msg)
	}
	return m, nil
}
