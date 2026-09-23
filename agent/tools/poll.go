package tools

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

const (
	// minPollInterval / maxPollInterval bound how often a poll may fire.
	// The floor keeps a poll from spamming the conversation; polls are for
	// periodic checks, not busy loops.
	minPollInterval = 10 * time.Second
	maxPollInterval = 24 * time.Hour
	// pollTickTimeoutMin/Max bound each one-shot tick run; the effective
	// budget is the interval clamped into this range so a hung tick dies
	// instead of stacking up.
	pollTickTimeoutMin = 5 * time.Second
	pollTickTimeoutMax = 2 * time.Minute
	// maxPolls caps concurrent polls per session.
	maxPolls = 32
	// pollPublicationBytes bounds the output excerpt attached to a poll
	// notice; the full tick record stays in the poll's log file.
	pollPublicationBytes = 4_000
	// pollStopTimeout bounds how long StopPoll/Close wait for a tick loop to
	// halt (its in-flight kill makes this fast; the cap is a backstop).
	pollStopTimeout = 5 * time.Second
)

// BackgroundPoll is one recurring shell command owned by BackgroundManager.
// Unlike a background task it never long-runs: every Interval the manager
// re-executes the command once (bounded by a per-tick timeout) and publishes
// a completion when the tick's stdout or exit status changed since the
// previous tick, so the agent gets new information delivered instead of
// babysitting a sleep loop.
type BackgroundPoll struct {
	ID        string
	Command   string
	Interval  time.Duration
	StartedAt time.Time
	// LogPath is the append-only tick log; every tick writes a timestamped
	// entry regardless of whether it published.
	LogPath string

	// Mutable state below is guarded by BackgroundManager.mu.
	ticks     int
	published int
	// lastFingerprint/baseline implement change suppression: a tick
	// publishes only when its fingerprint (stdout + exit status + run error)
	// differs from the previous tick's. baseline is false until the first
	// tick records one.
	lastFingerprint uint64
	baseline        bool
	stopped         bool
	// cancel kills an in-flight tick process (set per tick).
	cancel context.CancelFunc

	stop       chan struct{} // closed to halt the tick loop
	done       chan struct{} // closed when the tick loop has exited
	workingDir string
}

// pollPublication is one buffered, undrained poll tick result.
type pollPublication struct {
	pollID   string
	command  string
	tick     int
	exitCode int
	duration time.Duration
	output   string
	failure  string
	logPath  string
}

// pollTickResult is the raw outcome of one tick execution.
type pollTickResult struct {
	stdout   string
	stderr   string
	exitCode int
	failure  string
	duration time.Duration
}

func (r pollTickResult) fingerprint() uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(r.stdout))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(r.exitCode)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(r.failure))
	return h.Sum64()
}

// StartPoll launches a recurring poll. The first tick runs immediately and
// establishes the output baseline so steady-state commands stay quiet.
// Validation (interval bounds, poll cap) is the Poll tool's job; StartPoll
// itself only requires a positive interval and a non-empty command.
func (m *BackgroundManager) StartPoll(workingDir, command string, interval time.Duration) (*BackgroundPoll, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("poll command must not be empty")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("poll interval must be positive, got %s", interval)
	}
	dir, err := m.ensureDir()
	if err != nil {
		return nil, fmt.Errorf("creating background log directory: %w", err)
	}

	m.mu.Lock()
	m.nextPollID++
	poll := &BackgroundPoll{
		ID:         fmt.Sprintf("poll-%d", m.nextPollID),
		Command:    command,
		Interval:   interval,
		StartedAt:  time.Now(),
		LogPath:    dir + string(os.PathSeparator) + fmt.Sprintf("poll-%d.log", m.nextPollID),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		workingDir: workingDir,
	}
	m.polls[poll.ID] = poll
	m.mu.Unlock()

	go m.runPoll(poll)
	return poll, nil
}

// runPoll is the per-poll tick loop: tick immediately, then every interval,
// until stopped. Ticks are one-shot and bounded, so a stopped poll dies
// quickly even mid-tick (the tick process is killed).
func (m *BackgroundManager) runPoll(p *BackgroundPoll) {
	defer close(p.done)
	for {
		m.tick(p)
		timer := time.NewTimer(p.Interval)
		select {
		case <-timer.C:
		case <-p.stop:
			timer.Stop()
			return
		}
	}
}

