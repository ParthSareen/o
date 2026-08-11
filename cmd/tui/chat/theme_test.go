package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func withTrueColor(t *testing.T) {
	t.Helper()
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
}

func TestThemeForName(t *testing.T) {
	for _, name := range []string{"nord", "dracula", "catppuccin", "default"} {
		name := themeForName(name).Name
		if name == "" {
			t.Fatal("expected a theme")
		}
	}
	if got := themeForName("NORD").Name; got != "nord" {
		t.Fatalf("case-insensitive lookup got %q", got)
	}
	for _, bad := range []string{"", "bogus"} {
		if got := themeForName(bad).Name; got != "default" {
			t.Fatalf("themeForName(%q) = %q, want default", bad, got)
		}
	}
}

func TestResolveThemeNamePriority(t *testing.T) {
	if got := resolveThemeName("nord"); got != "nord" {
		t.Fatalf("option got %q", got)
	}
	t.Setenv("OLLAMA_THEME", "dracula")
	if got := resolveThemeName(""); got != "dracula" {
		t.Fatalf("env got %q", got)
	}
	if got := resolveThemeName("catppuccin"); got != "catppuccin" {
		t.Fatalf("option should beat env, got %q", got)
	}
}

func TestInputBorderColorStates(t *testing.T) {
	m := chatModel{theme: themeForName("nord")}
	if got := m.inputBorderColor(); got != "#4C566A" {
		t.Fatalf("idle = %q", got)
	}
	m.running = true
	if got := m.inputBorderColor(); got != "#EBCB8B" {
		t.Fatalf("running = %q", got)
	}
	m.thinking = true
	if got := m.inputBorderColor(); got != "#B48EAD" {
		t.Fatalf("thinking = %q", got)
	}
	m.approvalPrompt = &chatApprovalPrompt{}
	if got := m.inputBorderColor(); got != "#BF616A" {
		t.Fatalf("approval = %q", got)
	}
}

func TestDefaultThemeKeepsLegacyStyles(t *testing.T) {
	m := chatModel{theme: themeForName("default")}
	if m.inputBorderColor() != "" {
		t.Fatal("default theme must not recolor borders")
	}
	if m.spinnerFrame() != chatSpinnerFrames[0] {
		t.Fatalf("default theme must not recolor the spinner, got %q", m.spinnerFrame())
	}
}

func TestThemedSpinnerColored(t *testing.T) {
	withTrueColor(t)
	m := chatModel{theme: themeForName("dracula")}
	if m.spinnerFrame() == chatSpinnerFrames[0] {
		t.Fatal("themed spinner should be colored")
	}
}

func TestThemedInputBoxRendersBorderColor(t *testing.T) {
	withTrueColor(t)
	m := chatModel{theme: themeForName("catppuccin")}
	lines := renderInputBoxLines("hello", 5, 24, 1, "", m.inputBorderStyle(), m.inputPlaceholderStyle())
	joined := strings.Join(lines, "\n")
	if joined == stripANSI(joined) {
		t.Fatalf("themed border should carry ANSI color, got %q", joined)
	}
}

func TestThemeCommand(t *testing.T) {
	m := chatModel{}
	updated, _ := m.handleThemeCommand("")
	m = updated.(chatModel)
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].content, "catppuccin") {
		t.Fatalf("/theme list missing presets: %#v", m.entries)
	}

	updated, _ = m.handleThemeCommand("nord")
	m = updated.(chatModel)
	if m.theme.Name != "nord" || m.status != "theme: nord" {
		t.Fatalf("theme = %q status = %q", m.theme.Name, m.status)
	}

	updated, _ = m.handleThemeCommand("bogus")
	m = updated.(chatModel)
	last := m.entries[len(m.entries)-1]
	if last.role != "error" || !strings.Contains(last.content, "unknown theme") {
		t.Fatalf("unknown theme entry = %#v", last)
	}
}

func TestThemeCompletions(t *testing.T) {
	m := chatModel{input: []rune("/theme n")}
	comps := m.slashCompletions()
	if len(comps) != 1 || comps[0].value != "/theme nord" {
		t.Fatalf("completions = %#v", comps)
	}
	m.input = []rune("/theme zzz")
	comps = m.slashCompletions()
	if len(comps) != 1 || comps[0].label != "No matching themes" {
		t.Fatalf("no-match completions = %#v", comps)
	}
}
