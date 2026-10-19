package agent

import (
	"context"
	"testing"

	"github.com/ParthSareen/o/api"
)

func intPtr(v int) *int { return &v }

func metricsEvents(events []Event) []*api.Metrics {
	var collected []*api.Metrics
	for _, event := range events {
		if event.Type == EventMetrics {
			collected = append(collected, event.Metrics)
		}
	}
	return collected
}

func TestSessionEmitsMetricsPerChatPost(t *testing.T) {
	args := api.NewToolCallFunctionArguments()
	args.Set("value", "hello")
	client := &fakeClient{
		responses: [][]api.ChatResponse{
			{
				{Message: api.Message{Role: "assistant", Content: "checking."}},
				{
					Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
						ID: "call-1",
						Function: api.ToolCallFunction{
							Name:      "echo_tool",
							Arguments: args,
						},
					}}},
					Done:    true,
					Metrics: api.Metrics{PromptEvalCount: 100, EvalCount: 20, PromptEvalCachedCount: intPtr(80)},
				},
			},
			{
				{
					Message: api.Message{Role: "assistant", Content: "done"},
					Done:    true,
					Metrics: api.Metrics{PromptEvalCount: 140, EvalCount: 15, PromptEvalCachedCount: intPtr(120)},
				},
			},
		},
	}
	registry := &Registry{}
	registry.Register(staticTool{})
	events := &recordingEventSink{}
	session := &Session{
		Client:        client,
		Tools:         registry,
		ApprovalState: approvalStateForTest(true, nil),
		EventSinks:    []EventSink{events},
	}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "use a tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("chat posts = %d, want 2", len(client.requests))
	}

	metrics := metricsEvents(events.events)
	if len(metrics) != 2 {
		t.Fatalf("metrics events = %d (%#v), want one per chat POST", len(metrics), events.events)
	}
	if metrics[0].PromptEvalCount != 100 || metrics[0].EvalCount != 20 || metrics[0].PromptEvalCachedCount == nil || *metrics[0].PromptEvalCachedCount != 80 {
		t.Fatalf("round-1 metrics = %#v", metrics[0])
	}
	if metrics[1].PromptEvalCount != 140 || metrics[1].EvalCount != 15 || metrics[1].PromptEvalCachedCount == nil || *metrics[1].PromptEvalCachedCount != 120 {
		t.Fatalf("round-2 metrics = %#v", metrics[1])
	}
	if metrics[0] == metrics[1] {
		t.Fatal("metrics events aliased the same metrics pointer")
	}
	if len(result.Messages) < 3 {
		t.Fatalf("result messages lost the tool round: %#v", result.Messages)
	}
}

func TestSessionSkipsMetricsEventWithoutUsage(t *testing.T) {
	client := &fakeClient{
		responses: [][]api.ChatResponse{
			{
				{
					Message: api.Message{Role: "assistant", Content: "no usage here"},
					Done:    true,
				},
			},
		},
	}
	events := &recordingEventSink{}
	session := &Session{
		Client:     client,
		EventSinks: []EventSink{events},
	}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Messages) != 2 {
		t.Fatalf("result messages = %#v", result.Messages)
	}
	if got := metricsEvents(events.events); len(got) != 0 {
		t.Fatalf("usage-less chat post emitted metrics events: %#v", got)
	}
	if !hasEventType(events.events, EventRunFinished) {
		t.Fatalf("run finished event missing: %#v", events.events)
	}
}
