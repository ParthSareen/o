package chat

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/cmd/internal/filedata"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/ollama/ollama/api"
)

const (
	pastedTextPlaceholderMinRunes = 1000
	pastedTextPlaceholderMinLines = 8
)

func initialPromptHistory(ctx context.Context, opts Options) []string {
	var prompts []string
	for _, msg := range opts.Messages {
		if msg.Role == "user" {
			prompts = append(prompts, msg.Content)
		}
	}
	return normalizePromptHistory(prompts)
}

func normalizePromptHistory(prompts []string) []string {
	history := make([]string, 0, min(len(prompts), maxPromptHistory))
	for _, prompt := range prompts {
		prompt = strings.TrimSpace(prompt)
		if prompt == "" || coreagent.IsCompactionSummary(api.Message{Role: "user", Content: prompt}) {
			continue
		}
		history = append(history, prompt)
	}
	if len(history) > maxPromptHistory {
		history = history[len(history)-maxPromptHistory:]
	}
	return history
}

func (m *chatModel) addPromptHistory(prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	m.promptHistory = append(m.promptHistory, prompt)
	if len(m.promptHistory) > maxPromptHistory {
		m.promptHistory = m.promptHistory[len(m.promptHistory)-maxPromptHistory:]
	}
	m.resetPromptHistoryCursor()
}

func (m *chatModel) movePromptHistory(delta int) bool {
	if len(m.promptHistory) == 0 || delta == 0 {
		return false
	}
	if !m.promptActive {
		if delta > 0 {
			return false
		}
		m.promptDraft = slices.Clone(m.input)
		m.promptCursor = len(m.promptHistory) - 1
		m.promptActive = true
	} else {
		m.promptCursor += delta
		if m.promptCursor >= len(m.promptHistory) {
			m.input = slices.Clone(m.promptDraft)
			m.inputCursor = len(m.input)
			m.inputCursorSet = true
			m.inputAttachments = nil
			m.inputPastedTexts = nil
			m.resetPromptHistoryCursor()
			m.complete = 0
			return true
		}
		if m.promptCursor < 0 {
			m.promptCursor = 0
		}
	}

	m.inputPastedTexts = nil
	input := m.promptHistory[m.promptCursor]
	if placeholder, ok := m.pastedTextPlaceholder(input); ok {
		input = placeholder
	}
	m.input = []rune(input)
	m.inputCursor = len(m.input)
	m.inputCursorSet = true
	m.inputAttachments = nil
	m.complete = 0
	return true
}

func (m *chatModel) resetPromptHistoryCursor() {
	m.promptActive = false
	m.promptCursor = 0
	m.promptDraft = nil
}

func (m *chatModel) insertInputNewline() {
	m.insertInputRunes([]rune{'\n'})
}

func (m *chatModel) insertInputRunesFromKey(runes []rune, pasted bool) {
	if len(runes) == 0 {
		return
	}
	if m.opts.MultiModal && (pasted || len(runes) > 1) && m.insertInputFilePlaceholders(string(runes)) {
		return
	}
	if pasted && m.insertPastedTextPlaceholder(string(runes)) {
		return
	}
	m.insertInputRunes(runes)
}

func (m *chatModel) insertInputFilePlaceholders(input string) bool {
	cleaned, files, err := filedata.ExtractWithFiles(input)
	if err != nil {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: err.Error(), err: err.Error()}))
		m.status = "attachment failed"
		return true
	}
	if len(files) == 0 {
		return false
	}

	var parts []string
	if strings.TrimSpace(cleaned) != "" {
		if placeholder, ok := m.pastedTextPlaceholder(cleaned); ok {
			parts = append(parts, placeholder)
		} else {
			parts = append(parts, cleaned)
		}
	}
	for _, file := range files {
		kind := filedata.Kind(file.Path)
		placeholder := m.nextInputAttachmentPlaceholder(kind)
		m.inputAttachments = append(m.inputAttachments, chatInputAttachment{
			placeholder: placeholder,
			kind:        kind,
			data:        file.Data,
		})
		parts = append(parts, placeholder)
	}
	m.insertInputRunes([]rune(strings.Join(parts, " ")))
	return true
}

func (m *chatModel) insertPastedTextPlaceholder(input string) bool {
	placeholder, ok := m.pastedTextPlaceholder(input)
	if !ok {
		return false
	}
	m.insertInputRunes([]rune(placeholder))
	return true
}

