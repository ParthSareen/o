package chat

import (
	"context"
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
)

// --- /copy ---

func TestCopyLastResponseCopiesLatestAssistant(t *testing.T) {
	old := writeClipboard
	defer func() { writeClipboard = old }()
	var got string
	writeClipboard = func(ctx context.Context, text string) error {
		got = text
		return nil
	}

	m := chatModel{
		ctx: context.Background(),
		entries: []chatEntry{
			{role: "assistant", content: "first answer"},
			{role: "user", content: "next"},
			{role: "assistant", content: "latest answer"},
		},
	}
	updated, cmd := m.copyLastResponse()
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("want a copy command")
	}
	msg := cmd()
	if got != "latest answer" {
		t.Fatalf("clipboard = %q", got)
	}
	copied, ok := msg.(chatClipboardCopiedMsg)
	if !ok {
		t.Fatalf("msg = %#v, want chatClipboardCopiedMsg", msg)
	}
	next, _ := m.Update(copied)
	m = next.(chatModel)
	if !strings.Contains(m.status, "copied last response") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestCopyLastResponseNothingToCopy(t *testing.T) {
	m := chatModel{ctx: context.Background()}
	updated, cmd := m.copyLastResponse()
	m = updated.(chatModel)
	if cmd != nil {
		t.Fatal("no copy cmd without assistant content")
	}
	if m.status != "nothing to copy" {
		t.Fatalf("status = %q", m.status)
	}
}

// --- working indicator during quiet thinking ---

func TestActivityLineThinkingThenQuietShowsWorking(t *testing.T) {
	m := chatModel{running: true, thinking: true, thinkingTokens: 42, spinner: 0}
	if line := stripANSI(m.activityLine()); !strings.Contains(line, "Thinking") {
		t.Fatalf("fresh thinking should show thinking label, got %q", line)
	}
	m.spinner = idleWorkingDelayTicks
	if line := stripANSI(m.activityLine()); !strings.Contains(line, "Working") {
		t.Fatalf("quiet thinking should show Working, got %q", line)
	}
}

func TestThinkingDeltaResetsQuietTimer(t *testing.T) {
	m := chatModel{running: true, spinner: idleWorkingDelayTicks + 10}
	m.applyAgentEvent(coreagent.Event{Type: coreagent.EventThinkingDelta, Thinking: "hmm"})
	if m.spinner != 0 {
		t.Fatalf("thinking delta must reset the quiet timer, spinner=%d", m.spinner)
	}
	if !m.thinking {
		t.Fatal("thinking should be set")
	}
}
