package sessionstore

import (
	"path/filepath"
	"testing"

	"github.com/ParthSareen/o/api"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	home := dir
	t.Setenv("HOME", home)
	_ = filepath.Join(home, ".o") // ensure HOME override is picked up
	// dbPath uses os.UserHomeDir which reads $HOME on Unix
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndLoadSession(t *testing.T) {
	s := openTestStore(t)

	sess, err := s.CreateSession("llama3", "/tmp/work", "you are helpful", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("empty session ID")
	}

	msgs := []api.Message{
		{Role: "user", Content: "hello world"},
		{Role: "assistant", Content: "hi there", Thinking: "thinking about it"},
	}
	if err := s.AppendMessages(sess.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	loaded, err := s.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(loaded.Messages))
	}
	if loaded.Messages[0].Content != "hello world" {
		t.Errorf("msg 0 content = %q", loaded.Messages[0].Content)
	}
	if loaded.Messages[1].Thinking != "thinking about it" {
		t.Errorf("msg 1 thinking = %q", loaded.Messages[1].Thinking)
	}
	if loaded.Title != "hello world" {
		t.Errorf("title = %q, want %q", loaded.Title, "hello world")
	}
}

func TestAppendOnlyGrowsSeq(t *testing.T) {
	s := openTestStore(t)
	sess, _ := s.CreateSession("m", "", "", "")

	s.AppendMessages(sess.ID, []api.Message{{Role: "user", Content: "first"}})
	s.AppendMessages(sess.ID, []api.Message{{Role: "assistant", Content: "reply"}})
	s.AppendMessages(sess.ID, []api.Message{{Role: "user", Content: "second"}})

	loaded, _ := s.LoadSession(sess.ID)
	if len(loaded.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(loaded.Messages))
	}
	if loaded.Messages[2].Content != "second" {
		t.Errorf("msg 2 = %q", loaded.Messages[2].Content)
	}
}

func TestListSessions(t *testing.T) {
	s := openTestStore(t)
	s.CreateSession("a", "", "", "")
	sess2, _ := s.CreateSession("b", "", "", "")
	s.AppendMessages(sess2.ID, []api.Message{{Role: "user", Content: "hi"}})

	list, err := s.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
	// Most recently updated should be first
	if list[0].ID != sess2.ID {
		t.Errorf("expected session2 first, got %s", list[0].ID)
	}
}

func TestSessionName(t *testing.T) {
	s := openTestStore(t)
	sess, err := s.CreateSession("m", "", "", "my-session")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Name != "my-session" {
		t.Fatalf("session name = %q, want %q", sess.Name, "my-session")
	}

	loaded, err := s.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.Name != "my-session" {
		t.Fatalf("loaded name = %q, want %q", loaded.Name, "my-session")
	}

	if err := s.SetName(sess.ID, "renamed"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	list, _ := s.ListSessions(10)
	if len(list) != 1 || list[0].Name != "renamed" {
		t.Fatalf("list name = %q", list[0].Name)
	}
}

func TestMigrateAddsNameColumn(t *testing.T) {
	s := openTestStore(t)
	// A fresh store already has the column; verify it is queryable and
	// that re-migrating (e.g. on an older DB) is idempotent.
	if err := s.migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	sess, err := s.CreateSession("m", "", "", "probe")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Name != "probe" {
		t.Fatalf("session name = %q, want %q", sess.Name, "probe")
	}
	list, err := s.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 || list[0].Name != "probe" {
		t.Fatalf("name after migrate = %q", list[0].Name)
	}
}

func TestDeleteSession(t *testing.T) {
	s := openTestStore(t)
	sess, _ := s.CreateSession("m", "", "", "")
	s.AppendMessages(sess.ID, []api.Message{{Role: "user", Content: "x"}})

	if err := s.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.LoadSession(sess.ID); err == nil {
		t.Fatal("expected error loading deleted session")
	}
}

func TestPromptHistory(t *testing.T) {
	s := openTestStore(t)
	sess, _ := s.CreateSession("m", "", "", "")

	s.AddPrompt(sess.ID, "hello")
	s.AddPrompt(sess.ID, "world")
	s.AddPrompt(sess.ID, "  ") // empty after trim, should be skipped

	prompts, err := s.RecentPrompts(sess.ID, 10)
	if err != nil {
		t.Fatalf("RecentPrompts: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}
	// Most recent first
	if prompts[0] != "world" {
		t.Errorf("first prompt = %q, want %q", prompts[0], "world")
	}
}

func TestTitleTruncation(t *testing.T) {
	s := openTestStore(t)
	sess, _ := s.CreateSession("m", "", "", "")
	long := "This is a very long first line that definitely exceeds the eighty rune limit for session titles and should be truncated with an ellipsis"
	s.AppendMessages(sess.ID, []api.Message{{Role: "user", Content: long}})

	loaded, _ := s.LoadSession(sess.ID)
	if len([]rune(loaded.Title)) > 80 {
		t.Errorf("title too long: %d runes: %q", len([]rune(loaded.Title)), loaded.Title)
	}
}
