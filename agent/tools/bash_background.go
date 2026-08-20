package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ParthSareen/o/agent"
)

const (
	// backgroundNoticeTailBytes bounds the trailing log excerpt attached to
	// failure notices; the full log stays on disk for follow-up reads.
	backgroundNoticeTailBytes = 2_000
	// backgroundKillTimeout bounds how long Close waits for killed
	// processes to exit before giving up.
	backgroundKillTimeout = 5 * time.Second
)

// BackgroundManager owns background shell processes spawned via the shell
// tool's background=true flag. Each task gets an ID (bg-N), a combined
// stdout/stderr log, and an exit-status record written when the process
// exits, all under a per-process temp dir. The agent inspects progress with
// ordinary foreground commands (tail/grep the log, cat the exit record);
// completions are reported to the session via DrainCompletions, which the
// run loop turns into synthetic notices.
//
// TODO(lifecycle): Close is not yet wired into session/process teardown —
// registries are built in agentToolsRegistry with no destruction hook. Until
// then, tasks orphaned at process exit keep running on Unix; on Windows the
// job object's KILL_ON_JOB_CLOSE handles it. Wire Close into headless/TUI/
// pipe teardown as a follow-up.
type BackgroundManager struct {
	mu      sync.Mutex
	tasks   map[string]*BackgroundTask
	nextID  int
	dirOnce sync.Once
	dir     string
	dirErr  error
}

// BackgroundTask is one backgrounded shell command.
type BackgroundTask struct {
	ID        string
	Command   string
	PID       int
	StartedAt time.Time
	// EndedAt, exitCode, and err are written before done closes; readers may
	// only touch them after finished() reports true (channel close
	// establishes happens-before).
	EndedAt  time.Time
	exitCode int
	err      error
	// LogPath is the combined stdout/stderr log; ExitPath holds the
	// one-line exit record ("exit N" | "killed" | "error: ...") written when
	// the process exits.
	LogPath  string
	ExitPath string

	done   chan struct{} // closed when the process exits and records are written
	killed atomic.Bool
	cmd    *exec.Cmd
	// notified is set once the completion has been drained into a session
	// notice, so each completion is reported exactly once. Guarded by
	// BackgroundManager.mu.
	notified bool
}

func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{tasks: make(map[string]*BackgroundTask)}
}

// ensureDir lazily creates the temp log directory on first use so a process
// that never runs background commands leaves nothing behind.
func (m *BackgroundManager) ensureDir() (string, error) {
	m.dirOnce.Do(func() {
		m.dir, m.dirErr = os.MkdirTemp("", "o-agent-bg-*")
	})
	return m.dir, m.dirErr
}

// Start launches command detached from the calling tool call's context: the
// process must outlive the bounded, cancelable tool call that spawned it,
// so the platform constructor takes no context.
func (m *BackgroundManager) Start(workingDir, command string) (*BackgroundTask, error) {
	dir, err := m.ensureDir()
	if err != nil {
		return nil, fmt.Errorf("creating background log directory: %w", err)
	}

	m.mu.Lock()
	m.nextID++
	task := &BackgroundTask{
		ID:        fmt.Sprintf("bg-%d", m.nextID),
		Command:   command,
		StartedAt: time.Now(),
		LogPath:   filepath.Join(dir, fmt.Sprintf("bg-%d.log", m.nextID)),
		ExitPath:  filepath.Join(dir, fmt.Sprintf("bg-%d.exit", m.nextID)),
		done:      make(chan struct{}),
	}
	m.mu.Unlock()

	logFile, err := os.OpenFile(task.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating background log: %w", err)
	}

	cmd := newBackgroundBashCommand(command)
	// Passing *os.File (not pipes) means the child inherits the fd and the
	// kernel serializes combined-stream writes: no copy goroutines, and no
	// WaitDelay pipe leak for commands that spawn grandchildren inheriting
	// stdout.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if workingDir != "" {
		cmd.Dir = workingDir
	}

	// Register even failed starts so the failure surfaces in the completion
	// drain instead of vanishing.
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.mu.Unlock()

	if err := startBackgroundCommand(cmd); err != nil {
		task.err = err
		task.EndedAt = time.Now()
		_ = os.WriteFile(task.ExitPath, []byte("error: "+err.Error()+"\n"), 0o600)
		_ = logFile.Close()
		close(task.done)
		return nil, err
	}
	task.cmd = cmd
	task.PID = cmd.Process.Pid

	go m.wait(task, logFile)
	return task, nil
}

