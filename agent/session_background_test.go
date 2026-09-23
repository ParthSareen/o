package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/o/api"
)

// fakeBackgroundSource pops one slice per DrainCompletions call; when repeat
// is true it fabricates a fresh completion forever once the queue empties
// (used to exercise the continuation cap).
type fakeBackgroundSource struct {
	queue  [][]BackgroundCompletion
	repeat bool
	drains int
	nextID int
}

func (s *fakeBackgroundSource) DrainCompletions() []BackgroundCompletion {
	s.drains++
	if len(s.queue) > 0 {
		out := s.queue[0]
		s.queue = s.queue[1:]
		return out
	}
	if s.repeat {
		s.nextID++
		return []BackgroundCompletion{failedBackgroundCompletion(fmt.Sprintf("bg-%d", s.nextID))}
	}
	return nil
}

func failedBackgroundCompletion(id string) BackgroundCompletion {
	return BackgroundCompletion{
		ID:       id,
		Command:  "go test ./pkg/...",
		ExitCode: 3,
		Duration: 2 * time.Second,
		LogPath:  "/tmp/o-agent-bg-test/" + id + ".log",
		Tail:     "FAIL: TestFlaky\n    foo_test.go:42: boom",
	}
}

func noticeMessages(messages []api.Message) []api.Message {
	var out []api.Message
	for _, msg := range messages {
		if strings.Contains(msg.Content, "[background task update") {
			out = append(out, msg)
		}
	}
	return out
}

func TestSessionRunEndDrainsBackgroundCompletionAndContinues(t *testing.T) {
	source := &fakeBackgroundSource{queue: [][]BackgroundCompletion{
		nil, // run-start drain: nothing finished while idle
		{failedBackgroundCompletion("bg-7")},
		nil,
	}}
	events := &recordingEventSink{}
	client := &fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", Content: "all set"}}},
		{{Message: api.Message{Role: "assistant", Content: "saw the failure, investigating"}}},
	}}
	session := &Session{Client: client, EventSinks: []EventSink{events}, Background: source}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "kick it off"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("client calls = %d, want 2 (completion must extend the run one step)", client.calls)
	}
	if source.drains != 3 {
		t.Fatalf("drains = %d, want 3 (start, end of first step, end of continuation)", source.drains)
	}

	notices := noticeMessages(result.Messages)
	if len(notices) != 1 {
		t.Fatalf("notice messages = %d, want 1", len(notices))
	}
	notice := notices[0]
	if notice.Role != "user" {
		t.Fatalf("notice role = %q, want user (synthetic, system-attributed in content)", notice.Role)
	}
	for _, want := range []string{"bg-7", "failed: exit 3", "go test", "/tmp/o-agent-bg-test/bg-7.log", "FAIL: TestFlaky", "boom"} {
		if !strings.Contains(notice.Content, want) {
			t.Fatalf("notice = %q, want %q", notice.Content, want)
		}
	}
	// Chronology: assistant answer, then notice, then the reaction.
	roles := []string{result.Messages[1].Role, result.Messages[2].Role, result.Messages[3].Role}
	if strings.Join(roles, ",") != "assistant,user,assistant" {
		t.Fatalf("message roles = %v, want assistant,user-notice,assistant", roles)
	}
	if got := result.Messages[3].Content; got != "saw the failure, investigating" {
		t.Fatalf("final message = %q, want the reaction", got)
	}

	var bgEvents []Event
	for _, ev := range events.events {
		if ev.Type == EventBackgroundTasks {
			bgEvents = append(bgEvents, ev)
		}
	}
	if len(bgEvents) != 1 || bgEvents[0].Content != notice.Content {
		t.Fatalf("background_tasks events = %+v, want one carrying the notice", bgEvents)
	}
}

func TestSessionRunStartDrainsIdleCompletions(t *testing.T) {
	source := &fakeBackgroundSource{queue: [][]BackgroundCompletion{
		{failedBackgroundCompletion("bg-3")}, // finished while the session sat idle
	}}
	client := &fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", Content: "done"}}},
	}}
	session := &Session{Client: client, Background: source}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("client calls = %d, want 1 (start drain is context, not a continuation)", client.calls)
	}
	notices := noticeMessages(result.Messages)
	if len(notices) != 1 || !strings.Contains(notices[0].Content, "bg-3") {
		t.Fatalf("notices = %+v, want one start-drain notice for bg-3", notices)
	}
	// The notice must be visible to the very first model request.
	req := client.requests[0]
	found := false
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, "bg-3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("first request missing the idle-completion notice: %#v", req.Messages)
	}
}

func TestSessionBackgroundContinuationCapKeepsRemainders(t *testing.T) {
	source := &fakeBackgroundSource{repeat: true} // crash-looping task
	client := &fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", Content: "ack"}}}, // further calls return empty responses; the loop must still stop

	}}
	session := &Session{Client: client, Background: source}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "run it"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Note: fakeClient.calls saturates once canned responses run out, so the
	// drain counter is the reliable measure of model steps here: 1 start
	// drain + 1 end drain per step, capped continuations only.
	if want := 1 + int(maxBackgroundContinuations); source.drains != want {
		t.Fatalf("drains = %d, want %d (1 start + %d capped end-of-run drains)", source.drains, want, maxBackgroundContinuations)
	}
	if got := len(noticeMessages(result.Messages)); got != 1+maxBackgroundContinuations {
		t.Fatalf("notices = %d, want %d", got, 1+maxBackgroundContinuations)
	}
}

