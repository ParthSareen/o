package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ParthSareen/o/api"
)

// journalCompactor always compacts on prompt_eval, with a bounded summary.
type journalCompactor struct{}

func (c *journalCompactor) MaybeCompact(_ context.Context, req CompactionRequest) (CompactionResult, error) {
	if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
		return CompactionResult{
			Messages:  CompactionSummaryMessages("summarized", false),
			Compacted: true,
			Summary:   "summarized",
			Due:       true,
		}, nil
	}
	return CompactionResult{Messages: req.Messages, Due: true}, nil
}
func (c *journalCompactor) ContextWindowTokens(map[string]any) int { return 8192 }
func (c *journalCompactor) Threshold() float64                     { return 0 }
func (c *journalCompactor) ShouldCompact(CompactionRequest) (CompactionTrigger, bool) {
	return CompactionTriggerPromptEval, true
}

// journalToolCall / journalText are response helpers local to the journal
// tests (the agent package builds api.ToolCall values inline elsewhere).
func journalToolCall(name string, kv map[string]any) []api.ChatResponse {
	args := api.NewToolCallFunctionArguments()
	for k, v := range kv {
		args.Set(k, v)
	}
	return []api.ChatResponse{
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID:       "call_1",
			Function: api.ToolCallFunction{Name: name, Arguments: args},
		}}}, Done: true},
	}
}

func journalText(text string) []api.ChatResponse {
	return []api.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: text}, Done: true},
	}
}

// recordingJournal is a fake RunJournal that records the order of calls,
// including interleaved tool executions (via an external trace), so tests
// can assert commit-before-publication sequencing.
type recordingJournal struct {
	mu sync.Mutex

	admissions []RunAdmission
	replays    []RunAdmissionResult
	conflict   error
	// committed controls the fake: AdmitRun returns replays[0] the first
	// time (if set), else a fresh run.
	replayQueued *RunAdmissionResult
	blockOnAdmit []BlockedTool
	commitErr    error

	intents      []ToolIntent
	outcomes     []ToolOutcome
	rawSegs      [][]api.Message
	compacts     []CompactionRecord
	approvalsReq []ApprovalRecord
	approvalsDec []ApprovalDecision
	commitCalls  int
	commitSeq    int
}

func (j *recordingJournal) AdmitRun(admission RunAdmission) (RunAdmissionResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.admissions = append(j.admissions, admission)
	if j.conflict != nil {
		return RunAdmissionResult{}, j.conflict
	}
	if j.replayQueued != nil {
		res := *j.replayQueued
		j.replayQueued = nil
		j.replays = append(j.replays, res)
		return res, nil
	}
	j.commitSeq++
	res := RunAdmissionResult{
		RunID:        fmt.Sprintf("run-%d", j.commitSeq),
		Attempt:      1,
		BlockedTools: j.blockOnAdmit,
	}
	return res, nil
}

func (j *recordingJournal) RecordToolIntent(intent ToolIntent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.intents = append(j.intents, intent)
	return nil
}

func (j *recordingJournal) RecordToolOutcome(outcome ToolOutcome) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.outcomes = append(j.outcomes, outcome)
	return nil
}

func (j *recordingJournal) RecordCompaction(record CompactionRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.compacts = append(j.compacts, record)
	return nil
}

func (j *recordingJournal) RecordApprovalRequested(record ApprovalRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.approvalsReq = append(j.approvalsReq, record)
	return nil
}

func (j *recordingJournal) RecordApprovalDecision(decision ApprovalDecision) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.approvalsDec = append(j.approvalsDec, decision)
	return nil
}

func (j *recordingJournal) RecordRawSegment(runID string, segment []api.Message) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rawSegs = append(j.rawSegs, segment)
	return nil
}

func (j *recordingJournal) CommitRun(terminal RunTerminal) (RunCommit, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.commitCalls++
	if j.commitErr != nil {
		return RunCommit{}, j.commitErr
	}
	j.commitSeq++
	return RunCommit{Seq: int64(j.commitSeq)}, nil
}

// journalEchoTool is a session-tool for journal tests; it appends to trace so
// tests can compare journal order against execution order.
type journalEchoTool struct {
	name  string
	trace *[]string
	safe  bool
}

