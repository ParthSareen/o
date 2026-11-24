package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/api"
)

func toolCallResponse(calls ...api.ToolCall) []api.ChatResponse {
	return []api.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: calls}, Done: true}}
}

func namedCall(id, name string, kv map[string]any) api.ToolCall {
	args := api.NewToolCallFunctionArguments()
	for k, v := range kv {
		args.Set(k, v)
	}
	return api.ToolCall{ID: id, Function: api.ToolCallFunction{Name: name, Arguments: args}}
}

func finishedEvents(events []Event) map[string]Event {
	out := map[string]Event{}
	for _, ev := range events {
		if ev.Type == EventToolFinished {
			out[ev.ToolCallID] = ev
		}
	}
	return out
}

func outcomesByCall(outcomes []ToolOutcome) map[string]ToolOutcome {
	out := map[string]ToolOutcome{}
	for _, o := range outcomes {
		out[o.ToolCallID] = o
	}
	return out
}

func toolMessagesByCall(messages []api.Message) map[string]api.Message {
	out := map[string]api.Message{}
	for _, msg := range messages {
		if msg.Role == "tool" {
			out[msg.ToolCallID] = msg
		}
	}
	return out
}

// TestDeniedBatchSkipsUnpromptedCalls pins that a denial refuses only the
// calls the human saw; the rest of the batch is skipped, not run and not
// reported as denied, and only the prompted calls are refused for the rest
// of the run.
func TestDeniedBatchSkipsUnpromptedCalls(t *testing.T) {
	trace := &[]string{}
	registry := &Registry{}
	registry.Register(&journalEchoTool{name: "echo_tool", trace: trace})
	registry.Register(approvalTestTool{})
	prompter := &recordingApprovalPrompter{results: []Approval{{Reason: "no thanks", Continue: true}}}
	journal := &recordingJournal{}
	session, sink := journalSessionForTest(t, journal, registry)
	session.ApprovalPrompter = prompter
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		toolCallResponse(namedCall("read_1", "echo_tool", nil), namedCall("write_1", "approval_tool", nil)),
		journalText("ok, stopping"),
	}}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "do it"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*trace) != 0 {
		t.Fatalf("no call in a denied batch may run, ran %v", *trace)
	}
	if len(prompter.requests) != 1 || len(prompter.requests[0].Calls) != 1 || prompter.requests[0].Calls[0].ToolCallID != "write_1" {
		t.Fatalf("approval should cover only the approval-requiring call, got %+v", prompter.requests)
	}

	events := finishedEvents(sink.events)
	outcomes := outcomesByCall(journal.outcomes)
	messages := toolMessagesByCall(result.Messages)
	for id, want := range map[string]ToolStatus{"read_1": ToolStatusSkipped, "write_1": ToolStatusDenied} {
		if events[id].ToolStatus != want {
			t.Errorf("%s event status = %q, want %q", id, events[id].ToolStatus, want)
		}
		if outcomes[id].Status != string(want) {
			t.Errorf("%s journal status = %q, want %q", id, outcomes[id].Status, want)
		}
		if messages[id].Content == "" || messages[id].Content != events[id].Content {
			t.Errorf("%s message %q should match the event content %q", id, messages[id].Content, events[id].Content)
		}
	}
}

// TestToolOutcomesAgree pins that every settled call records the same
// outcome in the journal, the tool_finished event, and the model-facing
// message, across each way a call can end.
func TestToolOutcomesAgree(t *testing.T) {
	registry := &Registry{}
	registry.Register(staticTool{})
	registry.Register(&failingTool{})
	registry.Register(&journalEchoTool{name: "blocked_tool"})
	journal := &recordingJournal{blockOnAdmit: []BlockedTool{{
		ArgsKey: ToolArgsKey("blocked_tool", map[string]any{}), Name: "blocked_tool", PriorRunID: "prior", PriorCallID: "prior-call",
	}}}
	session, sink := journalSessionForTest(t, journal, registry)
	session.Client = &fakeClient{responses: [][]api.ChatResponse{
		toolCallResponse(
			namedCall("ok", "echo_tool", nil),
			namedCall("fail", "failing_tool", nil),
			namedCall("unknown", "no_such_tool", nil),
			namedCall("blocked", "blocked_tool", nil),
		),
		journalText("done"),
	}}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "go"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := map[string]struct {
		journal string
		status  ToolStatus
	}{
		"ok":      {ToolJournalDone, ToolStatusDone},
		"fail":    {ToolJournalFailed, ToolStatusFailed},
		"unknown": {ToolJournalFailed, ToolStatusFailed},
		"blocked": {ToolJournalBlocked, ToolStatusSkipped},
	}
	events := finishedEvents(sink.events)
	outcomes := outcomesByCall(journal.outcomes)
	messages := toolMessagesByCall(result.Messages)
	intents := map[string]bool{}
	for _, intent := range journal.intents {
		intents[intent.ToolCallID] = true
	}
	for id, w := range want {
		if !intents[id] {
			t.Errorf("%s: no journaled intent", id)
		}
		if outcomes[id].Status != w.journal {
			t.Errorf("%s: journal status = %q, want %q", id, outcomes[id].Status, w.journal)
		}
		if events[id].ToolStatus != w.status {
			t.Errorf("%s: event status = %q, want %q", id, events[id].ToolStatus, w.status)
		}
		if messages[id].Content == "" || messages[id].Content != events[id].Content {
			t.Errorf("%s: message %q should match event content %q", id, messages[id].Content, events[id].Content)
		}
	}
}

