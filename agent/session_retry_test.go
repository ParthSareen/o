package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

// scriptedClient plays one step per Chat call: stream the responses, then
// return the error.
type scriptedClient struct {
	steps    []scriptedStep
	requests []*api.ChatRequest
}

type scriptedStep struct {
	responses []api.ChatResponse
	err       error
}

func (c *scriptedClient) Chat(_ context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	c.requests = append(c.requests, req)
	if len(c.requests) > len(c.steps) {
		return nil
	}
	step := c.steps[len(c.requests)-1]
	for _, response := range step.responses {
		if err := fn(response); err != nil {
			return err
		}
	}
	return step.err
}

func noRetryDelays(t *testing.T) {
	t.Helper()
	saved := modelRetryDelays
	modelRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { modelRetryDelays = saved })
}

func runScripted(t *testing.T, steps ...scriptedStep) (*RunResult, *scriptedClient, *recordingEventSink, error) {
	t.Helper()
	client := &scriptedClient{steps: steps}
	sink := &recordingEventSink{}
	session := &Session{Client: client, EventSinks: []EventSink{sink}}
	result, err := session.Run(context.Background(), RunOptions{
		Model:       "m",
		NewMessages: []api.Message{{Role: "user", Content: "hi"}},
	})
	return result, client, sink, err
}

func countEvents(events []Event, kind EventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == kind {
			n++
		}
	}
	return n
}

var okStep = scriptedStep{responses: []api.ChatResponse{{Message: api.Message{Role: "assistant", Content: "ok"}, Done: true}}}

func TestRunRetriesTransientModelErrorWithoutHistory(t *testing.T) {
	noRetryDelays(t)
	result, client, sink, err := runScripted(t,
		scriptedStep{err: api.StatusError{StatusCode: 503, ErrorMessage: "overloaded"}},
		okStep,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	if len(client.requests[1].Messages) != len(client.requests[0].Messages) {
		t.Fatal("a transient retry must resend the same request, not add messages")
	}
	if got := countEvents(sink.events, EventModelRetry); got != 1 {
		t.Fatalf("model_retry events = %d, want 1", got)
	}
	if n := len(result.Messages); n != 2 {
		t.Fatalf("history = %d messages, want prompt + answer", n)
	}
}

func TestRunGivesUpAfterRetryBudget(t *testing.T) {
	noRetryDelays(t)
	fail := scriptedStep{err: api.StatusError{StatusCode: 429, ErrorMessage: "rate limited"}}
	_, client, sink, err := runScripted(t, fail, fail, fail, fail, okStep)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("Run err = %v, want the rate limit error", err)
	}
	if len(client.requests) != len(modelRetryDelays)+1 {
		t.Fatalf("requests = %d, want %d", len(client.requests), len(modelRetryDelays)+1)
	}
	if got := countEvents(sink.events, EventModelRetry); got != len(modelRetryDelays) {
		t.Fatalf("model_retry events = %d, want %d", got, len(modelRetryDelays))
	}
}

func TestRunDoesNotRetryAfterPartialOutput(t *testing.T) {
	noRetryDelays(t)
	_, client, _, err := runScripted(t,
		scriptedStep{
			responses: []api.ChatResponse{{Message: api.Message{Role: "assistant", Content: "partial"}}},
			err:       api.StatusError{StatusCode: 502, ErrorMessage: "bad gateway"},
		},
		okStep,
	)
	if err == nil {
		t.Fatal("a failure after streamed output should not be retried")
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
}

func TestRunDoesNotRetryClientErrors(t *testing.T) {
	noRetryDelays(t)
	_, client, _, err := runScripted(t, scriptedStep{err: api.StatusError{StatusCode: 404, ErrorMessage: "model not found"}}, okStep)
	if err == nil || len(client.requests) != 1 {
		t.Fatalf("404 should fail without retry: err=%v requests=%d", err, len(client.requests))
	}
}

func TestRunNudgesModelOnToolCallParseError(t *testing.T) {
	noRetryDelays(t)
	result, client, sink, err := runScripted(t,
		scriptedStep{err: api.StatusError{StatusCode: 500, ErrorMessage: "error parsing tool call: raw='{bad'"}},
		okStep,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if countEvents(sink.events, EventModelRetry) != 0 {
		t.Fatal("parse errors are corrected by the model, not resent")
	}
	last := client.requests[1].Messages[len(client.requests[1].Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "error parsing tool call") {
		t.Fatalf("second request should carry the correction prompt, got %+v", last)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("history = %d messages, want prompt + correction + answer", len(result.Messages))
	}
}
