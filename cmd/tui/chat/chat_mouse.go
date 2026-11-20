package chat

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type chatSelectionPoint struct {
	line int
	col  int
}

type chatSelection struct {
	active   bool
	dragging bool
	anchor   chatSelectionPoint
	cursor   chatSelectionPoint
}

func startChatSelection(selection *chatSelection, msg tea.MouseMsg, contains func(tea.MouseMsg) bool, point func(tea.MouseMsg) chatSelectionPoint) {
	if !contains(msg) {
		*selection = chatSelection{}
		return
	}
	p := point(msg)
	*selection = chatSelection{active: true, dragging: true, anchor: p, cursor: p}
}

func dragChatSelection(selection *chatSelection, msg tea.MouseMsg, point func(tea.MouseMsg) chatSelectionPoint, scrollEdge func(tea.MouseMsg)) {
	if !selection.active || !selection.dragging {
		return
	}
	selection.cursor = point(msg)
	scrollEdge(msg)
}

func finishChatSelection(selection *chatSelection, msg tea.MouseMsg, point func(tea.MouseMsg) chatSelectionPoint) {
	if !selection.active || !selection.dragging {
		return
	}
	selection.cursor = point(msg)
	selection.dragging = false
	if selection.anchor == selection.cursor {
		*selection = chatSelection{}
	}
}

// withMouseCapture emits the mouse-mode command when the UI's needs changed.
// A pane never owns the terminal, so it leaves mouse mode alone.
func (m chatModel) withMouseCapture(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.pane {
		return m, cmd
	}
	want := m.wantsMouseCapture()
	if want == m.mouseCaptured {
		return m, cmd
	}
	m.mouseCaptured = want
	if !want {
		// A drag that never got its release leaves a stale highlight behind.
		m.selection = chatSelection{}
		if m.side != nil {
			m.side.selection = chatSelection{}
		}
	}
	mouse := mouseCaptureCommand(want)
	if cmd == nil {
		return m, mouse
	}
	return m, tea.Batch(cmd, mouse)
}

// mouseCaptureCommand is the command that puts the terminal's mouse reporting
// into the given state.
func mouseCaptureCommand(capture bool) tea.Cmd {
	if capture {
		return tea.EnableMouseCellMotion
	}
	return tea.DisableMouse
}

// sideSplitVisible reports whether the terminal is wide and tall enough to
// draw the split, which is also when the pane can be clicked.
func (m chatModel) sideSplitVisible() bool {
	if m.side == nil {
		return false
	}
	width, height := m.viewSize()
	return width >= minSideSplitWidth && height >= 8
}

// wantsMouseCapture reports whether the visible UI needs mouse events. Mouse
// reporting is terminal-wide: while it is on, the terminal sends clicks to the
// program instead of acting on them itself, which costs cmd+click link
// opening, hover underlines, and native drag selection.
func (m chatModel) wantsMouseCapture() bool {
	return m.promptDebug != nil || m.modelPicker != nil || m.sideSplitVisible()
}

// handleTranscriptScrollKey processes wheel events over the transcript.
// Reaching the bottom deliberately does NOT flush: the pending tail simply
// re-enters the live view, and the next streamed delta resumes incremental
// flow printing. Flushing on resume would only duplicate what the view
// already shows.
func (m chatModel) handleTranscriptScrollKey(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.MouseWheelUp:
		m.scrollBy(3)
	case tea.MouseWheelDown:
		m.scrollBy(-3)
	}
	return m, nil
}

func (m chatModel) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.promptDebug != nil {
		switch msg.Type {
		case tea.MouseWheelUp:
			m.promptDebug.scroll = clamp(m.promptDebug.scroll-3, 0, m.promptDebugMaxScroll())
		case tea.MouseWheelDown:
			m.promptDebug.scroll = clamp(m.promptDebug.scroll+3, 0, m.promptDebugMaxScroll())
		}
		return m, nil
	}
	if m.modelPicker != nil {
		switch msg.Type {
		case tea.MouseWheelUp:
			m.modelPicker.Move(-3)
		case tea.MouseWheelDown:
			m.modelPicker.Move(3)
		}
		return m, nil
	}
	if m.sideSplitVisible() {
		width, _ := m.viewSize()
		mainWidth, _ := splitPaneWidths(width)
		sideDragging := m.side.selection.active && m.side.selection.dragging
		rootDragging := m.selection.active && m.selection.dragging
		switch {
		case sideDragging:
			return m.updateSideMouse(msg, max(0, msg.X-(mainWidth+1)))
		case rootDragging:
			// Keep delivering the drag to the pane it started in.
		case msg.X >= mainWidth:
			return m.updateSideMouse(msg, max(0, msg.X-(mainWidth+1)))
		case msg.Type == tea.MouseLeft:
			m.sideFocus = false
		}
	}
	if !m.mouseInTranscript(msg) && !m.selection.active {
		return m.handleTranscriptScrollKey(msg)
	}
	switch msg.Type {
	case tea.MouseWheelUp:
		m.scrollBy(3)
	case tea.MouseWheelDown:
		m.scrollBy(-3)
	case tea.MouseLeft:
		switch msg.Action {
		case tea.MouseActionPress:
			m.startTranscriptSelection(msg)
		case tea.MouseActionMotion:
			m.dragTranscriptSelection(msg)
		default:
			if msg.Action == 0 {
				m.startTranscriptSelection(msg)
			}
		}
	case tea.MouseMotion:
		m.dragTranscriptSelection(msg)
	case tea.MouseRelease:
		return m, m.finishTranscriptSelection(msg)
	}
	return m, nil
}

func (m chatModel) mouseInTranscript(msg tea.MouseMsg) bool {
	top, height := m.transcriptLayout()
	if msg.X < 0 || msg.X >= m.transcriptWidth() {
		return false
	}
	return msg.Y >= top && msg.Y < top+height
}

func (m chatModel) mouseTranscriptPoint(msg tea.MouseMsg) chatSelectionPoint {
	top, height := m.transcriptLayout()
	visibleY := clamp(msg.Y-top, 0, max(0, height-1))
	line := m.visibleTranscriptStartLine(m.transcriptWidth(), height) + visibleY
	col := max(0, msg.X)
	return chatSelectionPoint{line: line, col: col}
}

func (m *chatModel) startTranscriptSelection(msg tea.MouseMsg) {
	startChatSelection(&m.selection, msg, m.mouseInTranscript, m.mouseTranscriptPoint)
}

func (m *chatModel) dragTranscriptSelection(msg tea.MouseMsg) {
	dragChatSelection(&m.selection, msg, m.mouseTranscriptPoint, func(msg tea.MouseMsg) {
		top, height := m.transcriptLayout()
		if msg.Y <= top {
			m.scrollBy(1)
		} else if msg.Y >= top+height-1 {
			m.scrollBy(-1)
		}
	})
}

func (m *chatModel) finishTranscriptSelection(msg tea.MouseMsg) tea.Cmd {
	finishChatSelection(&m.selection, msg, m.mouseTranscriptPoint)
	if !m.selection.active {
		return nil
	}
	selected := m.selectedTranscriptText(m.transcriptWidth())
	if strings.TrimSpace(selected) == "" {
		m.selection = chatSelection{}
		return nil
	}
	m.selection = chatSelection{}
	m.status = "copied"
	return copyTextCmd(m.ctx, selected)
}
