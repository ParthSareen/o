package chat

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

type sideTestCloser struct {
	closed int
}

func (c *sideTestCloser) DrainCompletions() []coreagent.BackgroundCompletion {
	return nil
}

func (c *sideTestCloser) Close() error {
	c.closed++
	return nil
}

func sideTestRegistry(closer *sideTestCloser) *coreagent.Registry {
	registry := &coreagent.Registry{}
	registry.Register(chatTestTool{})
	if closer != nil {
		registry.Background = closer
	}
	return registry
}

func TestSidePaneForksHistoryAndOwnsItsTools(t *testing.T) {
	closer := &sideTestCloser{}
	rootRegistry := sideTestRegistry(nil)
	systemPrompts := 0
	m := chatModel{
		ctx:      context.Background(),
		width:    140,
		height:   30,
		chatID:   "root-chat",
		messages: []api.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "yes"}},
		entries:  entriesFromMessages([]api.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "yes"}}),
		opts: Options{
			Model: "model-a",
			Tools: rootRegistry,
			ToolRegistryForModel: func(context.Context, string) *coreagent.Registry {
				return sideTestRegistry(closer)
			},
			SystemPromptForModel: func(_ context.Context, model string, _ *coreagent.Registry, _ bool) string {
				systemPrompts++
				return "side prompt for " + model
			},
			Store: nil,
		},
	}

	updated, _ := m.openSidePane()
	m = updated.(chatModel)
	if m.side == nil || !m.sideFocus {
		t.Fatal("side pane should open focused")
	}
	side := m.side
	if !side.pane {
		t.Fatal("side chat should be marked as a pane")
	}
	if side.chatID != "" || side.opts.Store != nil {
		t.Fatalf("side chat must stay ephemeral, chatID=%q store=%v", side.chatID, side.opts.Store)
	}
	if len(side.messages) != 2 || side.messages[0].Content != "earlier" {
		t.Fatalf("side messages = %+v, want the fork snapshot", side.messages)
	}
	if len(side.fork) != 2 {
		t.Fatalf("fork snapshot = %+v", side.fork)
	}
	if side.opts.Tools == rootRegistry {
		t.Fatal("side chat must not share the root tool registry")
	}
	if _, ok := side.opts.Tools.Get("fake_tool"); !ok {
		t.Fatal("side chat should get its own tools")
	}
	if systemPrompts != 1 || side.opts.SystemPrompt != "side prompt for model-a" {
		t.Fatalf("system prompt = %q (calls=%d), want a prompt rebuilt for the fork", side.opts.SystemPrompt, systemPrompts)
	}
	if m.opts.Model != "model-a" || m.opts.SystemPrompt == side.opts.SystemPrompt {
		t.Fatal("opening the side chat must not mutate the root options")
	}
}

func TestSplitPaneWidths(t *testing.T) {
	tests := []struct {
		width      int
		main, side int
	}{
		{width: 120, main: 71, side: 48},
		{width: 100, main: 59, side: 40},
		{width: 90, main: 53, side: 36},
		{width: 400, main: 239, side: 160},
	}
	for _, test := range tests {
		mainWidth, sideWidth := splitPaneWidths(test.width)
		if mainWidth != test.main || sideWidth != test.side {
			t.Fatalf("splitPaneWidths(%d) = (%d, %d), want (%d, %d)", test.width, mainWidth, sideWidth, test.main, test.side)
		}
		if mainWidth+sideWidth+1 != test.width {
			t.Fatalf("splitPaneWidths(%d) leaves unused width", test.width)
		}
	}
}

func TestOpenSidePaneRequiresAModelAndRoom(t *testing.T) {
	noModel := chatModel{width: 120, height: 20}
	updated, _ := noModel.openSidePane()
	if fm := updated.(chatModel); fm.side != nil {
		t.Fatal("side chat should not open without a model")
	}
	narrow := chatModel{width: 70, height: 20, opts: Options{Model: "model-a"}}
	updated, _ = narrow.openSidePane()
	if fm := updated.(chatModel); fm.side != nil {
		t.Fatal("side chat should not open in a narrow terminal")
	}
}

