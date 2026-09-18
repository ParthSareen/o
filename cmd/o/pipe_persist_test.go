package main

import (
	"context"
	"io"
	"slices"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// startPipeWithStore is startPipe with a session store wired in, for tests
// that assert persistence.
func startPipeWithStore(t *testing.T, fc *fakeClient, registry *coreagent.Registry, store *sessionstore.Store) *pipeHarness {
	t.Helper()
	inR, inW := io.Pipe()
	h := &pipeHarness{
		t:      t,
		stdin:  inW,
		outBuf: &lockedBuffer{},
		errBuf: &lockedBuffer{},
		codeCh: make(chan int, 1),
	}
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	go func() {
		h.codeCh <- runPipeSession(context.Background(), fc, opts, store, nil, registry, "system prompt", nil, t.TempDir(), inR, h.outBuf, h.errBuf, "")
	}()
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionOpened })
	return h
}

func messageContents(msgs []api.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role+":"+m.Content)
	}
	return out
}

func openTestStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := sessionstore.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func storedContents(t *testing.T, store *sessionstore.Store, id string) []string {
	t.Helper()
	sess, err := store.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return messageContents(sess.Messages)
}

// TestPipeHistoryExactAcrossTurns pins the request contents across three
// turns. Session.Run returns the full history, so the pre-fix code — which
// appended the result to r.history — duplicated prior turns from the third
// request on: [system, one, first, one, first, two, second, three].
func TestPipeHistoryExactAcrossTurns(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{
		textChunks("first answer"),
		textChunks("second answer"),
		textChunks("third answer"),
	}}
	h := startPipe(t, fc, &coreagent.Registry{})
	h.promptAndWait(t, "one")
	h.promptAndWait(t, "two")
	h.promptAndWait(t, "three")
	h.close(t)

	if len(fc.requests) != 3 {
		t.Fatalf("requests = %d", len(fc.requests))
	}
	want := []string{
		"system:system prompt",
		"user:one",
		"assistant:first answer",
		"user:two",
		"assistant:second answer",
		"user:three",
	}
	if got := messageContents(fc.requests[2].Messages); !slices.Equal(got, want) {
		t.Fatalf("third request = %v, want %v", got, want)
	}
}

// TestPipePersistsDeltaAcrossTurns pins that each message lands in the store
// exactly once. The pre-fix code appended the full run history, so the store
// held [one, first, one, first, two, second] after two turns.
func TestPipePersistsDeltaAcrossTurns(t *testing.T) {
	store := openTestStore(t)
	fc := &fakeClient{responses: [][]api.ChatResponse{
		textChunks("first answer"),
		textChunks("second answer"),
	}}
	h := startPipeWithStore(t, fc, &coreagent.Registry{}, store)
	h.promptAndWait(t, "one")
	h.promptAndWait(t, "two")
	h.close(t)

	sessions, err := store.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	want := []string{
		"user:one",
		"assistant:first answer",
		"user:two",
		"assistant:second answer",
	}
	if got := storedContents(t, store, sessions[0].ID); !slices.Equal(got, want) {
		t.Fatalf("stored = %v, want %v", got, want)
	}
}

// TestPipeResumePersistsOnlyDelta pins that a resumed pipe session appends
// only the new turn to the store and sends the loaded history to the model
// exactly once (the pre-fix code duplicated the loaded history from the
// first post-resume turn on).
func TestPipeResumePersistsOnlyDelta(t *testing.T) {
	store := openTestStore(t)
	sess, err := store.CreateSession("test-model", "", "", "resumed")
	if err != nil {
		t.Fatal(err)
	}
	prior := []api.Message{
		{Role: "user", Content: "before"},
		{Role: "assistant", Content: "earlier answer"},
	}
	if err := store.AppendMessages(sess.ID, prior); err != nil {
		t.Fatal(err)
	}
	sess.Messages = prior

	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("resumed answer")}}
	inR, inW := io.Pipe()
	out := &lockedBuffer{}
	errBuf := &lockedBuffer{}
	codeCh := make(chan int, 1)
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	go func() {
		codeCh <- runPipeSession(context.Background(), fc, opts, store, nil, &coreagent.Registry{}, "system prompt", sess, t.TempDir(), inR, out, errBuf, "")
	}()
	h := &pipeHarness{t: t, stdin: inW, outBuf: out, errBuf: errBuf, codeCh: codeCh}
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionOpened })
	h.promptAndWait(t, "after resume")
	h.close(t)

	if len(fc.requests) != 1 {
		t.Fatalf("requests = %d", len(fc.requests))
	}
	wantRequest := []string{
		"system:system prompt",
		"user:before",
		"assistant:earlier answer",
		"user:after resume",
	}
	if got := messageContents(fc.requests[0].Messages); !slices.Equal(got, wantRequest) {
		t.Fatalf("request = %v, want %v", got, wantRequest)
	}

	wantStore := []string{
		"user:before",
		"assistant:earlier answer",
		"user:after resume",
		"assistant:resumed answer",
	}
	if got := storedContents(t, store, sess.ID); !slices.Equal(got, wantStore) {
		t.Fatalf("stored = %v, want %v", got, wantStore)
	}
}

