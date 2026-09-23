//go:build !windows

package tools

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
)

func shellToolName() string {
	return "bash"
}

func shellToolDescription() string {
	return "Execute a bash command on the system. Use this to inspect files, run tests, and perform development tasks. " +
		"Supports background=true for long-running commands (dev servers, builds): they write a log file and report completion in a [background task update] notice. " +
		"For recurring checks (poll a build, watch for a change), use the poll tool instead of a background sleep loop."
}

func shellCommandDescription() string {
	return "The bash command to execute."
}

func newBashCommand(ctx context.Context, command, cwdPath string) *exec.Cmd {
	script := command + "\n__ollama_status=$?\npwd -P > " + shellQuote(cwdPath) + "\nexit $__ollama_status"
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	configureBashCommand(cmd)
	return cmd
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func configureBashCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func runBashCommand(cmd *exec.Cmd) error {
	return cmd.Run()
}

func killBashCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return nil
}

// newBackgroundBashCommand builds a detached background command. Unlike
// newBashCommand it is NOT bound to the tool call's (bounded, cancelable)
// context — the process must outlive the call that launched it — and it
// skips the final-working-directory wrapper: background tasks do not move
// the session working directory.
func newBackgroundBashCommand(command string) *exec.Cmd {
	cmd := exec.Command("bash", "-c", command)
	configureBashCommand(cmd)
	return cmd
}

// newPollTickCommand builds a one-shot command for one poll tick. It IS
// bound to the tick's context (poll ticks must die on timeout/stop) and
// skips the working-directory wrapper for the same reason as background
// commands. Setpgid lets the caller's Cancel override group-kill the tick.
func newPollTickCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	configureBashCommand(cmd)
	return cmd
}

// startBackgroundCommand/waitBackgroundCommand split runBashCommand so the
// background manager can start the process and reap it in a goroutine.
func startBackgroundCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

func waitBackgroundCommand(cmd *exec.Cmd) error {
	return cmd.Wait()
}
