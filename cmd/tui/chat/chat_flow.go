package chat

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m chatModel) flowView(width int) string {
	allTranscriptLines := m.transcriptLines(width)
	bottomLines := m.bottomLines(width, 0)
	bottomGap := transcriptInputGap(0, len(bottomLines), len(allTranscriptLines))

	printed := clamp(m.flowPrintedLines, 0, len(allTranscriptLines))
	lines := slices.Clone(allTranscriptLines[printed:])
	for range bottomGap {
		lines = append(lines, "")
	}
	lines = append(lines, bottomLines...)
	return strings.Join(lines, "\n")
}

func (m chatModel) withFlowTranscriptFlush(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.pane || m.side != nil {
		return m, cmd
	}
	if m.scroll > 0 {
		// flow printing is suspended while the app-level scrollback viewport
		// is showing; pending lines print when the user returns to the bottom
		return m, cmd
	}
	next, printCmd := m.flowTranscriptFlushCmd()
	return next, tea.Batch(printCmd, cmd)
}

func (m chatModel) withFlowTranscriptRepaint(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.pane || m.side != nil {
		return m, cmd
	}
	if m.scroll > 0 {
		// the app-level scrollback viewport owns the screen; leave the printed
		// scrollback untouched and repaint on return to the bottom
		return m, cmd
	}
	if m.flowPrintedLines == 0 {
		return m.withFlowTranscriptFlush(cmd)
	}
	m.flowPrintedLines = 0
	next, printCmd := m.flowTranscriptFlushCmd()
	return next, tea.Sequence(tea.ClearScreen, printCmd, cmd)
}

func (m chatModel) withFlowTranscriptRefreshAfter(before []string, printed int, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.pane || m.side != nil {
		return m, cmd
	}
	next, printCmd := m.flowTranscriptRefreshAfterCmd(before, printed)
	return next, tea.Batch(printCmd, cmd)
}

func (m chatModel) flowTranscriptRefreshAfterCmd(before []string, printed int) (chatModel, tea.Cmd) {
	if m.scroll > 0 {
		return m, nil
	}
	width := m.viewWidth()
	after := m.transcriptLines(width)
	start := flowTranscriptChangedPrefixStart(before, after, printed)
	if start < 0 {
		return m.flowTranscriptFlushCmd()
	}

	printed = clamp(printed, 0, len(before))
	start = clamp(start, 0, len(after))
	rewind := printed - start
	if rewind <= 0 {
		return m.flowTranscriptFlushCmd()
	}

	flushCount := m.flowTranscriptFlushCount(after, width)
	flushCount = clamp(flushCount, start, len(after))
	rewriteLines := slices.Clone(after[start:flushCount])
	m.flowPrintedLines = flushCount
	return m, tea.Printf("%s", flowTranscriptRewriteSequence(rewind, rewriteLines))
}

func flowTranscriptChangedPrefixStart(before, after []string, printed int) int {
	printed = clamp(printed, 0, len(before))
	if printed == 0 {
		return -1
	}
	limit := min(printed, len(after))
	for i := range limit {
		if before[i] != after[i] {
			return i
		}
	}
	if len(after) < printed {
		return len(after)
	}
	return -1
}

func flowTranscriptRewriteSequence(rewind int, lines []string) string {
	var b strings.Builder
	if rewind > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", rewind)
	}
	b.WriteString("\r\x1b[J")
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

func (m chatModel) flowTranscriptFlushCmd() (chatModel, tea.Cmd) {
	if m.pane || m.side != nil {
		return m, nil
	}
	if m.promptDebug != nil || m.modelPicker != nil || m.sessionPicker != nil || m.thinkPicker != nil || m.cloudAuthPrompt != nil {
		return m, nil
	}
	width := m.viewWidth()
	lines := m.transcriptLines(width)
	if len(lines) == 0 {
		m.flowPrintedLines = 0
		return m, nil
	}
	m.flowPrintedLines = clamp(m.flowPrintedLines, 0, len(lines))
	flushCount := m.flowTranscriptFlushCount(lines, width)
	if flushCount <= m.flowPrintedLines {
		return m, nil
	}
	pending := strings.Join(lines[m.flowPrintedLines:flushCount], "\n")
	m.flowPrintedLines = flushCount
	return m, tea.Println(pending)
}

func (m chatModel) flowTranscriptFlushCount(lines []string, width int) int {
	holdFrom := m.flowTranscriptHoldEntryIndex()
	if holdFrom < 0 {
		return len(lines)
	}
	clone := m
	clone.entries = slices.Clone(m.entries[:holdFrom])
	clone.selection = chatSelection{}
	return len(clone.transcriptLines(width))
}

func (m chatModel) flowTranscriptHoldEntryIndex() int {
	if !m.running && !m.compacting {
		return -1
	}
	if len(m.entries) == 0 {
		return -1
	}
	index := len(m.entries) - 1
	entry := m.entries[index]
	switch entry.role {
	case "assistant":
		if entry.content != "" {
			return index
		}
	case "tool":
		if isToolActiveStatus(entry.status) {
			return index
		}
	}
	return -1
}