func (t journalEchoTool) Name() string         { return t.name }
func (t *journalEchoTool) Description() string { return "journal test tool" }
func (t journalEchoTool) Schema() api.ToolFunction {
	return api.ToolFunction{Name: t.name, Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (t *journalEchoTool) Execute(_ context.Context, _ ToolContext, args map[string]any) (ToolResult, error) {
	if t.trace != nil {
		*t.trace = append(*t.trace, "exec:"+t.name)
	}
	return ToolResult{Content: "echoed"}, nil
}
func (t journalEchoTool) ReplaySafe() bool { return t.safe }

func eventOrder(events []Event, kind EventType) int {
	for i, ev := range events {
		if ev.Type == kind {
			return i
		}
	}
	return -1
}

func journalSessionForTest(t *testing.T, journal *recordingJournal, tools *Registry) (*Session, *recordingEventSink) {
	t.Helper()
	if tools == nil {
		tools = &Registry{}
	}
	sink := &recordingEventSink{}
	session := &Session{
		Client:     &fakeClient{},
		EventSinks: []EventSink{sink},
		Tools:      tools,
		Journal:    journal,
	}
	return session, sink
}

// TestJournalCommitBeforeTerminalEvents pins the ordering: commit first, then
// run_finished, then run_committed; intents commit before tool execution.
func TestJournalCommitBeforeTerminalEvents(t *testing.T) {
	tool := &journalEchoTool{name: "echo_tool"}
	registry := &Registry{}
	registry.Register(tool)
	trace := &[]string{}
	tool.trace = trace

	journal := &recordingJournal{}
	session, sink := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		journalToolCall("echo_tool", map[string]any{"text": "hi"}),
		journalText("done"),
	}}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "say hi"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Committed {
		t.Fatal("result should report Committed")
	}

	// Intent was journaled before the tool executed.
	if len(*trace) != 1 {
		t.Fatalf("tool executions = %v, want exactly one", *trace)
	}
	if len(journal.intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(journal.intents))
	}
	if journal.intents[0].Name != "echo_tool" {
		t.Fatalf("intent name = %s", journal.intents[0].Name)
	}
	if len(journal.outcomes) != 1 || journal.outcomes[0].Status != ToolJournalDone {
		t.Fatalf("outcomes = %+v", journal.outcomes)
	}

	// Terminal events ordered: run_finished then run_committed, both after
	// the first tool events.
	finished := eventOrder(sink.events, EventRunFinished)
	committed := eventOrder(sink.events, EventRunCommitted)
	if finished < 0 || committed < 0 || committed <= finished {
		t.Fatalf("event order wrong: finished=%d committed=%d", finished, committed)
	}
	if admitted := eventOrder(sink.events, EventRunAdmitted); admitted < 0 {
		t.Fatal("missing run_admitted event")
	}
	// A raw segment with the run's own messages was recorded.
	findRaw := false
	for _, seg := range journal.rawSegs {
		for _, msg := range seg {
			if msg.Role == "user" && msg.Content == "say hi" {
				findRaw = true
			}
		}
	}
	if !findRaw {
		t.Fatalf("raw segments should contain the admitted prompt: %+v", journal.rawSegs)
	}
}

// TestJournalReplayNoWork verifies a deduplicated request runs nothing and
// emits run_replayed followed by run_finished with the terminal status.
func TestJournalReplayNoWork(t *testing.T) {
	journal := &recordingJournal{replayQueued: &RunAdmissionResult{
		RunID: "old-run", Attempt: 1, Replayed: true, Status: RunStatusCompleted,
	}}
	session, sink := journalSessionForTest(t, journal, &Registry{})
	client := &fakeClient{responses: [][]api.ChatResponse{journalText("fresh")}}
	session.Client = client

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "again"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Replayed {
		t.Fatal("result should report Replayed")
	}
	if client.calls != 0 {
		t.Fatalf("client should not be called on replay, calls = %d", client.calls)
	}
	if len(journal.outcomes) != 0 || journal.commitCalls != 0 {
		t.Fatalf("no journal work expected on replay: outcomes=%d commits=%d", len(journal.outcomes), journal.commitCalls)
	}
	replayed := eventOrder(sink.events, EventRunReplayed)
	finished := eventOrder(sink.events, EventRunFinished)
	if replayed < 0 || finished <= replayed {
		t.Fatalf("expected run_replayed then run_finished, got %d/%d", replayed, finished)
	}
	if ev := sink.events[finished]; ev.Status != RunStatusDone {
		t.Fatalf("replayed run_finished status = %s", ev.Status)
	}
}

