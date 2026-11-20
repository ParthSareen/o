package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m chatModel) findToolEntry(toolID string) int {
	if toolID == "" {
		return -1
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		if m.entries[i].role == "tool" && m.entries[i].toolID == toolID {
			return i
		}
	}
	return -1
}

func (m chatModel) findActiveToolEntry(toolID string) int {
	idx := m.findToolEntry(toolID)
	if idx < 0 || !isToolActiveStatus(m.entries[idx].status) {
		return -1
	}
	return idx
}

func (m chatModel) toolStartedAt(toolID string) time.Time {
	idx := m.findToolEntry(toolID)
	if idx < 0 {
		return time.Time{}
	}
	return m.entries[idx].startedAt
}

func entryHasExpandableOutput(entry chatEntry) bool {
	return (entry.role == "tool" && (len(entry.args) > 0 || strings.TrimSpace(entry.content) != "" || len(entry.tools) > 0)) ||
		(entry.role == "tool_group" && len(entry.tools) > 0) ||
		(entry.role == "compaction_summary" && strings.TrimSpace(entry.content) != "")
}

func entryHasToolOutputMode(entry chatEntry) bool {
	return (entry.role == "tool" && (isToolActiveStatus(entry.status) || isToolResultStatus(entry.status) || entry.content != "" || len(entry.tools) > 0)) ||
		(entry.role == "tool_group" && len(entry.tools) > 0) ||
		(entry.role == "compaction_summary" && strings.TrimSpace(entry.content) != "") ||
		(entry.role == "review" && strings.TrimSpace(entry.content) != "") ||
		(entry.role == "thinking" && strings.TrimSpace(entry.content) != "")
}

func (m *chatModel) applyToolOutputMode() {
	if !m.toolOutputMode {
		return
	}
	for i := range m.entries {
		m.applyToolOutputModeTo(i)
	}
}

func (m *chatModel) applyToolOutputModeTo(index int) {
	if !m.toolOutputMode || index < 0 || index >= len(m.entries) {
		return
	}
	if !entryHasToolOutputMode(m.entries[index]) {
		return
	}
	if m.entries[index].expanded == m.toolOutputOpen {
		return
	}
	m.entries[index].expanded = m.toolOutputOpen
	m.markEntryDirty(index)
}

func (m *chatModel) groupCompletedToolHistory() {
	if m.hasPendingDetectedToolCalls() {
		m.applyToolOutputMode()
		return
	}
	m.entries = groupCompletedToolEntries(m.entries, m.detectedToolCalls...)
	m.applyToolOutputMode()
}

func (m chatModel) hasPendingDetectedToolCalls() bool {
	if len(m.detectedToolCalls) == 0 {
		return false
	}
	results := map[string]struct{}{}
	for _, tool := range flattenToolHistory(m.entries) {
		if tool.toolID == "" || !isToolResultStatus(tool.status) {
			continue
		}
		results[tool.toolID] = struct{}{}
	}
	for _, tool := range m.detectedToolCalls {
		if tool.toolID == "" {
			continue
		}
		if _, ok := results[tool.toolID]; !ok {
			return true
		}
	}
	return false
}

func renderToolResultLines(entry chatEntry, width int) []string {
	lines := wrapChatText(toolStatusLine(entry), width)
	if !entry.expanded {
		return lines
	}

	detailLines := renderToolCallDetailLines(entry, width)
	if len(detailLines) > 0 {
		lines = append(lines, "")
		lines = append(lines, detailLines...)
	}
	if strings.TrimSpace(entry.content) != "" {
		lines = append(lines, "")
		lines = append(lines, renderToolOutputLines(entry, entry.content, width)...)
	}
	return lines
}

func renderToolGroupLines(entry chatEntry, width int) []string {
	lines := wrapChatText(toolGroupStatusLine(entry), width)
	if !entry.expanded {
		return lines
	}

	for i, tool := range entry.tools {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "  "+toolGroupChildStatusLine(tool))
		if detailLines := renderToolCallDetailLines(tool, width-4); len(detailLines) > 0 {
			lines = append(lines, indentLines(detailLines, "    ")...)
		}
		if strings.TrimSpace(tool.content) == "" {
			continue
		}
		lines = append(lines, indentLines(renderToolOutputLines(tool, tool.content, width-4), "    ")...)
	}
	return lines
}