func TestSplitViewFillsTheTerminalOnResize(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(chatModel)
	if m.width != 160 || m.height != 40 || m.side.height != 40 {
		t.Fatalf("resize not applied: root=%dx%d side=%dx%d", m.width, m.height, m.side.width, m.side.height)
	}
	lines := strings.Split(stripANSI(m.View()), "\n")
	if len(lines) != 40 {
		t.Fatalf("split view lines = %d, want 40", len(lines))
	}
	mainWidth, sideWidth := splitPaneWidths(160)
	for i, line := range lines {
		if got := len([]rune(line)); got != mainWidth+sideWidth+1 {
			t.Fatalf("line %d width = %d, want %d", i, got, mainWidth+sideWidth+1)
		}
	}
}

func TestSplitViewRendersBothConversations(t *testing.T) {
	rootMessages := []api.Message{{Role: "user", Content: "root alpha"}}
	sideMessages := []api.Message{{Role: "user", Content: "side beta"}}
	m := chatModel{
		width:    120,
		height:   20,
		messages: rootMessages,
		entries:  entriesFromMessages(rootMessages),
		opts:     Options{Model: "model-a"},
	}
	side := chatModel{
		pane:     true,
		width:    48,
		height:   20,
		messages: sideMessages,
		entries:  entriesFromMessages(sideMessages),
		opts:     Options{Model: "model-a"},
		route:    sideMessageRoute,
	}
	m.side = &side

	out := stripANSI(m.View())
	lines := strings.Split(out, "\n")
	if len(lines) != 20 {
		t.Fatalf("split view has %d lines, want 20", len(lines))
	}
	var left, right int
	for _, line := range lines {
		if strings.Contains(line, "root alpha") {
			left++
		}
		if strings.Contains(line, "side beta") {
			right++
		}
		if strings.Contains(line, "root alpha") && strings.Contains(line, sideDividerGlyph) {
			// root content must stay left of the divider
			if strings.Index(line, "root alpha") > strings.Index(line, sideDividerGlyph) {
				t.Fatalf("root content crossed the divider: %q", line)
			}
		}
		if strings.Contains(line, "side beta") && strings.Index(line, "side beta") < strings.Index(line, sideDividerGlyph) {
			t.Fatalf("side content crossed the divider: %q", line)
		}
	}
	if left == 0 || right == 0 {
		t.Fatalf("split view missing panes (root=%d side=%d):\n%s", left, right, out)
	}
	if !strings.Contains(out, "side") {
		t.Fatalf("split view missing the side header:\n%s", out)
	}
}

func TestSidePaneSuppressesScrollbackPrinting(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	if next, cmd := m.flowTranscriptFlushCmd(); cmd != nil {
		t.Fatalf("closed side pane should flush scrollback: %#v", next)
	}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)
	if _, cmd := m.flowTranscriptFlushCmd(); cmd != nil {
		t.Fatal("open split view must not print to the terminal scrollback")
	}
	if cmd := m.armBackgroundWatcher(); cmd != nil {
		t.Fatal("split view must not arm the root background watcher")
	}
}

func TestCloseSidePaneClosesItsRegistry(t *testing.T) {
	closer := &sideTestCloser{}
	rootCloser := &sideTestCloser{}
	m := chatModel{
		ctx:    context.Background(),
		width:  120,
		height: 20,
		opts: Options{
			Model: "model-a",
			Tools: sideTestRegistry(rootCloser),
			ToolRegistryForModel: func(context.Context, string) *coreagent.Registry {
				return sideTestRegistry(closer)
			},
		},
	}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)
	updated, _ = m.closeSidePane()
	m = updated.(chatModel)
	if m.side != nil || m.sideFocus {
		t.Fatal("close should drop the pane")
	}
	if closer.closed != 1 {
		t.Fatalf("side registry closed %d times, want 1", closer.closed)
	}
	if rootCloser.closed != 0 {
		t.Fatal("closing the side pane must not close the root registry")
	}
}

