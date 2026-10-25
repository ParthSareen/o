package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	coreagent "github.com/ParthSareen/o/agent"
	agenttools "github.com/ParthSareen/o/agent/tools"
)

// newBackgroundWakeTestModel builds the sparsest chatModel that exercises the
// wake path: a real BackgroundManager (started on a command that exits near
// instantaneously), no client — the triggered run fails in its own goroutine
// without blocking the assertions, which run synchronously in Update.
func newBackgroundWakeTestModel(t *testing.T, manager *agenttools.BackgroundManager) chatModel {
	t.Helper()
	return chatModel{
		ctx:    context.Background(),
		opts:   Options{Tools: &coreagent.Registry{Background: manager}, Model: "test-model"},
		status: "ready",
	}
}

func bufferBackgroundCompletion(t *testing.T, manager *agenttools.BackgroundManager) {
	t.Helper()
	if _, err := manager.Start("", "true"); err != nil {
		t.Fatalf("start background task: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !manager.PendingBackground() {
		if time.Now().After(deadline) {
			t.Fatal("background task completion never became pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundWakeStartsRunWhenIdle(t *testing.T) {
	manager := agenttools.NewBackgroundManager()
	bufferBackgroundCompletion(t, manager)

	m := newBackgroundWakeTestModel(t, manager)
	updated, _ := m.Update(chatBackgroundWakeMsg{})
	fm := updated.(chatModel)

	if !fm.running {
		t.Fatal("wake with pending background work should start a run")
	}
	if fm.unattendedBackgroundRuns != 1 {
		t.Fatalf("unattendedBackgroundRuns = %d, want 1", fm.unattendedBackgroundRuns)
	}
	if len(fm.entries) != 1 {
		t.Fatalf("entries = %d, want exactly the run-start system entry (no fake user entry)", len(fm.entries))
	}
	entry := fm.entries[0]
	if entry.role != "system" || !strings.Contains(entry.content, "Background") {
		t.Fatalf("entry = %#v, want a system entry explaining the wake", entry)
	}
}

func TestBackgroundWakeIgnoresStaleSignal(t *testing.T) {
	manager := agenttools.NewBackgroundManager()
	bufferBackgroundCompletion(t, manager)
	// Simulate a finishing run's end-of-run drain beating the watcher.
	manager.DrainCompletions()

	m := newBackgroundWakeTestModel(t, manager)
	updated, cmd := m.Update(chatBackgroundWakeMsg{})
	fm := updated.(chatModel)

	if fm.running {
		t.Fatal("stale wake (nothing pending) must not start a run")
	}
	if cmd == nil {
		t.Fatal("stale wake should re-arm the watcher")
	}
	if fm.unattendedBackgroundRuns != 0 {
		t.Fatalf("unattendedBackgroundRuns = %d, want 0", fm.unattendedBackgroundRuns)
	}
}

func TestBackgroundWakeDefersToActiveRun(t *testing.T) {
	manager := agenttools.NewBackgroundManager()
	bufferBackgroundCompletion(t, manager)

	m := newBackgroundWakeTestModel(t, manager)
	m.running = true
	updated, cmd := m.Update(chatBackgroundWakeMsg{})
	fm := updated.(chatModel)

	if fm.unattendedBackgroundRuns != 0 {
		t.Fatalf("unattendedBackgroundRuns = %d, want 0", fm.unattendedBackgroundRuns)
	}
	if cmd != nil {
		t.Fatal("wake during an active run must not re-arm; run completion re-arms")
	}
	if len(fm.entries) != 0 {
		t.Fatalf("entries = %d, want none", len(fm.entries))
	}
}

func TestBackgroundWakeRespectsUnattendedCap(t *testing.T) {
	manager := agenttools.NewBackgroundManager()
	bufferBackgroundCompletion(t, manager)

	m := newBackgroundWakeTestModel(t, manager)
	m.unattendedBackgroundRuns = maxUnattendedBackgroundRuns
	updated, cmd := m.Update(chatBackgroundWakeMsg{})
	fm := updated.(chatModel)

	if fm.running {
		t.Fatal("capped wake must not start a run")
	}
	if !strings.Contains(fm.status, "paused") {
		t.Fatalf("status = %q, want a paused hint", fm.status)
	}
	if cmd == nil {
		t.Fatal("capped wake should keep the watcher armed")
	}
}

func TestBackgroundWakeCapResetsOnUserRun(t *testing.T) {
	manager := agenttools.NewBackgroundManager()

	m := newBackgroundWakeTestModel(t, manager)
	m.unattendedBackgroundRuns = maxUnattendedBackgroundRuns
	updated, _ := m.startRun("hello")
	fm := updated.(chatModel)

	if fm.unattendedBackgroundRuns != 0 {
		t.Fatalf("unattendedBackgroundRuns = %d, want reset to 0", fm.unattendedBackgroundRuns)
	}
	if !fm.running {
		t.Fatal("user-submitted run should start")
	}
}

func TestBackgroundWakeDefersUnderModalUIWithoutBusyLoop(t *testing.T) {
	manager := agenttools.NewBackgroundManager()
	bufferBackgroundCompletion(t, manager)

	m := newBackgroundWakeTestModel(t, manager)
	m.modelPicker = &chatModelPicker{}
	updated, cmd := m.Update(chatBackgroundWakeMsg{})
	fm := updated.(chatModel)

	if fm.running {
		t.Fatal("wake under modal UI must not start a run")
	}
	if cmd == nil {
		t.Fatal("wake under modal UI should stay armed for the next change")
	}

	// The deferred watcher must block despite already-pending work: firing
	// immediately would re-enter this branch forever and burn the Update
	// pump for as long as the modal stays open. Other messages that ride
	// along with this update (the mouse-mode change the picker asks for) may
	// arrive right away, so only the wake message counts as a failure.
	msgCh := runChatCmd(cmd)
	quiet := time.After(100 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case msg := <-msgCh:
			if _, ok := msg.(chatBackgroundWakeMsg); ok {
				t.Fatal("deferred watcher fired on already-pending work; would busy-loop")
			}
		case <-quiet:
			waiting = false
		}
	}

	// A genuinely new event does wake it.
	if _, err := manager.Start("", "true"); err != nil {
		t.Fatalf("start background task: %v", err)
	}
	wake := time.After(5 * time.Second)
	for {
		select {
		case msg := <-msgCh:
			if _, ok := msg.(chatBackgroundWakeMsg); ok {
				return
			}
		case <-wake:
			t.Fatal("watcher missed the next completion")
		}
	}
}

// runChatCmd executes a command the way the event loop does: a batch is
// flattened and its commands run concurrently, each reporting its message.
func runChatCmd(cmd tea.Cmd) <-chan tea.Msg {
	out := make(chan tea.Msg, 8)
	go func() {
		msg := cmd()
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			out <- msg
			return
		}
		for _, inner := range batch {
			go func(c tea.Cmd) {
				if produced := c(); produced != nil {
					out <- produced
				}
			}(inner)
		}
	}()
	return out
}