func toolGroupChildStatusLine(entry chatEntry) string {
	label := toolGroupChildStatusLabel(entry)

	segment := renderToolStatusSegment(entry)
	if segment == "" {
		return boldToolInvocationName(label)
	}
	return fmt.Sprintf("%s %s", boldToolInvocationName(label), segment)
}

func toolGroupChildStatusLabel(entry chatEntry) string {
	if strings.TrimSpace(entry.label) != "" {
		return entry.label
	}
	if strings.TrimSpace(entry.detail) != "" {
		return toolInvocationLabel(entry.detail, entry.args)
	}
	return toolEntryStatusLabel(entry)
}

func boldToolInvocationName(label string) string {
	name, rest, ok := strings.Cut(label, "(")
	if !ok || name == "" {
		return chatHeaderStyle.Render(label)
	}
	return chatHeaderStyle.Render(name) + "(" + rest
}

func renderToolOutputLines(entry chatEntry, output string, width int) []string {
	output = stripInternalToolTruncationMarkers(output)
	output = truncateCtrlOToolOutput(output)
	if looksLikeUnifiedDiff(output) {
		return splitRenderedBody(renderDiffForView(output, width))
	}
	if toolOutputUsesMarkdown(entry.detail) {
		return styleLines(splitRenderedBody(renderMarkdownForView(output, width)), chatToolOutputStyle)
	}
	return styleLines(wrapChatText(output, width), chatToolOutputStyle)
}

func styleLines(lines []string, style lipgloss.Style) []string {
	for i := range lines {
		lines[i] = style.Render(lines[i])
	}
	return lines
}

func truncateCtrlOToolOutput(output string) string {
	runes := []rune(output)
	if len(runes) <= maxCtrlOToolOutputRunes {
		return output
	}
	return string(runes[:maxCtrlOToolOutputRunes-3]) + "..."
}

func stripInternalToolTruncationMarkers(output string) string {
	lines := strings.Split(output, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[tool output truncated: ") && strings.HasSuffix(trimmed, "]") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func looksLikeUnifiedDiff(output string) bool {
	lines := strings.Split(output, "\n")
	hasOldFile := false
	hasNewFile := false
	hasHunk := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			return true
		case strings.HasPrefix(line, "--- "):
			hasOldFile = true
		case strings.HasPrefix(line, "+++ "):
			hasNewFile = true
		case strings.HasPrefix(line, "@@ "):
			hasHunk = true
		}
	}
	return hasHunk || (hasOldFile && hasNewFile)
}

func renderDiffForView(diff string, width int) string {
	if width < 20 {
		width = 20
	}
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		style := diffLineStyle(line)
		for _, wrapped := range wrapChatText(line, width) {
			rendered = append(rendered, style.Render(wrapped))
		}
	}
	return strings.Join(rendered, "\n")
}

func diffLineStyle(line string) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "diff --git "),
		strings.HasPrefix(line, "--- "),
		strings.HasPrefix(line, "+++ "):
		return chatDiffFileStyle
	case strings.HasPrefix(line, "@@ "):
		return chatDiffHunkStyle
	case strings.HasPrefix(line, "+"):
		return chatDiffAddStyle
	case strings.HasPrefix(line, "-"):
		return chatDiffDeleteStyle
	case strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file "),
		strings.HasPrefix(line, "deleted file "),
		strings.HasPrefix(line, "similarity index "),
		strings.HasPrefix(line, "rename from "),
		strings.HasPrefix(line, "rename to "),
		strings.HasPrefix(line, "\\ "):
		return chatDiffMetaStyle
	default:
		return chatToolStyle
	}
}

func isToolActiveStatus(status string) bool {
	return status == "queued" || status == "running" || status == "approval"
}