func TestSideKeyRoutingFollowsFocus(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")})
	m = updated.(chatModel)
	if string(m.side.input) != "hi" {
		t.Fatalf("side input = %q, want typed text", string(m.side.input))
	}
	if len(m.input) != 0 {
		t.Fatalf("root input = %q, want untouched", string(m.input))
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("]")})
	m = updated.(chatModel)
	if m.sideFocus {
		t.Fatal("opt+] should switch focus back to the root chat")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yo")})
	m = updated.(chatModel)
	if string(m.input) != "yo" {
		t.Fatalf("root input = %q, want typed text after refocus", string(m.input))
	}
	if string(m.side.input) != "hi" {
		t.Fatalf("side input = %q, want unchanged", string(m.side.input))
	}
}

func TestCtrlCloseBracketTogglesThePane(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = updated.(chatModel)
	if m.side == nil || !m.sideFocus {
		t.Fatal("ctrl+] should open the side chat")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = updated.(chatModel)
	if m.side == nil || m.sideFocus {
		t.Fatal("ctrl+] should switch focus back to the root chat")
	}
}

func TestSidePaneCommands(t *testing.T) {
	m := chatModel{ctx: context.Background(), width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	m.side.input = []rune("/close")
	updated, cmd := m.side.handleSubmit()
	*m.side = updated.(chatModel)
	if cmd == nil {
		t.Fatal("/close should ask the root to close the pane")
	}
	if _, ok := cmd().(chatSideCloseMsg); !ok {
		t.Fatal("/close should emit chatSideCloseMsg")
	}

	m.side.input = []rune("/clear")
	updated, _ = m.side.handleSubmit()
	*m.side = updated.(chatModel)
	if len(m.side.messages) != len(m.side.fork) {
		t.Fatalf("/clear messages = %d, want the fork snapshot %d", len(m.side.messages), len(m.side.fork))
	}

	m.side.input = []rune("/compact")
	updated, _ = m.side.handleSubmit()
	*m.side = updated.(chatModel)
	if m.side.running {
		t.Fatal("/compact must not start a run in the side chat")
	}
	if got := m.side.entries[len(m.side.entries)-1].role; got != "slash" {
		t.Fatalf("unknown pane command entry role = %q, want slash", got)
	}
}

func TestSideRunUsesForkHistory(t *testing.T) {
	client := &chatCaptureClient{}
	fork := []api.Message{
		{Role: "user", Content: "earlier"},
		{Role: "assistant", Content: "yes"},
	}
	m := chatModel{
		ctx:      context.Background(),
		pane:     true,
		width:    48,
		height:   20,
		messages: fork,
		fork:     fork,
		entries:  entriesFromMessages(fork),
		opts: Options{
			Client:        client,
			Model:         "model-a",
			WorkingDir:    t.TempDir(),
			ToolsDisabled: true,
		},
	}
	updated, cmd := m.startRun("what about this?")
	m = updated.(chatModel)
	if cmd == nil || !m.running {
		t.Fatal("side run should start")
	}
	done := waitForRunDone(t, m.events)
	updated, _ = m.Update(done)
	m = updated.(chatModel)
	if m.running {
		t.Fatal("side run should finish")
	}
	if len(client.requests) != 1 {
		t.Fatalf("client requests = %d, want 1", len(client.requests))
	}
	request := client.requests[0]
	if len(request.Messages) < 3 {
		t.Fatalf("request messages = %+v, want fork history plus the new prompt", request.Messages)
	}
	if request.Messages[0].Content != "earlier" || request.Messages[len(request.Messages)-1].Content != "what about this?" {
		t.Fatalf("request messages = %+v", request.Messages)
	}
	if len(m.messages) != 4 {
		t.Fatalf("side messages after run = %+v, want the fork plus user and reply", m.messages)
	}
}

func TestInheritedSideMessagesTrimToTheContextBudget(t *testing.T) {
	filler := strings.Repeat("word ", 700) // ~875 tokens
	var messages []api.Message
	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, api.Message{Role: role, Content: filler})
	}
	m := chatModel{
		width:    120,
		height:   20,
		messages: messages,
		opts:     Options{Model: "model-a", ContextWindowTokens: 2100}, // budget ~1260 tokens
	}
	kept, trimmed := m.inheritedSideMessages()
	if trimmed == 0 || len(kept) >= len(messages) {
		t.Fatalf("kept %d of %d messages (trimmed=%d), want a trimmed suffix", len(kept), len(messages), trimmed)
	}
	if kept[0].Role == "tool" {
		t.Fatal("trimmed history must not start on a tool result")
	}
	if kept[len(kept)-1].Content != messages[len(messages)-1].Content {
		t.Fatal("trimmed history should keep the newest message")
	}
}