func (m *chatModel) pastedTextPlaceholder(input string) (string, bool) {
	if !shouldCollapsePastedText(input) {
		return "", false
	}
	placeholder := m.nextInputPastedTextPlaceholder(input)
	m.inputPastedTexts = append(m.inputPastedTexts, chatInputPastedText{
		placeholder: placeholder,
		content:     input,
	})
	return placeholder, true
}

func shouldCollapsePastedText(input string) bool {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return false
	}
	return len([]rune(trimmed)) >= pastedTextPlaceholderMinRunes || pastedTextLineCount(trimmed) >= pastedTextPlaceholderMinLines
}

func pastedTextLineCount(input string) int {
	if input == "" {
		return 0
	}
	return strings.Count(input, "\n") + 1
}

func (m *chatModel) insertInputRunes(runes []rune) {
	runes = normalizeInputRunes(runes)
	if len(runes) == 0 {
		return
	}
	m.resetPromptHistoryCursor()
	m.disarmQuit()
	cursor := m.normalizedInputCursor()
	next := make([]rune, 0, len(m.input)+len(runes))
	next = append(next, m.input[:cursor]...)
	next = append(next, runes...)
	next = append(next, m.input[cursor:]...)
	m.input = next
	m.inputCursor = cursor + len(runes)
	m.inputCursorSet = true
	m.complete = 0
}

func normalizeInputRunes(runes []rune) []rune {
	for _, r := range runes {
		if r == '\r' {
			return normalizeInputRunesSlow(runes)
		}
	}
	return runes
}

func normalizeInputRunesSlow(runes []rune) []rune {
	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\r' {
			out = append(out, runes[i])
			continue
		}
		out = append(out, '\n')
		if i+1 < len(runes) && runes[i+1] == '\n' {
			i++
		}
	}
	return out
}

func (m *chatModel) deleteInputBackward() {
	cursor := m.normalizedInputCursor()
	if cursor <= 0 {
		return
	}
	start, end, ok := m.placeholderRangeForBackspace(cursor)
	if !ok {
		start, end = cursor-1, cursor
	}
	m.deleteInputRange(start, end)
}

func (m *chatModel) deleteInputWordBackward() {
	cursor := m.normalizedInputCursor()
	if cursor <= 0 {
		return
	}
	start, end, ok := m.placeholderRangeForWordDelete(cursor)
	if !ok {
		start, end = previousInputWordStart(m.input, cursor), cursor
	}
	m.deleteInputRange(start, end)
}

func (m *chatModel) deleteInputForward() {
	cursor := m.normalizedInputCursor()
	if cursor >= len(m.input) {
		return
	}
	start, end, ok := m.placeholderRangeForForwardDelete(cursor)
	if !ok {
		start, end = cursor, cursor+1
	}
	m.deleteInputRange(start, end)
}

func (m *chatModel) deleteInputRange(start, end int) {
	start = clamp(start, 0, len(m.input))
	end = clamp(end, start, len(m.input))
	m.input = append(slices.Clone(m.input[:start]), m.input[end:]...)
	m.inputCursor = start
	m.inputCursorSet = true
	m.complete = 0
	m.syncInputPlaceholders()
}

func (m chatModel) placeholderRangeForBackspace(cursor int) (int, int, bool) {
	cursor = clamp(cursor, 0, len(m.input))
	input := string(m.input)
	for _, placeholder := range m.inputPlaceholders() {
		if placeholder == "" {
			continue
		}
		start, end, ok := inputPlaceholderRuneRange(input, placeholder)
		if ok && cursor > start && cursor <= end {
			return start, end, true
		}
	}
	return 0, 0, false
}

func (m chatModel) placeholderRangeForForwardDelete(cursor int) (int, int, bool) {
	cursor = clamp(cursor, 0, len(m.input))
	input := string(m.input)
	for _, placeholder := range m.inputPlaceholders() {
		if placeholder == "" {
			continue
		}
		start, end, ok := inputPlaceholderRuneRange(input, placeholder)
		if ok && cursor >= start && cursor < end {
			return start, end, true
		}
	}
	return 0, 0, false
}

