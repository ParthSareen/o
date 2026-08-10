package chat

import (
	"fmt"
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func newScrollableModel(entries int, width, height int) chatModel {
	m := chatModel{width: width, height: height, running: true}
	for i := range entries {
		m.entries = append(m.entries, chatEntry{role: "user", content: fmt.Sprintf("transcript-line-%02d", i)})
	}
	return m
}

func TestScrolledViewRendersWindowNotTail(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	if m.maxScroll() == 0 {
		t.Fatal("test needs a scrollable transcript")
	}
	m.scroll = m.maxScroll() // to oldest content
	view := stripANSI(m.View())
	if !strings.Contains(view, "transcript-line-00") {
		t.Fatalf("scrolled-to-top view should show oldest content:\n%s", view)
	}
	if strings.Contains(view, "transcript-line-29") {
		t.Fatalf("scrolled-to-top view should not show the newest content:\n%s", view)
	}
	// input/bottom area stays visible while scrolled
	if !strings.Contains(view, "█") {
		t.Fatalf("scrolled view should keep the input area visible:\n%s", view)
	}
}

func TestScrolledViewAnchorsWhileStreaming(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	// scroll up a bit: look at lines in the middle
	m.scrollBy(6)
	before := stripANSI(m.View())
	beforeStart := m.visibleTranscriptStartLine(m.viewWidth(), m.transcriptHeight())
	scrollBefore := m.scroll

	// stream several deltas (appends lines to the tail)
	for i := range 5 {
		updated, _ := m.Update(chatAgentMsg{event: coreagent.Event{Type: coreagent.EventMessageDelta, Content: fmt.Sprintf("streamed chunk %d ", i)}})
		m = updated.(chatModel)
	}

	after := stripANSI(m.View())
	if got := m.visibleTranscriptStartLine(m.viewWidth(), m.transcriptHeight()); got != beforeStart {
		t.Fatalf("visible start drifted while streaming: %d -> %d", beforeStart, got)
	}
	if m.scroll <= scrollBefore {
		t.Fatalf("scroll offset should grow to compensate for appended lines: %d -> %d", scrollBefore, m.scroll)
	}
	if before != after {
		t.Fatalf("scrolled window changed while streaming:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// and the streamed content must NOT be in the scrolled window
	if strings.Contains(after, "streamed chunk 4") {
		t.Fatalf("new streamed content must not appear in the scrolled window:\n%s", after)
	}
}

func TestFlowPrintingSuspendedWhileScrolled(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	m.scrollBy(6)
	printedBefore := m.flowPrintedLines
	next, _ := m.withFlowTranscriptFlush(nil)
	m = next.(chatModel)
	if m.flowPrintedLines != printedBefore {
		t.Fatalf("flow printing must suspend while scrolled: %d -> %d", printedBefore, m.flowPrintedLines)
	}
}

func TestFlowResumesOnScrollToBottom(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	m.scrollBy(3)
	// scroll back to the bottom via wheel
	for m.scroll > 0 {
		updated, _ := m.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
		m = updated.(chatModel)
	}
	if m.scroll != 0 {
		t.Fatal("should reach bottom")
	}
	// the pending tail re-enters the live view at the bottom (no re-flush:
	// printed-incrementally semantics resume with the next delta)
	if got := stripANSI(m.View()); !strings.Contains(got, "transcript-line-29") {
		t.Fatalf("bottom view should show the newest content:\n%s", got)
	}
	// and flow printing genuinely resumes: a streamed delta at the bottom
	// prints lines again
	updated, _ := m.Update(chatAgentMsg{event: coreagent.Event{Type: coreagent.EventMessageDelta, Content: "post-resume chunk"}})
	m = updated.(chatModel)
	if m.flowPrintedLines == 0 {
		t.Fatal("flow printing must resume streaming once the user is back at the bottom")
	}
	if m.scroll != 0 {
		t.Fatalf("bottom stays pinned after resume, scroll=%d", m.scroll)
	}
}

func TestScrolledViewRepaintGatedWhileScrolled(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	m.flowPrintedLines = 10
	m.scrollBy(6)
	next, _ := m.withFlowTranscriptRepaint(nil)
	m = next.(chatModel)
	if m.flowPrintedLines != 10 {
		t.Fatalf("repaint while scrolled must not reset printed lines, got %d", m.flowPrintedLines)
	}
}

func TestAnchorNoopWhilePinnedToBottom(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	if snap := m.scrollAnchorSnapshot(); snap.lineCount != 0 || snap.lines != nil {
		t.Fatalf("snapshot should be a no-op at the bottom, got %+v", snap)
	}
	m.anchorScrollToAppendedLines(scrollAnchor{lineCount: 10})
	if m.scroll != 0 {
		t.Fatalf("anchoring must not run at the bottom, scroll=%d", m.scroll)
	}
	// streaming at the bottom must keep the tail pinned
	updated, _ := m.Update(chatAgentMsg{event: coreagent.Event{Type: coreagent.EventMessageDelta, Content: "tail chunk"}})
	m = updated.(chatModel)
	if m.scroll != 0 {
		t.Fatalf("bottom should stay pinned while streaming, scroll=%d", m.scroll)
	}
	if got := stripANSI(m.View()); !strings.Contains(got, "tail chunk") {
		t.Fatalf("bottom view should show freshly streamed content:\n%s", got)
	}
}

func TestScrolledViewAnchorsWhenTranscriptShrinksAbove(t *testing.T) {
	// Regression: while scrolled up, if lines *above* the viewport disappear
	// (thinking collapse, compaction, fence reflow) or lines are inserted
	// there, the visible content must stay put. The old anchor only
	// compensated for appended lines, so the window drifted and text slid
	// under other content.
	m := newScrollableModel(6, 80, 12)
	// first entry is long; the rest are short
	m.entries[0] = chatEntry{role: "user", content: strings.Join([]string{
		"thinking-line-00", "thinking-line-01", "thinking-line-02", "thinking-line-03",
		"thinking-line-04", "thinking-line-05", "thinking-line-06", "thinking-line-07",
	}, "\n")}

	// scroll into the middle
	m.scrollBy(5)
	before := stripANSI(m.View())
	if !strings.Contains(before, "transcript-line-02") {
		t.Fatalf("setup: expected transcript-line-02 visible:\n%s", before)
	}

	snap := m.scrollAnchorSnapshot()

	// collapse the long first entry to one line (lines above the viewport vanish)
	m.entries[0] = chatEntry{role: "user", content: "thinking collapsed"}
	m.markEntryDirty(0)

	m.anchorScrollToAppendedLines(snap)

	after := stripANSI(m.View())
	if before != after {
		t.Fatalf("window drifted after lines vanished above:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if strings.Contains(after, "thinking-line-") {
		t.Fatalf("collapsed content should be gone:\n%s", after)
	}
}

func TestScrolledViewAnchorsWhenLinesInsertedAbove(t *testing.T) {
	// Insertion above the scrolled viewport (e.g. a table forming mid-history)
	// must also keep the visible window on the same content.
	m := newScrollableModel(6, 80, 12)
	m.scrollBy(5)
	before := stripANSI(m.View())
	snap := m.scrollAnchorSnapshot()

	// grow the first entry by three lines (insertion above the viewport)
	m.entries[0] = chatEntry{role: "user", content: "transcript-line-00\nadded-1\nadded-2\nadded-3"}
	m.markEntryDirty(0)

	m.anchorScrollToAppendedLines(snap)

	after := stripANSI(m.View())
	if before != after {
		t.Fatalf("window changed after insertion above:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Companion to TestFlowResumesOnScrollToBottom for the real-world path where
// the wheel event lands *over the transcript* (mouseInTranscript()==true),
// routing through the selection branch. The scroll offset must reach the
// bottom and the newest content must be visible there.
func TestScrollToBottomWithMouseOverTranscript(t *testing.T) {
	m := newScrollableModel(30, 80, 12)
	if !m.mouseInTranscript(tea.MouseMsg{Type: tea.MouseWheelDown}) {
		t.Skip("model without transcript hit-region can't reproduce")
	}
	m.scrollBy(3)
	for m.scroll > 0 {
		updated, _ := m.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
		m = updated.(chatModel)
	}
	if got := stripANSI(m.View()); !strings.Contains(got, "transcript-line-29") {
		t.Fatalf("bottom view should show the newest content:\n%s", got)
	}
}
