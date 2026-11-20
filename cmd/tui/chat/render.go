package chat

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/charmbracelet/lipgloss"
)

type chatEntry struct {
	role       string
	content    string
	label      string
	detail     string
	status     string
	err        string
	toolID     string
	args       map[string]any
	expanded   bool
	startedAt  time.Time
	finishedAt time.Time
	tools      []chatEntry
	metrics    *api.Metrics

	version     int
	renderKey   chatEntryRenderKey
	renderLines []string
	codeBlocks  map[markdownCodeBlockCacheKey]string
}

const (
	chatMessageIndent       = "  "
	chatUserMessagePrefix   = ""
	maxCtrlOToolOutputRunes = 400

	defaultViewWidth  = 80
	defaultViewHeight = 24
)

type chatEntryRenderKey struct {
	width   int
	version int
}

func (m *chatModel) ensureAssistantEntry() int {
	if len(m.entries) > 0 && m.entries[len(m.entries)-1].role == "assistant" {
		return len(m.entries) - 1
	}
	m.entries = append(m.entries, newChatEntry(chatEntry{role: "assistant"}))
	return len(m.entries) - 1
}

func (m chatModel) renderTranscript(width int) string {
	var b strings.Builder
	first := true
	for index, entry := range m.entries {
		prefix, body := m.renderEntry(entry)
		prefixWidth := lipgloss.Width(prefix)
		continuation := ""
		if prefixWidth > 0 {
			continuation = strings.Repeat(" ", prefixWidth)
		}
		lines := m.renderEntryLinesCached(index, entry, body, width-prefixWidth)
		if len(lines) == 0 {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		first = false
		for i, line := range lines {
			if i == 0 {
				b.WriteString(prefix)
				b.WriteString(line)
			} else {
				b.WriteString(continuation)
				b.WriteString(line)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// renderEntryLinesCached renders (and memoizes) the wrapped lines for an entry.
// Despite the value receiver, the cache write below mutates the caller's entries
// in place: m.entries is a slice, so the value-receiver copy shares its backing
// array with the caller. markEntryDirty invalidates a cached entry by bumping
// its version, which entryRenderKey compares against to decide whether to reuse
// or re-render.
func (m chatModel) renderEntryLinesCached(index int, entry chatEntry, body string, width int) []string {
	key := entryRenderKey(entry, width)
	if index >= 0 && index < len(m.entries) {
		cached := m.entries[index]
		if cached.renderKey == key && cached.renderLines != nil {
			return cached.renderLines
		}
	}

	var codeCache *map[markdownCodeBlockCacheKey]string
	if index >= 0 && index < len(m.entries) {
		codeCache = &m.entries[index].codeBlocks
	}
	lines := m.renderEntryLines(entry, body, width, codeCache)
	if index >= 0 && index < len(m.entries) {
		m.entries[index].renderKey = key
		m.entries[index].renderLines = slices.Clone(lines)
	}
	return lines
}

func (m chatModel) transcriptLines(width int) []string {
	transcript := m.renderTranscript(width)
	transcript = strings.TrimRight(transcript, "\n")
	if transcript == "" {
		return nil
	}
	return strings.Split(transcript, "\n")
}

func (m chatModel) visibleTranscriptStartLine(width, available int) int {
	return m.visibleTranscriptStartLineForLines(len(m.transcriptLines(width)), available)
}

func (m chatModel) visibleTranscriptStartLineForLines(total, available int) int {
	if total <= available || available <= 0 {
		return 0
	}
	maxScroll := total - available
	scroll := clamp(m.scroll, 0, maxScroll)
	return maxScroll - scroll
}

func (m chatModel) viewWidth() int {
	width, _ := m.viewSize()
	return width
}

// defaultSize clamps caller-supplied dimensions to the standard fallbacks
// (80x24) when unset. Free functions that receive width/height as arguments
// (rather than reading them off the model) use this directly; methods use
// viewSize, which reads m.width/m.height first.
func defaultSize(width, height int) (int, int) {
	if width <= 0 {
		width = defaultViewWidth
	}
	if height <= 0 {
		height = defaultViewHeight
	}
	return width, height
}

func (m chatModel) viewSize() (int, int) {
	return defaultSize(m.width, m.height)
}

func (m chatModel) transcriptHeight() int {
	width := m.transcriptWidth()
	_, height := m.viewSize()
	lineCount := len(m.transcriptLines(width))
	baseHeaderHeight := 2
	baseMaxHeight := height - baseHeaderHeight
	baseBottomLines := m.bottomLines(width, baseMaxHeight)
	baseHeight := max(0, baseMaxHeight-len(baseBottomLines)-transcriptInputGap(baseMaxHeight, len(baseBottomLines), lineCount))
	if lineCount <= baseHeight {
		return lineCount
	}
	statusHeaderHeight := 3
	statusMaxHeight := height - statusHeaderHeight
	statusBottomLines := m.bottomLines(width, statusMaxHeight)
	return max(0, statusMaxHeight-len(statusBottomLines)-transcriptInputGap(statusMaxHeight, len(statusBottomLines), lineCount))
}

// transcriptWidth is the width of the root transcript: the full terminal
// normally, the left column while the side split is showing.
func (m chatModel) transcriptWidth() int {
	width, _ := m.viewSize()
	if m.sideSplitVisible() {
		width, _ = splitPaneWidths(width)
	}
	return width
}

func (m chatModel) transcriptLayout() (top, height int) {
	width := m.transcriptWidth()
	_, height = m.viewSize()
	headerHeight := len(m.headerLines())
	bottomLines := m.bottomLines(width, height-headerHeight)
	transcriptLineCount := len(m.transcriptLines(width))
	return headerHeight, max(0, height-headerHeight-len(bottomLines)-transcriptInputGap(height-headerHeight, len(bottomLines), transcriptLineCount))
}

func (m chatModel) maxScroll() int {
	width, _ := m.viewSize()
	return max(0, len(m.transcriptLines(width))-m.transcriptHeight())
}

func (m chatModel) bottomLines(width, maxHeight int) []string {
	var lines []string
	if m.modelPicker != nil {
		lines = append(lines, m.renderInlineModelPicker(width)...)
	} else {
		lines = append(lines, m.completionLines(width)...)
	}

	actionStatusLines := m.renderActionStatusLines(width)
	approvalLines := m.renderApprovalPromptLines(width)
	if maxHeight > 0 {
		maxApprovalLines := max(0, maxHeight-len(lines)-len(actionStatusLines)-3)
		if len(approvalLines) > maxApprovalLines {
			approvalLines = approvalLines[:maxApprovalLines]
		}
		maxActionStatusLines := max(1, maxHeight-len(lines)-len(approvalLines)-3)
		if len(actionStatusLines) > maxActionStatusLines {
			actionStatusLines = actionStatusLines[:maxActionStatusLines]
		}
	}

	lines = append(lines, actionStatusLines...)
	lines = append(lines, approvalLines...)
	modelLines := m.renderModelStatusLines(width)
	fixedLines := len(lines) + 2
	if len(modelLines) > 0 {
		fixedLines += len(modelLines)
	}
	inputBodyLines := maxInputBoxBodyLines
	if maxHeight > 0 {
		inputBodyLines = min(inputBodyLines, max(1, maxHeight-fixedLines))
	}
	inputCursor := m.normalizedInputCursor()
	if m.approvalPrompt != nil || m.cloudAuthPrompt != nil {
		inputCursor = -1
	}
	lines = append(lines, renderInputBoxLines(string(m.input), inputCursor, width, inputBodyLines, m.emptyInputPlaceholder())...)
	if len(modelLines) > 0 {
		lines = append(lines, modelLines...)
	}
	return lines
}

func (m chatModel) renderModelStatusLines(width int) []string {
	if m.modelPicker != nil {
		return nil
	}
	var parts []string
	if model := strings.TrimSpace(m.opts.Model); model != "" {
		parts = append(parts, model)
	}
	if contextStatus := m.contextStatus(); contextStatus != "" {
		parts = append(parts, contextStatus)
	}
	if turnMetrics := m.turnMetricsStatus(); turnMetrics != "" {
		parts = append(parts, turnMetrics)
	}
	if notice := m.permissionModeNotice(); notice != "" {
		parts = append(parts, notice)
	}
	if len(parts) == 0 {
		return nil
	}
	indent := inputBoxTextIndent()
	lines := wrapChatText(strings.Join(parts, "   "), max(20, width-lipgloss.Width(indent)))
	for i := range lines {
		lines[i] = renderFooterPlainLine(indent + lines[i])
	}
	return lines
}

func (m chatModel) renderActionStatusLines(width int) []string {
	if activity := m.activityLine(); activity != "" {
		return []string{chatMetaStyle.Render(inputBoxTextIndent() + activity)}
	}
	if notificationLines := m.renderNotificationLines(width); len(notificationLines) > 0 {
		return notificationLines
	}
	return nil
}

func transcriptInputGap(maxHeight, bottomLineCount, transcriptLineCount int) int {
	const desiredGap = 1
	if transcriptLineCount == 0 {
		return 0
	}
	if maxHeight <= 0 {
		return desiredGap
	}
	available := maxHeight - bottomLineCount
	if available <= 1 {
		return 0
	}
	return min(desiredGap, available-1)
}

// scrolledView renders the app-level scrollback viewport shown while the
// user has scrolled away from the bottom (m.scroll > 0). It is rendered by
// the app (not the terminal's native scrollback), and scroll anchoring keeps
// the same transcript lines on screen while new content streams in
// underneath, so the reading position never drifts.
func (m chatModel) scrolledView(width, height int) string {
	all := m.transcriptLines(width)
	bottomLines := m.bottomLines(width, 0)
	available := max(1, m.transcriptHeight())
	start := m.visibleTranscriptStartLineForLines(len(all), available)
	end := min(len(all), start+available)
	lines := slices.Clone(all[start:end])
	for range transcriptInputGap(0, len(bottomLines), len(all)) {
		lines = append(lines, "")
	}
	lines = append(lines, bottomLines...)
	return strings.Join(lines, "\n")
}

// scrollAnchor captures the scrolled viewport's position by *content*: the
// lines currently at the top of the visible window. Anchoring by content
// (rather than by total line count) keeps the window stable across any
// transcript change — appends at the tail, reflows, and lines appearing or
// disappearing *above* the viewport (thinking collapse, table formation,
// compaction).
type scrollAnchor struct {
	lineCount int
	startLine int
	lines     []string
}

// scrollAnchorFingerprintLines is how many visible lines the fingerprint spans.
const scrollAnchorFingerprintLines = 4

// scrollAnchorSnapshot captures the scrolled position. It is a no-op
// (returns the zero value) while pinned to the bottom.
func (m chatModel) scrollAnchorSnapshot() scrollAnchor {
	if m.scroll <= 0 {
		return scrollAnchor{}
	}
	width := m.viewWidth()
	all := m.transcriptLines(width)
	available := max(1, m.transcriptHeight())
	start := m.visibleTranscriptStartLineForLines(len(all), available)
	end := min(start+scrollAnchorFingerprintLines, len(all))
	return scrollAnchor{
		lineCount: len(all),
		startLine: start,
		lines:     slices.Clone(all[start:end]),
	}
}

// anchorScrollToAppendedLines re-pins the scrolled viewport to the content
// captured in prev: it first locates the fingerprint in the new transcript
// (handling arbitrary mid-transcript edits above the viewport), and falls
// back to the signed line-count delta when the content itself was edited
// (pure rewrites can't be located). Only runs while scrolled up; the
// bottom-pinned case (scroll == 0) tracks the tail as before.
func (m *chatModel) anchorScrollToAppendedLines(prev scrollAnchor) {
	if m.scroll <= 0 || prev.lineCount <= 0 {
		return
	}
	all := m.transcriptLines(m.viewWidth())
	maxScroll := len(all) - max(1, m.transcriptHeight())
	if maxScroll <= 0 {
		return // no longer scrollable; scroll clamps to 0 elsewhere
	}
	if idx, ok := findScrollAnchorOffset(all, prev); ok {
		m.scroll = clamp(maxScroll-idx, 0, maxScroll)
		return
	}
	m.scroll = clamp(m.scroll+len(all)-prev.lineCount, 0, maxScroll)
}

// findScrollAnchorOffset locates the anchor fingerprint in the new
// transcript, preferring longer fingerprints (more specific) and, among
// matches at the same length, the one closest to the old position.
func findScrollAnchorOffset(all []string, prev scrollAnchor) (int, bool) {
	for size := len(prev.lines); size > 0; size-- {
		probe := prev.lines[:size]
		if allBlankLines(probe) {
			continue
		}
		best := -1
		for i := 0; i+size <= len(all); i++ {
			match := true
			for j, want := range probe {
				if all[i+j] != want {
					match = false
					break
				}
			}
			if match && (best < 0 || dist(i, prev.startLine) < dist(best, prev.startLine)) {
				best = i
			}
		}
		if best >= 0 {
			return best, true
		}
	}
	return 0, false
}

func dist(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

func (m *chatModel) scrollBy(lines int) {
	if lines == 0 {
		return
	}
	m.scroll = clamp(m.scroll+lines, 0, m.maxScroll())
}

func (m chatModel) normalizedSelectionRange() (chatSelectionPoint, chatSelectionPoint, bool) {
	return normalizedSelectionRangeFor(m.selection)
}

func normalizedSelectionRangeFor(selection chatSelection) (chatSelectionPoint, chatSelectionPoint, bool) {
	if !selection.active {
		return chatSelectionPoint{}, chatSelectionPoint{}, false
	}
	start, end := selection.anchor, selection.cursor
	if start.line > end.line || (start.line == end.line && start.col > end.col) {
		start, end = end, start
	}
	if start.line == end.line && start.col == end.col {
		return chatSelectionPoint{}, chatSelectionPoint{}, false
	}
	return start, end, true
}

func (m chatModel) selectedTranscriptText(width int) string {
	start, end, ok := m.normalizedSelectionRange()
	if !ok {
		return ""
	}
	lines := m.transcriptLines(width)
	if len(lines) == 0 {
		return ""
	}
	start.line = clamp(start.line, 0, len(lines)-1)
	end.line = clamp(end.line, 0, len(lines)-1)
	var selected []string
	for lineIndex := start.line; lineIndex <= end.line; lineIndex++ {
		text := transcriptLineTextForSelection(stripChatANSI(lines[lineIndex]))
		runes := []rune(text)
		startCol, endCol := 0, len(runes)
		if lineIndex == start.line {
			startCol = displayColumnToRuneIndex(text, start.col)
		}
		if lineIndex == end.line {
			endCol = displayColumnToRuneIndex(text, end.col)
		}
		if startCol > endCol {
			startCol, endCol = endCol, startCol
		}
		selected = append(selected, string(runes[startCol:endCol]))
	}
	return strings.TrimRight(strings.Join(selected, "\n"), "\n")
}

func transcriptLineTextForSelection(text string) string {
	firstPrefix := chatMessageIndent + chatUserMessagePrefix
	if strings.HasPrefix(text, firstPrefix) {
		return chatMessageIndent + strings.TrimPrefix(text, firstPrefix)
	}
	continuationPrefix := chatMessageIndent + strings.Repeat(" ", lipgloss.Width(chatUserMessagePrefix))
	if strings.HasPrefix(text, continuationPrefix) {
		return chatMessageIndent + strings.TrimPrefix(text, continuationPrefix)
	}
	return text
}

func (m chatModel) renderEntry(entry chatEntry) (string, string) {
	switch entry.role {
	case "user":
		return "", entry.content
	case "assistant":
		return "", entry.content
	case "thinking":
		return chatMetaStyle.Render("•") + " ", thinkingStatusLine(entry)
	case "review":
		prefix := chatMetaStyle.Render("⟳ ") + " "
		return prefix, entry.label
	case "slash":
		return chatMetaStyle.Render("•") + " ", entry.content
	case "compaction_summary":
		prefix := toolStatusStyle(entry.status).Render("•") + " "
		return prefix, compactionSummaryStatusLine(entry)
	case "tool":
		prefix := toolStatusStyle(entry.status).Render("•") + " "
		return prefix, toolStatusLine(entry)
	case "tool_group":
		prefix := toolGroupPrefixStyle(entry).Render("•") + " "
		return prefix, toolGroupStatusLine(entry)
	case "error":
		return chatErrorStyle.Render("err ") + " ", entry.content
	case "system":
		return "", entry.content
	default:
		return "", entry.content
	}
}

func (m chatModel) renderEntryLines(entry chatEntry, body string, width int, codeCache *map[markdownCodeBlockCacheKey]string) []string {
	if width < 20 {
		width = 20
	}
	switch entry.role {
	case "assistant":
		innerWidth := max(1, width-lipgloss.Width(chatMessageIndent))
		lines := indentLines(splitRenderedBody(renderMarkdownForViewWithCodeCache(body, innerWidth, codeCache)), chatMessageIndent)
		lines = append(lines, indentLines(renderMetricsLines(entry.metrics, innerWidth), chatMessageIndent)...)
		return lines
	case "thinking":
		return renderThinkingLines(entry, width)
	case "review":
		return renderReviewLines(entry, width)
	case "system", "slash":
		return splitRenderedBody(renderMarkdownForViewWithCodeCache(body, width, codeCache))
	case "user":
		return renderUserMessageLines(body, width)
	case "compaction_summary":
		return renderCompactionSummaryLines(entry, width)
	case "tool":
		if entryHasExpandableOutput(entry) {
			return renderToolResultLines(entry, width)
		}
		if isToolResultStatus(entry.status) {
			return renderToolResultLines(entry, width)
		}
		return wrapChatText(body, width)
	case "tool_group":
		return renderToolGroupLines(entry, width)
	default:
		return wrapChatText(body, width)
	}
}

func renderUserMessageLines(content string, width int) []string {
	if width < 20 {
		width = 20
	}
	firstPrefix := chatMessageIndent + chatUserMessagePrefix
	continuationPrefix := chatMessageIndent + strings.Repeat(" ", lipgloss.Width(chatUserMessagePrefix))
	innerWidth := max(1, width-lipgloss.Width(firstPrefix))
	lines := wrapChatText(content, innerWidth)
	for i, line := range lines {
		prefix := continuationPrefix
		if i == 0 {
			prefix = firstPrefix
		}
		lines[i] = chatUserBlockStyle.Render(padRenderedLine(prefix+line, width))
	}
	return lines
}

func renderMetricsLines(metrics *api.Metrics, width int) []string {
	summary := metricsSummaryLines(metrics)
	if len(summary) == 0 {
		return nil
	}
	var lines []string
	for _, line := range summary {
		for _, wrapped := range wrapChatText(line, width) {
			lines = append(lines, chatMetaStyle.Render(wrapped))
		}
	}
	return lines
}

func metricsSummaryLines(metrics *api.Metrics) []string {
	if metrics == nil || metricsEmpty(*metrics) {
		return nil
	}
	var lines []string
	if metrics.TotalDuration > 0 {
		lines = append(lines, fmt.Sprintf("total duration:       %v", metrics.TotalDuration))
	}
	if metrics.LoadDuration > 0 {
		lines = append(lines, fmt.Sprintf("load duration:        %v", metrics.LoadDuration))
	}
	if metrics.PromptEvalCount > 0 {
		lines = append(lines, fmt.Sprintf("prompt eval count:    %d token(s)", metrics.PromptEvalCount))
	}
	if metrics.PromptEvalDuration > 0 {
		lines = append(lines, fmt.Sprintf("prompt eval duration: %s", metrics.PromptEvalDuration))
		lines = append(lines, fmt.Sprintf("prompt eval rate:     %.2f tokens/s", float64(metrics.PromptEvalCount)/metrics.PromptEvalDuration.Seconds()))
	}
	if metrics.EvalCount > 0 {
		lines = append(lines, fmt.Sprintf("eval count:           %d token(s)", metrics.EvalCount))
	}
	if metrics.EvalDuration > 0 {
		lines = append(lines, fmt.Sprintf("eval duration:        %s", metrics.EvalDuration))
		lines = append(lines, fmt.Sprintf("eval rate:            %.2f tokens/s", float64(metrics.EvalCount)/metrics.EvalDuration.Seconds()))
	}
	return lines
}

func metricsEmpty(metrics api.Metrics) bool {
	return metrics.TotalDuration <= 0 &&
		metrics.LoadDuration <= 0 &&
		metrics.PromptEvalCount <= 0 &&
		metrics.PromptEvalDuration <= 0 &&
		metrics.EvalCount <= 0 &&
		metrics.EvalDuration <= 0
}

func historyRoleStyle(role string) lipgloss.Style {
	switch role {
	case "system":
		return chatHistorySystemRoleStyle
	case "user":
		return chatHistoryUserRoleStyle
	case "assistant":
		return chatHistoryAssistantRoleStyle
	case "tool":
		return chatHistoryToolRoleStyle
	default:
		return chatHistoryTitleStyle
	}
}

func renderCompactionSummaryLines(entry chatEntry, width int) []string {
	lines := wrapChatText(compactionSummaryStatusLine(entry), width)
	if !entry.expanded || strings.TrimSpace(entry.content) == "" {
		return lines
	}
	lines = append(lines, "")
	lines = append(lines, indentLines(splitRenderedBody(renderMarkdownForView(entry.content, width-2)), "  ")...)
	return lines
}

func compactionSummaryStatusLine(entry chatEntry) string {
	segment := toolStatusStyle(entry.status).Render(toolStatusLabel(entry))
	if segment == "" {
		return "Compacted summary"
	}
	return fmt.Sprintf("Compacted summary %s", segment)
}

// reviewStatusLabel formats the auto-review verdict for the transcript.
// The grading model is named when it differs from the session model.
func reviewStatusLabel(review *coreagent.ReviewDecision, sessionModel string) string {
	if review == nil {
		return ""
	}
	label := fmt.Sprintf("auto review %s (%s risk", review.Outcome, review.Risk)
	if review.Duration > 0 {
		label += ", " + review.Duration.Round(time.Millisecond).String()
	}
	if review.Calls > 1 {
		label += fmt.Sprintf(", %d calls", review.Calls)
	}
	label += ")"
	if review.Model != "" && review.Model != sessionModel {
		label += " · graded by " + review.Model
	}
	return label
}

func renderReviewLines(entry chatEntry, width int) []string {
	lines := wrapChatText(strings.TrimSpace(entry.label), width)
	if !entry.expanded || strings.TrimSpace(entry.content) == "" {
		return lines
	}
	lines = append(lines, "")
	lines = append(lines, indentLines(wrapChatText(entry.content, width-2), "  ")...)
	return lines
}

func renderThinkingLines(entry chatEntry, width int) []string {
	if !entry.expanded || strings.TrimSpace(entry.content) == "" {
		return nil
	}
	lines := wrapChatText(thinkingStatusLine(entry), width)
	lines = append(lines, "")
	lines = append(lines, indentLines(splitRenderedBody(renderMarkdownForView(entry.content, width-2)), "  ")...)
	return lines
}

func thinkingStatusLine(entry chatEntry) string {
	if strings.TrimSpace(entry.label) != "" {
		return entry.label
	}
	return "Thinking"
}

func (m chatModel) thinkingLabel() string {
	return thinkingActivityLabel(m.thinkingTokens)
}

func thinkingActivityLabel(tokens int) string {
	if tokens > 0 {
		return "Thinking ↓ " + formatTokenCount(tokens)
	}
	return "Thinking"
}

func (m *chatModel) syncThinkingEntry() {
	if strings.TrimSpace(m.latestLiveThinking()) == "" {
		return
	}
	idx := -1
	if len(m.entries) > 0 && m.entries[len(m.entries)-1].role == "thinking" && m.entries[len(m.entries)-1].status == "running" {
		idx = len(m.entries) - 1
	}
	if idx < 0 {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "thinking", status: "running", expanded: m.toolOutputOpen}))
		idx = len(m.entries) - 1
	}
	m.entries[idx].content = m.latestLiveThinking()
	m.entries[idx].label = m.thinkingLabel()
	m.entries[idx].status = "running"
	// Leave entry.expanded alone: ctrl+o may be showing the live thinking.
	m.markEntryDirty(idx)
}

func (m chatModel) latestLiveThinking() string {
	for i := len(m.liveMessages) - 1; i >= 0; i-- {
		if m.liveMessages[i].Role == "assistant" && strings.TrimSpace(m.liveMessages[i].Thinking) != "" {
			return m.liveMessages[i].Thinking
		}
	}
	return ""
}

func (m *chatModel) finishThinkingEntry() {
	if len(m.entries) == 0 {
		return
	}
	idx := len(m.entries) - 1
	if m.entries[idx].role != "thinking" || m.entries[idx].status != "running" {
		return
	}
	m.entries[idx].status = "done"
	m.entries[idx].label = m.thinkingLabel()
	m.markEntryDirty(idx)
}

func (m chatModel) notificationLine() string {
	status := strings.TrimSpace(m.status)
	if status == "" || status == "ready" {
		return ""
	}
	if m.running || m.compacting || m.approvalPrompt != nil {
		return ""
	}
	switch status {
	case "running", "compacting", "approval required", "full access enabled", "review mode enabled":
		return ""
	default:
		return status
	}
}

func (m chatModel) renderNotificationLines(width int) []string {
	line := m.notificationLine()
	if line == "" {
		return nil
	}
	indent := inputBoxTextIndent()
	lines := wrapChatText(line, max(20, width-lipgloss.Width(indent)))
	for i, wrapped := range lines {
		lines[i] = chatNotificationStyle.Render(indent + wrapped)
	}
	return lines
}

func renderFooterPlainLine(line string) string {
	const fullAccess = "full access"
	if !strings.Contains(line, fullAccess) {
		return chatFooterStyle.Render(line)
	}

	var b strings.Builder
	for {
		before, after, ok := strings.Cut(line, fullAccess)
		if before != "" {
			b.WriteString(chatFooterStyle.Render(before))
		}
		if !ok {
			break
		}
		b.WriteString(chatFullAccessStyle.Render(fullAccess))
		line = after
	}
	return b.String()
}

func (m chatModel) permissionModeNotice() string {
	if notice := strings.TrimSpace(m.permissionNotice); notice != "" {
		return notice
	}
	switch strings.TrimSpace(m.status) {
	case "full access enabled", "review mode enabled":
		return strings.TrimSpace(m.status)
	}
	if m.allowAllToolsEnabled() && m.notificationLine() == "" {
		return "full access enabled"
	}
	return ""
}

func (m *chatModel) refreshContextWindowTokens(modelName string) {
	if m == nil || m.opts.ContextWindowTokensForModel == nil {
		return
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	tokens := m.opts.ContextWindowTokensForModel(ctx, modelName, m.opts.ContextWindowTokens)
	m.updateContextWindowTokens(tokens)
}

func (m *chatModel) updateContextWindowTokens(tokens int) {
	if tokens <= 0 || tokens == m.opts.ContextWindowTokens {
		return
	}
	m.opts.ContextWindowTokens = tokens
	if compactor, ok := m.opts.Compactor.(*coreagent.SimpleCompactor); ok && compactor != nil {
		compactor.Options.ContextWindowTokens = tokens
	}
}

func (m chatModel) currentWorkingDir() string {
	if strings.TrimSpace(m.workingDir) != "" {
		return m.workingDir
	}
	return m.opts.WorkingDir
}

func (m chatModel) activityLine() string {
	if m.approvalPrompt != nil {
		return ""
	}
	if m.preloadingModel != "" && !m.running && !m.compacting {
		return ""
	}
	if !m.running && !m.compacting && m.preloadingModel == "" && m.approvalPrompt == nil {
		return ""
	}
	label := m.activityLabel()
	if label == "" {
		if m.awaitingToolStart() {
			return statusWithSpinner(m.spinnerFrame(), "Working")
		}
		if m.awaitingModel {
			return statusWithSpinner(m.spinnerFrame(), "Working")
		}
		if !m.waitingForModel() || m.spinner < idleWorkingDelayTicks {
			return ""
		}
		if m.preloadingModel != "" {
			return statusWithSpinner(m.spinnerFrame(), "Working")
		}
		return statusWithSpinner(m.spinnerFrame(), "Working")
	}
	if m.thinking {
		// thinking deltas reset the spinner every tick interval they arrive;
		// when the stream goes quiet (the model has stopped thinking and is
		// building its (tool) call) surface the idle Working indicator
		if m.spinner >= idleWorkingDelayTicks {
			return statusWithSpinner(m.spinnerFrame(), "Working")
		}
		return label
	}
	return statusWithSpinner(m.spinnerFrame(), label)
}

func (m chatModel) activityLabel() string {
	if m.status == "canceling" {
		return "canceling"
	}
	if m.compacting {
		if m.compactingTokens > 0 {
			return "Compacting " + formatTokenCount(m.compactingTokens)
		}
		return "Compacting"
	}
	if m.thinking {
		return thinkingActivityLabel(m.thinkingTokens)
	}
	start := m.currentTurnEntryStart()
	for i := len(m.entries) - 1; i >= start; i-- {
		entry := m.entries[i]
		switch entry.role {
		case "tool":
			if isToolActiveStatus(entry.status) {
				return ""
			}
		case "tool_group":
			if entryHasActiveTool(entry) {
				return ""
			}
		case "assistant":
			if entry.content != "" {
				return ""
			}
		}
	}
	return ""
}

func (m chatModel) waitingForModel() bool {
	if m.preloadingModel != "" && !m.compacting && m.approvalPrompt == nil && m.status != "canceling" {
		return true
	}
	if !m.running || m.compacting || m.approvalPrompt != nil || m.thinking || m.status == "canceling" {
		return false
	}
	if m.awaitingModel {
		return true
	}
	start := m.currentTurnEntryStart()
	for i := len(m.entries) - 1; i >= start; i-- {
		entry := m.entries[i]
		switch entry.role {
		case "tool":
			if isToolActiveStatus(entry.status) {
				return false
			}
		case "tool_group":
			if entryHasActiveTool(entry) {
				return false
			}
		case "assistant":
			return true
		}
	}
	return true
}

func (m chatModel) currentTurnEntryStart() int {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].role == "user" {
			return i + 1
		}
	}
	return 0
}

func (m *chatModel) applyResponseMetrics(response *api.ChatResponse) {
	if response == nil {
		return
	}
	tokens := 0
	if response.PromptEvalCount > 0 {
		tokens = response.PromptEvalCount
	}
	if response.EvalCount > 0 {
		tokens += response.EvalCount
	}
	if tokens <= 0 {
		return
	}
	if m.running {
		if tokens > m.contextTokens {
			m.contextTokens = tokens
		}
		return
	}
	if tokens > 0 {
		m.contextTokens = tokens
		m.contextEstimate = false
	}
}

func (m chatModel) estimatePromptTokens(messages []api.Message, systemPrompt string) int {
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = m.systemPrompt("")
	}
	var tools api.Tools
	if m.opts.Tools != nil {
		tools = m.opts.Tools.Tools()
	}
	return estimatePromptTokenCount(systemPrompt, messages, tools, m.opts.Format)
}

func estimatePromptTokenCount(systemPrompt string, messages []api.Message, tools api.Tools, format string) int {
	total := approximateTokenCount(systemPrompt)
	for _, msg := range messages {
		total += approximateTokenCount(msg.Role)
		total += approximateTokenCount(msg.Content)
		total += approximateTokenCount(msg.Thinking)
		total += approximateTokenCount(msg.ToolName)
		total += approximateTokenCount(msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			total += approximateTokenCount(call.Function.Name)
			total += approximateTokenCount(call.Function.Arguments.String())
		}
	}
	total += approximateTokenCount(tools.String())
	total += approximateTokenCount(format)
	return total
}

func approximateTokenCount(text string) int {
	n := len([]rune(text))
	if n <= 0 {
		return 0
	}
	return max(1, (n+3)/4)
}

func formatTokenCount(count int) string {
	if count == 1 {
		return "1 token"
	}
	return fmt.Sprintf("%d tokens", count)
}

// turnMetricsStatus renders the last chat POST's usage segment for the
// status line: input tokens the server evaluated (the full prompt on
// servers that report cached tokens), output tokens it generated, and the
// cache hit rate as a percentage of the input. The segment is omitted
// until Ollama reports usage, and the cache hit is omitted when the server
// does not report cached tokens (older local servers and Ollama Cloud).
func (m chatModel) turnMetricsStatus() string {
	metrics := m.lastTurnMetrics
	if metrics == nil {
		return ""
	}
	input := max(metrics.PromptEvalCount, 0)
	output := max(metrics.EvalCount, 0)
	if input <= 0 && output <= 0 {
		return ""
	}
	status := "in " + formatContextTokenCount(input) + "  out " + formatContextTokenCount(output)
	if cached, ok := metrics.CachedPromptTokens(); ok && input > 0 {
		hit := (cached*100 + input/2) / input
		status += fmt.Sprintf("  ch %d%%", min(hit, 100))
	}
	return status
}

func (m chatModel) contextStatus() string {
	window := m.displayContextWindowTokens()
	if window <= 0 {
		return ""
	}
	used := max(m.contextTokens, 0)
	percent := 0
	if window > 0 {
		percent = (used*100 + window/2) / window
	}

	prefix := ""
	if m.contextEstimate {
		prefix = "~"
	}

	threshold := coreagent.ResolveCompactionThreshold(m.opts.CompactionThreshold)
	compactAt := int(float64(window)*threshold + 0.999999)
	if compactAt <= 0 || compactAt > window {
		compactAt = window
	}

	if used >= compactAt {
		return fmt.Sprintf("ctx %s%s / %s (%d%% used)", prefix, formatContextTokenCount(used), formatContextTokenCount(window), percent)
	}

	noticeDistance := int(float64(window)*0.1 + 0.999999)
	if noticeDistance < 1 {
		noticeDistance = 1
	}
	if compactAt-used <= noticeDistance {
		return fmt.Sprintf("ctx %s%s / %s (%d%% used)", prefix, formatContextTokenCount(used), formatContextTokenCount(window), percent)
	}

	if percent > 60 {
		return fmt.Sprintf("ctx %s%s / %s (%d%% used)", prefix, formatContextTokenCount(used), formatContextTokenCount(window), percent)
	}

	return ""
}

func (m chatModel) displayContextWindowTokens() int {
	if n := chatIntOption(m.opts.Options, "num_ctx"); n > 0 {
		return n
	}
	return max(0, m.opts.ContextWindowTokens)
}

func chatIntOption(options map[string]any, key string) int {
	if options == nil {
		return 0
	}
	switch v := options[key].(type) {
	case int:
		return max(0, v)
	case int32:
		return max(0, int(v))
	case int64:
		return max(0, int(v))
	case uint:
		return int(v)
	case uint32:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		if v == float64(int(v)) {
			return max(0, int(v))
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return max(0, n)
		}
	}
	return 0
}

func formatContextTokenCount(value int) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	if value >= 950_000 {
		return fmt.Sprintf("%s%dM", sign, int(float64(value)/1_000_000+0.5))
	}
	if value >= 10_240 {
		return fmt.Sprintf("%s%dk", sign, int(float64(value)/1024+0.5))
	}
	return sign + formatInteger(value)
}

