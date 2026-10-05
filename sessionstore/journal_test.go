package sessionstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// openJournalStore opens an isolated store in HOME-overridden temp dir.
func openJournalStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// admission builds a journal admission for tests.
func admission(chatID, requestID, prompt string) agent.RunAdmission {
	return agent.RunAdmission{
		ChatID:       chatID,
		RequestID:    requestID,
		Model:        "test-model",
		SystemPrompt: "system",
		Prompt:       prompt,
	}
}

func TestJournalAdmissionReplayAndConflict(t *testing.T) {
	store := openJournalStore(t)
	sess, err := store.CreateSession("test-model", "", "system", "")
	if err != nil {
		t.Fatal(err)
	}

	res1, err := store.AdmitRun(admission(sess.ID, "r1", "hello"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res1.Replayed || res1.Attempt != 1 {
		t.Fatalf("first admission: %+v", res1)
	}

	commit, err := store.CommitRun(agent.RunTerminal{
		RunID: res1.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted,
		History: []api.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if commit.Seq <= 0 {
		t.Fatalf("commit seq = %d", commit.Seq)
	}

	// Same request id + same input → replay, no new run.
	res2, err := store.AdmitRun(admission(sess.ID, "r1", "hello"))
	if err != nil {
		t.Fatalf("replay admit: %v", err)
	}
	if !res2.Replayed || res2.RunID != res1.RunID || res2.Status != agent.RunStatusCompleted {
		t.Fatalf("replay: %+v", res2)
	}
	runs, _ := store.ListRuns(sess.ID)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (replay must not insert)", len(runs))
	}

	// Same request id + different input → conflict.
	_, err = store.AdmitRun(admission(sess.ID, "r1", "Something Else"))
	var conflict *agent.RunConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want RunConflictError, got %v", err)
	}
	if conflict.ExistingRunID != res1.RunID {
		t.Fatalf("conflict existing run = %s", conflict.ExistingRunID)
	}
}

// TestJournalCrashRecoveryThenRetryBlocksUnsafeTools simulates a process
// crash (abandoned store handle, forced recovery on a fresh handle) and
// verifies the retry: interrupted classification, unknown-outcome intents,
// and refusal blocks for unsafe tools only.
func TestJournalCrashRecoveryThenRetryBlocksUnsafeTools(t *testing.T) {
	homedir := t.TempDir()
	t.Setenv("HOME", homedir)
	store1, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store1.CreateSession("test-model", "", "system", "")
	if err != nil {
		t.Fatal(err)
	}

	res1, err := store1.AdmitRun(admission(sess.ID, "r2", "do work"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := store1.RecordToolIntent(agent.ToolIntent{
		RunID: res1.RunID, ToolCallID: "c1", Name: "bash",
		Args: map[string]any{"command": "rm -rf build"},
	}); err != nil {
		t.Fatalf("intent: %v", err)
	}
	if err := store1.RecordToolIntent(agent.ToolIntent{
		RunID: res1.RunID, ToolCallID: "c2", Name: "read", ReplaySafe: true,
		Args: map[string]any{"path": "a.txt"},
	}); err != nil {
		t.Fatalf("intent: %v", err)
	}
	// Crash: handle1 abandoned without Close; state stays non-terminal.

	// "Restart": fresh handle on the same database.
	store2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	t.Setenv("HOME", homedir) // pin second store to the same file

	lines := store2.RecoverSession(sess.ID)
	if len(lines) == 0 {
		t.Fatal("recovery should report what it classified")
	}

	runs, _ := store2.ListRuns(sess.ID)
	if len(runs) != 1 || runs[0].Status != agent.RunStatusInterrupted {
		t.Fatalf("runs after recovery: %+v", runs)
	}
	calls, _ := store2.ListToolCalls(sess.ID)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d", len(calls))
	}
	for _, c := range calls {
		if c.Status != agent.ToolJournalUnknown {
			t.Fatalf("call %s status = %s, want unknown", c.ToolCallID, c.Status)
		}
	}

	// Retry the same request: a new attempt is admitted, and the unsafe
	// call is blocked while the replay-safe one is not.
	res2, err := store2.AdmitRun(admission(sess.ID, "r2", "do work"))
	if err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if res2.Replayed || res2.Attempt != 2 {
		t.Fatalf("readmit: %+v", res2)
	}
	if len(res2.BlockedTools) != 1 || res2.BlockedTools[0].Name != "bash" {
		t.Fatalf("blocked tools = %+v", res2.BlockedTools)
	}
	if res2.BlockedTools[0].ArgsKey != agent.ToolArgsKey("bash", map[string]any{"command": "rm -rf build"}) {
		t.Fatalf("blocked key mismatch: %s", res2.BlockedTools[0].ArgsKey)
	}
	// The recovered run keeps its identity; the new attempt is a new run.
	if res2.RunID == res1.RunID {
		t.Fatal("retry must be a new run, not the interrupted one")
	}
}

// TestJournalRunLockRejectsSecondWriter verifies the single-writer contract:
// a second process-like handle cannot admit a run while one is in flight.
func TestJournalRunLockRejectsSecondWriter(t *testing.T) {
	homedir := t.TempDir()
	t.Setenv("HOME", homedir)
	store1, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store1.Close()
	sess, err := store1.CreateSession("test-model", "", "system", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store1.AdmitRun(admission(sess.ID, "r3", "busy work")); err != nil {
		t.Fatalf("admit: %v", err)
	}

	store2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	_, err = store2.AdmitRun(admission(sess.ID, "r4", "second writer"))
	var busy *agent.RunBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("want RunBusyError, got %v", err)
	}
	if busy.SessionID != sess.ID || busy.Owner == "" {
		t.Fatalf("busy error: %+v", busy)
	}
}

// TestJournalDoubleCommitFails verifies terminal state is committed once;
// a second CommitRun errors instead of re-publishing.
func TestJournalDoubleCommitFails(t *testing.T) {
	store := openJournalStore(t)
	sess, _ := store.CreateSession("test-model", "", "system", "")
	res, err := store.AdmitRun(admission(sess.ID, "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRun(agent.RunTerminal{RunID: res.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRun(agent.RunTerminal{RunID: res.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err == nil {
		t.Fatal("second commit must fail")
	}
}

// TestJournalRawHistoryAndCompactionWatermark verifies the append-only raw
// record ordering and that a compaction's watermark covers exactly the rows
// written before it.
func TestJournalRawHistoryAndCompactionWatermark(t *testing.T) {
	store := openJournalStore(t)
	sess, _ := store.CreateSession("test-model", "", "system", "")
	res, err := store.AdmitRun(admission(sess.ID, "", "compact me"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRawSegment(res.RunID, []api.Message{{Role: "user", Content: "compact me"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRawSegment(res.RunID, []api.Message{{Role: "assistant", Content: "working"}}); err != nil {
		t.Fatal(err)
	}
	count, err := store.RawHistoryCount(sess.ID)
	if err != nil || count != 2 {
		t.Fatalf("raw count = %d (%v)", count, err)
	}
	if err := store.RecordCompaction(agent.CompactionRecord{RunID: res.RunID, Trigger: "estimate", Summary: "summarized"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListEvents(sess.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	// Raw segments land as raw_history rows (not events); the compaction
	// records its own event.
	if len(kinds) < 2 || kinds[0] != "run_admitted" {
		t.Fatalf("events = %v", kinds)
	}
	var sawCompaction bool
	for _, k := range kinds {
		if k == "compaction" {
			sawCompaction = true
		}
	}
	if !sawCompaction {
		t.Fatalf("expected a compaction event: %v", kinds)
	}
	// Raw rows stay append-only around the compaction.
	if err := store.RecordRawSegment(res.RunID, []api.Message{{Role: "assistant", Content: "after compaction"}}); err != nil {
		t.Fatal(err)
	}
	count, _ = store.RawHistoryCount(sess.ID)
	if count != 3 {
		t.Fatalf("raw count after compaction = %d, want 3", count)
	}

	// Terminal commit syncs the projection (replace on drift) but does not
	// touch raw history.
	terminal := agent.RunTerminal{
		RunID: res.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted,
		History: []api.Message{{Role: "system", Content: "summarized"}, {Role: "user", Content: "compact me"}, {Role: "assistant", Content: "after compaction"}},
	}
	if _, err := store.CommitRun(terminal); err != nil {
		t.Fatalf("commit: %v", err)
	}
	count, _ = store.RawHistoryCount(sess.ID)
	if count != 3 {
		t.Fatalf("raw count must survive the terminal commit; got %d", count)
	}
	loaded, err := store.LoadSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 3 || loaded.Messages[0].Role != "system" {
		t.Fatalf("projection load: %v", loaded.Messages)
	}
}

// TestJournalApprovalLifecycle verifies pending→decided with settlement
// protections: a duplicate decision is rejected, expiry never grants, and
// recovering an interrupted run expires its pending approvals.
func TestJournalApprovalLifecycle(t *testing.T) {
	store := openJournalStore(t)
	sess, _ := store.CreateSession("test-model", "", "system", "")

	// A live run: pending approval can be approved once.
	res, err := store.AdmitRun(admission(sess.ID, "", "needs approval"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordApprovalRequested(agent.ApprovalRecord{
		ApprovalID: "app-1", RunID: res.RunID, Calls: `[{"tool":"bash"}]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordApprovalDecision(agent.ApprovalDecision{ApprovalID: "app-1", Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordApprovalDecision(agent.ApprovalDecision{ApprovalID: "app-1", Approved: false}); !errors.Is(err, agent.ErrApprovalSettled) {
		t.Fatalf("want ErrApprovalSettled, got %v", err)
	}
	approvals, _ := store.ListApprovals(sess.ID)
	if len(approvals) != 1 || approvals[0].Status != "approved" {
		t.Fatalf("approvals = %+v", approvals)
	}
	if _, err := store.CommitRun(agent.RunTerminal{RunID: res.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err != nil {
		t.Fatal(err)
	}

	// An interrupted run's pending approval expires at recovery; it can
	// never silently become a grant.
	res2, err := store.AdmitRun(admission(sess.ID, "", "second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordApprovalRequested(agent.ApprovalRecord{ApprovalID: "app-2", RunID: res2.RunID, Calls: "[]"}); err != nil {
		t.Fatal(err)
	}
	store.RecoverSession(sess.ID)
	approvals, _ = store.ListApprovals(sess.ID)
	for _, a := range approvals {
		if a.ID == "app-2" && a.Status != "expired" {
			t.Fatalf("recovered pending approval should expire; got %+v", a)
		}
		if a.ID == "app-1" && a.Status != "approved" {
			t.Fatalf("settled approval must stay approved; got %+v", a)
		}
	}
}

// TestJournalToolIntentOutcomeSequence verifies intents and outcomes land as
// ordered session events with the tool rows linking both.
func TestJournalToolIntentOutcomeSequence(t *testing.T) {
	store := openJournalStore(t)
	sess, _ := store.CreateSession("test-model", "", "system", "")
	res, err := store.AdmitRun(admission(sess.ID, "", "use tools"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolIntent(agent.ToolIntent{RunID: res.RunID, ToolCallID: "c1", Name: "webfetch", Args: map[string]any{"url": "https://example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolOutcome(agent.ToolOutcome{RunID: res.RunID, ToolCallID: "c1", Status: agent.ToolJournalDone, Result: "page"}); err != nil {
		t.Fatal(err)
	}
	// A duplicate outcome is rejected: settled outcomes are append-once.
	if err := store.RecordToolOutcome(agent.ToolOutcome{RunID: res.RunID, ToolCallID: "c1", Status: agent.ToolJournalFailed, Err: "nope"}); err == nil {
		t.Fatal("duplicate outcome must fail")
	}
	if _, err := store.CommitRun(agent.RunTerminal{RunID: res.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err != nil {
		t.Fatal(err)
	}

	events, _ := store.ListEvents(sess.ID, 0)
	var kinds []string
	var lastSeq int64 = -1
	for _, e := range events {
		if e.Seq <= lastSeq {
			t.Fatalf("event sequences not increasing: %+v", events)
		}
		lastSeq = e.Seq
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 4 {
		t.Fatalf("events = %v, want run_admitted, tool_intent, tool_outcome, run_committed", kinds)
	}
	calls, _ := store.ListToolCalls(sess.ID)
	if len(calls) != 1 || calls[0].Status != agent.ToolJournalDone || calls[0].Result != "page" {
		t.Fatalf("tool calls: %+v", calls)
	}
	if calls[0].ArgsHash != agent.ToolArgsKey("webfetch", map[string]any{"url": "https://example.com"}) {
		t.Fatalf("args hash mismatch: %s", calls[0].ArgsHash)
	}
}

// TestJournalLegacyNoRequestID verifies request-id-less admissions stay
// per-call (legacy behavior): no dedupe, fresh run each time.
func TestJournalLegacyNoRequestID(t *testing.T) {
	store := openJournalStore(t)
	sess, _ := store.CreateSession("test-model", "", "system", "")
	res1, err := store.AdmitRun(admission(sess.ID, "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	// The first run must reach a terminal commit (releasing its lock)
	// before the next admission is possible; that is the single-writer rule.
	if _, err := store.CommitRun(agent.RunTerminal{RunID: res1.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	// While an admitted run is in flight, a concurrent admission is rejected.
	res2, err := store.AdmitRun(admission(sess.ID, "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitRun(admission(sess.ID, "r-busy", "hello")); err == nil {
		t.Fatal("concurrent admission while the lock is held must be rejected")
	}
	if res1.Replayed || res2.Replayed || res1.RunID == res2.RunID {
		t.Fatalf("legacy admissions must not dedupe: %+v / %+v", res1, res2)
	}
	runs, _ := store.ListRuns(sess.ID)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
}

// TestMigrationRepeatableUpgradesLegacyDB verifies the journal migration is
// re-runnable: a legacy-shaped store upgrades (twice), prompt history and
// sessions survive, and the journal tables work right after the upgrade.
func TestMigrationRepeatableUpgradesLegacyDB(t *testing.T) {
	homedir := t.TempDir()
	t.Setenv("HOME", homedir)

	store1, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store1.CreateSession("m", "", "system", "legacy-name")
	if err != nil {
		t.Fatal(err)
	}
	if err := store1.AddPrompt(sess.ID, "keep me"); err != nil {
		t.Fatal(err)
	}

	// "Upgrade" twice: reopen the same database and run Open's migration
	// again; legacy rows must survive and journal writes must work.
	for i := 0; i < 2; i++ {
		store2, err := Open()
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		loaded, err := store2.LoadSession(sess.ID)
		if err != nil || loaded.Name != "legacy-name" {
			t.Fatalf("reopen %d: session did not survive migration: %v", i, err)
		}
		prompts, err := store2.RecentPrompts(sess.ID, 10)
		if err != nil || len(prompts) != 1 {
			t.Fatalf("reopen %d: prompts = %v (%v)", i, prompts, err)
		}
		admitted, err := store2.AdmitRun(admission(sess.ID, fmt.Sprintf("r-%d", i), "post-upgrade"))
		if err != nil {
			t.Fatalf("reopen %d: admit after upgrade: %v", i, err)
		}
		if _, err := store2.CommitRun(agent.RunTerminal{RunID: admitted.RunID, ChatID: sess.ID, Status: agent.RunStatusCompleted}); err != nil {
			t.Fatalf("reopen %d: commit after upgrade: %v", i, err)
		}
		if err := store2.Close(); err != nil {
			t.Fatalf("reopen %d: close: %v", i, err)
		}
	}
	_ = store1.Close()
}