// tick executes one bounded run of the poll command, appends it to the log,
// and publishes when the result changed. The process runs detached from any
// tool-call context; the timeout and Stop's cancel are what keep it finite.
func (m *BackgroundManager) tick(p *BackgroundPoll) {
	timeout := pollTickTimeout(p.Interval)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	m.mu.Lock()
	if p.stopped {
		m.mu.Unlock()
		cancel()
		return
	}
	p.cancel = cancel
	tickNo := p.ticks + 1
	m.mu.Unlock()

	started := time.Now()
	result := runPollTick(ctx, p.workingDir, p.Command)
	result.duration = time.Since(started)
	// cancel before bookkeeping: Stop may already have fired it; either way
	// the tick's context is done.
	cancel()

	m.mu.Lock()
	if p.stopped {
		// Stopped mid-tick (Stop cancels the in-flight run): drop the result
		// so a dying poll never interrupts the agent.
		m.mu.Unlock()
		return
	}
	p.ticks++
	p.cancel = nil
	fp := result.fingerprint()
	changed := !p.baseline || fp != p.lastFingerprint
	p.baseline = true
	p.lastFingerprint = fp
	m.appendPollTickLogLocked(p, tickNo, result)

	// Publish only on change AND when there is something worth saying:
	// new stdout, or a non-zero/failed run. An unchanged or empty steady
	// state stays silent — that is the anti-spam contract documented on the
	// poll tool.
	publish := changed && (result.stdout != "" || result.exitCode != 0 || result.failure != "")
	if publish {
		p.published++
		output := result.stdout
		if output == "" {
			output = result.stderr
		}
		m.pollPubs[p.ID] = pollPublication{
			pollID:   p.ID,
			command:  p.Command,
			tick:     tickNo,
			exitCode: result.exitCode,
			duration: result.duration,
			output:   truncatePollOutput(output),
			failure:  result.failure,
			logPath:  p.LogPath,
		}
	}
	m.mu.Unlock()
	if publish {
		m.ping()
	}
}

// appendPollTickLogLocked records every tick (published or not) so the agent
// can audit history with ordinary foreground commands. Best effort: the
// in-memory publication still delivers if the write fails. Caller holds mu.
func (m *BackgroundManager) appendPollTickLogLocked(p *BackgroundPoll, tickNo int, result pollTickResult) {
	f, err := os.OpenFile(p.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	status := fmt.Sprintf("tick %d at %s (%s, exit %d)", tickNo, time.Now().Format(time.RFC3339), result.duration.Round(10*time.Millisecond), result.exitCode)
	if result.failure != "" {
		status += ", " + result.failure
	}
	var sb strings.Builder
	sb.WriteString("=== " + status + " ===\n")
	if result.stdout != "" {
		sb.WriteString(result.stdout)
		if !strings.HasSuffix(result.stdout, "\n") {
			sb.WriteString("\n")
		}
	}
	if result.stderr != "" {
		sb.WriteString("stderr:\n")
		sb.WriteString(result.stderr)
		if !strings.HasSuffix(result.stderr, "\n") {
			sb.WriteString("\n")
		}
	}
	_, _ = f.WriteString(sb.String())
}

// runPollTick runs one bounded, cancelable one-shot command — the same
// discipline as a foreground bash call (timeout, group-kill on cancel,
// bounded output) minus the working-directory capture: ticks inherit the
// poll's launch directory and must never move the session.
func runPollTick(ctx context.Context, workingDir, command string) pollTickResult {
	budget := ""
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline).Round(time.Second).String()
	}
	cmd := newPollTickCommand(ctx, command)
	cmd.WaitDelay = bashWaitDelay
	cmd.Cancel = func() error {
		return killBashCommand(cmd)
	}
	if workingDir != "" {
		cmd.Dir = workingDir
	}

	var stdout, stderr boundedOutput
	stdout.Limit = maxBashOutputBytes
	stderr.Limit = maxBashOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := runBashCommand(cmd)
	result := pollTickResult{
		stdout: stdout.String("stdout"),
		stderr: stderr.String("stderr"),
	}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.exitCode = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay):
		result.failure = fmt.Sprintf("tick output pipes did not close after %s", bashWaitDelay)
	case ctx.Err() == context.DeadlineExceeded:
		if budget != "" {
			result.failure = "tick timed out after " + budget
		} else {
			result.failure = "tick timed out"
		}
	default:
		result.failure = err.Error()
	}
	return result
}

func pollTickTimeout(interval time.Duration) time.Duration {
	switch {
	case interval < pollTickTimeoutMin:
		return pollTickTimeoutMin
	case interval > pollTickTimeoutMax:
		return pollTickTimeoutMax
	default:
		return interval
	}
}

func truncatePollOutput(output string) string {
	runes := []rune(output)
	if len(runes) <= pollPublicationBytes {
		return output
	}
	return string(runes[:pollPublicationBytes]) + fmt.Sprintf("\n[truncated: ~%d more tokens in the poll log]", agent.ApproximateTokens(len(runes)-pollPublicationBytes))
}

