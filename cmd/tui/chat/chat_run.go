package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/cmd/internal/filedata"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/ollama/ollama/api"
)

func (m *chatModel) startRun(input string) (tea.Model, tea.Cmd) {
	// A user message resets the unattended background auto-run budget.
	m.unattendedBackgroundRuns = 0
	displayInput, message, err := m.userMessageFromInput(input, input)
	if err != nil {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: err.Error(), err: err.Error()}))
		m.status = "error"
		return *m, nil
	}
	return m.startRunWithMessages(displayInput, message.Content, []api.Message{message}, "", "")
}

func (m *chatModel) userMessageFromInput(displayInput, userInput string) (string, api.Message, error) {
	content := m.expandPastedTextPlaceholders(userInput)
	images := slices.Clone(m.opts.Images)
	preloaded := len(images)
	placeholderAttachments := m.activeInputAttachmentsFor(userInput)
	for _, attachment := range placeholderAttachments {
		images = append(images, attachment.data)
	}
	extracted := 0

	if m.opts.MultiModal {
		cleaned, files, err := filedata.ExtractWithFiles(content)
		if err != nil {
			return "", api.Message{}, err
		}
		content = cleaned
		extracted = len(files)
		for _, file := range files {
			images = append(images, file.Data)
		}
	}
	m.opts.Images = nil
	m.inputAttachments = nil
	m.inputPastedTexts = nil

	if len(images) > 0 {
		base := displayInput
		if extracted > 0 {
			base = content
		}
		if len(placeholderAttachments) > 0 && extracted == 0 && preloaded == 0 {
			displayInput = strings.TrimSpace(content)
		} else {
			displayInput = chatDisplayInputWithAttachments(base, len(images))
		}
	}

	return displayInput, api.Message{Role: "user", Content: content, Images: images}, nil
}