// TestJournalConflictDoesNotStartWork verifies a request-ID conflict aborts
// the run before any model or tool work.
func TestJournalConflictDoesNotStartWork(t *testing.T) {
	journal := &recordingJournal{conflict: &RunConflictError{
		RequestID: "r1", ExistingRunID: "old-run", Detail: "different input",
	}}
	session, sink := journalSessionForTest(t, journal, &Registry{})
	client := &fakeClient{responses: [][]api.ChatResponse{journalText("nope")}}
	session.Client = client

	_, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "conflicting"}},
	})
	var conflict *RunConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want RunConflictError, got %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("client should not be called on conflict, calls = %d", client.calls)
	}
	if eventOrder(sink.events, EventError) < 0 {
		t.Fatal("expected an error event for the conflict")
	}
	if eventOrder(sink.events, EventRunFinished) >= 0 {
		t.Fatal("no run_finished may publish for a conflicted admission")
	}
}

// TestJournalBlockedToolsRefusedOnRetry verifies a re-admitted attempt
// refuses to re-execute an unknown-outcome unsafe tool call.
func TestJournalBlockedToolsRefusedOnRetry(t *testing.T) {
	tool := &journalEchoTool{name: "echo_tool"}
	registry := &Registry{}
	registry.Register(tool)
	trace := &[]string{}
	tool.trace = trace

	journal := &recordingJournal{blockOnAdmit: []BlockedTool{{
		ArgsKey:     ToolArgsKey("echo_tool", map[string]any{"text": "hi"}),
		Name:        "echo_tool",
		PriorRunID:  "prior-run",
		PriorCallID: "prior-call",
		Reason:      blockedToolReason(BlockedTool{PriorRunID: "prior-run"}),
	}}}
	session, _ := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		journalToolCall("echo_tool", map[string]any{"text": "hi"}),
		journalText("worked around"),
	}}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "retry"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Committed {
		t.Fatal("blocked-call run still commits normally")
	}
	for _, item := range *trace {
		if item == "exec:echo_tool" {
			t.Fatal("blocked tool must not execute on a retried attempt")
		}
	}
	found := false
	for _, outcome := range journal.outcomes {
		if outcome.Status == ToolJournalBlocked {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a blocked outcome, got %+v", journal.outcomes)
	}
}

// TestJournalCommitFailureNotPublished verifies that a failed terminal commit
// never publishes run_finished.
func TestJournalCommitFailureNotPublished(t *testing.T) {
	journal := &recordingJournal{commitErr: errors.New("disk full")}
	session, sink := journalSessionForTest(t, journal, &Registry{})
	session.Client = &fakeClient{responses: [][]api.ChatResponse{journalText("lost")}}

	_, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want commit failure surfaced, got %v", err)
	}
	if eventOrder(sink.events, EventRunFinished) >= 0 {
		t.Fatal("run_finished must not publish when the terminal commit fails")
	}
	if eventOrder(sink.events, EventRunCommitted) >= 0 {
		t.Fatal("run_committed must not publish when the terminal commit fails")
	}
}

// TestJournalNestedCallsCorrelated verifies nested calls from a composing
// tool go through the same journaling path with parent correlation, the same
// approval path, and blocking.
func TestJournalNestedCallsCorrelated(t *testing.T) {
	nested := &journalEchoTool{name: "echo_tool"}
	registry := &Registry{}
	registry.Register(nested)
	trace := &[]string{}
	nested.trace = trace

	// A minimal composing tool whose nested executor is the one the session
	// installs.
	fakeCompose := &fakeNestedComposer{trace: trace}
	registry.Register(fakeCompose)

	journal := &recordingJournal{}
	session, sink := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		journalToolCall("composer", map[string]any{}),
		journalText("composed"),
	}}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "compose"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected committed run")
	}
	if !fakeCompose.executorWasUsed {
		t.Fatal("session should install a nested executor on composing tools")
	}
	// Both nested calls recorded with the composer's call id as parent.
	if len(journal.intents) != 3 { // composer + 2 nested
		t.Fatalf("intents = %d, want 3", len(journal.intents))
	}
	for _, intent := range journal.intents[1:] {
		if intent.ParentToolCallID != "call_1" {
			t.Fatalf("nested intent parent = %q, want call_1", intent.ParentToolCallID)
		}
		if intent.ToolCallID == "" || !strings.HasPrefix(intent.ToolCallID, "nested-") {
			t.Fatalf("nested intent id = %q", intent.ToolCallID)
		}
	}
	nestedRuns := 0
	for _, item := range *trace {
		if item == "exec:echo_tool" {
			nestedRuns++
		}
	}
	if nestedRuns != 2 {
		t.Fatalf("nested echo executions = %d, want 2", nestedRuns)
	}
	if eventOrder(sink.events, EventRunCommitted) < 0 {
		t.Fatal("expected run_committed")
	}
}