// stopPollLocked halts a poll: close its stop channel and kill any in-flight
// tick. Caller holds mu. Idempotent.
func (m *BackgroundManager) stopPollLocked(p *BackgroundPoll) {
	if p.stopped {
		return
	}
	p.stopped = true
	close(p.stop)
	if p.cancel != nil {
		p.cancel()
	}
}

// waitPollStopped blocks until the poll's tick loop exits or the timeout
// passes (an in-flight tick is killed on stop, so this is fast in practice).
func (m *BackgroundManager) waitPollStopped(p *BackgroundPoll) {
	timer := time.NewTimer(pollStopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	}
}

// BackgroundPollInfo is a point-in-time listing snapshot for the poll tool.
type BackgroundPollInfo struct {
	ID        string
	Command   string
	Interval  time.Duration
	StartedAt time.Time
	Ticks     int
	Published int
	LogPath   string
}

// PollInfos lists active polls sorted by ID.
func (m *BackgroundManager) PollInfos() []BackgroundPollInfo {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]BackgroundPollInfo, 0, len(m.polls))
	for _, p := range m.polls {
		infos = append(infos, BackgroundPollInfo{
			ID:        p.ID,
			Command:   p.Command,
			Interval:  p.Interval,
			StartedAt: p.StartedAt,
			Ticks:     p.ticks,
			Published: p.published,
			LogPath:   p.LogPath,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

// StopPoll halts one poll by ID, waiting for its tick loop to exit. The
// poll's buffered (undrained) publications are kept: stopping silences the
// future, not the past.
func (m *BackgroundManager) StopPoll(id string) (BackgroundPollInfo, bool) {
	m.mu.Lock()
	p, ok := m.polls[id]
	if ok {
		delete(m.polls, id)
		m.stopPollLocked(p)
	}
	m.mu.Unlock()
	if !ok {
		return BackgroundPollInfo{}, false
	}
	m.waitPollStopped(p)
	return BackgroundPollInfo{
		ID:        p.ID,
		Command:   p.Command,
		Interval:  p.Interval,
		StartedAt: p.StartedAt,
		Ticks:     p.ticks,
		Published: p.published,
		LogPath:   p.LogPath,
	}, true
}

// stopAllPolls halts every active poll; called by Close.
func (m *BackgroundManager) stopAllPolls() {
	m.mu.Lock()
	polls := make([]*BackgroundPoll, 0, len(m.polls))
	for id, p := range m.polls {
		delete(m.polls, id)
		polls = append(polls, p)
		m.stopPollLocked(p)
	}
	m.mu.Unlock()
	for _, p := range polls {
		m.waitPollStopped(p)
	}
}

// Poll runs a shell command on a recurring interval, pushing new output to
// the agent as it happens. Background is the owning manager (shared with the
// shell tool's background tasks); nil means polling is disabled.
type Poll struct {
	Background *BackgroundManager
}

func (p *Poll) Name() string {
	return "poll"
}

func (p *Poll) Description() string {
	return "Run a shell command repeatedly on an interval and get new results pushed to you automatically — a recurring check instead of " +
		"a long-lived background task. Each tick runs the command once with a bounded timeout; when a tick's stdout or exit status differs " +
		"from the previous tick, the new output interrupts your current work as a [background task update] notice so you can react " +
		"immediately, even mid-task. Unchanged output is suppressed, so write commands that print a stable, meaningful snapshot " +
		"(CI status, deployment state, file existence, queue depth) rather than timestamps or counters that always differ. " +
		"Use this for watch-style work (poll a build, wait for a deploy, watch for a file) instead of the shell tool's background=true " +
		"with a sleep loop, which would run forever without ever reporting. Polls stop when the session ends."
}

func (p *Poll) Schema() api.ToolFunction {
	props := api.NewToolPropertiesMap()
	props.Set("action", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "One of: start (requires command and interval), stop (requires id; \"all\" stops every poll), list.",
		Enum:        []any{"start", "stop", "list"},
	})
	props.Set("command", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "The shell command each tick runs; required for action=start.",
	})
	props.Set("interval", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "How often the command runs, as a Go duration like \"30s\", \"5m\", or \"1h\"; required for action=start. Must be between 10s and 24h.",
	})
	props.Set("id", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "The poll ID (e.g. \"poll-1\", shown at start and by list); required for action=stop.",
	})
	return api.ToolFunction{
		Name:        p.Name(),
		Description: p.Description(),
		Parameters: api.ToolFunctionParameters{
			Type:       "object",
			Properties: props,
			Required:   []string{"action"},
		},
	}
}

// RequiresApproval gates action=start: starting a poll means the command
// runs unattended once per interval, so the approval at start is the
// security gate (like background=true); ticks never re-prompt. stop and list
// are safe.
func (p *Poll) RequiresApproval(args map[string]any) bool {
	action, _ := args["action"].(string)
	return strings.EqualFold(strings.TrimSpace(action), "start")
}

