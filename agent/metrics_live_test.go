package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/ParthSareen/o/api"
)

// TestLiveMetricsEventsAgainstLocalServer proves the per-POST metrics
// plumbing end-to-end: two turns over one model/chat must each emit a
// metrics event, and the second turn — whose prompt extends the first —
// must report cached prompt tokens. Skip by default; run with:
//
//	OLLAMA_LIVE=1 OLLAMA_LIVE_MODEL=llama3.2:latest \
//	  go test ./agent -run TestLiveMetricsEventsAgainstLocalServer -v
func TestLiveMetricsEventsAgainstLocalServer(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 (optionally OLLAMA_LIVE_HOST, OLLAMA_LIVE_MODEL)")
	}
	host := os.Getenv("OLLAMA_LIVE_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	model := os.Getenv("OLLAMA_LIVE_MODEL")
	if model == "" {
		model = "llama3.2:latest"
	}
	base, err := url.Parse(host)
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(base, http.DefaultClient)

	var metrics []*api.Metrics
	session := &Session{
		Client:     client,
		EventSinks: []EventSink{EventSinkFunc(func(event Event) error {
			if event.Type == EventMetrics && event.Metrics != nil {
				metrics = append(metrics, event.Metrics)
			}
			return nil
		})},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chatID := fmt.Sprintf("live-metrics-%d", time.Now().UnixNano())

	first, err := session.Run(ctx, RunOptions{
		ChatID:      chatID,
		Model:       model,
		NewMessages: []api.Message{{Role: "user", Content: "Remember the number 17 for me. Reply OK."}},
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(metrics) != 1 || metrics[0].PromptEvalCount <= 0 || metrics[0].EvalCount <= 0 {
		t.Fatalf("turn-1 metrics events = %d, latest = %#v", len(metrics), metrics)
	}
	if got := metrics[0].PromptEvalCachedCount; got != nil && *got > metrics[0].PromptEvalCount {
		t.Fatalf("turn-1 cached prompt tokens %d exceed evaluated prompt %d", *got, metrics[0].PromptEvalCount)
	}
	t.Logf("turn 1: in=%d out=%d cached=%v", metrics[0].PromptEvalCount, metrics[0].EvalCount, metrics[0].PromptEvalCachedCount)

	// Second turn in the same session: the server should reuse the KV cache
	// across POSTs, so cached tokens must be reported on a #16428-era server.
	_, err = session.Run(ctx, RunOptions{
		ChatID:      chatID,
		Model:       model,
		Messages:    first.Messages,
		NewMessages: []api.Message{{Role: "user", Content: "Half it mentally. Reply with just the number."}},
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("turn-2 metrics events total = %d, want 2", len(metrics))
	}
	cached, ok := metrics[1].CachedPromptTokens()
	if !ok {
		t.Fatalf("turn-2 prompt_eval_cached_count missing: %#v (server may predate #16428)", metrics[1])
	}
	if cached <= 0 || cached > metrics[1].PromptEvalCount {
		t.Fatalf("turn-2 cached tokens = %d, prompt = %d", cached, metrics[1].PromptEvalCount)
	}
	hit := float64(cached) / float64(metrics[1].PromptEvalCount) * 100
	t.Logf("turn 2: in=%d out=%d cached=%d ch=%.0f%%", metrics[1].PromptEvalCount, metrics[1].EvalCount, cached, hit)
}
