package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// startPipeApprovalsWithStore runs a pipe session with frontend approval
// replies enabled (no blanket grant; no auto review unless asked).
func startPipeApprovalsWithStore(t *testing.T, fc *fakeClient, registry *coreagent.Registry, store *sessionstore.Store) *pipeHarness {
	t.Helper()
	inR, inW := io.Pipe()
	h := &pipeHarness{
		t:      t,
		stdin:  inW,
		outBuf: &lockedBuffer{},
		errBuf: &lockedBuffer{},
		codeCh: make(chan int, 1),
	}
	opts := &agentTUIOptions{Model: "test-model", PipeApprovals: true, Options: map[string]any{}}
	go func() {
		h.codeCh <- runPipeSession(context.Background(), fc, opts, store, nil, registry, "system prompt", nil, t.TempDir(), inR, h.outBuf, h.errBuf, "")
	}()
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionOpened })
	return h
}

// promptAndWait returns the events for one prompt turn.
func promptAndWait(t *testing.T, h *pipeHarness, line string) []coreagent.Event {
	t.Helper()
	before := len(h.outBuf.events(t))
	h.send(t, line)
	h.waitForAfter(before, func(ev coreagent.Event) bool { return ev.Type == coreagent.EventRunFinished })
	return h.outBuf.events(t)[before:]
}

func pipePromptLine(t *testing.T, text, requestID string) string {
	t.Helper()
	if requestID == "" {
		return `{"cmd":"prompt","text":` + jsonString(text) + `}`
	}
	return `{"cmd":"prompt","text":` + jsonString(text) + `,"requestId":` + jsonString(requestID) + `}`
}

// TestPipeRequestDedupeAndCommittedEvents pins the Stage 1 contract end to
// end: one logical run per request id, a committed terminal event after the
// durable commit, and replay events instead of duplicate work.
func TestPipeRequestDedupeAndCommittedEvents(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("hello")}}
	store := openTestStore(t)
	h := startPipeWithStore(t, fc, &coreagent.Registry{}, store)

	turn1 := promptAndWait(t, h, pipePromptLine(t, "say hi", "r1"))
	assigned := h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionAssigned })
	if assigned.ChatID == "" {
		t.Fatal("expected session_assigned with a chat id")
	}
	chatID := assigned.ChatID
	if replayed := eventIndex(turn1, coreagent.EventRunReplayed); replayed >= 0 {
		t.Fatalf("first turn must not be a replay: %v", turn1)
	}
	committed := eventIndex(turn1, coreagent.EventRunCommitted)
	finished := eventIndex(turn1, coreagent.EventRunFinished)
	if committed < 0 || committed < finished {
		t.Fatalf("run_committed must follow run_finished; events: %+v", turn1)
	}

	// Same request id + same input → replay, no second model call.
	turn2 := promptAndWait(t, h, pipePromptLine(t, "say hi", "r1"))
	if eventIndex(turn2, coreagent.EventRunReplayed) < 0 {
		t.Fatalf("deduplicated turn must emit run_replayed: %+v", turn2)
	}
	if fc.calls != 1 {
		t.Fatalf("model calls = %d, want 1", fc.calls)
	}

	// The stored projection matches what the run committed.
	sess, err := store.LoadSession(chatID)
	if err != nil {
		t.Fatal(err)
	}
	got := messageContents(sess.Messages)
	if len(got) != 2 || got[0] != "user:say hi" || got[1] != "assistant:hello" {
		t.Fatalf("stored messages = %v", got)
	}

	// The event ledger is ordered and replayed requests are auditable.
	runs, _ := store.ListRuns(chatID)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	events, _ := store.ListEvents(chatID, 0)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	// Ledger order: admitted, committed…, then the replay observation.
	if len(kinds) < 3 || kinds[0] != "run_admitted" ||
		!containsString(kinds, "run_committed") || !containsString(kinds, "run_replayed") {
		t.Fatalf("event ledger = %v", kinds)
	}
	if kinds[len(kinds)-1] != "run_replayed" {
		t.Fatalf("replay must be the last ledger event; got %v", kinds)
	}

	_ = h.close(t)
}

// TestPipeRequestConflict verifies a changed payload under the same request
// id is rejected without starting work.
func TestPipeRequestConflict(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("hello")}}
	store := openTestStore(t)
	h := startPipeWithStore(t, fc, &coreagent.Registry{}, store)

	promptAndWait(t, h, pipePromptLine(t, "say hi", "r1"))
	before := len(h.outBuf.events(t))
	h.send(t, pipePromptLine(t, "Something Else", "r1"))
	h.waitForAfter(before, func(ev coreagent.Event) bool { return ev.Type == coreagent.EventError })
	evs := h.outBuf.events(t)[before:]
	var conflictText string
	for _, ev := range evs {
		if ev.Type == coreagent.EventError && strings.Contains(ev.Error, "request conflict") {
			conflictText = ev.Error
		}
	}
	if conflictText == "" {
		t.Fatalf("expected a request-conflict error event; got %+v", evs)
	}
	if fc.calls != 1 {
		t.Fatalf("model calls = %d, want 1 (conflict must not run the model)", fc.calls)
	}
	// No duplicate run row.
	runs, _ := store.ListRuns(h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionAssigned }).ChatID)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	_ = h.close(t)
}

