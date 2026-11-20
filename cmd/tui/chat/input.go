package chat

import (
	"strings"

	"github.com/ParthSareen/o/cmd/internal/filedata"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *chatModel) handleSubmit() (tea.Model, tea.Cmd) {
	m.syncInputPlaceholders()
	input := strings.TrimSpace(string(m.input))
	if input == "" {
		return *m, nil
	}
	_, _, hasSlashCommand := slashCommandInvocation(input)
	if (m.running || m.compacting) && !hasSlashCommand {
		m.status = "wait for current response"
		return *m, nil
	}

	attachments := cloneInputAttachments(m.inputAttachments)
	pastedTexts := cloneInputPastedTexts(m.inputPastedTexts)
	m.input = nil
	m.inputCursor = 0
	m.inputCursorSet = false
	m.inputAttachments = attachments
	m.inputPastedTexts = pastedTexts
	m.complete = 0
	m.resetPromptHistoryCursor()
	return m.submitInput(input)
}

func (m *chatModel) submitInput(input string) (tea.Model, tea.Cmd) {
	if m.pane {
		return m.submitPaneInput(input)
	}
	command, args, _ := slashCommandInvocation(input)
	if command != "" {
		input = strings.TrimSpace(command + " " + args)
	}
	skillName, skillPrompt, skillOK := m.skillSlashInvocation(input)

	switch {
	case command == "/bye":
		m.quitting = true
		return *m, m.quitCmd()
	case command == "/help":
		m.entries = append(m.entries, newSlashEntry(m.helpSummary()))
		return *m, nil
	case command == "/model":
		return m.openModelPicker(args)
	case command == "/reviewer":
		return m.handleReviewerCommand(args)
	case command == "/think" && args == "":
		return m.openThinkPicker()
	case command == "/think":
		return m.handleThinkCommand(args)
	case command == "/tools":
		return m.handleToolsCommand(args)
	case command == "/system":
		return m.handleSystemCommand(args)
	case command == "/skills":
		return m.handleSkillsCommand(args)
	case command == "/prompt":
		return m.handlePromptCommand(args)
	case command == "/save":
		return m.handleSaveCommand(args)
	case command == "/new" && args == "":
		return m.resetChat("new chat")
	case command == "/sessions" && args == "":
		return m.openSessionPicker()
	case command == "/resume":
		return m.handleResumeCommand(args)
	case command == "/name":
		return m.handleNameCommand(args)
	case command == "/side" && args == "":
		return m.openSidePane()
	case command == "/side" && args == "close":
		return m.closeSidePane()
	case command == "/side":
		m.entries = append(m.entries, newSlashEntry("usage: /side [close]"))
		return *m, nil
	case command == "/compact" && args == "":
		return m.startManualCompaction()
	case command == "/copy" && args == "":
		return m.copyLastResponse()
	case input == "/diffview":
		return m.openDiffViewer()
	case input == "/nvim":
		return m.openEditor()
	case skillOK:
		return m.startSkillRun(skillName, skillPrompt)
	case strings.HasPrefix(input, "/") && m.slashInputIsMultimodalFile(input):
		return m.startRun(input)
	}

	return m.startRun(input)
}

func (m chatModel) slashInputIsMultimodalFile(input string) bool {
	if !m.opts.MultiModal {
		return false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	for _, file := range filedata.ExtractNames(input) {
		if strings.HasPrefix(file, fields[0]) {
			return true
		}
	}
	return false
}

func (m chatModel) systemPrompt(extra string) string {
	var parts []string
	if !m.systemPromptDisabled && strings.TrimSpace(m.opts.SystemPrompt) != "" {
		parts = append(parts, strings.TrimSpace(m.opts.SystemPrompt))
	}
	if strings.TrimSpace(extra) != "" {
		parts = append(parts, strings.TrimSpace(extra))
	}
	return strings.Join(parts, "\n\n")
}