func formatInteger(value int) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	s := strconv.Itoa(value)
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	b.WriteString(sign)
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func (m chatModel) spinnerFrame() string {
	if len(chatSpinnerFrames) == 0 {
		return ""
	}
	return chatSpinnerFrames[m.spinner%len(chatSpinnerFrames)]
}

func statusWithSpinner(frame, label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return strings.TrimSpace(frame)
	}
	return label + frame
}

func renderFullFrame(content string, width, height int) string {
	width, height = defaultSize(width, height)
	rendered := lipgloss.NewStyle().MaxWidth(width).Render(content)
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func renderFrameLines(lines []string, width, height int) string {
	width, height = defaultSize(width, height)
	if len(lines) > height {
		lines = lines[:height]
	}
	out := make([]string, 0, height)
	for _, line := range lines {
		out = append(out, padRenderedLine(clipRenderedLine(line, width), width))
	}
	for len(out) < height {
		out = append(out, strings.Repeat(" ", width))
	}
	return strings.Join(out, "\n")
}

func truncateRenderedLine(line string, width int) string {
	if width <= 0 || lipgloss.Width(line) <= width {
		return line
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

func clipRenderedLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line, _, _ = strings.Cut(line, "\n")
	if lipgloss.Width(line) <= width {
		return line
	}
	clipped := lipgloss.NewStyle().MaxWidth(width).Render(line)
	clipped, _, _ = strings.Cut(clipped, "\n")
	return clipped
}

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func newChatEntry(entry chatEntry) chatEntry {
	if entry.version <= 0 {
		entry.version = 1
	}
	return entry
}

func newSlashEntry(content string) chatEntry {
	return newChatEntry(chatEntry{role: "slash", content: content})
}

func (m *chatModel) markEntryDirty(index int) {
	if index < 0 || index >= len(m.entries) {
		return
	}
	entry := &m.entries[index]
	entry.version++
	if entry.version <= 0 {
		entry.version = 1
	}
	entry.renderKey = chatEntryRenderKey{}
	entry.renderLines = nil
}

func entryRenderKey(entry chatEntry, width int) chatEntryRenderKey {
	return chatEntryRenderKey{width: width, version: entry.version}
}

func entriesFromMessages(messages []api.Message) []chatEntry {
	entries := make([]chatEntry, 0, len(messages))
	toolCalls := make(map[string]api.ToolCall)
	for _, msg := range messages {
		switch msg.Role {
		case "user", "system":
			if summary, ok := compactionSummaryContent(msg); ok {
				entries = append(entries, newChatEntry(chatEntry{
					role:    "compaction_summary",
					content: summary,
					status:  "done",
				}))
				continue
			}
			entries = append(entries, newChatEntry(chatEntry{role: msg.Role, content: msg.Content}))
		case "assistant":
			if strings.TrimSpace(msg.Thinking) != "" {
				entries = append(entries, newChatEntry(chatEntry{
					role:    "thinking",
					content: msg.Thinking,
					label:   "Thinking",
					status:  "done",
				}))
			}
			for _, call := range msg.ToolCalls {
				if call.ID != "" {
					toolCalls[call.ID] = call
				}
			}
			if strings.TrimSpace(msg.Content) != "" {
				entries = append(entries, newChatEntry(chatEntry{role: "assistant", content: msg.Content}))
			}
		case "tool":
			if summary, ok := compactionSummaryContent(msg); ok {
				entries = append(entries, newChatEntry(chatEntry{
					role:    "compaction_summary",
					content: summary,
					status:  "done",
				}))
				continue
			}
			toolName := msg.ToolName
			var args map[string]any
			if call, ok := toolCalls[msg.ToolCallID]; ok {
				if toolName == "" {
					toolName = call.Function.Name
				}
				args = call.Function.Arguments.ToMap()
			}
			status := "done"
			if isDisabledToolResult(msg.Content) {
				status = "disabled"
			} else if isDeniedToolResult(msg.Content) {
				status = "denied"
			}
			entries = append(entries, newChatEntry(chatEntry{
				role:    "tool",
				content: msg.Content,
				label:   toolInvocationLabel(toolName, args),
				detail:  toolName,
				status:  status,
				toolID:  msg.ToolCallID,
				args:    args,
			}))
		}
	}
	return groupCompletedToolEntries(entries)
}

func compactionSummaryContent(msg api.Message) (string, bool) {
	return coreagent.CompactionSummaryContent(msg)
}

func indentLines(lines []string, prefix string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		if line == "" {
			out[i] = prefix
		} else {
			out[i] = prefix + line
		}
	}
	return out
}
