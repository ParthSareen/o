package chat

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBuildEditorCmdDefault(t *testing.T) {
	old := editorLookPath
	defer func() { editorLookPath = old }()
	editorLookPath = func(string) (string, error) { return "/usr/local/bin/nvim", nil }
	t.Setenv("O_NVIM", "")

	cmd, err := buildEditorCmd("/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "nvim ." {
		t.Fatalf("args = %q", got)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q", cmd.Dir)
	}
}

func TestBuildEditorCmdOverride(t *testing.T) {
	t.Setenv("O_NVIM", "nvim /tmp/notes.md")
	cmd, err := buildEditorCmd("/work")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "sh -c nvim /tmp/notes.md" {
		t.Fatalf("args = %q", got)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q", cmd.Dir)
	}
}

func TestBuildEditorCmdNoNvim(t *testing.T) {
	old := editorLookPath
	defer func() { editorLookPath = old }()
	editorLookPath = func(string) (string, error) { return "", errTestNoNvim }
	t.Setenv("O_NVIM", "")
	if _, err := buildEditorCmd("/work"); err == nil || !strings.Contains(err.Error(), "nvim not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestCtrlTOpensEditor(t *testing.T) {
	m := chatModel{workingDir: "/work"}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("ctrl+t should return an exec cmd")
	}
	if m.status != "editor" {
		t.Fatalf("status = %q", m.status)
	}
}

func TestHiddenSlashNvimNotInSuggestions(t *testing.T) {
	for _, name := range BuiltinSlashCommandNames() {
		if name == "nvim" {
			t.Fatal("/nvim must stay hidden from the command list")
		}
	}

	m := chatModel{workingDir: "/work", input: []rune("/nvim")}
	updated, cmd := m.handleSubmit()
	m = updated.(chatModel)
	if cmd == nil {
		t.Fatal("/nvim should start the editor")
	}
	if m.status != "editor" {
		t.Fatalf("status = %q", m.status)
	}
}

func TestEditorClosedMsgSetsStatus(t *testing.T) {
	m := chatModel{}
	updated, _ := m.Update(chatEditorClosedMsg{})
	m = updated.(chatModel)
	if m.status != "editor closed" {
		t.Fatalf("status = %q", m.status)
	}
	updated, _ = m.Update(chatEditorClosedMsg{err: errTest("boom")})
	m = updated.(chatModel)
	if !strings.Contains(m.status, "boom") {
		t.Fatalf("status = %q", m.status)
	}
}
