package chat

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// chatDiffViewerClosedMsg reports the nvim diff viewer returning control.
type chatDiffViewerClosedMsg struct {
	err error
}

var diffViewerLookPath = exec.LookPath

// buildDiffViewerCmd returns the process that opens the user's nvim diff
// viewer. Default: `nvim -c DiffviewOpen` in the working directory; O_NVIM_DIFF
// overrides the whole command (run through the shell).
func buildDiffViewerCmd(workingDir string) (*exec.Cmd, error) {
	if override := strings.TrimSpace(os.Getenv("O_NVIM_DIFF")); override != "" {
		cmd := exec.Command("sh", "-c", override)
		cmd.Dir = workingDir
		return cmd, nil
	}
	if _, err := diffViewerLookPath("nvim"); err != nil {
		return nil, fmt.Errorf("nvim not found (set O_NVIM_DIFF to override the diff command): %w", err)
	}
	cmd := exec.Command("nvim", "-c", "DiffviewOpen")
	cmd.Dir = workingDir
	return cmd, nil
}

// openDiffViewer suspends the TUI and opens the nvim diff viewer (ctrl+g). It
// is also reachable through the hidden /diffview command.
func (m chatModel) openDiffViewer() (tea.Model, tea.Cmd) {
	dir := m.currentWorkingDir()
	if strings.TrimSpace(dir) == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			m.status = "diffview failed: " + err.Error()
			return m, nil
		}
	}
	execCmd, err := buildDiffViewerCmd(dir)
	if err != nil {
		m.status = "diffview failed: " + err.Error()
		return m, nil
	}
	m.status = "diffview"
	return m, tea.ExecProcess(execCmd, func(err error) tea.Msg {
		return chatDiffViewerClosedMsg{err: err}
	})
}

// chatEditorClosedMsg reports the plain nvim editor returning control.
type chatEditorClosedMsg struct {
	err error
}

var editorLookPath = exec.LookPath

// buildEditorCmd returns the process that opens a plain nvim session (no
// DiffviewOpen) in the working directory; O_NVIM overrides the whole command
// (run through the shell), mirroring O_NVIM_DIFF for the diff viewer.
func buildEditorCmd(workingDir string) (*exec.Cmd, error) {
	if override := strings.TrimSpace(os.Getenv("O_NVIM")); override != "" {
		cmd := exec.Command("sh", "-c", override)
		cmd.Dir = workingDir
		return cmd, nil
	}
	if _, err := editorLookPath("nvim"); err != nil {
		return nil, fmt.Errorf("nvim not found (set O_NVIM to override the editor command): %w", err)
	}
	cmd := exec.Command("nvim", ".")
	cmd.Dir = workingDir
	return cmd, nil
}

// openEditor suspends the TUI and opens a plain nvim session (ctrl+t). It is
// also reachable through the hidden /nvim command.
func (m chatModel) openEditor() (tea.Model, tea.Cmd) {
	dir := m.currentWorkingDir()
	if strings.TrimSpace(dir) == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			m.status = "editor failed: " + err.Error()
			return m, nil
		}
	}
	execCmd, err := buildEditorCmd(dir)
	if err != nil {
		m.status = "editor failed: " + err.Error()
		return m, nil
	}
	m.status = "editor"
	return m, tea.ExecProcess(execCmd, func(err error) tea.Msg {
		return chatEditorClosedMsg{err: err}
	})
}