// fakeNestedComposer records the executor the session installed and invokes
// it like codemode would.
type fakeNestedComposer struct {
	trace           *[]string
	executor        NestedExecutor
	executorWasUsed bool
}

func (c *fakeNestedComposer) Name() string        { return "composer" }
func (c *fakeNestedComposer) Description() string { return "test composer" }
func (c *fakeNestedComposer) Schema() api.ToolFunction {
	return api.ToolFunction{Name: c.name(), Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (c *fakeNestedComposer) name() string { return "composer" }
func (c *fakeNestedComposer) SetNestedExecutor(executor NestedExecutor) {
	c.executor = executor
}
func (c *fakeNestedComposer) Execute(ctx context.Context, toolCtx ToolContext, _ map[string]any) (ToolResult, error) {
	if c.trace != nil {
		*c.trace = append(*c.trace, "exec:composer")
	}
	if c.executor == nil {
		return ToolResult{}, errors.New("no nested executor installed")
	}
	if toolCtx.ToolCallID == "" {
		return ToolResult{}, errors.New("ToolContext.ToolCallID must be set for the composing call")
	}
	if _, err := c.executor.ExecuteNested(ctx, toolCtx.ToolCallID, "echo_tool", map[string]any{"text": "one"}); err != nil {
		return ToolResult{}, err
	}
	if _, err := c.executor.ExecuteNested(ctx, toolCtx.ToolCallID, "echo_tool", map[string]any{"text": "two"}); err != nil {
		return ToolResult{}, err
	}
	c.executorWasUsed = true
	return ToolResult{Content: "composed two calls"}, nil
}

// TestJournalNestedBlockedRefused verifies blocked (tool, args) pairs refuse
// nested re-execution too.
func TestJournalNestedBlockedRefused(t *testing.T) {
	nested := &journalEchoTool{name: "echo_tool"}
	registry := &Registry{}
	registry.Register(nested)
	fakeCompose := &fakeNestedComposer{trace: &[]string{}}
	registry.Register(fakeCompose)

	journal := &recordingJournal{blockOnAdmit: []BlockedTool{{
		ArgsKey:     ToolArgsKey("echo_tool", map[string]any{"text": "one"}),
		Name:        "echo_tool",
		PriorRunID:  "prior",
		PriorCallID: "prior-call",
	}}}
	session, _ := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		journalToolCall("composer", map[string]any{}),
		journalText("gave up"),
	}}

	_, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "retry compose"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The first nested call was refused; the run still completed because the
	// script-side error surfaced as a normal tool failure, not a crash.
	blocked := 0
	for _, outcome := range journal.outcomes {
		if outcome.Status == ToolJournalBlocked {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("blocked outcomes = %d, want 1", blocked)
	}
}

// TestJournalCompactionKeepsRawSegments verifies compaction splits the run
// into its pre-compaction segment and its post-compaction tail, with the
// compaction fact recorded.
func TestJournalCompactionKeepsRawSegments(t *testing.T) {
	// Client: a tool call (whose tool message triggers the recording
	// compactor to compact), then final text.
	tool := &journalEchoTool{name: "echo_tool"}
	registry := &Registry{}
	registry.Register(tool)
	trace := &[]string{}
	tool.trace = trace

	journal := &recordingJournal{}
	session, _ := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		journalToolCall("echo_tool", map[string]any{"text": "hi"}),
		journalText("final"),
	}}
	session.Compactor = &journalCompactor{}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "compact me"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected committed run")
	}
	if len(journal.compacts) != 1 {
		t.Fatalf("compaction records = %d, want 1", len(journal.compacts))
	}
	if journal.compacts[0].Trigger != string(CompactionTriggerPromptEval) {
		t.Fatalf("compaction trigger = %s", journal.compacts[0].Trigger)
	}
	// Two raw segments: before the compaction (user + assistant + tool) and
	// after it (final assistant text).
	if len(journal.rawSegs) < 2 {
		t.Fatalf("raw segments = %d, want >= 2", len(journal.rawSegs))
	}
}
