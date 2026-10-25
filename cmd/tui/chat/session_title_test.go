package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// titleNamingClient answers every naming call with a canned title and records
// requests on a channel so tests can tell whether a call fired.
type titleNamingClient struct {
	requests chan *api.ChatRequest
	title    string
}

func (c *titleNamingClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	select {
	case c.requests <- req:
	default:
	}
	return fn(api.ChatResponse{Message: api.Message{Role: "assistant", Content: c.title}, Done: true})
}

// titleFailingClient answers with an error; the naming flow must leave the
// derived title in place.
type titleFailingClient struct {
	requests chan *api.ChatRequest
}

func (c *titleFailingClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	select {
	case c.requests <- req:
	default:
	}
	return errors.New("model unavailable")
}

const titleOpener = "fix the build please"

func newTitleTestModel(t *testing.T, client coreagent.ChatClient, model string) (*chatModel, string) {
	t.Helper()
	store := openChatTestStore(t)
	sess := newTestSession(t, store, "test-model", "", titleOpener)
	m := &chatModel{
		ctx:    context.Background(),
		store:  store,
		chatID: sess.ID,
		opts:   Options{Model: model, Client: client},
	}
	return m, sess.ID
}

// titleHistory builds a completed-run history of n user messages (plus
// interleaved assistant replies) starting with the session's opener.
func titleHistory(n int) []api.Message {
	history := []api.Message{}
	for i := 0; i < n; i++ {
		if i > 0 {
			history = append(history, api.Message{Role: "assistant", Content: "answer"})
		}
		history = append(history, api.Message{Role: "user", Content: titleOpener + " " + strings.Repeat("x", i)})
	}
	return history
}

func waitTitle(t *testing.T, store *sessionstore.Store, chatID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		title, err := store.Title(chatID)
		if err == nil && title == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("title = %q (err %v), want %q", title, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func drainRequests(t *testing.T, ch chan *api.ChatRequest) []*api.ChatRequest {
	t.Helper()
	var got []*api.ChatRequest
	for {
		select {
		case req := <-ch:
			got = append(got, req)
			continue
		case <-time.After(50 * time.Millisecond):
			return got
		}
	}
}

// TestMaybeNameSessionNamesAtFiveUsers pins the headline behavior: the
// autonamer fires once, when the history first holds the first five user
// messages, and writes the model's title over the derived one.
func TestMaybeNameSessionNamesAtFiveUsers(t *testing.T) {
	client := &titleNamingClient{requests: make(chan *api.ChatRequest, 4), title: "Fix the build"}
	m, chatID := newTitleTestModel(t, client, "test-model")

	m.maybeNameSession(titleHistory(sessionTitleMinUserMessages - 1))
	if reqs := drainRequests(t, client.requests); len(reqs) != 0 {
		t.Errorf("naming fired below the threshold, want 0 calls")
	}

	history := titleHistory(sessionTitleMinUserMessages)
	m.maybeNameSession(history)
	if !m.titleNamed {
		t.Error("naming run did not arm titleNamed")
	}
	waitTitle(t, m.store, chatID, "Fix the build")

	reqs := drainRequests(t, client.requests)
	if len(reqs) != 1 {
		t.Fatalf("naming fired %d times, want 1", len(reqs))
	}
	// The prompt carries exactly the first five user messages.
	body := reqs[0].Messages[1].Content
	if !strings.Contains(body, titleOpener) || !strings.Contains(body, "Reply with only the title.") {
		t.Errorf("naming prompt %q missing opener or instruction", body)
	}

	m.maybeNameSession(history) // later completed runs: already named
	if reqs := drainRequests(t, client.requests); len(reqs) != 0 {
		t.Errorf("naming fired %d times, want 0", len(reqs))
	}
}

// Once a title is stored the autonamer never runs again for the session —
// including when a new process resumes it (cross-process skip via the store)
// or after compaction rewrites the history.
func TestMaybeNameSessionTitledSessionNeverReNames(t *testing.T) {
	client := &titleNamingClient{requests: make(chan *api.ChatRequest, 4), title: "Fix the build"}
	m, chatID := newTitleTestModel(t, client, "test-model")
	if err := m.store.SetTitle(chatID, "named elsewhere"); err != nil {
		t.Fatal(err)
	}

	history := titleHistory(sessionTitleMinUserMessages)
	m.maybeNameSession(history)
	if !m.titleNamed {
		t.Error("titled session did not arm titleNamed")
	}
	if reqs := drainRequests(t, client.requests); len(reqs) != 0 {
		t.Errorf("naming fired %d times, want 0", len(reqs))
	}

	// Compaction rewrites the history; a titled session still never re-names.
	summary := append(coreagent.CompactionSummaryMessages("earlier work", true),
		api.Message{Role: "user", Content: "a later turn"},
		api.Message{Role: "assistant", Content: "ok"})
	m.maybeNameSession(summary)
	if title, err := m.store.Title(chatID); err != nil || title != "named elsewhere" {
		t.Errorf("title = %q (err %v), want untouched", title, err)
	}
	if reqs := drainRequests(t, client.requests); len(reqs) != 0 {
		t.Errorf("naming fired %d times, want 0", len(reqs))
	}
}

// A failed naming call is non-fatal: the derived title stands.
func TestMaybeNameSessionKeepsDerivedTitleOnError(t *testing.T) {
	client := &titleFailingClient{requests: make(chan *api.ChatRequest, 4)}
	m, chatID := newTitleTestModel(t, client, "test-model")

	m.maybeNameSession(titleHistory(sessionTitleMinUserMessages))
	if reqs := drainRequests(t, client.requests); len(reqs) != 1 {
		t.Fatalf("naming fired %d times, want 1", len(reqs))
	}
	// Give the (returned-early) goroutine a moment; the title must be untouched.
	time.Sleep(50 * time.Millisecond)
	title, err := m.store.Title(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if title != sessionstore.DerivedTitle(titleOpener) {
		t.Errorf("title = %q, want derived %q", title, sessionstore.DerivedTitle(titleOpener))
	}
}

// A late run in a fresh process on a session that never got named (a failed
// call, or one from before the feature) is named on its next completed run.
func TestMaybeNameSessionNamesUnnamedResumedSession(t *testing.T) {
	client := &titleNamingClient{requests: make(chan *api.ChatRequest, 4), title: "Fix the build"}
	m, chatID := newTitleTestModel(t, client, "test-model")

	m.maybeNameSession(titleHistory(sessionTitleMinUserMessages))
	waitTitle(t, m.store, chatID, "Fix the build")
	if reqs := drainRequests(t, client.requests); len(reqs) != 1 {
		t.Errorf("naming fired %d times, want 1", len(reqs))
	}
}