// TestPipeManualCompactionPersists pins that /compact replaces the stored
// history with the compacted form: the pre-compaction messages must be gone
// from the store, and the next turn must persist on top of the compacted
// history without reintroducing duplicates.
func TestPipeManualCompactionPersists(t *testing.T) {
	store := openTestStore(t)
	fc := &fakeClient{responses: [][]api.ChatResponse{
		textChunks("one"), textChunks("two"), textChunks("three"),
		textChunks("the summary"),
		textChunks("four"),
	}}
	h := startPipeWithStore(t, fc, &coreagent.Registry{}, store)
	h.promptAndWait(t, "turn 1")
	h.promptAndWait(t, "turn 2")
	h.promptAndWait(t, "turn 3")

	sessions, err := store.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	id := sessions[0].ID
	before, err := store.MessageCount(id)
	if err != nil {
		t.Fatal(err)
	}
	if before != 6 {
		t.Fatalf("messages before compaction = %d, want 6", before)
	}

	h.send(t, `{"cmd":"compact"}`)
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventCompacted })

	loaded, err := store.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	// The compacted form replaces the stored history: the archived turn is
	// gone, and the summary pair is present. (The compactor keeps recent
	// turns, so the length alone does not prove replacement.)
	var sawSummaryPair bool
	for _, m := range loaded.Messages {
		if m.ToolCallID == coreagent.CompactionToolCallID {
			sawSummaryPair = true
		}
		if m.Content == "one" || m.Content == "turn 1" {
			t.Fatalf("archived messages still in the store: %v", messageContents(loaded.Messages))
		}
	}
	if !sawSummaryPair {
		t.Fatalf("store lacks the compaction summary: %v", messageContents(loaded.Messages))
	}
	compactedCount := len(loaded.Messages)

	// the next turn appends exactly the new pair on top of the compacted form
	h.promptAndWait(t, "turn 4")
	loaded, err = store.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != compactedCount+2 {
		t.Fatalf("stored = %d messages, want %d (compacted %d + turn-4 pair)", len(loaded.Messages), compactedCount+2, compactedCount)
	}
	var sawFour bool
	for _, m := range loaded.Messages {
		if m.ToolCallID == coreagent.CompactionToolCallID {
			sawSummaryPair = true
		}
		if m.Content == "four" {
			if sawFour {
				t.Fatal("turn 4 persisted twice")
			}
			sawFour = true
		}
		if m.Content == "one" || m.Content == "turn 1" {
			t.Fatal("archived turn resurrected after the next persist")
		}
	}
	if !sawSummaryPair || !sawFour {
		t.Fatalf("store missing compacted summary or turn 4: %v", messageContents(loaded.Messages))
	}
}

// TestPipeSetThinkDuringRunIsDeferred pins that control commands arriving
// mid-run apply after it finishes: the in-flight run keeps its original
// settings, and the next turn picks up the deferred change.
func TestPipeSetThinkDuringRunIsDeferred(t *testing.T) {
	tool := &upperTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)
	// round 1: tool call; round 2: the answer; round 3: the next prompt
	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("upper", map[string]any{"text": "hi"}),
		textChunks("done"),
		textChunks("second answer"),
	}}
	h := startPipe(t, fc, registry)
	h.send(t, `{"cmd":"prompt","text":"run the tool"}`)

	// Wait for the tool call event: round 1's request is recorded before it
	// streams (polling fc.requests here would race with the turn goroutine).
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventToolCallDetected })
	h.send(t, `{"cmd":"set_think","value":"high"}`)
	h.waitForAfter(0, func(ev coreagent.Event) bool { return ev.Type == coreagent.EventRunFinished })

	if len(fc.requests) < 2 {
		t.Fatalf("requests = %d, want at least 2", len(fc.requests))
	}
	if fc.requests[1].Think != nil {
		t.Fatalf("in-flight run must keep its original think setting, got %+v", fc.requests[1].Think)
	}

	// the next turn picks up the deferred setting
	h.promptAndWait(t, "one more")
	if len(fc.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(fc.requests))
	}
	if fc.requests[2].Think == nil {
		t.Fatal("deferred set_think did not apply to the next turn")
	}
	h.close(t)
}
