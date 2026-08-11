package tools

import (
	"errors"
	"testing"

	"github.com/ParthSareen/o/agent"
)

// TestSubagentEventSinkTagsEvents tests that the subagentEventSink tags
// forwarded events with the parent tool call ID.
func TestSubagentEventSinkTagsEvents(t *testing.T) {
	var received []agent.Event
	sink := agent.EventSinkFunc(func(e agent.Event) error {
		received = append(received, e)
		return nil
	})

	wrapper := subagentEventSink{
		parentSinks: []agent.EventSink{sink},
		subagentID:  "parent-call-1",
	}

	err := wrapper.Emit(agent.Event{
		Type:       agent.EventToolStarted,
		ToolCallID: "child-call-1",
		ToolName:   "bash",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if received[0].SubagentID != "parent-call-1" {
		t.Fatalf("expected SubagentID %q, got %q", "parent-call-1", received[0].SubagentID)
	}
}

// TestSubagentEventSinkMultipleSinks tests that events are forwarded to all
// parent sinks.
func TestSubagentEventSinkMultipleSinks(t *testing.T) {
	var count int
	sink1 := agent.EventSinkFunc(func(e agent.Event) error {
		count++
		return nil
	})
	sink2 := agent.EventSinkFunc(func(e agent.Event) error {
		count++
		return nil
	})

	wrapper := subagentEventSink{
		parentSinks: []agent.EventSink{sink1, sink2},
		subagentID:  "parent-call-1",
	}

	_ = wrapper.Emit(agent.Event{Type: agent.EventMessageDelta, Content: "hello"})

	if count != 2 {
		t.Fatalf("expected 2 sink calls, got %d", count)
	}
}

// TestSubagentEventSinkErrorPropagation tests that errors from parent sinks
// are joined and returned.
func TestSubagentEventSinkErrorPropagation(t *testing.T) {
	errA := errors.New("sink A failed")
	sink := agent.EventSinkFunc(func(e agent.Event) error {
		return errA
	})

	wrapper := subagentEventSink{
		parentSinks: []agent.EventSink{sink},
		subagentID:  "parent-call-1",
	}

	err := wrapper.Emit(agent.Event{Type: agent.EventMessageDelta})
	if err == nil {
		t.Fatal("expected error from sink, got nil")
	}
}
