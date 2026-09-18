package chat

import (
	"context"
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

func TestPromptDebugRendersCompactionSummary(t *testing.T) {
	m := chatModel{
		opts: Options{Model: "test", Client: chatTestClient{}},
		messages: append([]api.Message{
			{Role: "user", Content: "recent question"},
			{Role: "assistant", Content: "recent answer"},
		}, coreagent.CompactionSummaryMessages("Earlier we discussed X and decided Y.", true)...),
	}
	m.handlePromptCommand("")
	plain := stripANSI(strings.Join(m.promptDebugLines(100), "\n"))
	for _, want := range []string{
		"tool call 1: summary",
		"tool_call_id: " + coreagent.CompactionToolCallID,
		coreagent.CompactionSummaryMessagePrefix,
		"Earlier we discussed X and decided Y.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("prompt debug missing %q", want)
		}
	}
	// recent turns survive compaction alongside the summary pair
	if !strings.Contains(plain, "recent question") {
		t.Error("prompt debug missing recent turns")
	}
}

// TestPersistRunResultPersistsCompactedHistory pins that a compacted history
// replaces the stored messages instead of being dropped: previously compaction
// was never persisted, so a resume resurrected the full history. The compacted
// form here has the same length as the store (the compactor archives an old
// turn and keeps recent ones), which a length-only check would miss.
func TestPersistRunResultPersistsCompactedHistory(t *testing.T) {
	store := openChatTestStore(t)
	sess := newTestSession(t, store, "test-model", "", "old question")
	if err := store.AppendMessages(sess.ID, []api.Message{
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "another question"},
		{Role: "assistant", Content: "another answer"},
	}); err != nil {
		t.Fatal(err)
	}

	m := &chatModel{ctx: context.Background(), store: store, chatID: sess.ID}
	compacted := append(
		coreagent.CompactionSummaryMessages("the summary", true),
		api.Message{Role: "user", Content: "recent question"},
		api.Message{Role: "assistant", Content: "recent answer"},
	)
	m.persistRunResult(compacted)

	loaded, err := store.LoadSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != len(compacted) {
		t.Fatalf("stored = %d messages, want %d", len(loaded.Messages), len(compacted))
	}
	for _, msg := range loaded.Messages {
		if msg.Content == "old question" {
			t.Fatalf("pre-compaction messages still in the store: %+v", loaded.Messages)
		}
	}
}

// TestPersistRunResultAppendsDelta pins the ordinary-turn path: the delta is
// appended, nothing duplicated.
func TestPersistRunResultAppendsDelta(t *testing.T) {
	store := openChatTestStore(t)
	sess := newTestSession(t, store, "test-model", "", "u1")
	m := &chatModel{ctx: context.Background(), store: store, chatID: sess.ID}
	// The in-memory history mirrors the store: the user message was persisted
	// at session creation.
	m.messages = []api.Message{{Role: "user", Content: "u1"}}

	result := append(append([]api.Message{}, m.messages...),
		api.Message{Role: "assistant", Content: "a1"},
		api.Message{Role: "user", Content: "u2"},
		api.Message{Role: "assistant", Content: "a2"},
	)
	m.persistRunResult(result)

	loaded, err := store.LoadSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 4 {
		t.Fatalf("stored = %d messages, want 4: %+v", len(loaded.Messages), loaded.Messages)
	}
	if loaded.Messages[1].Content != "a1" || loaded.Messages[3].Content != "a2" {
		t.Fatalf("stored = %+v", loaded.Messages)
	}
}