func chatDisplayInputWithAttachments(input string, count int) string {
	note := fmt.Sprintf("[attached %d file%s]", count, pluralSuffix(count))
	input = strings.TrimSpace(input)
	if input == "" {
		return note
	}
	return input + "\n\n" + note
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func (m *chatModel) startSkillRun(name, prompt string) (tea.Model, tea.Cmd) {
	m.unattendedBackgroundRuns = 0
	name = strings.TrimSpace(name)
	prompt = strings.TrimSpace(prompt)
	// The skill instructions are delivered via the synthetic tool result that
	// follows this user turn, so this message orients the model rather than
	// re-requesting the skill. When a prompt is supplied it becomes the task.
	content := prompt
	if content == "" {
		content = "The " + name + " skill is loaded; follow its instructions for this request."
	}
	message := api.Message{Role: "user", Content: content}
	displayInput := "/" + name
	if prompt != "" {
		displayInput = "/" + name + " " + prompt
	}
	return m.startRunWithMessages(displayInput, "", []api.Message{message}, "", name)
}

func (m *chatModel) startRunWithMessages(displayInput, historyInput string, newMessages []api.Message, extraSystemPrompt, skillName string) (tea.Model, tea.Cmd) {
	if historyInput != "" {
		m.addPromptHistory(historyInput)
		m.persistPrompt(historyInput)
	}
	if displayInput != "" {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "user", content: displayInput}))
	}
	if len(newMessages) > 1 {
		m.entries = append(m.entries, entriesFromMessages(newMessages[1:])...)
	}
	m.running = true
	m.awaitingModel = true
	m.status = "running"
	m.spinner = 0
	m.scroll = 0
	m.detectedToolCalls = nil
	m.thinking = false
	m.thinkingTokens = 0
	m.eventErrorRendered = false
	systemPrompt := m.systemPrompt(extraSystemPrompt)
	m.liveMessages = append(slices.Clone(m.messages), newMessages...)
	m.contextTokens = m.estimatePromptTokens(m.liveMessages, systemPrompt)
	m.contextEstimate = true

	runCtx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	events := make(chan tea.Msg, 128)
	m.events = events
	m.approvalController = newChatApprovalController(events, m.ensureApprovalState())

	var runEmitted bool
	eventSinks := []coreagent.EventSink{chatEventSink{ctx: runCtx, ch: events, runEmitted: &runEmitted}}
	eventSinks = append(eventSinks, m.opts.EventSinks...)

	var journal coreagent.RunJournal
	if m.store != nil && m.chatID != "" {
		journal = m.store
	}
	session := coreagent.NewSession(coreagent.SessionConfig{
		Client:           m.opts.Client,
		EventSinks:       eventSinks,
		Tools:            m.opts.Tools,
		Skills:           m.opts.Skills,
		DisableTools:     m.opts.ToolsDisabled,
		ApprovalPrompter: m.approvalPrompterForRun(m.autoReviewSessionPrompter()),
		ApprovalState:    m.ensureApprovalState(),
		WorkingDir:       m.currentWorkingDir(),
		SupportsImages:   m.opts.MultiModal,
		Journal:          journal,
		Compactor:        m.opts.Compactor,
	})
	opts := coreagent.RunOptions{
		ChatID:       m.chatID,
		Model:        m.opts.Model,
		SystemPrompt: systemPrompt,
		Messages:     slices.Clone(m.messages),
		NewMessages:  slices.Clone(newMessages),
		Format:       m.opts.Format,
		Options:      m.opts.Options,
		Think:        m.opts.Think,
		KeepAlive:    m.opts.KeepAlive,
		SkillName:    skillName,
	}

	persistedMessages := make([]api.Message, 0, len(m.messages)+len(newMessages))
	persistedMessages = append(persistedMessages, slices.Clone(m.messages)...)
	persistedMessages = append(persistedMessages, slices.Clone(newMessages)...)
	// Wait on the chat's lifetime, not the run's: a canceled run still has a
	// result (and committed history) the chat must receive.
	chatCtx := m.ctx
	go func() {
		defer close(events)
		result, err := session.Run(runCtx, opts)
		select {
		case events <- chatRunDoneMsg{result: result, err: err, runEmitted: runEmitted, persistedMessages: persistedMessages}:
		case <-chatCtx.Done():
		}
	}()

	tickCmd := m.scheduleTick()
	flushModel, flushCmd := m.flowTranscriptFlushCmd()
	*m = flushModel
	return *m, tea.Batch(flushCmd, m.waitEvents(events), tickCmd)
}

func (m *chatModel) finishLiveMessagesForStoppedRun(promote bool, persistedMessages []api.Message) {
	if len(m.liveMessages) == 0 {
		return
	}
	if m.store != nil && m.chatID != "" {
		// A journaled run commits its own projection (or none, if it never
		// started); the store is the source of truth. Syncing the in-memory
		// copy here could truncate what the journal committed.
		if sess, err := m.store.LoadSession(m.chatID); err == nil {
			m.messages = sess.Messages
		}
		m.maybeNameSession(m.messages)
		m.liveMessages = nil
		m.contextTokens = m.estimatePromptTokens(m.messages, "")
		m.contextEstimate = true
		return
	}
	if promote {
		if len(persistedMessages) > 0 {
			m.messages = slices.Clone(persistedMessages)
		} else if !messagesHavePendingToolCalls(m.liveMessages) {
			m.messages = slices.Clone(m.liveMessages)
		}
	}
	m.persistRunResult(m.messages)
	m.maybeNameSession(m.messages)
	m.liveMessages = nil
	m.contextTokens = m.estimatePromptTokens(m.messages, "")
	m.contextEstimate = true
}

func messagesHavePendingToolCalls(messages []api.Message) bool {
	pending := map[string]struct{}{}
	for _, msg := range messages {
		if msg.Role == "assistant" {
			for _, call := range msg.ToolCalls {
				pending[call.ID] = struct{}{}
			}
		}
		if msg.Role == "tool" && msg.ToolCallID != "" {
			delete(pending, msg.ToolCallID)
		}
	}
	return len(pending) > 0
}

func isChatContextCanceledError(err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"))
}
