package chat

import (
	"slices"
	"strings"

	"github.com/ParthSareen/o/api"
	tea "github.com/charmbracelet/bubbletea"

	apptui "github.com/ParthSareen/o/cmd/tui"
)

// initSession ensures the chatModel has a chatID. If a store is configured
// and chatID is empty, a new session row is created. If messages were
// pre-loaded (e.g. from --resume), they are persisted if not already stored.
func (m *chatModel) initSession() {
	if m.store == nil {
		return
	}
	if m.chatID == "" {
		sess, err := m.store.CreateSession(m.opts.Model, m.opts.WorkingDir, m.opts.SystemPrompt, m.opts.Name)
		if err != nil {
			// Non-fatal: chat works without persistence.
			return
		}
		m.chatID = sess.ID
		m.chatName = sess.Name
		// Persist any pre-loaded messages (resume scenario where the caller
		// passed messages via Options but no ChatID — shouldn't normally happen
		// since resume passes the ChatID, but handle it just in case).
		if len(m.messages) > 0 {
			_ = m.store.AppendMessages(m.chatID, m.messages)
		}
	}

	// Load prompt history from the store if available.
	if prompts, err := m.store.RecentPrompts(m.chatID, maxPromptHistory); err == nil && len(prompts) > 0 {
		// Reverse to chronological order (RecentPrompts returns newest first).
		slices.Reverse(prompts)
		// Merge with in-memory history, dedup, cap at maxPromptHistory.
		merged := dedupPrompts(prompts, m.promptHistory)
		if len(merged) > maxPromptHistory {
			merged = merged[len(merged)-maxPromptHistory:]
		}
		m.promptHistory = merged
	}
}

// persistRunResult saves new messages from a completed run to the store.
// It compares the result messages against the in-memory messages to find
// what's new, then appends only the delta.
func (m *chatModel) persistRunResult(resultMessages []api.Message) {
	if m.store == nil || m.chatID == "" {
		return
	}
	// Find messages in resultMessages that aren't yet in m.messages.
	// After a successful run, m.messages is updated to resultMessages by the
	// caller, so we persist based on how many messages we had before the run.
	// The caller handles setting m.messages; here we just append all messages
	// that are new since the last persist.
	//
	// Simple approach: count how many messages we already have persisted,
	// and append the rest.
	count, err := m.store.MessageCount(m.chatID)
	if err != nil {
		return
	}
	if len(resultMessages) <= count {
		return
	}
	newMsgs := resultMessages[count:]
	if len(newMsgs) == 0 {
		return
	}
	_ = m.store.AppendMessages(m.chatID, newMsgs)
}

// persistPrompt saves the user's prompt to the store for history.
func (m *chatModel) persistPrompt(prompt string) {
	if m.store == nil || m.chatID == "" {
		return
	}
	_ = m.store.AddPrompt(m.chatID, prompt)
}

// resetSession creates a fresh session for a new chat.
func (m *chatModel) resetSession() {
	if m.store == nil {
		return
	}
	sess, err := m.store.CreateSession(m.opts.Model, m.opts.WorkingDir, m.opts.SystemPrompt, m.opts.Name)
	if err != nil {
		return
	}
	m.chatID = sess.ID
	m.chatName = sess.Name
}