func isToolResultStatus(status string) bool {
	return status == "done" || status == "error" || status == "denied" || status == "disabled"
}

// renderToolStatusSegment returns the styled status text for a tool entry,
// or "" when the entry has no status word to show (e.g. a completed or failed
// tool, where the colored dot alone conveys the result).
func renderToolStatusSegment(entry chatEntry) string {
	s := toolStatusLabel(entry)
	if s == "" {
		return ""
	}
	return chatMetaStyle.Render(s)
}

func toolStatusLine(entry chatEntry) string {
	label := toolEntryStatusLabel(entry)

	segment := renderToolStatusSegment(entry)
	if segment == "" {
		return label
	}
	return fmt.Sprintf("%s %s", label, segment)
}

func toolEntryStatusLabel(entry chatEntry) string {
	if isShellToolName(entry.detail) {
		switch entry.status {
		case "approval":
			if entry.label != "" {
				return entry.label
			}
			return toolInvocationLabel(entry.detail, entry.args)
		case "denied":
			if entry.label != "" {
				return entry.label + " denied"
			}
			return toolInvocationLabel(entry.detail, entry.args) + " denied"
		case "queued", "running", "done", "error":
			if entry.label != "" {
				return entry.label
			}
			return toolInvocationLabel(entry.detail, entry.args)
		}
	}
	if entry.status == "denied" {
		if entry.label != "" {
			return entry.label + " denied"
		}
		return toolDisplayName(entry.detail) + " denied"
	}
	if entry.label != "" {
		return entry.label
	}
	return toolDisplayName(entry.detail)
}

func toolGroupStatusLine(entry chatEntry) string {
	label := entry.label
	if label == "" || strings.HasPrefix(label, "Tool calls (") {
		label = toolGroupSummary(entry.tools)
	}

	segment := renderToolStatusSegment(entry)
	if segment == "" {
		return label
	}
	return fmt.Sprintf("%s %s", label, segment)
}

func toolGroupSummary(tools []chatEntry) string {
	if len(tools) == 0 {
		return "Used tools"
	}

	type actionCount struct {
		action string
		count  int
	}

	var counts []actionCount
	indexes := map[string]int{}
	for _, tool := range tools {
		action := toolActionForEntry(tool)
		if index, ok := indexes[action]; ok {
			counts[index].count++
			continue
		}
		indexes[action] = len(counts)
		counts = append(counts, actionCount{action: action, count: 1})
	}

	phrases := make([]string, 0, len(counts))
	for _, count := range counts {
		phrases = append(phrases, toolActionPhrase(count.action, count.count))
	}
	return joinToolActionPhrases(phrases)
}

func toolActionForEntry(tool chatEntry) string {
	if tool.status == "denied" {
		if isShellToolName(tool.detail) || strings.Contains(strings.ToLower(tool.label), "bash(") || strings.Contains(strings.ToLower(tool.label), "powershell(") {
			return "denied_command"
		}
		return "denied_tool"
	}
	if tool.status == "disabled" {
		if isShellToolName(tool.detail) || strings.Contains(strings.ToLower(tool.label), "bash(") || strings.Contains(strings.ToLower(tool.label), "powershell(") {
			return "disabled_command"
		}
		return "disabled_tool"
	}
	action := toolAction(tool.detail)
	if action == "" {
		action = toolAction(tool.label)
	}
	if action == "" {
		action = "tool"
	}
	return action
}

func toolAction(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	switch {
	case isShellToolName(name):
		return "command"
	case strings.Contains(name, "bash") || strings.Contains(name, "powershell"):
		return "command"
	case strings.HasPrefix(name, "edit("):
		return "edit"
	case strings.HasPrefix(name, "read("):
		return "read"
	case strings.HasPrefix(name, "list("):
		return "list"
	case strings.HasPrefix(name, "web search("):
		return "search"
	case strings.HasPrefix(name, "web fetch("):
		return "fetch"
	case strings.HasPrefix(name, "skill("):
		return "skill"
	}
	switch name {
	case "edit":
		return "edit"
	case "read":
		return "read"
	case "list":
		return "list"
	case "web_search":
		return "search"
	case "web_fetch":
		return "fetch"
	case "skill":
		return "skill"
	default:
		return "tool"
	}
}