func TestInheritedSideMessagesSkipsOrphanedToolResults(t *testing.T) {
	big := strings.Repeat("x", 8000)
	messages := []api.Message{
		{Role: "assistant", Content: big},
		{Role: "tool", Content: big, ToolCallID: "call-1"},
		{Role: "user", Content: "latest question"},
	}
	m := chatModel{
		width:    120,
		height:   20,
		messages: messages,
		opts:     Options{Model: "model-a", ContextWindowTokens: 2400},
	}
	kept, _ := m.inheritedSideMessages()
	if len(kept) == 0 || kept[0].Role == "tool" {
		t.Fatalf("kept messages start on %q, want a clean turn", kept[0].Role)
	}
	if kept[len(kept)-1].Content != "latest question" {
		t.Fatalf("latest message lost: %+v", kept)
	}
}

func TestMouseDragKeepsThePaneItStartedIn(t *testing.T) {
	var copied string
	oldClipboard := writeClipboard
	writeClipboard = func(_ context.Context, text string) error {
		copied = text
		return nil
	}
	defer func() { writeClipboard = oldClipboard }()

	rootMessages := []api.Message{{Role: "user", Content: "root alpha"}}
	m := chatModel{
		width:    120,
		height:   20,
		messages: rootMessages,
		entries:  entriesFromMessages(rootMessages),
		opts:     Options{Model: "model-a"},
	}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)
	top, _ := m.transcriptLayout()
	mainWidth, _ := splitPaneWidths(120)

	updated, _ = m.Update(tea.MouseMsg{Type: tea.MouseLeft, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 2, Y: top})
	m = updated.(chatModel)
	updated, _ = m.Update(tea.MouseMsg{Type: tea.MouseLeft, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion, X: mainWidth + 10, Y: top})
	m = updated.(chatModel)
	if !m.selection.dragging {
		t.Fatal("root drag should keep its owner when it crosses the divider")
	}
	if m.side.selection.active {
		t.Fatal("side pane should not start a selection mid-drag")
	}
	updated, cmd := m.Update(tea.MouseMsg{Type: tea.MouseRelease, Action: tea.MouseActionRelease, X: mainWidth + 10, Y: top})
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("release over the side column should finish the root selection")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("clipboard command message = %#v, want nil", msg)
	}
	if !strings.Contains(copied, "alpha") {
		t.Fatalf("copied text = %q, want the root selection", copied)
	}
	if m.selection.active {
		t.Fatal("root selection should clear after copy")
	}
}

func TestClickingTheSideInputDoesNotSelectTranscript(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	updated, _ = m.Update(tea.MouseMsg{Type: tea.MouseLeft, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 118, Y: 19})
	m = updated.(chatModel)
	if !m.sideFocus {
		t.Fatal("clicking the side pane should focus it")
	}
	if m.side.selection.active {
		t.Fatal("clicking the input box must not start a transcript selection")
	}
}

