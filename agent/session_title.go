package agent

import (
	"context"
	"strings"

	"github.com/ParthSareen/o/api"
)

// Session naming: after a session accumulates a few user turns, a small LLM
// call rewrites its auto-derived title into a short, one-line name that is
// easier to parse in the /sessions and /resume lists.
const (
	// SessionTitleMaxUserMessages caps how many leading user messages the
	// naming prompt contains.
	SessionTitleMaxUserMessages = 5
	// sessionTitleMessageMaxRunes caps each user message in the prompt; the
	// shape of a request, not its prose, is what names a session.
	sessionTitleMessageMaxRunes = 240
	// SessionTitleMaxRunes caps the generated title.
	SessionTitleMaxRunes = 60
)

const sessionTitleSystemPrompt = `You generate session titles for a terminal chat application.
Reply with only the title: a few words describing what the session is about.
No quotes, no markdown, no trailing punctuation, no explanations, no "Title:" prefix.`

// SessionTitleUserMessages returns the contents of up to the first n=limit
// user messages and the total number of user messages in the history. Only
// plain-text user messages count: tool results ride on other roles, and
// attachment-only turns (empty content) carry nothing to name the session by.
func SessionTitleUserMessages(messages []api.Message, limit int) (first []string, total int) {
	for _, msg := range messages {
		if msg.Role != "user" || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		total++
		if len(first) < limit {
			first = append(first, msg.Content)
		}
	}
	return first, total
}

// sessionTitlePrompt renders the user prompt for the naming call.
func sessionTitlePrompt(messages []string) string {
	var b strings.Builder
	b.WriteString("Name this chat session from its user messages:\n\n")
	for _, content := range messages {
		b.WriteString(strings.TrimSpace(truncateRunesAutoReview(content, sessionTitleMessageMaxRunes)))
		b.WriteString("\n\n")
	}
	b.WriteString("Reply with only the title.")
	return b.String()
}

// GenerateSessionTitle asks the model for a short session title from the
// session's first user messages (pass the slice returned by
// SessionTitleUserMessages). It returns "" when there is nothing to name the
// session with or the model answered nothing usable; callers keep the derived
// title in that case.
func GenerateSessionTitle(ctx context.Context, client ChatClient, model string, firstMessages []string) (string, error) {
	if client == nil || strings.TrimSpace(model) == "" || len(firstMessages) == 0 {
		return "", nil
	}
	first := firstMessages

	buildRequest := func(think *api.ThinkValue) *api.ChatRequest {
		return &api.ChatRequest{
			Model: model,
			Messages: []api.Message{
				{Role: "system", Content: sessionTitleSystemPrompt},
				{Role: "user", Content: sessionTitlePrompt(first)},
			},
			Think: think,
		}
	}

	var out strings.Builder
	req := buildRequest(&api.ThinkValue{Value: false})
	err := client.Chat(ctx, req, func(response api.ChatResponse) error {
		out.WriteString(response.Message.Content)
		return nil
	})
	if err != nil && isUnsupportedThinkError(err) {
		// The model or proxy rejects think=false; ask without a think field.
		out.Reset()
		err = client.Chat(ctx, buildRequest(nil), func(response api.ChatResponse) error {
			out.WriteString(response.Message.Content)
			return nil
		})
	}
	if err != nil {
		return "", err
	}
	return CleanSessionTitle(out.String()), nil
}

// CleanSessionTitle normalizes a model's answer into a single-line title:
// surrounding quotes and fences stripped, whitespace collapsed, capped at
// SessionTitleMaxRunes without splitting a word.
func CleanSessionTitle(s string) string {
	s = strings.TrimSpace(s)
	// Fenced answer.
	for _, fence := range []string{"```", "\"", "'", "`"} {
		s = strings.TrimSpace(s)
		if len(s) >= 2*len(fence) && strings.HasPrefix(s, fence) && strings.HasSuffix(s, fence) {
			s = s[len(fence) : len(s)-len(fence)]
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	// Models sometimes answer "Title: Fix the bug" despite instructions.
	for _, prefix := range []string{"title:", "session title:"} {
		if len(s) > len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			s = strings.TrimSpace(s[len(prefix):])
		}
	}
	s = strings.Trim(s, "\"'`")
	s = strings.TrimRight(s, " .!:-")
	r := []rune(s)
	if len(r) > SessionTitleMaxRunes {
		s = string(r[:SessionTitleMaxRunes])
		if i := strings.LastIndex(s, " "); i > 0 {
			s = s[:i]
		}
		s = strings.TrimRight(s, " .!:-")
	}
	return s
}