// dedupPrompts merges two prompt slices (both chronological), removing
// consecutive duplicates, preserving order.
func dedupPrompts(a, b []string) []string {
	merged := make([]string, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	// Remove consecutive duplicates.
	result := merged[:0]
	for i, p := range merged {
		if i > 0 && result[len(result)-1] == p {
			continue
		}
		result = append(result, p)
	}
	return result
}

// sessionListEntry is a UI-facing session summary.
type sessionListEntry struct {
	ID    string
	Name  string
	Model string
	Title string
}

// loadSessionList returns recent sessions for the /sessions picker.
func (m *chatModel) loadSessionList() []sessionListEntry {
	if m.store == nil {
		return nil
	}
	metas, err := m.store.ListSessions(20)
	if err != nil {
		return nil
	}
	entries := make([]sessionListEntry, 0, len(metas))
	for _, meta := range metas {
		title := meta.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		entries = append(entries, sessionListEntry{
			ID:    meta.ID,
			Name:  meta.Name,
			Model: meta.Model,
			Title: title,
		})
	}
	return entries
}

// resumeSession loads a session by ID, replacing the current chat.
func (m *chatModel) resumeSession(id string) bool {
	if m.store == nil {
		return false
	}
	sess, err := m.store.LoadSession(id)
	if err != nil {
		return false
	}
	m.chatID = sess.ID
	m.chatName = sess.Name
	m.opts.Model = sess.Model
	m.opts.Name = sess.Name
	m.messages = slices.Clone(sess.Messages)
	m.liveMessages = nil
	m.entries = entriesFromMessages(sess.Messages)
	m.workingDir = sess.WorkingDir
	m.nextImageID, m.nextAudioID = nextInputAttachmentIDsFromMessages(m.messages)
	m.nextPastedTextID = nextInputPastedTextIDFromMessages(m.messages)
	m.contextTokens = m.estimatePromptTokens(m.messages, "")
	m.contextEstimate = true
	m.scroll = 0
	m.flowPrintedLines = 0
	// Reload prompt history for this session.
	if prompts, err := m.store.RecentPrompts(sess.ID, maxPromptHistory); err == nil && len(prompts) > 0 {
		slices.Reverse(prompts)
		m.promptHistory = normalizePromptHistory(prompts)
	} else {
		m.promptHistory = initialPromptHistory(m.ctx, m.opts)
	}
	return true
}


// openSessionPicker opens a selector modal listing recent sessions.
func (m *chatModel) openSessionPicker() (tea.Model, tea.Cmd) {
	if m.store == nil {
		m.entries = append(m.entries, newSlashEntry("Session history unavailable (no store)."))
		return *m, nil
	}
	entries := m.loadSessionList()
	if len(entries) == 0 {
		m.entries = append(m.entries, newSlashEntry("No saved sessions."))
		return *m, nil
	}
	items := make([]apptui.SelectItem, 0, len(entries))
	for _, e := range entries {
		label := sessionDisplayLabel(e.Name, e.Title)
		desc := label
		if e.Model != "" {
			desc = e.Model + " — " + label
		}
		items = append(items, apptui.SelectItem{
			Name:        e.ID,
			Description: desc,
		})
	}
	picker := apptui.NewModelSelectorModel("Resume session", items, m.chatID, "")
	picker.SetHelpText("↑/↓ navigate • enter resume • esc cancel")
	m.sessionPicker = &picker
	m.sessionPickerEntries = entries
	m.status = "sessions"
	return *m, nil
}

func (m chatModel) updateSessionPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.sessionPicker == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		m.sessionPicker = nil
		m.sessionPickerEntries = nil
		m.status = "ready"
		return m, nil
	case tea.KeyEnter:
		return m.selectSession()
	default:
		m.sessionPicker.UpdateNavigation(msg)
	}
	return m, nil
}

func (m chatModel) selectSession() (tea.Model, tea.Cmd) {
	if m.sessionPicker == nil {
		return m, nil
	}
	selectedItem, ok := m.sessionPicker.SelectedItem()
	if !ok {
		return m, nil
	}
	id := selectedItem.Name
	m.sessionPicker = nil
	m.sessionPickerEntries = nil
	if !m.resumeSession(id) {
		m.entries = append(m.entries, newSlashEntry("Could not load session."))
		m.status = "ready"
		return m, nil
	}
	m.status = "ready"
	return m.withFlowTranscriptFlush(nil)
}

func (m chatModel) renderSessionPicker(width int) string {
	if m.sessionPicker == nil {
		return ""
	}
	return m.sessionPicker.RenderContent()
}

// sessionDisplayLabel picks a human-readable label for a session, preferring
// a user-set name and falling back to the auto-derived title.
func sessionDisplayLabel(name, title string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if title = strings.TrimSpace(title); title != "" {
		return title
	}
	return "(untitled)"
}
