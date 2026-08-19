package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ParthSareen/o/agent"
)

// waitForTask polls until the task finishes or the deadline passes.
func waitForTask(t *testing.T, task *BackgroundTask, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !task.finished() {
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not finish within %s", task.ID, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBashBackgroundLaunchLifecycle(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })
	bash := &Bash{Background: manager}
	workdir := t.TempDir()

	result, err := bash.Execute(context.Background(), agent.ToolContext{WorkingDir: workdir}, map[string]any{
		"command":    shellTestCommand("echo hello-bg && pwd", "Write-Output hello-bg; Get-Location"),
		"background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Started background task bg-1") {
		t.Fatalf("content = %q, want launch confirmation with task id", result.Content)
	}
	if result.WorkingDir != "" {
		t.Fatalf("background launch must not move the session working dir, got %q", result.WorkingDir)
	}

	manager.mu.Lock()
	task := manager.tasks["bg-1"]
	manager.mu.Unlock()
	if task == nil {
		t.Fatal("task bg-1 not tracked")
	}
	for _, want := range []string{"Output log (combined stdout/stderr): ", "Exit status record (written when it exits): ", task.LogPath, task.ExitPath, "pid "} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content = %q, want %q", result.Content, want)
		}
	}

	waitForTask(t, task, 30*time.Second)

	log, err := os.ReadFile(task.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "hello-bg") {
		t.Fatalf("log = %q, want command output", log)
	}
	exitRecord, err := os.ReadFile(task.ExitPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(exitRecord)) != "exit 0" {
		t.Fatalf("exit record = %q, want exit 0", exitRecord)
	}
	// The background shell must not cd-track; the log's pwd (if any) is the
	// launch working dir.
	if !strings.Contains(string(log), filepath.Base(workdir)) {
		t.Fatalf("log = %q, want launch working dir %q", log, workdir)
	}

	drains := manager.DrainCompletions()
	if len(drains) != 1 || drains[0].ID != "bg-1" || drains[0].ExitCode != 0 {
		t.Fatalf("drain = %+v, want single exit-0 completion for bg-1", drains)
	}
	if drains[0].Tail != "" {
		t.Fatalf("success tail = %q, want empty (tails are for failures)", drains[0].Tail)
	}
	if again := manager.DrainCompletions(); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty (completions report once)", again)
	}
}

func TestBashBackgroundFailureCarriesTail(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })
	bash := &Bash{Background: manager}

	result, err := bash.Execute(context.Background(), agent.ToolContext{WorkingDir: t.TempDir()}, map[string]any{
		"command":    shellTestCommand("echo boom-output && exit 3", "Write-Output boom-output; exit 3"),
		"background": "true", // tolerate the string form models emit
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = result

	manager.mu.Lock()
	task := manager.tasks["bg-1"]
	manager.mu.Unlock()
	waitForTask(t, task, 30*time.Second)

	drains := manager.DrainCompletions()
	if len(drains) != 1 {
		t.Fatalf("drain = %+v, want one completion", drains)
	}
	c := drains[0]
	if c.ExitCode != 3 || c.Killed || c.Failure != "" {
		t.Fatalf("completion = %+v, want exit 3", c)
	}
	if !strings.Contains(c.Tail, "boom-output") {
		t.Fatalf("failure tail = %q, want command output", c.Tail)
	}
}

func TestBashBackgroundLaunchWithoutManager(t *testing.T) {
	result, err := (&Bash{}).Execute(context.Background(), agent.ToolContext{WorkingDir: t.TempDir()}, map[string]any{
		"command":    "echo hi",
		"background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Error: background execution is unavailable") {
		t.Fatalf("content = %q, want unavailable error", result.Content)
	}
}

func TestBashBackgroundRejectsUnsafeCommand(t *testing.T) {
	manager := NewBackgroundManager()
	t.Cleanup(func() { _ = manager.Close() })
	_, err := (&Bash{Background: manager}).Execute(context.Background(), agent.ToolContext{WorkingDir: t.TempDir()}, map[string]any{
		"command":    "rm -rf /",
		"background": true,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to run unsafe command") {
		t.Fatalf("err = %v, want unsafe command rejection before launch", err)
	}
}

func TestBackgroundManagerCloseKillsRunningTasks(t *testing.T) {
	manager := NewBackgroundManager()
	dir, err := manager.ensureDir()
	if err != nil {
		t.Fatal(err)
	}

	_, err = manager.Start(t.TempDir(), shellTestCommand("sleep 60", "Start-Sleep -Seconds 60"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	task := manager.tasks["bg-1"]
	manager.mu.Unlock()
	if !task.finished() {
		t.Fatal("task still running after Close")
	}
	drains := manager.DrainCompletions()
	if len(drains) != 1 || !drains[0].Killed {
		t.Fatalf("drain = %+v, want one killed completion", drains)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("log dir stat err = %v, want removed after Close", err)
	}
}

func TestReadLogTailTrimsToWindowAndUTF8Boundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	// Place a two-byte rune so the tail window starts ON its continuation
	// byte: 100 x's + é (bytes 100-101) + 63 y's = 165 bytes; a 64-byte
	// window starts at 101, mid-rune. The rune must be dropped, not mangled.
	payload := strings.Repeat("x", 100) + "é" + strings.Repeat("y", 63)
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, total, err := readLogTail(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("total = %d, want %d", total, len(payload))
	}
	if !utf8.ValidString(tail) || strings.ContainsRune(tail, 'é') {
		t.Fatalf("tail = %q, want valid UTF-8 with the straddling rune dropped", tail)
	}
	if want := strings.Repeat("y", 63); tail != want {
		t.Fatalf("tail = %q, want %q", tail, want)
	}
}