func (m *BackgroundManager) wait(task *BackgroundTask, logFile *os.File) {
	err := waitBackgroundCommand(task.cmd)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		task.exitCode = exitErr.ExitCode()
	default:
		task.err = err
	}

	status := fmt.Sprintf("exit %d\n", task.exitCode)
	switch {
	case task.killed.Load():
		status = "killed\n"
	case task.err != nil:
		status = "error: " + task.err.Error() + "\n"
	}
	// Best effort: the in-memory state still drives notices if this fails.
	_ = os.WriteFile(task.ExitPath, []byte(status), 0o600)

	_ = logFile.Close()
	task.EndedAt = time.Now()
	close(task.done)
}

func (t *BackgroundTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// DrainCompletions implements agent.BackgroundSource: finished tasks not yet
// reported, marked reported as a side effect.
func (m *BackgroundManager) DrainCompletions() []agent.BackgroundCompletion {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	var drained []*BackgroundTask
	for _, task := range m.tasks {
		if task.notified || !task.finished() {
			continue
		}
		task.notified = true
		drained = append(drained, task)
	}
	m.mu.Unlock()

	sort.Slice(drained, func(i, j int) bool { return drained[i].ID < drained[j].ID })
	completions := make([]agent.BackgroundCompletion, 0, len(drained))
	for _, task := range drained {
		completion := agent.BackgroundCompletion{
			ID:       task.ID,
			Command:  task.Command,
			ExitCode: task.exitCode,
			Killed:   task.killed.Load(),
			Duration: task.EndedAt.Sub(task.StartedAt),
			LogPath:  task.LogPath,
		}
		if task.err != nil {
			completion.Failure = task.err.Error()
		}
		// Attach a bounded log tail only for failures: a notice should carry
		// enough context to act on without a follow-up read, but successes
		// and deliberate kills stay one line.
		if !completion.Killed && (completion.Failure != "" || completion.ExitCode != 0) {
			if tail, _, err := readLogTail(task.LogPath, backgroundNoticeTailBytes); err == nil {
				completion.Tail = tail
			}
		}
		completions = append(completions, completion)
	}
	return completions
}

// Close kills all still-running tasks and removes the log directory.
func (m *BackgroundManager) Close() error {
	m.mu.Lock()
	var running []*BackgroundTask
	for _, task := range m.tasks {
		if !task.finished() {
			running = append(running, task)
		}
	}
	m.mu.Unlock()

	for _, task := range running {
		task.killed.Store(true)
		if task.cmd != nil {
			_ = killBashCommand(task.cmd)
		}
	}
	deadline := time.Now().Add(backgroundKillTimeout)
	for _, task := range running {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-task.done:
			timer.Stop()
		case <-timer.C:
		}
	}
	m.mu.Lock()
	dir := m.dir
	m.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// readLogTail returns up to the last maxBytes of the log plus its total
// size, trimming partial UTF-8 off the front of the window.
func readLogTail(path string, maxBytes int) (tail string, total int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	total = info.Size()
	start := int64(0)
	if total > int64(maxBytes) {
		start = total - int64(maxBytes)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", total, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", total, err
	}
	if start > 0 {
		data = utf8SafeSuffix(data)
	}
	return string(data), total, nil
}

// utf8SafeSuffix skips leading continuation bytes so a tail window never
// starts mid-rune.
func utf8SafeSuffix(p []byte) []byte {
	for i := 0; i < len(p); i++ {
		if p[i]&0xC0 != 0x80 {
			return p[i:]
		}
	}
	return p[:0]
}
