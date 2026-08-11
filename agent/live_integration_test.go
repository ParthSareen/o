package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/o/api"
)

// TestLiveCompactionAgainstLocalServer proves compaction works end-to-end
// against a real running server: event emission, message replacement, and a
// usable post-compaction response. Skip by default; run with:
//
//	OLLAMA_LIVE=1 go test ./agent -run TestLiveCompactionAgainstLocalServer -v
func TestLiveCompactionAgainstLocalServer(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("set OLLAMA_LIVE=1 (optionally OLLAMA_LIVE_HOST, OLLAMA_LIVE_MODEL)")
	}
	host := os.Getenv("OLLAMA_LIVE_HOST")
	if host == "" {
		host = "http://localhost:11439"
	}
	model := os.Getenv("OLLAMA_LIVE_MODEL")
	if model == "" {
		model = "glm-5.2:cloud"
	}
	base, err := url.Parse(host)
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(base, http.DefaultClient)

	var seen []EventType
	session := &Session{
		Client: client,
		Compactor: &SimpleCompactor{
			Client:  client,
			Options: CompactionOptions{ContextWindowTokens: 2200},
		},
		EventSinks: []EventSink{EventSinkFunc(func(event Event) error {
			seen = append(seen, event.Type)
			return nil
		})},
	}

	// Preload a conversation sized between the trigger threshold (~1760 with
	// the 2200-token window) and the window itself, so the in-run estimate
	// check compacts instead of tripping the pre-flight budget guard.
	padding := strings.Repeat("the quick brown fox jumps over the lazy dog ", 77)
	messages := []api.Message{
		{Role: "user", Content: "Please remember this passage and keep discussing it: " + padding},
		{Role: "assistant", Content: "Got it, noted the passage."},
		{Role: "user", Content: "Now restate it in French: " + padding},
		{Role: "assistant", Content: "Voici le passage, encore une fois."},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, err := session.Run(ctx, RunOptions{
		ChatID:      fmt.Sprintf("live-compact-%d", time.Now().UnixNano()),
		Model:       model,
		Messages:    messages,
		NewMessages: []api.Message{{Role: "user", Content: "Reply with a single short sentence confirming you are still here."}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, want := range []EventType{EventCompactionStarted, EventCompacted} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %s in %v", want, seen)
		}
	}

	hasSummary := false
	for _, msg := range result.Messages {
		if strings.Contains(msg.Content, CompactionSummaryMessagePrefix) {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Errorf("no compaction summary in %d post-run messages", len(result.Messages))
	}
	t.Logf("events: %v", seen)
}
