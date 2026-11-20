package chat

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m chatModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.promptDebug != nil {
		return m.updatePromptDebug(msg)
	}
	if !m.rootModalOpen() {
		if handled, cmd := m.handleSideShortcut(msg); handled {
			return m, cmd
		}
		if m.side != nil && (m.sideFocus || m.side.paneModalOpen()) {
			return m.updateSideKey(msg)
		}
	}
	if msg.Type == tea.KeyCtrlO {
		m.toggleInlineToolOutput()
		m.disarmQuit()
		m.disarmEsc()
		return m.withFlowTranscriptRepaint(nil)
	}
	if msg.Type == tea.KeyShiftTab {
		return m.togglePermissionMode()
	}
	if m.approvalPrompt != nil {
		return m.updateApprovalPrompt(msg)
	}
	if m.cloudAuthPrompt != nil {
		return m.updateCloudAuthPrompt(msg)
	}
	if m.sessionPicker != nil {
		return m.updateSessionPicker(msg)
	}
	if m.modelPicker != nil {
		return m.updateModelPicker(msg)
	}
	if m.thinkPicker != nil {
		return m.updateThinkPicker(msg)
	}
	if msg.Type != tea.KeyCtrlC && msg.Type != tea.KeyCtrlD {
		m.disarmQuit()
	}
	if msg.Type != tea.KeyEsc {
		m.disarmEsc()
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		return m.updateCtrlC()
	case tea.KeyCtrlD:
		return m.updateCtrlD()
	case tea.KeyEsc:
		return m.updateEsc()
	case tea.KeyEnter:
		if msg.Alt {
			m.insertInputNewline()
			return m, nil
		}
		if m.applySlashCompletion() {
			return m, nil
		}
		if m.applyMentionCompletion() {
			return m, nil
		}
		return m.handleSubmit()
	case tea.KeyCtrlJ:
		m.insertInputNewline()
		return m, nil
	case tea.KeyCtrlG:
		return m.openDiffViewer()
	case tea.KeyCtrlT:
		return m.openEditor()
	case tea.KeyCtrlP:
		return m.updateUpKey()
	case tea.KeyCtrlN:
		return m.updateDownKey()
	case tea.KeyUp:
		return m.updateUpKey()
	case tea.KeyDown:
		return m.updateDownKey()
	case tea.KeyLeft:
		if msg.Alt {
			m.moveInputCursorWord(-1)
		} else {
			m.moveInputCursorHorizontal(-1)
		}
	case tea.KeyRight:
		if msg.Alt {
			m.moveInputCursorWord(1)
		} else {
			m.moveInputCursorHorizontal(1)
		}
	case tea.KeyCtrlA:
		m.moveInputCursorToLineStart()
	case tea.KeyCtrlE:
		m.moveInputCursorToLineEnd()
	case tea.KeyCtrlB:
		m.moveInputCursorHorizontal(-1)
	case tea.KeyCtrlF:
		m.moveInputCursorHorizontal(1)
	case tea.KeyCtrlLeft:
		m.moveInputCursorWord(-1)
	case tea.KeyCtrlRight:
		m.moveInputCursorWord(1)
	case tea.KeyPgUp:
		m.scrollBy(max(1, m.transcriptHeight()-1))
	case tea.KeyPgDown:
		m.scrollBy(-max(1, m.transcriptHeight()-1))
		if m.scroll == 0 {
			return m.withFlowTranscriptFlush(nil)
		}
	case tea.KeyHome:
		m.moveInputCursorToLineStart()
	case tea.KeyEnd:
		m.moveInputCursorToLineEnd()
	case tea.KeyCtrlHome:
		m.scroll = m.maxScroll()
	case tea.KeyCtrlEnd:
		m.scroll = 0
		return m.withFlowTranscriptFlush(nil)
	case tea.KeyBackspace, tea.KeyCtrlH:
		m.resetPromptHistoryCursor()
		if msg.Alt {
			m.deleteInputWordBackward()
		} else {
			m.deleteInputBackward()
		}
	case tea.KeyCtrlW:
		m.resetPromptHistoryCursor()
		m.deleteInputWordBackward()
	case tea.KeyCtrlU:
		m.resetPromptHistoryCursor()
		m.clearInput()
	case tea.KeyCtrlK:
		m.resetPromptHistoryCursor()
		m.deleteInputForward()
	case tea.KeyDelete:
		m.resetPromptHistoryCursor()
		m.deleteInputForward()
	case tea.KeyTab:
		m.applyCompletion()
	case tea.KeySpace:
		m.insertInputRunes([]rune{' '})
	case tea.KeyRunes:
		if !msg.Alt || !m.handleInputAltRunes(msg.Runes) {
			m.insertInputRunesFromKey(msg.Runes, msg.Paste)
		}
	}
	return m, nil
}

func (m *chatModel) toggleInlineToolOutput() {
	m.toolOutputMode = true
	m.toolOutputOpen = !m.toolOutputOpen
	m.applyToolOutputMode()
	m.selection = chatSelection{}
	m.scroll = 0
}