func toolActionPhrase(action string, count int) string {
	plural := count != 1
	switch action {
	case "denied_command":
		if plural {
			return fmt.Sprintf("Denied %d commands", count)
		}
		return "Denied a command"
	case "denied_tool":
		if plural {
			return fmt.Sprintf("Denied %d tools", count)
		}
		return "Denied a tool"
	case "disabled_command":
		if plural {
			return fmt.Sprintf("Skipped %d commands", count)
		}
		return "Skipped a command"
	case "disabled_tool":
		if plural {
			return fmt.Sprintf("Skipped %d tools", count)
		}
		return "Skipped a tool"
	case "command":
		if plural {
			return fmt.Sprintf("Ran %d commands", count)
		}
		return "Ran 1 command"
	case "edit":
		if plural {
			return fmt.Sprintf("Edited %d files", count)
		}
		return "Edited a file"
	case "read":
		if plural {
			return fmt.Sprintf("Read %d files", count)
		}
		return "Read a file"
	case "list":
		if plural {
			return fmt.Sprintf("Listed files %d times", count)
		}
		return "Listed files"
	case "search":
		if plural {
			return fmt.Sprintf("Searched the web %d times", count)
		}
		return "Searched the web"
	case "fetch":
		if plural {
			return fmt.Sprintf("Fetched %d URLs", count)
		}
		return "Fetched a URL"
	case "skill":
		if plural {
			return fmt.Sprintf("Loaded %d skills", count)
		}
		return "Loaded a skill"
	default:
		if plural {
			return fmt.Sprintf("Used %d tools", count)
		}
		return "Used a tool"
	}
}

func joinToolActionPhrases(phrases []string) string {
	switch len(phrases) {
	case 0:
		return "Used tools"
	case 1:
		return phrases[0]
	case 2:
		return phrases[0] + " and " + lowerInitial(phrases[1])
	default:
		for i := 1; i < len(phrases); i++ {
			phrases[i] = lowerInitial(phrases[i])
		}
		return strings.Join(phrases[:len(phrases)-1], ", ") + ", and " + phrases[len(phrases)-1]
	}
}

func lowerInitial(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = []rune(strings.ToLower(string(runes[0])))[0]
	return string(runes)
}

func toolGroupPrefixStyle(entry chatEntry) lipgloss.Style {
	succeeded, failed, denied := toolGroupResultCounts(entry.tools)
	switch {
	case succeeded > 0 && (failed > 0 || denied > 0):
		return chatToolMixedStyle
	case succeeded > 0:
		return chatToolDoneStyle
	case denied > 0 && failed == 0:
		return toolStatusStyle("denied")
	default:
		return toolStatusStyle(entry.status)
	}
}

func toolGroupResultCounts(tools []chatEntry) (succeeded int, failed int, denied int) {
	for _, tool := range tools {
		if tool.status == "denied" || tool.status == "disabled" {
			denied++
			continue
		}
		if tool.err != "" || tool.status == "error" {
			failed++
			continue
		}
		if tool.status == "done" {
			succeeded++
		}
	}
	return succeeded, failed, denied
}

func toolStatusLabel(entry chatEntry) string {
	if entry.status == "approval" {
		return "needs approval"
	}
	if entry.status == "running" || entry.status == "queued" {
		return ""
	}
	return ""
}

func toolStatusStyle(status string) lipgloss.Style {
	switch status {
	case "queued", "running", "approval":
		return chatToolRunningStyle
	case "done":
		return chatToolDoneStyle
	case "error":
		return chatErrorStyle
	case "denied", "disabled":
		return chatToolMixedStyle
	default:
		return chatMetaStyle
	}
}