func TestCtrlDClosesTheSidePane(t *testing.T) {
	m := chatModel{width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = updated.(chatModel)
	if m.quitting {
		t.Fatal("ctrl+d in the side pane must not quit the app")
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("second ctrl+d should close the side pane")
	}
	if _, ok := cmd().(chatSideCloseMsg); !ok {
		t.Fatal("second ctrl+d should ask the root to close the pane")
	}
	if m.quitting {
		t.Fatal("closing the pane must not quit the app")
	}
}

func TestSideModelSwitchClosesTheOldRegistry(t *testing.T) {
	first := &sideTestCloser{}
	second := &sideTestCloser{}
	rootCloser := &sideTestCloser{}
	registries := []*coreagent.Registry{sideTestRegistry(first), sideTestRegistry(second)}
	calls := 0
	m := chatModel{
		ctx:    context.Background(),
		width:  120,
		height: 20,
		opts: Options{
			Model: "model-a",
			Tools: sideTestRegistry(rootCloser),
			ModelOptions: func(context.Context) ([]ModelOption, error) {
				return []ModelOption{{Name: "model-a"}, {Name: "model-b"}}, nil
			},
			ToolRegistryForModel: func(context.Context, string) *coreagent.Registry {
				registry := registries[min(calls, len(registries)-1)]
				calls++
				return registry
			},
			SystemPromptForModel: func(context.Context, string, *coreagent.Registry, bool) string {
				return "side prompt"
			},
		},
	}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)
	if m.side.opts.Model != "model-a" {
		t.Fatalf("initial side model = %q", m.side.opts.Model)
	}

	updated, _ = m.side.openModelPicker("model-b")
	*m.side = updated.(chatModel)
	updated, _ = m.side.selectModel()
	*m.side = updated.(chatModel)

	if m.side.opts.Model != "model-b" {
		t.Fatalf("side model after switch = %q, want model-b", m.side.opts.Model)
	}
	if first.closed != 1 {
		t.Fatalf("old side registry closed %d times, want 1", first.closed)
	}
	if second.closed != 0 {
		t.Fatal("the new side registry should stay open")
	}
	if rootCloser.closed != 0 {
		t.Fatal("switching the side model must not close the root registry")
	}
}

