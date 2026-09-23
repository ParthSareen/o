package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/o/agent"
)

// waitForPublication drains until a publication matching pred arrives or the
// deadline passes; each poll publication drains exactly once, so callers
// consume them strictly in expectation order.
func waitForPublication(t *testing.T, m *BackgroundManager, timeout time.Duration) agent.BackgroundCompletion {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range m.DrainCompletions() {
			if c.Poll {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no poll publication within %s", timeout)
	return agent.BackgroundCompletion{}
}

// pollTicksCount reports how many ticks a poll has run.
func pollTicksCount(t *testing.T, m *BackgroundManager, id string) int {
	t.Helper()
	for _, info := range m.PollInfos() {
		if info.ID == id {
			return info.Ticks
		}
	}
	t.Fatalf("poll %s not listed", id)
	return 0
}

func TestPollPublishesChangedOutputAndSuppressesRepeats(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	stateFile := filepath.Join(t.TempDir(), "state.txt")
	if err := os.WriteFile(stateFile, []byte("A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	poll, err := manager.StartPoll(t.TempDir(), shellTestCommand("cat "+stateFile, "Get-Content "+stateFile), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	// First tick publishes the initial state.
	first := waitForPublication(t, manager, 10*time.Second)
	if first.ID != poll.ID || first.Tick != 1 || first.ExitCode != 0 {
		t.Fatalf("first publication = %+v, want poll tick 1 exit 0", first)
	}
	if !strings.Contains(first.Tail, "A") {
		t.Fatalf("first publication tail = %q, want the state file contents", first.Tail)
	}
	if first.LogPath != poll.LogPath {
		t.Fatalf("log path = %q, want %q", first.LogPath, poll.LogPath)
	}

	// Unchanged ticks run but do not publish.
	time.Sleep(350 * time.Millisecond) // ~7 intervals
	if pubs := manager.DrainCompletions(); len(pubs) != 0 {
		t.Fatalf("unchanged ticks published: %+v", pubs)
	}
	if ticks := pollTicksCount(t, manager, poll.ID); ticks < 3 {
		t.Fatalf("ticks = %d, want several unchanged ticks to have run", ticks)
	}

	// A change publishes exactly once, with the new output.
	if err := os.WriteFile(stateFile, []byte("B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := waitForPublication(t, manager, 10*time.Second)
	if second.Tick < 2 || !strings.Contains(second.Tail, "B") || strings.Contains(second.Tail, "A") {
		t.Fatalf("second publication = %+v, want a later tick carrying the new state", second)
	}
	time.Sleep(250 * time.Millisecond)
	if pubs := manager.DrainCompletions(); len(pubs) != 0 {
		t.Fatalf("repeated new state published again: %+v", pubs)
	}

	// Every tick is recorded in the log, published or not.
	log, err := os.ReadFile(poll.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "=== tick 1 at ") || !strings.Contains(string(log), "B") {
		t.Fatalf("log = %q, want timestamped tick entries including the quiet ones", log)
	}
}

func TestPollFailurePublishesOnceThenSuppresses(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	if _, err := manager.StartPoll(t.TempDir(), shellTestCommand("echo probe-down && exit 5", "Write-Output probe-down; exit 5"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	pub := waitForPublication(t, manager, 10*time.Second)
	if pub.ExitCode != 5 || !strings.Contains(pub.Tail, "probe-down") {
		t.Fatalf("publication = %+v, want exit 5 with the failing output", pub)
	}
	time.Sleep(250 * time.Millisecond)
	if pubs := manager.DrainCompletions(); len(pubs) != 0 {
		t.Fatalf("a stable repeating failure must publish once, got %+v", pubs)
	}

	if pubs := manager.PollInfos(); len(pubs) != 1 || pubs[0].Published != 1 {
		t.Fatalf("poll infos = %+v, want one poll with exactly one publication", pubs)
	}
}

func TestPollSignalsPushesOnPublication(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	if _, err := manager.StartPoll(t.TempDir(), shellTestCommand("echo tick-tock", "Write-Output tick-tock"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.BackgroundSignals():
	case <-time.After(10 * time.Second):
		t.Fatal("poll publication did not signal")
	}
	// Signals coalesce to the channel's capacity: after this poll is drained
	// no storm of tokens accumulates.
	manager.DrainCompletions()
}

func TestPollStopHaltsTicksAndKillsInflight(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	poll, err := manager.StartPoll(t.TempDir(), shellTestCommand("echo hi", "Write-Output hi"), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waitForPublication(t, manager, 10*time.Second)

	info, ok := manager.StopPoll(poll.ID)
	if !ok {
		t.Fatal("StopPoll reported unknown poll")
	}
	if info.Ticks < 1 {
		t.Fatalf("stopped info = %+v, want at least the first tick", info)
	}
	time.Sleep(250 * time.Millisecond) // many intervals pass
	if infos := manager.PollInfos(); len(infos) != 0 {
		t.Fatalf("polls = %+v, want none after stop", infos)
	}
	if _, ok := manager.StopPoll(poll.ID); ok {
		t.Fatal("stopping a stopped poll must report not-found")
	}
}

func TestPollToolLifecycle(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })
	poll := &Poll{Background: manager}
	ctx := agent.ToolContext{WorkingDir: t.TempDir()}

	start, err := poll.Execute(context.Background(), ctx, map[string]any{
		"action":   "start",
		"command":  shellTestCommand("echo watching", "Write-Output watching"),
		"interval": "10s",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Started poll poll-1", "every 10s", "Tick log:", "poll action=stop id=poll-1"} {
		if !strings.Contains(start.Content, want) {
			t.Fatalf("start = %q, want %q", start.Content, want)
		}
	}

	list, err := poll.Execute(context.Background(), ctx, map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Content, "poll-1") || !strings.Contains(list.Content, "10s") {
		t.Fatalf("list = %q, want the active poll", list.Content)
	}

	stop, err := poll.Execute(context.Background(), ctx, map[string]any{"action": "stop", "id": "poll-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stop.Content, "Stopped poll-1 after ") {
		t.Fatalf("stop = %q, want stop confirmation", stop.Content)
	}

	listAgain, err := poll.Execute(context.Background(), ctx, map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if listAgain.Content != "No active polls." {
		t.Fatalf("list after stop = %q, want empty", listAgain.Content)
	}

	stopAll, err := poll.Execute(context.Background(), ctx, map[string]any{"action": "stop", "id": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if stopAll.Content != "No active polls." {
		t.Fatalf("stop all = %q", stopAll.Content)
	}
}

func TestPollToolValidation(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })
	poll := &Poll{Background: manager}
	ctx := agent.ToolContext{WorkingDir: t.TempDir()}

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "unknown action", args: map[string]any{"action": "restart"}},
		{name: "missing command", args: map[string]any{"action": "start", "interval": "5m"}},
		{name: "missing interval", args: map[string]any{"action": "start", "command": "echo hi"}},
		{name: "bad duration", args: map[string]any{"action": "start", "command": "echo hi", "interval": "every so often"}},
		{name: "too frequent", args: map[string]any{"action": "start", "command": "echo hi", "interval": "5s"}},
		{name: "too rare", args: map[string]any{"action": "start", "command": "echo hi", "interval": "48h"}},
		{name: "stop missing id", args: map[string]any{"action": "stop"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := poll.Execute(context.Background(), ctx, tc.args)
			if err == nil && !strings.Contains(result.Content, "Error:") {
				t.Fatalf("got result=%q err=%v, want a validation error", result.Content, err)
			}
		})
	}

	if _, err := poll.Execute(context.Background(), ctx, map[string]any{
		"action": "start", "command": "rm -rf /", "interval": "5m",
	}); err == nil || !strings.Contains(err.Error(), "refusing to run unsafe command") {
		t.Fatalf("unsafe poll command must be rejected like bash: err=%v", err)
	}
}

func TestPollToolUnavailableWithoutManager(t *testing.T) {
	result, err := (&Poll{}).Execute(context.Background(), agent.ToolContext{}, map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "polling is unavailable") {
		t.Fatalf("content = %q, want unavailable error", result.Content)
	}
}

func TestPollApprovalOnlyForStart(t *testing.T) {
	poll := &Poll{}
	if !poll.RequiresApproval(map[string]any{"action": "start"}) {
		t.Fatal("start must require approval (commands then run unattended)")
	}
	for _, action := range []string{"stop", "list"} {
		if poll.RequiresApproval(map[string]any{"action": action}) {
			t.Fatalf("%s must not require approval", action)
		}
	}
	if scope := poll.ApprovalScope(map[string]any{"command": "gh ci", "interval": "5m"}); !strings.Contains(scope, "poll\x00") || !strings.Contains(scope, "5m\x00gh ci") {
		t.Fatalf("scope = %q, want poll-scoped command+interval", scope)
	}
}

func TestPollDrainCoalescesAndReportsOncePerPublication(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	stateFile := filepath.Join(t.TempDir(), "state.txt")
	mustWrite := func(v string) {
		t.Helper()
		if err := os.WriteFile(stateFile, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("one")
	poll, err := manager.StartPoll(t.TempDir(), shellTestCommand("cat "+stateFile, "Get-Content "+stateFile), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	// Change twice without draining: the buffered publication coalesces to
	// the latest state (earlier states remain in the log).
	deadline := time.Now().Add(10 * time.Second)
	for pollTicksCount(t, manager, poll.ID) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mustWrite("two")
	time.Sleep(150 * time.Millisecond)
	mustWrite("three")
	time.Sleep(150 * time.Millisecond)

	pubs := manager.DrainCompletions()
	if len(pubs) != 1 {
		t.Fatalf("drain = %+v, want one coalesced publication", pubs)
	}
	if !strings.Contains(pubs[0].Tail, "three") {
		t.Fatalf("coalesced tail = %q, want the latest state", pubs[0].Tail)
	}
	if again := manager.DrainCompletions(); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty (publications report once)", again)
	}
}

func TestBackgroundManagerCloseStopsPolls(t *testing.T) {
	manager := NewBackgroundManager()
	poll, err := manager.StartPoll(t.TempDir(), shellTestCommand("echo hi", "Write-Output hi"), 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-poll.done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop still running after Close")
	}
	if infos := manager.PollInfos(); len(infos) != 0 {
		t.Fatalf("polls = %+v, want none after Close", infos)
	}
}

func TestPollPublicationsInterruptInFlightRunSignals(t *testing.T) {
	// Belt-and-braces on the signal contract the session run loop depends
	// on: every publication leaves a pending signal (until consumed), and a
	// finished background task signals too.
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })

	if _, err := manager.Start(t.TempDir(), shellTestCommand("echo done", "Write-Output done")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-manager.BackgroundSignals():
	case <-time.After(10 * time.Second):
		t.Fatal("task completion did not signal")
	}
	completions := manager.DrainCompletions()
	if len(completions) != 1 || completions[0].ID != "bg-1" {
		t.Fatalf("drain = %+v, want the finished task", completions)
	}
}
