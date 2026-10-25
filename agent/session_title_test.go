package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ParthSareen/o/api"
)

type titleCaptureClient struct {
	requests []*api.ChatRequest
	// content returned on every call.
	content string
	// failWith, when set, makes the first failWith failures return without
	// streaming content.
	failWith error
	failed   int
}

func (c *titleCaptureClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	c.requests = append(c.requests, req)
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.failWith != nil && c.failed < 1 {
		c.failed++
		return c.failWith
	}
	return fn(api.ChatResponse{Message: api.Message{Role: "assistant", Content: c.content}, Done: true})
}

func TestSessionTitleUserMessagesFiltersAndCounts(t *testing.T) {
	messages := []api.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "  "}, // attachment-only turn: nothing to name by
		{Role: "tool", Content: "tool output"},
		{Role: "user", Content: "second"},
		{Role: "user", Content: "third"},
		{Role: "user", Content: "fourth"},
		{Role: "user", Content: "fifth"},
		{Role: "user", Content: "sixth"},
	}
	first, total := SessionTitleUserMessages(messages, SessionTitleMaxUserMessages)
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
	if len(first) != SessionTitleMaxUserMessages {
		t.Errorf("len(first) = %d, want %d", len(first), SessionTitleMaxUserMessages)
	}
	for i, want := range []string{"first", "second", "third", "fourth", "fifth"} {
		if first[i] != want {
			t.Errorf("first[%d] = %q, want %q", i, first[i], want)
		}
	}
	// "sixth" must not be in: naming is grounded in the first five turns.
	if strings.Contains(sessionTitlePrompt(first), "sixth") {
		t.Error("prompt contains messages beyond the first five")
	}
}

func TestGenerateSessionTitleRequestShape(t *testing.T) {
	client := &titleCaptureClient{content: " Refactor agent code. "}
	name, err := GenerateSessionTitle(context.Background(), client, "test-model", []string{"first", "second"})
	if err != nil {
		t.Fatalf("GenerateSessionTitle: %v", err)
	}
	if name != "Refactor agent code" {
		t.Errorf("name = %q, want %q", name, "Refactor agent code")
	}
	if len(client.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(client.requests))
	}
	req := client.requests[0]
	if req.Model != "test-model" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages = %v, want one system and one user", req.Messages)
	}
	if req.Think == nil || req.Think.Value != false {
		t.Errorf("think = %v, want disabled", req.Think)
	}
	body := req.Messages[1].Content
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Errorf("prompt %q missing user messages", body)
	}
	if !strings.Contains(body, "Reply with only the title.") {
		t.Errorf("prompt %q missing reply instruction", body)
	}
}

func TestGenerateSessionTitleFallsBackOnUnsupportedThink(t *testing.T) {
	client := &titleCaptureClient{
		content:  "Fix the build",
		failWith: errors.New("this model does not support think"),
	}
	name, err := GenerateSessionTitle(context.Background(), client, "test-model", []string{"fix the build"})
	if err != nil {
		t.Fatalf("GenerateSessionTitle: %v", err)
	}
	if name != "Fix the build" {
		t.Errorf("name = %q", name)
	}
	if len(client.requests) != 2 {
		t.Fatalf("got %d requests, want 2 (retired think=false)", len(client.requests))
	}
	if client.requests[1].Think != nil {
		t.Error("retry still carries a think value")
	}
}

func TestGenerateSessionTitleGuards(t *testing.T) {
	client := &titleCaptureClient{content: "x"}
	for _, tc := range []struct {
		name     string
		client   ChatClient
		model    string
		messages []string
	}{
		{"nil client", nil, "m", []string{"a"}},
		{"empty model", client, "  ", []string{"a"}},
		{"no user messages", client, "m", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateSessionTitle(context.Background(), tc.client, tc.model, tc.messages)
			if got != "" || err != nil {
				t.Errorf("GenerateSessionTitle = %q, %v; want \"\", nil", got, err)
			}
		})
	}
	if len(client.requests) != 0 {
		t.Errorf("guard paths issued %d requests, want 0", len(client.requests))
	}
}

func TestGenerateSessionTitleLongMessagesTruncated(t *testing.T) {
	client := &titleCaptureClient{content: "title"}
	GenerateSessionTitle(context.Background(), client, "m", []string{strings.Repeat("a", sessionTitleMessageMaxRunes+100)})
	body := client.requests[0].Messages[1].Content
	// Truncation marker + the instruction line, but not the full 340 runes.
	if !strings.Contains(body, "…") {
		t.Errorf("long message not truncated: %q", body[:80])
	}
	if strings.Contains(body, strings.Repeat("a", 300)) {
		t.Errorf("truncation cap exceeded: body holds over %d a's", sessionTitleMessageMaxRunes)
	}
}

func TestCleanSessionTitle(t *testing.T) {
	cases := map[string]string{
		"  Refactor agent code  ":            "Refactor agent code",
		"\"Fix the build\"":                  "Fix the build",
		"```\nDebug flaky test\n```":         "Debug flaky test",
		"Title: Explain the retry loop":      "Explain the retry loop",
		"Session title: add caching":         "add caching",
		"Add   multi-line\n\ttitle handling": "Add multi-line title handling",
		"'Migrate the DB.'":                  "Migrate the DB",
	}
	// 60 runes hold "word " twelve times; the cap trims back to whole words.
	cases[strings.Repeat("word ", 30)] = strings.TrimRight(strings.Repeat("word ", 12), " ")
	for in, want := range cases {
		if got := CleanSessionTitle(in); got != want {
			t.Errorf("CleanSessionTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanSessionTitle("   "); got != "" {
		t.Errorf("whitespace-only = %q, want empty", got)
	}
}