func (m chatModel) updateCtrlC() (tea.Model, tea.Cmd) {
	if m.pane {
		switch {
		case len(m.input) > 0:
			m.clearInput()
			m.resetPromptHistoryCursor()
			m.disarmQuit()
			m.status = "ready"
			return m, nil
		case (m.running || m.compacting) && m.cancel != nil:
			m.cancel()
			m.disarmQuit()
			m.status = "canceling"
			return m, nil
		case !m.quitArmed || m.quitArmedKey != "ctrl+c":
			m.armQuit("ctrl+c", "press ctrl+c again to close the side chat")
			return m, nil
		}
		return m, func() tea.Msg { return chatSideCloseMsg{} }
	}
	if len(m.input) > 0 {
		m.clearInput()
		m.resetPromptHistoryCursor()
		m.disarmQuit()
		m.status = "ready"
		return m, nil
	}
	if (m.running || m.compacting) && m.cancel != nil {
		m.cancel()
		m.disarmQuit()
		m.status = "canceling"
		return m, nil
	}
	if !m.quitArmed || m.quitArmedKey != "ctrl+c" {
		m.armQuit("ctrl+c", "press ctrl+c again to quit")
		return m, nil
	}
	m.quitting = true
	m.dropSidePane()
	return m, m.quitCmd()
}

func (m chatModel) updateCtrlD() (tea.Model, tea.Cmd) {
	if m.pane {
		if len(m.input) > 0 {
			return m, nil
		}
		if !m.quitArmed || m.quitArmedKey != "ctrl+d" {
			m.armQuit("ctrl+d", "press ctrl+d again to close the side chat")
			return m, nil
		}
		return m, func() tea.Msg { return chatSideCloseMsg{} }
	}
	if len(m.input) > 0 {
		return m, nil
	}
	if !m.quitArmed || m.quitArmedKey != "ctrl+d" {
		m.armQuit("ctrl+d", "press ctrl+d again to quit")
		return m, nil
	}
	m.quitting = true
	m.dropSidePane()
	if (m.running || m.compacting) && m.cancel != nil {
		m.cancel()
	}
	return m, m.quitCmd()
}

func (m chatModel) quitCmd() tea.Cmd {
	return tea.Quit
}

func (m *chatModel) armQuit(key, status string) {
	m.quitArmed = true
	m.quitArmedKey = key
	m.status = status
}

func (m chatModel) updateEsc() (tea.Model, tea.Cmd) {
	if m.pane && !m.running && !m.compacting && len(m.input) == 0 {
		if !m.escArmed {
			m.escArmed = true
			m.status = "press esc again to close the side chat"
			return m, nil
		}
		m.escArmed = false
		return m, func() tea.Msg { return chatSideCloseMsg{} }
	}
	if (m.running || m.compacting) && m.cancel != nil {
		m.cancel()
		m.disarmQuit()
		m.escArmed = false
		m.status = "canceling"
		return m, nil
	}
	if !m.escArmed {
		m.escArmed = true
		switch {
		case len(m.input) > 0:
			m.status = "press esc again to clear input"
		default:
			m.status = "ready"
		}
		return m, nil
	}

	m.escArmed = false
	cleared := false
	if len(m.input) > 0 {
		m.clearInput()
		m.resetPromptHistoryCursor()
		cleared = true
	}
	if cleared {
		m.status = "ready"
	} else {
		m.status = "ready"
	}
	return m, nil
}

func (m *chatModel) clearInput() {
	m.input = nil
	m.inputCursor = 0
	m.inputCursorSet = false
	m.inputAttachments = nil
	m.inputPastedTexts = nil
	m.complete = 0
}

func (m chatModel) updateUpKey() (tea.Model, tea.Cmd) {
	if m.promptActive && m.movePromptHistory(-1) {
		return m, nil
	}
	if m.moveCompletion(-1) {
		return m, nil
	}
	if m.moveInputCursorVertical(-1) {
		return m, nil
	}
	if slices.Contains(m.input, '\n') {
		return m, nil
	}
	m.movePromptHistory(-1)
	return m, nil
}

func (m chatModel) updateDownKey() (tea.Model, tea.Cmd) {
	if m.promptActive && m.movePromptHistory(1) {
		return m, nil
	}
	if m.moveCompletion(1) {
		return m, nil
	}
	if m.moveInputCursorVertical(1) {
		return m, nil
	}
	if slices.Contains(m.input, '\n') {
		return m, nil
	}
	m.movePromptHistory(1)
	return m, nil
}

func (m *chatModel) disarmQuit() {
	if !m.quitArmed {
		return
	}
	m.quitArmed = false
	m.quitArmedKey = ""
	if strings.HasPrefix(m.status, "press ctrl+") && strings.HasSuffix(m.status, "again to quit") {
		m.status = "ready"
	}
}

func (m *chatModel) disarmEsc() {
	if !m.escArmed {
		return
	}
	m.escArmed = false
	if strings.HasPrefix(m.status, "press esc again") {
		m.status = "ready"
	}
}

func (m chatModel) canEditInput() bool {
	return m.promptDebug == nil && m.approvalPrompt == nil && m.cloudAuthPrompt == nil && m.modelPicker == nil && m.sessionPicker == nil && m.thinkPicker == nil
}