// TestPipeToolJournalOrdering verifies tool intent/outcome land as ordered
// ledger events between run_admitted and run_committed for a tool round.
func TestPipeToolJournalOrdering(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("upper", map[string]any{"text": "hi"}),
		textChunks("final"),
	}}
	tool := &upperTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)
	store := openTestStore(t)
	h := startPipeWithStore(t, fc, registry, store)

	promptAndWait(t, h, pipePromptLine(t, "upper it", "r-tools"))
	chatID := h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionAssigned }).ChatID

	events, _ := store.ListEvents(chatID, 0)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	wantOrder := []string{"run_admitted", "tool_intent", "tool_outcome", "run_committed"}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := -1
		for i, k := range kinds {
			if k == want && i > lastIdx {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("missing %s in ledger %v", want, kinds)
		}
		lastIdx = idx
	}
	calls, _ := store.ListToolCalls(chatID)
	if len(calls) != 1 || calls[0].Name != "upper" || calls[0].Status != "done" || calls[0].Result != "HI" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if tool.called != 1 {
		t.Fatalf("tool executed %d times", tool.called)
	}
	_ = h.close(t)
}

// TestPipeApprovalsRoundTrip verifies the opt-in approval flow: request over
// the wire, persisted decision before execution, stale replies rejected.
func TestPipeApprovalsRoundTrip(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("risky", map[string]any{}),
		textChunks("did the risky thing"),
	}}
	risky := &riskyTool{}
	registry := &coreagent.Registry{}
	registry.Register(risky)
	store := openTestStore(t)
	h := startPipeApprovalsWithStore(t, fc, registry, store)

	before := len(h.outBuf.events(t))
	h.send(t, pipePromptLine(t, "do the risky thing", "r-approvals"))
	chatID := h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionAssigned }).ChatID

	req := h.waitForAfter(before, func(ev coreagent.Event) bool {
		return ev.Type == coreagent.EventApprovalRequested
	})
	if req.ApprovalID == "" || len(req.ApprovalCalls) != 1 || req.ApprovalCalls[0].ToolName != "risky" {
		t.Fatalf("approval_requested event: %+v", req)
	}

	time.Sleep(50 * time.Millisecond)
	if risky.called != 0 {
		t.Fatal("no tool call may run before the approval is decided")
	}

	// A stale reply first: rejected, nothing authorized.
	h.send(t, `{"cmd":"approval","approvalId":"bogus-id","allow":true}`)
	h.waitForAfter(before, func(ev coreagent.Event) bool {
		return ev.Type == coreagent.EventError && strings.Contains(ev.Error, "stale approval reply")
	})
	if risky.called != 0 {
		t.Fatal("a stale reply must not authorize the tool")
	}

	h.send(t, `{"cmd":"approval","approvalId":`+jsonString(req.ApprovalID)+`,"allow":true}`)
	h.waitForAfter(before, func(ev coreagent.Event) bool {
		return ev.Type == coreagent.EventRunFinished
	})
	if risky.called != 1 {
		t.Fatalf("risky tool ran %d times, want 1", risky.called)
	}
	approvals, _ := store.ListApprovals(chatID)
	if len(approvals) != 1 || approvals[0].Status != "approved" {
		t.Fatalf("approvals = %+v", approvals)
	}
	_ = h.close(t)
}

// TestHeadlessRequestDedupe verifies a retried headless prompt with the same
// request id against the same session replays instead of re-running.
func TestHeadlessRequestDedupe(t *testing.T) {
	store := openTestStore(t)
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("the answer")}}
	var out, errb strings.Builder
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, RequestID: "hr-1", Options: map[string]any{}}
	code := runHeadlessSession(context.Background(), fc, opts, store, nil, &coreagent.Registry{}, "system prompt", "same prompt", t.TempDir(), &out, &errb)
	if code != 0 {
		t.Fatalf("first run code = %d (stderr: %s)", code, errb.String())
	}
	if fc.calls != 1 {
		t.Fatalf("model calls = %d", fc.calls)
	}
	sessionRef := ""
	for _, line := range strings.Split(errb.String(), "\n") {
		if strings.HasPrefix(line, "session: ") {
			sessionRef = strings.TrimSpace(strings.TrimPrefix(line, "session: "))
		}
	}
	if sessionRef == "" {
		t.Fatalf("no session id on stderr: %s", errb.String())
	}
	sess, err := store.LoadSession(sessionRef)
	if err != nil {
		t.Fatal(err)
	}

	// Retry through the resume path with the same request id: dedupe, no
	// fresh model call, exit 0.
	var out2, errb2 strings.Builder
	resumeOpts := &agentTUIOptions{Model: sess.Model, AllowAllTools: true, RequestID: "hr-1", Options: map[string]any{}}
	grantState := &coreagent.ApprovalState{}
	grantState.GrantAll()
	code = runHeadlessResumeSession(context.Background(), fc, resumeOpts, store, nil, &coreagent.Registry{}, "system prompt", sess, "same prompt", t.TempDir(), &out2, &errb2, grantState, headlessPrompter{allowAll: true})
	if code != 0 {
		t.Fatalf("replay code = %d (stderr: %s)", code, errb2.String())
	}
	if fc.calls != 1 {
		t.Fatalf("model calls after replay = %d, want 1", fc.calls)
	}
}

func eventIndex(events []coreagent.Event, kind coreagent.EventType) int {
	for i, ev := range events {
		if ev.Type == kind {
			return i
		}
	}
	return -1
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