func (m chatModel) placeholderRangeForWordDelete(cursor int) (int, int, bool) {
	cursor = clamp(cursor, 0, len(m.input))
	end := cursor
	for end > 0 && unicode.IsSpace(m.input[end-1]) {
		end--
	}
	input := string(m.input)
	for _, placeholder := range m.inputPlaceholders() {
		if placeholder == "" {
			continue
		}
		start, placeholderEnd, ok := inputPlaceholderRuneRange(input, placeholder)
		if ok && end > start && end <= placeholderEnd {
			return start, cursor, true
		}
	}
	return 0, 0, false
}

func (m chatModel) inputPlaceholders() []string {
	placeholders := make([]string, 0, len(m.inputAttachments)+len(m.inputPastedTexts))
	for _, attachment := range m.inputAttachments {
		placeholders = append(placeholders, attachment.placeholder)
	}
	for _, pastedText := range m.inputPastedTexts {
		placeholders = append(placeholders, pastedText.placeholder)
	}
	return placeholders
}

func inputPlaceholderRuneRange(input, placeholder string) (int, int, bool) {
	byteStart := strings.Index(input, placeholder)
	if byteStart < 0 {
		return 0, 0, false
	}
	start := len([]rune(input[:byteStart]))
	end := start + len([]rune(placeholder))
	return start, end, true
}

func previousInputWordStart(input []rune, cursor int) int {
	end := clamp(cursor, 0, len(input))
	for end > 0 && unicode.IsSpace(input[end-1]) {
		end--
	}
	start := end
	for start > 0 && !unicode.IsSpace(input[start-1]) {
		start--
	}
	return start
}

func (m *chatModel) syncInputPlaceholders() {
	m.inputAttachments = m.activeInputAttachmentsFor(string(m.input))
	m.inputPastedTexts = m.activeInputPastedTextsFor(string(m.input))
}

func (m chatModel) activeInputAttachmentsFor(input string) []chatInputAttachment {
	if len(m.inputAttachments) == 0 {
		return nil
	}
	active := make([]chatInputAttachment, 0, len(m.inputAttachments))
	for _, attachment := range m.inputAttachments {
		if strings.Contains(input, attachment.placeholder) {
			active = append(active, attachment)
		}
	}
	return active
}

func cloneInputAttachments(in []chatInputAttachment) []chatInputAttachment {
	return slices.Clone(in)
}

type chatInputPastedText struct {
	placeholder string
	content     string
}

func (m chatModel) activeInputPastedTextsFor(input string) []chatInputPastedText {
	if len(m.inputPastedTexts) == 0 {
		return nil
	}
	active := make([]chatInputPastedText, 0, len(m.inputPastedTexts))
	for _, pastedText := range m.inputPastedTexts {
		if strings.Contains(input, pastedText.placeholder) {
			active = append(active, pastedText)
		}
	}
	return active
}

func cloneInputPastedTexts(in []chatInputPastedText) []chatInputPastedText {
	return slices.Clone(in)
}

func (m chatModel) expandPastedTextPlaceholders(input string) string {
	for _, pastedText := range m.activeInputPastedTextsFor(input) {
		input = strings.ReplaceAll(input, pastedText.placeholder, pastedText.content)
	}
	return input
}

func (m *chatModel) nextInputPastedTextPlaceholder(content string) string {
	if m.nextPastedTextID <= 0 {
		m.nextPastedTextID = 1
	}
	id := m.nextPastedTextID
	m.nextPastedTextID++
	return fmt.Sprintf("[Pasted text #%d +%d lines]", id, pastedTextLineCount(strings.TrimSpace(content)))
}

func (m *chatModel) nextInputAttachmentPlaceholder(kind string) string {
	label := inputAttachmentLabel(kind)
	switch kind {
	case "audio":
		id := m.nextAudioID
		m.nextAudioID++
		return fmt.Sprintf("[%s #%d]", label, id)
	default:
		id := m.nextImageID
		m.nextImageID++
		return fmt.Sprintf("[%s #%d]", label, id)
	}
}

func inputAttachmentLabel(kind string) string {
	switch kind {
	case "audio":
		return "Audio"
	default:
		return "Image"
	}
}

var (
	inputAttachmentPlaceholderPattern = regexp.MustCompile(`\[(Image|Audio) #([0-9]+)\]`)
	inputPastedTextPlaceholderPattern = regexp.MustCompile(`\[Pasted text #([0-9]+) \+[0-9]+ lines?\]`)
)