func toolInvocationLabel(name string, args map[string]any) string {
	displayName := toolDisplayName(name)
	for _, key := range []string{"query", "url", "command", "path", "name"} {
		if value, ok := rawStringArg(args, key); ok {
			if isShellToolName(name) && key == "command" {
				value = truncateRunes(value, 100)
			}
			return fmt.Sprintf("%s(%s)", displayName, strconv.Quote(value))
		}
	}
	if len(args) == 0 {
		return displayName
	}
	return fmt.Sprintf("%s(%s)", displayName, formatDisplayArgs(args))
}

func toolDisplayName(name string) string {
	switch name {
	case "web_search":
		return "Web Search"
	case "web_fetch":
		return "Web Fetch"
	case "bash":
		return "Bash"
	case "powershell":
		return "PowerShell"
	case "read":
		return "Read"
	case "list":
		return "List"
	case "edit":
		return "Edit"
	default:
		if name == "" {
			return "Tool"
		}
		return name
	}
}

func toolOutputUsesMarkdown(name string) bool {
	switch name {
	case "read", "skill", "web_search", "web_fetch":
		return true
	default:
		return false
	}
}

func renderToolCallDetailLines(entry chatEntry, width int) []string {
	if len(entry.args) == 0 {
		return nil
	}
	if isShellToolName(entry.detail) {
		if command, ok := rawStringArg(entry.args, "command"); ok {
			return renderToolCallArgLine(shellPromptPrefix(entry.detail)+command, width)
		}
	}
	switch entry.detail {
	case "web_search":
		if query, ok := rawStringArg(entry.args, "query"); ok {
			return renderToolCallArgLine("query: "+query, width)
		}
	case "web_fetch":
		if targetURL, ok := rawStringArg(entry.args, "url"); ok {
			return renderToolCallArgLine("url: "+targetURL, width)
		}
	}
	return renderToolCallArgs(entry.args, width)
}

func isShellToolName(name string) bool {
	return name == "bash" || name == "powershell"
}

func formatDisplayArgs(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := truncateRunes(fmt.Sprintf("%v", args[key]), 100)
		parts = append(parts, fmt.Sprintf("%s=%s", key, strconv.Quote(value)))
	}
	return strings.Join(parts, ", ")
}

func shellPromptPrefix(toolName string) string {
	if toolName == "powershell" {
		return "PS> "
	}
	return "$ "
}

func renderToolCallArgs(args map[string]any, width int) []string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var lines []string
	for _, key := range keys {
		value := toolArgDisplayValue(args[key])
		if strings.Contains(value, "\n") {
			lines = append(lines, chatMetaStyle.Render(key+":"))
			lines = append(lines, indentLines(renderToolCallArgLine(value, max(20, width-2)), "  ")...)
			continue
		}
		lines = append(lines, renderToolCallArgLine(key+": "+value, width)...)
	}
	return lines
}

func renderToolCallArgLine(line string, width int) []string {
	wrapped := wrapChatText(line, width)
	for i := range wrapped {
		wrapped[i] = chatMetaStyle.Render(wrapped[i])
	}
	return wrapped
}

func toolArgDisplayValue(value any) string {
	if value == nil {
		return "null"
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		return string(data)
	}
	return fmt.Sprint(value)
}

func rawStringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func isDeniedToolResult(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "tool execution denied") ||
		strings.Contains(value, "tool approval canceled")
}

func isDisabledToolResult(value string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(value)), "tool execution disabled")
}

func groupCompletedToolEntries(entries []chatEntry, detected ...chatEntry) []chatEntry {
	grouped := make([]chatEntry, 0, len(entries))
	visibleToolIDs := visibleToolIDs(entries)
	for i := 0; i < len(entries); {
		if !isGroupableToolHistoryEntry(entries[i]) {
			grouped = append(grouped, entries[i])
			i++
			continue
		}

		start := i
		var tools []chatEntry
		for i < len(entries) {
			if isGroupableToolHistoryEntry(entries[i]) {
				tools = append(tools, flattenToolHistory([]chatEntry{entries[i]})...)
				i++
				continue
			}
			if isInvisibleToolGroupingBoundary(entries[i]) && nextGroupableToolHistoryIndex(entries, i+1) >= 0 {
				i++
				continue
			}
			break
		}

		summaryTools := toolSummaryEntries(tools, detected, visibleToolIDs)
		if len(summaryTools) <= 1 {
			grouped = append(grouped, flattenToolHistory(entries[start:i])...)
			continue
		}

		group := chatEntry{
			role:       "tool_group",
			label:      toolGroupSummary(summaryTools),
			status:     aggregateToolStatus(tools),
			expanded:   anyToolExpanded(tools),
			startedAt:  firstToolStartedAt(tools),
			finishedAt: lastToolFinishedAt(tools),
			tools:      tools,
		}
		if group.status == "error" {
			group.err = "one or more tool calls failed"
		}
		grouped = append(grouped, newChatEntry(group))
	}
	return grouped
}