type failingTool struct{}

func (failingTool) Name() string        { return "failing_tool" }
func (failingTool) Description() string { return "always fails" }
func (failingTool) Schema() api.ToolFunction {
	return api.ToolFunction{Name: "failing_tool", Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (failingTool) Execute(context.Context, ToolContext, map[string]any) (ToolResult, error) {
	return ToolResult{}, errors.New("boom")
}

// cdTool reports a new working directory, like bash after a cd.
type cdTool struct{ dir string }

func (t cdTool) Name() string        { return "cd_tool" }
func (t cdTool) Description() string { return "changes directory" }
func (t cdTool) Schema() api.ToolFunction {
	return api.ToolFunction{Name: "cd_tool", Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (t cdTool) Execute(context.Context, ToolContext, map[string]any) (ToolResult, error) {
	return ToolResult{Content: "moved", WorkingDir: t.dir}, nil
}

// cdComposer runs cd_tool as a nested call.
type cdComposer struct{ executor NestedExecutor }

func (c *cdComposer) Name() string        { return "composer" }
func (c *cdComposer) Description() string { return "nested cd" }
func (c *cdComposer) Schema() api.ToolFunction {
	return api.ToolFunction{Name: "composer", Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (c *cdComposer) SetNestedExecutor(executor NestedExecutor) { c.executor = executor }
func (c *cdComposer) Execute(ctx context.Context, toolCtx ToolContext, _ map[string]any) (ToolResult, error) {
	if _, err := c.executor.ExecuteNested(ctx, toolCtx.ToolCallID, "cd_tool", map[string]any{}); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: "composed"}, nil
}

// TestNestedCallAppliesWorkingDir pins parity with top-level calls: a nested
// call that changes directory moves the session.
func TestNestedCallAppliesWorkingDir(t *testing.T) {
	start, next := t.TempDir(), t.TempDir()
	next, err := filepath.EvalSymlinks(next)
	if err != nil {
		t.Fatal(err)
	}
	registry := &Registry{}
	registry.Register(cdTool{dir: next})
	registry.Register(&cdComposer{})
	session := &Session{
		Client:     &fakeClient{responses: [][]api.ChatResponse{toolCallResponse(namedCall("c1", "composer", nil)), journalText("done")}},
		Tools:      registry,
		WorkingDir: start,
	}
	result, err := session.Run(context.Background(), RunOptions{Model: "m", NewMessages: []api.Message{{Role: "user", Content: "cd"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.WorkingDir != next {
		t.Fatalf("working dir = %q, want %q after a nested cd", result.WorkingDir, next)
	}
}

// failingOutcomeJournal fails every outcome write.
type failingOutcomeJournal struct{ recordingJournal }

func (j *failingOutcomeJournal) RecordToolOutcome(ToolOutcome) error {
	return errors.New("disk full")
}

// TestNestedJournalFailureFailsCall pins that a nested call whose outcome
// cannot be journaled fails, as a top-level call would.
func TestNestedJournalFailureFailsCall(t *testing.T) {
	registry := &Registry{}
	registry.Register(cdTool{dir: ""})
	journal := &failingOutcomeJournal{}
	session := &Session{Tools: registry, Journal: journal}
	_, err := session.executeNestedToolCall(context.Background(), nestedRunIDs{runID: "run-1"}, "parent", "cd_tool", map[string]any{})
	if err == nil {
		t.Fatal("nested call should fail when its outcome cannot be journaled")
	}
}