// Mouse reporting is terminal-wide: while it is on, the terminal sends clicks
// to the program instead of opening cmd+click links or selecting natively. The
// pane is the only thing that needs the mouse, so capture stays scoped to it.
func TestMouseCaptureIsScopedToThePane(t *testing.T) {
	m := chatModel{ctx: context.Background(), width: 120, height: 20, opts: Options{Model: "model-a"}}
	if m.wantsMouseCapture() || m.mouseCaptured {
		t.Fatal("a plain chat must start without mouse capture")
	}

	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m = updated.(chatModel)
	if cmd != nil || m.mouseCaptured {
		t.Fatal("resizing without a pane must not touch mouse capture")
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = updated.(chatModel)
	if m.side == nil {
		t.Fatal("ctrl+] should open the side chat")
	}
	if !m.mouseCaptured {
		t.Fatal("opening the pane should capture the mouse so the pane is clickable")
	}
	if cmd == nil {
		t.Fatal("opening the pane should emit the mouse-mode command")
	}
	if msg := cmd(); msg != tea.EnableMouseCellMotion() {
		t.Fatalf("open command = %#v, want the mouse-enable message", msg)
	}

	updated, cmd = m.Update(chatSideCloseMsg{})
	m = updated.(chatModel)
	if m.side != nil {
		t.Fatal("closing the pane should drop the fork")
	}
	if m.mouseCaptured {
		t.Fatal("closing the pane should hand the mouse back to the terminal")
	}
	if cmd == nil {
		t.Fatal("closing the pane should emit a command")
	}

	// Closing again is a no-op: no pane means no mouse-mode churn.
	updated, cmd = m.Update(chatSideCloseMsg{})
	if next := updated.(chatModel); next.mouseCaptured || cmd != nil {
		t.Fatal("closing when no pane is open must not touch mouse capture")
	}
}

// The prompt debug screen scrolls with the wheel, so it captures the mouse and
// has to release it on close. It used to release unconditionally, which left the
// pane's click handling dead after any /prompt.
func TestPromptDebugReleasesMouseCapture(t *testing.T) {
	m := chatModel{ctx: context.Background(), width: 120, height: 20, opts: Options{Model: "model-a"}}

	m.input = []rune("/prompt")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(chatModel)
	if m.promptDebug == nil {
		t.Fatal("/prompt should open the prompt debug screen")
	}
	if !m.mouseCaptured || cmd == nil {
		t.Fatal("/prompt should capture the mouse")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(chatModel)
	if m.promptDebug != nil {
		t.Fatal("esc should close the prompt debug screen")
	}
	if m.mouseCaptured {
		t.Fatal("closing /prompt should release the mouse")
	}
}

// A pane never owns the terminal, so its own update loop must not emit
// mouse-mode commands on behalf of the root.
func TestPaneUpdateLeavesMouseCaptureAlone(t *testing.T) {
	m := chatModel{ctx: context.Background(), width: 120, height: 20, opts: Options{Model: "model-a"}}
	updated, _ := m.openSidePane()
	m = updated.(chatModel)

	pane := *m.side
	pane.mouseCaptured = false
	updated, cmd := pane.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if next := updated.(chatModel); next.mouseCaptured {
		t.Fatal("the pane must not turn on mouse capture")
	}
	if cmd != nil {
		t.Fatalf("the pane must not emit mouse commands, got %#v", cmd())
	}
}

func TestMouseCaptureCommands(t *testing.T) {
	if msg := mouseCaptureCommand(true)(); msg != tea.EnableMouseCellMotion() {
		t.Fatalf("enable command = %#v", msg)
	}
	if msg := mouseCaptureCommand(false)(); msg != tea.DisableMouse() {
		t.Fatalf("disable command = %#v", msg)
	}
}

// A split that does not fit is not drawn, so it must not hold the mouse either:
// capture follows what is on screen, not what exists in the model.
func TestMouseCaptureFollowsSplitVisibility(t *testing.T) {
	m := chatModel{ctx: context.Background(), width: 140, height: 30, opts: Options{Model: "model-a"}}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	m = updated.(chatModel)
	if !m.mouseCaptured {
		t.Fatal("a visible split should capture the mouse")
	}

	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = updated.(chatModel)
	if m.side == nil {
		t.Fatal("shrinking must keep the fork, only hide it")
	}
	if m.mouseCaptured {
		t.Fatal("a hidden split should release the mouse")
	}
	if cmd == nil || cmd() != tea.DisableMouse() {
		t.Fatal("hiding the split should disable mouse capture")
	}

	updated, cmd = m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = updated.(chatModel)
	if !m.mouseCaptured {
		t.Fatal("bringing the split back should capture the mouse again")
	}
	if cmd == nil || cmd() != tea.EnableMouseCellMotion() {
		t.Fatal("showing the split should enable mouse capture")
	}
}

// Returning from the editor or the diff viewer restores the terminal without
// mouse reporting, so a pane that was clickable before has to be captured
// again instead of silently staying dead.
func TestExecProcessReturnRecapturesMouseForThePane(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{name: "editor", msg: chatEditorClosedMsg{}},
		{name: "diffview", msg: chatDiffViewerClosedMsg{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := chatModel{ctx: context.Background(), width: 140, height: 30, opts: Options{Model: "model-a"}}
			updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
			m = updated.(chatModel)
			if !m.mouseCaptured {
				t.Fatal("a visible split should capture the mouse")
			}

			updated, cmd := m.Update(tc.msg)
			m = updated.(chatModel)
			if m.side == nil {
				t.Fatal("returning from an external process must keep the pane")
			}
			if !m.mouseCaptured {
				t.Fatal("the pane should still hold the mouse after the process returns")
			}
			if cmd == nil || cmd() != tea.EnableMouseCellMotion() {
				t.Fatal("returning with a pane open should re-enable mouse capture")
			}
		})
	}
}

// The model picker scrolls with the wheel, so it takes the mouse while it is up.
func TestModelPickerWantsMouseCapture(t *testing.T) {
	m := chatModel{width: 120, height: 20}
	if m.wantsMouseCapture() {
		t.Fatal("a plain chat should not capture the mouse")
	}
	m.modelPicker = &chatModelPicker{}
	if !m.wantsMouseCapture() {
		t.Fatal("the model picker should capture the mouse")
	}
}