func nextInputAttachmentIDsFromMessages(messages []api.Message) (imageID int, audioID int) {
	for _, msg := range messages {
		for _, match := range inputAttachmentPlaceholderPattern.FindAllStringSubmatch(msg.Content, -1) {
			if len(match) != 3 {
				continue
			}
			id, err := strconv.Atoi(match[2])
			if err != nil {
				continue
			}
			switch match[1] {
			case "Image":
				imageID = max(imageID, id+1)
			case "Audio":
				audioID = max(audioID, id+1)
			}
		}
	}
	return imageID, audioID
}

func nextInputPastedTextIDFromMessages(messages []api.Message) int {
	nextID := 1
	for _, msg := range messages {
		for _, match := range inputPastedTextPlaceholderPattern.FindAllStringSubmatch(msg.Content, -1) {
			if len(match) != 2 {
				continue
			}
			id, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			nextID = max(nextID, id+1)
		}
	}
	return nextID
}

func (m *chatModel) moveInputCursorHorizontal(delta int) bool {
	if delta == 0 {
		return false
	}
	cursor := clamp(m.normalizedInputCursor()+delta, 0, len(m.input))
	if cursor == m.normalizedInputCursor() {
		return false
	}
	m.inputCursor = cursor
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}

func (m *chatModel) moveInputCursorToLineStart() bool {
	cursor := m.normalizedInputCursor()
	start, _ := inputLineBounds(m.input, cursor)
	if start == cursor {
		return false
	}
	m.inputCursor = start
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}

func (m *chatModel) moveInputCursorToLineEnd() bool {
	cursor := m.normalizedInputCursor()
	_, end := inputLineBounds(m.input, cursor)
	if end == cursor {
		return false
	}
	m.inputCursor = end
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}

func (m *chatModel) moveInputCursorWord(delta int) bool {
	if delta == 0 || len(m.input) == 0 {
		return false
	}
	cursor := m.normalizedInputCursor()
	target := cursor
	if delta < 0 {
		for target > 0 && unicode.IsSpace(m.input[target-1]) {
			target--
		}
		for target > 0 && !unicode.IsSpace(m.input[target-1]) {
			target--
		}
	} else {
		for target < len(m.input) && !unicode.IsSpace(m.input[target]) {
			target++
		}
		for target < len(m.input) && unicode.IsSpace(m.input[target]) {
			target++
		}
	}
	if target == cursor {
		return false
	}
	m.inputCursor = target
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}

func (m *chatModel) handleInputAltRunes(runes []rune) bool {
	if len(runes) != 1 {
		return false
	}
	switch runes[0] {
	case 'b', 'B':
		m.moveInputCursorWord(-1)
		return true
	case 'f', 'F':
		m.moveInputCursorWord(1)
		return true
	default:
		return false
	}
}

func (m *chatModel) moveInputCursorVertical(delta int) bool {
	if delta == 0 || len(m.input) == 0 {
		return false
	}
	cursor := m.normalizedInputCursor()
	lineStart, lineEnd := inputLineBounds(m.input, cursor)
	column := cursor - lineStart
	var targetStart, targetEnd int
	if delta < 0 {
		if lineStart == 0 {
			return false
		}
		targetEnd = lineStart - 1
		targetStart, _ = inputLineBounds(m.input, targetEnd)
	} else {
		if lineEnd >= len(m.input) {
			return false
		}
		targetStart = lineEnd + 1
		_, targetEnd = inputLineBounds(m.input, targetStart)
	}
	target := min(targetStart+column, targetEnd)
	m.inputCursor = target
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}

func inputLineBounds(input []rune, cursor int) (int, int) {
	cursor = clamp(cursor, 0, len(input))
	start := cursor
	for start > 0 && input[start-1] != '\n' {
		start--
	}
	end := cursor
	for end < len(input) && input[end] != '\n' {
		end++
	}
	return start, end
}

func (m chatModel) normalizedInputCursor() int {
	if !m.inputCursorSet {
		return len(m.input)
	}
	return clamp(m.inputCursor, 0, len(m.input))
}

func isShiftEnterCSI(msg tea.Msg) bool {
	switch fmt.Sprint(msg) {
	case "?CSI[49 51 59 50 117]?", // \x1b[13;2u
		"?CSI[49 51 59 50 126]?",          // \x1b[13;2~
		"?CSI[50 55 59 50 59 49 51 126]?": // \x1b[27;2;13~
		return true
	default:
		return false
	}
}