func TestSessionWithoutBackgroundSourceDrainsNothing(t *testing.T) {
	client := &fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", Content: "done"}}},
	}}
	session := &Session{Client: client}
	if _, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("client calls = %d, want 1", client.calls)
	}
}

// signalingBackgroundSource mixes the queue-driven fake with the push
// interface: fire() queues a completion and pushes a (coalescing) signal.
type signalingBackgroundSource struct {
	fakeBackgroundSource
	signals chan struct{}
}

func (s *signalingBackgroundSource) BackgroundSignals() <-chan struct{} {
	return s.signals
}

func (s *signalingBackgroundSource) fire(completion BackgroundCompletion) {
	s.queue = append([][]BackgroundCompletion{{completion}}, s.queue...)
	select {
	case s.signals <- struct{}{}:
	default:
	}
}

// signalAfterCallsClient fires once after the Nth chat call, simulating a
// poll publication that lands while the run is mid tool-loop.
type signalAfterCallsClient struct {
	fakeClient
	fire func()
}

func (c *signalAfterCallsClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	err := c.fakeClient.Chat(ctx, req, fn)
	if fire := c.fire; fire != nil && c.fakeClient.calls == 1 {
		c.fire = nil
		fire()
	}
	return err
}

func TestSessionRunInjectsMidRunBackgroundEvents(t *testing.T) {
	source := &signalingBackgroundSource{
		fakeBackgroundSource: fakeBackgroundSource{queue: [][]BackgroundCompletion{
			nil, // run-start drain: nothing while idle
		}},
		signals: make(chan struct{}, 1),
	}
	args := api.NewToolCallFunctionArguments()
	args.Set("value", "hello")
	client := &signalAfterCallsClient{fakeClient: fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call-1",
			Function: api.ToolCallFunction{
				Name:      "echo_tool",
				Arguments: args,
			},
		}}}}},
		{{Message: api.Message{Role: "assistant", Content: "poll said the deploy landed, wrapping up"}}},
	}}}
	// The publication arrives mid-run: after the first model step (which
	// requested a tool call) and before the second model step.
	client.fire = func() {
		source.fire(BackgroundCompletion{
			ID:       "poll-1",
			Command:  "gh pr checks 42",
			Poll:     true,
			Tick:     3,
			Duration: 900 * time.Millisecond,
			LogPath:  "/tmp/o-agent-bg-test/poll-1.log",
			Tail:     "pass\tbuild (push)\npass\ttest (push)",
		})
	}

	registry := &Registry{}
	registry.Register(staticTool{})
	events := &recordingEventSink{}
	session := &Session{
		Client:     client,
		EventSinks: []EventSink{events},
		Tools:      registry,
		Background: source,
	}

	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "wait for CI"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("client calls = %d, want 2", client.calls)
	}
	if source.drains != 3 {
		t.Fatalf("drains = %d, want 3 (start, mid-run injection, end)", source.drains)
	}

	// The notice must interrupt before the SECOND model request — the very
	// next step after the signal, not a run-end continuation.
	req := client.requests[1]
	found := false
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, "poll-1 tick 3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("second request missing the mid-run poll notice: %#v", req.Messages)
	}

	notices := noticeMessages(result.Messages)
	if len(notices) != 1 {
		t.Fatalf("notice messages = %d, want 1 (no duplicate at run end)", len(notices))
	}
	for _, want := range []string{"poll-1 tick 3", "new output", "gh pr checks 42", "/tmp/o-agent-bg-test/poll-1.log", "pass\tbuild (push)"} {
		if !strings.Contains(notices[0].Content, want) {
			t.Fatalf("notice = %q, want %q", notices[0].Content, want)
		}
	}
	// Message chronology: assistant tool call, tool result, notice, reaction.
	var roles []string
	for _, msg := range result.Messages[1:] {
		roles = append(roles, msg.Role)
	}
	if strings.Join(roles, ",") != "assistant,tool,user,assistant" {
		t.Fatalf("message roles = %v, want tool loop then notice then reaction", roles)
	}
	if result.Messages[len(result.Messages)-1].Content != "poll said the deploy landed, wrapping up" {
		t.Fatalf("final reply = %q, want the model's reaction to the notice", result.Messages[len(result.Messages)-1].Content)
	}

	var bgEvents int
	for _, ev := range events.events {
		if ev.Type == EventBackgroundTasks {
			bgEvents++
		}
	}
	if bgEvents != 1 {
		t.Fatalf("background_tasks events = %d, want exactly 1", bgEvents)
	}
}

func TestSessionStaleBackgroundSignalIsANoOp(t *testing.T) {
	// A signal whose buffer another drain already consumed must not inject
	// an empty message or otherwise disturb the run.
	source := &signalingBackgroundSource{
		fakeBackgroundSource: fakeBackgroundSource{queue: [][]BackgroundCompletion{nil, nil, nil}},
		signals:              make(chan struct{}, 1),
	}
	source.signals <- struct{}{} // pending but drain finds nothing
	client := &fakeClient{responses: [][]api.ChatResponse{
		{{Message: api.Message{Role: "assistant", Content: "done"}}},
	}}
	session := &Session{Client: client, Background: source}
	result, err := session.Run(context.Background(), RunOptions{
		ChatID:      "chat-1",
		Model:       "model",
		NewMessages: []api.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(noticeMessages(result.Messages)) != 0 {
		t.Fatalf("stale signal injected a notice: %#v", noticeMessages(result.Messages))
	}
	if client.calls != 1 {
		t.Fatalf("client calls = %d, want 1", client.calls)
	}
}