func isInvisibleToolGroupingBoundary(entry chatEntry) bool {
	switch entry.role {
	case "assistant":
		return strings.TrimSpace(entry.content) == "" &&
			strings.TrimSpace(entry.label) == "" &&
			strings.TrimSpace(entry.detail) == "" &&
			entry.metrics == nil
	case "thinking":
		return !entry.expanded
	default:
		return false
	}
}

func nextGroupableToolHistoryIndex(entries []chatEntry, index int) int {
	for index < len(entries) && isInvisibleToolGroupingBoundary(entries[index]) {
		index++
	}
	if index < len(entries) && isGroupableToolHistoryEntry(entries[index]) {
		return index
	}
	return -1
}

func anyToolExpanded(tools []chatEntry) bool {
	for _, tool := range tools {
		if tool.expanded {
			return true
		}
	}
	return false
}

func isGroupableToolHistoryEntry(entry chatEntry) bool {
	return (entry.role == "tool" && isToolResultStatus(entry.status)) ||
		(entry.role == "tool_group" && len(entry.tools) > 0)
}

func visibleToolIDs(entries []chatEntry) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, tool := range flattenToolHistory(entries) {
		if tool.toolID != "" {
			ids[tool.toolID] = struct{}{}
		}
	}
	return ids
}

func toolSummaryEntries(tools []chatEntry, detected []chatEntry, visible map[string]struct{}) []chatEntry {
	if len(detected) == 0 {
		return tools
	}
	seen := make(map[string]struct{}, len(visible)+len(tools))
	for id := range visible {
		seen[id] = struct{}{}
	}
	summary := slices.Clone(tools)
	for _, tool := range detected {
		if tool.toolID != "" {
			if _, ok := seen[tool.toolID]; ok {
				continue
			}
			seen[tool.toolID] = struct{}{}
		}
		summary = append(summary, tool)
	}
	return summary
}

func entryHasActiveTool(entry chatEntry) bool {
	switch entry.role {
	case "tool":
		return isToolActiveStatus(entry.status)
	case "tool_group":
		for _, tool := range entry.tools {
			if isToolActiveStatus(tool.status) {
				return true
			}
		}
	}
	return false
}

func flattenToolHistory(entries []chatEntry) []chatEntry {
	var tools []chatEntry
	for _, entry := range entries {
		switch entry.role {
		case "tool":
			tools = append(tools, entry)
		case "tool_group":
			tools = append(tools, entry.tools...)
		}
	}
	return tools
}

func aggregateToolStatus(tools []chatEntry) string {
	denied := false
	active := false
	for _, tool := range tools {
		if tool.err != "" || tool.status == "error" {
			return "error"
		}
		if tool.status == "denied" {
			denied = true
		}
		if isToolActiveStatus(tool.status) {
			active = true
		}
	}
	if denied {
		return "denied"
	}
	if active {
		return "running"
	}
	return "done"
}

func firstToolStartedAt(tools []chatEntry) time.Time {
	for _, tool := range tools {
		if !tool.startedAt.IsZero() {
			return tool.startedAt
		}
	}
	return time.Time{}
}

func lastToolFinishedAt(tools []chatEntry) time.Time {
	for i := len(tools) - 1; i >= 0; i-- {
		if !tools[i].finishedAt.IsZero() {
			return tools[i].finishedAt
		}
	}
	return time.Time{}
}