// ApprovalScope scopes poll approval to the exact command plus interval, so
// "always allow" applies only to rerunning that same watch (same NUL
// separator convention as the shell tool).
func (p *Poll) ApprovalScope(args map[string]any) string {
	command, _ := args["command"].(string)
	interval, _ := args["interval"].(string)
	return p.Name() + "\x00" + strings.TrimSpace(interval) + "\x00" + strings.TrimSpace(command)
}

func (p *Poll) Execute(_ context.Context, toolCtx agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if p.Background == nil {
		return agent.ToolResult{Content: "Error: polling is unavailable in this session (disabled via OLLAMA_AGENT_DISABLE_BACKGROUND_SHELL)."}, nil
	}
	switch action {
	case "start":
		return p.start(toolCtx, args)
	case "stop":
		return p.stop(args)
	case "list":
		return p.list(), nil
	default:
		return agent.ToolResult{Content: `Error: action must be one of "start", "stop", or "list".`}, nil
	}
}

func (p *Poll) start(toolCtx agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return agent.ToolResult{}, fmt.Errorf("command parameter is required for action=start")
	}
	if err := rejectUnsafeShellCommand(command); err != nil {
		return agent.ToolResult{}, err
	}
	intervalRaw, _ := args["interval"].(string)
	intervalRaw = strings.TrimSpace(intervalRaw)
	if intervalRaw == "" {
		return agent.ToolResult{}, fmt.Errorf(`interval parameter is required for action=start (e.g. "30s", "5m", "1h")`)
	}
	interval, err := time.ParseDuration(intervalRaw)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("invalid interval %q: %v (use a duration like \"30s\", \"5m\", \"1h\")", intervalRaw, err)
	}
	if interval < minPollInterval || interval > maxPollInterval {
		return agent.ToolResult{}, fmt.Errorf("interval must be between %s and %s, got %s", minPollInterval, maxPollInterval, interval)
	}
	if len(p.Background.PollInfos()) >= maxPolls {
		return agent.ToolResult{Content: fmt.Sprintf("Error: at most %d polls can be active at once; stop one with action=stop first (see action=list).", maxPolls)}, nil
	}

	poll, err := p.Background.StartPoll(toolCtx.WorkingDir, command, interval)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: could not start poll: %v", err)}, nil
	}
	workingDir := toolCtx.WorkingDir
	if workingDir == "" {
		workingDir = "(process default)"
	}
	return agent.ToolResult{Content: fmt.Sprintf(
		"Started poll %s: runs every %s (per-tick timeout %s), first tick already running.\n"+
			"Command: %s\n"+
			"Working directory: %s\n"+
			"Tick log: %s\n\n"+
			"The command re-runs on every interval. Whenever a tick's output differs from the previous tick — or the command starts failing — "+
			"you get an automatic [background task update] notice with the new output, including while you are mid-task. Identical output is "+
			"suppressed, so make the command print only a meaningful snapshot. You can always inspect history with a foreground command on the "+
			"tick log. Stop it with poll action=stop id=%s.",
		poll.ID, poll.Interval, pollTickTimeout(poll.Interval), command, workingDir, poll.LogPath, poll.ID,
	)}, nil
}

func (p *Poll) stop(args map[string]any) (agent.ToolResult, error) {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return agent.ToolResult{}, fmt.Errorf("id parameter is required for action=stop (or \"all\")")
	}
	if strings.EqualFold(id, "all") {
		infos := p.Background.PollInfos()
		if len(infos) == 0 {
			return agent.ToolResult{Content: "No active polls."}, nil
		}
		for _, info := range infos {
			_, _ = p.Background.StopPoll(info.ID)
		}
		return agent.ToolResult{Content: fmt.Sprintf("Stopped %d poll(s).", len(infos))}, nil
	}
	info, ok := p.Background.StopPoll(id)
	if !ok {
		return agent.ToolResult{Content: fmt.Sprintf("Error: no active poll with ID %q. Use action=list to see active polls.", id)}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf(
		"Stopped %s after %d ticks (%d published). Any result it published before stopping is still delivered. Tick log: %s",
		info.ID, info.Ticks, info.Published, info.LogPath,
	)}, nil
}

func (p *Poll) list() agent.ToolResult {
	infos := p.Background.PollInfos()
	if len(infos) == 0 {
		return agent.ToolResult{Content: "No active polls."}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d active poll(s):\n", len(infos))
	for _, info := range infos {
		fmt.Fprintf(&sb, "%s: every %s, %d ticks (%d published) — %q\n  tick log: %s\n",
			info.ID, info.Interval, info.Ticks, info.Published, info.Command, info.LogPath)
	}
	return agent.ToolResult{Content: strings.TrimRight(sb.String(), "\n")}
}
