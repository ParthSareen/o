package chat

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBuildDiffViewerCmdDefault(t *testing.T) {
	old := diffViewerLookPath
	defer func() { diffViewerLookPath = old }()
	diffViewerLookPath = func(string) (string, error) { return "/usr/local/bin/nvim", nil }
	t.Setenv("O_NVIM_DIFF", "")

	cmd, err := buildDiffViewerCmd("/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "nvim -c DiffviewOpen" {
		t.Fatalf("args = %q", got)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q", cmd.Dir)
	}
}

func TestBuildDiffViewerCmdOverride(t *testing.T) {
	t.Setenv("O_NVIM_DIFF", "nvim -c 'DiffviewOpen main...HEAD'")
	cmd, err := buildDiffViewerCmd("/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "sh -c nvim -c 'DiffviewOpen main...HEAD'" {
		t.Fatalf("args = %q", got)
	}
}

func TestBuildDiffViewerCmdNoNvim(t *testing.T) {
	old := diffViewerLookPath
	defer func() { diffViewerLookPath = old }()
	diffViewerLookPath = func(string) (string, error) { return "", errTestNoNvim }
	t.Setenv("O_NVIM_DIFF", "")
	if _, err := buildDiffViewerCmd("/work"); err == nil || !strings.Contains(err.Error(), "nvim not found") {
		t.Fatalf("err = %v", err)
	}
}

var errTestNoNvim = errTest("no nvim")

type errTest string

func (e errTest) Error() string { return string(e) }

func TestCtrlGOpensDiffViewer(t *testing.T) {
	m := chatModel{workingDir: "/work"}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("ctrl+g should return an exec cmd")
	}
	if m.status != "diffview" {
		t.Fatalf("status = %q", m.status)
	}
}

func TestHiddenSlashDiffViewNotInSuggestions(t *testing.T) {
	for _, name := range BuiltinSlashCommandNames() {
		if name == "diffview" {
			t.Fatal("/diffview must stay hidden from the command list")
		}
	}

	m := chatModel{workingDir: "/work", input: []rune("/diffview")}
	updated, cmd := m.handleSubmit()
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("/diffview should start the diff viewer")
	}
	if m.status != "diffview" {
		t.Fatalf("status = %q", m.status)
	}
}

func TestDiffViewerClosedMsgSetsStatus(t *testing.T) {
	m := chatModel{}
	updated, _ := m.Update(chatDiffViewerClosedMsg{})
	m = updated.(chatModel)
	if m.status != "diffview closed" {
		t.Fatalf("status = %q", m.status)
	}
	updated, _ = m.Update(chatDiffViewerClosedMsg{err: errTest("boom")})
	m = updated.(chatModel)
	if !strings.Contains(m.status, "boom") {
		t.Fatalf("status = %q", m.status)
	}
}
