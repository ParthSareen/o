package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/ParthSareen/o/api"
)

// maxBackgroundContinuations caps how many times a single run can extend
// itself to report background task completions. Each completion is reported
// exactly once so continuations terminate on their own; the cap only guards
// against pathological crash-loops, and anything still buffered past the
// cap surfaces at the next run's start drain.
const maxBackgroundContinuations = 8

// injectPendingBackgroundEvents delivers background work that finished (or
// polls that published) while the run was in flight: when the source pushes
// a signal, buffered completions are drained into a synthetic user notice
// ahead of the next model request, so the agent reacts mid-run instead of
// only at run boundaries. Injection happens only before a model
// step: at that point the message tail is turn-complete and appending a user
// notice is protocol-safe — during tool execution the tail is an assistant
// message with unanswered tool calls. Signals are capacity-1 coalesced and
// never consumed without draining, so a signal observed during a non-model
// phase waits for the next model step.
func (r *run) injectPendingBackgroundEvents() {
	if r.phase != runPhaseModel || r.session.Background == nil {
		return
	}
	signaler, ok := r.session.Background.(BackgroundSignaler)
	if !ok {
		return
	}
	select {
	case <-signaler.BackgroundSignals():
	default:
		return
	}
	if notice := r.session.backgroundNotice(); notice != "" {
		r.messages = append(r.messages, api.Message{Role: "user", Content: notice})
		// Best-effort like the boundary drains: a dropped render must not
		// fail the run.
		_ = r.session.emit(newBackgroundTasks(newEventMetadata(r.runID, r.opts), notice))
	}
}

// backgroundNotice drains pending background task completions and renders
// them as one synthetic user message. Empty when no source is set or nothing
// finished. Completions are removed from the source by the drain, so each is
// reported exactly once.
func (s *Session) backgroundNotice() string {
	if s == nil || s.Background == nil {
		return ""
	}
	completions := s.Background.DrainCompletions()
	if len(completions) == 0 {
		return ""
	}
	return formatBackgroundNotice(completions)
}

// formatBackgroundNotice renders completion entries plus bounded failure
// tails as a system-attributed notice.
func formatBackgroundNotice(completions []BackgroundCompletion) string {
	var sb strings.Builder
	sb.WriteString("[background task update — system notice, not a user message]\n")
	for _, c := range completions {
		if c.Poll {
			formatPollNotice(&sb, c)
			continue
		}
		var status string
		switch {
		case c.Killed:
			status = "killed"
		case c.Failure != "":
			status = "failed to run: " + c.Failure
		case c.ExitCode == 0:
			status = "finished: exit 0"
		default:
			status = fmt.Sprintf("failed: exit %d", c.ExitCode)
		}
		fmt.Fprintf(&sb, "\n%s: %s after %s — %q\n  log: %s\n",
			c.ID, status, formatBackgroundTaskDuration(c.Duration), backgroundCommandSummary(c.Command), c.LogPath)
		if c.SessionID != "" {
			fmt.Fprintf(&sb, "  o session %s: after the child exits, follow up with o --resume-id %s --headless --allow-all-tools \"follow-up\"\n", c.SessionID, c.SessionID)
		}
		if tail := strings.TrimRight(c.Tail, "\n"); tail != "" {
			sb.WriteString("  log tail:\n")
			for _, line := range strings.Split(tail, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatPollNotice(sb *strings.Builder, c BackgroundCompletion) {
	var status string
	switch {
	case c.Failure != "":
		status = "tick failed: " + c.Failure
	case c.ExitCode != 0:
		status = fmt.Sprintf("tick command exited %d", c.ExitCode)
	default:
		status = "new output"
	}
	fmt.Fprintf(sb, "\n%s tick %d: %s after %s — %q\n  log: %s\n",
		c.ID, c.Tick, status, formatBackgroundTaskDuration(c.Duration), backgroundCommandSummary(c.Command), c.LogPath)
	if output := strings.TrimRight(c.Tail, "\n"); output != "" {
		sb.WriteString("  output:\n")
		for _, line := range strings.Split(output, "\n") {
			sb.WriteString("    " + line + "\n")
		}
	}
}

func backgroundCommandSummary(command string) string {
	summary := strings.Join(strings.Fields(command), " ")
	runes := []rune(summary)
	if len(runes) > 60 {
		summary = string(runes[:60]) + "…"
	}
	return summary
}

func formatBackgroundTaskDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
