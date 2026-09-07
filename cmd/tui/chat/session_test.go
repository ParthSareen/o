package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

func openChatTestStore(t *testing.T) *sessionstore.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := sessionstore.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestSession(t *testing.T, store *sessionstore.Store, model, name, message string) *sessionstore.Session {
	t.Helper()
	sess, err := store.CreateSession(model, t.TempDir(), "system", name)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if message != "" {
		if err := store.AppendMessages(sess.ID, []api.Message{{Role: "user", Content: message}}); err != nil {
			t.Fatalf("AppendMessages: %v", err)
		}
	}
	return sess
}

func TestChatResumeMostRecent(t *testing.T) {
	store := openChatTestStore(t)
	current := newTestSession(t, store, "model-a", "", "current chat")
	other := newTestSession(t, store, "model-b", "", "previous chat")

	m := &chatModel{ctx: context.Background(), store: store, chatID: current.ID}
	updated, _ := m.handleResumeCommand("")
	fm := updated.(chatModel)
	if fm.chatID != other.ID {
		t.Fatalf("chatID = %q, want the most recent other session %q", fm.chatID, other.ID)
	}
	if len(fm.messages) != 1 || fm.messages[0].Content != "previous chat" {
		t.Fatalf("messages = %+v, want the resumed history", fm.messages)
	}
	if fm.opts.Model != "model-b" {
		t.Fatalf("model = %q, want the resumed session's model", fm.opts.Model)
	}
}

func TestChatResumeByRef(t *testing.T) {
	store := openChatTestStore(t)
	older := newTestSession(t, store, "model-a", "", "older")
	named := newTestSession(t, store, "model-b", "review work", "named session")
	_ = older

	for _, ref := range []string{"review work", strings.ToUpper(named.ID[:8]), named.ID} {
		m := &chatModel{ctx: context.Background(), store: store, chatID: ""}
		updated, _ := m.handleResumeCommand(ref)
		fm := updated.(chatModel)
		if fm.chatID != named.ID {
			t.Fatalf("/resume %q: chatID = %q, want %q", ref, fm.chatID, named.ID)
		}
	}
}

func TestChatResumeGuards(t *testing.T) {
	store := openChatTestStore(t)
	current := newTestSession(t, store, "model-a", "", "current chat")

	// Busy run: refuse.
	m := &chatModel{ctx: context.Background(), store: store, chatID: current.ID, thinking: true}
	updated, _ := m.handleResumeCommand("")
	fm := updated.(chatModel)
	if fm.chatID != current.ID {
		t.Fatal("must not resume while a run is active")
	}
	last := fm.entries[len(fm.entries)-1]
	if !strings.Contains(last.content, "Wait for the current run") {
		t.Fatalf("entry = %q, want busy warning", last.label)
	}

	// No match: report and stay.
	m = &chatModel{ctx: context.Background(), store: store, chatID: current.ID}
	updated, _ = m.handleResumeCommand("no-such-session")
	fm = updated.(chatModel)
	if fm.chatID != current.ID {
		t.Fatal("failed resume must not switch sessions")
	}
	last = fm.entries[len(fm.entries)-1]
	if !strings.Contains(last.content, "No saved session matches") {
		t.Fatalf("entry = %q, want no-match message", last.label)
	}

	// No store: report and stay.
	m = &chatModel{ctx: context.Background(), chatID: current.ID}
	updated, _ = m.handleResumeCommand("")
	fm = updated.(chatModel)
	if fm.chatID != current.ID {
		t.Fatal("no store must not switch sessions")
	}
}

func TestMatchSessionRef(t *testing.T) {
	metas := []sessionstore.SessionMeta{
		{ID: "aa11-first", Name: "alpha"},
		{ID: "bb22-second", Name: "Beta Work"},
	}
	tests := []struct {
		ref  string
		want string
	}{
		{"aa11-first", "aa11-first"},
		{"alpha", "aa11-first"},
		{"ALPHA", "aa11-first"},
		{"beta work", "bb22-second"},
		{"AA11", "aa11-first"},
		{"bb", "bb22-second"},
		{"gamma", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := matchSessionRef(metas, tt.ref); got != tt.want {
			t.Errorf("matchSessionRef(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
