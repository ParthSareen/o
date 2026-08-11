package chat

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	chatAnsiRed         = "1"
	chatAnsiGreen       = "2"
	chatAnsiYellow      = "3"
	chatAnsiBlue        = "4"
	chatAnsiCyan        = "6"
	chatAnsiBrightBlack = "8"
)

var (
	chatHeaderStyle = lipgloss.NewStyle().
			Bold(true)

	chatMetaStyle = lipgloss.NewStyle().
			Faint(true)

	chatFooterStyle = lipgloss.NewStyle().
			Faint(true)

	chatInputBorderStyle = lipgloss.NewStyle().
				Faint(true)

	chatInputPlaceholderStyle = lipgloss.NewStyle().
					Foreground(lipgloss.Color("8"))

	chatCursorStyle = lipgloss.NewStyle().
			Reverse(true)

	chatBlankCursorStyle = lipgloss.NewStyle().
				Faint(true)

	chatNotificationStyle = chatMetaStyle

	chatUserStyle = lipgloss.NewStyle()

	chatUserBlockStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#777777", Dark: "#8a8a8a"})

	chatToolStyle = lipgloss.NewStyle()

	chatInlineCodeStyle = lipgloss.NewStyle().
				Bold(true)

	chatStrongStyle = lipgloss.NewStyle().
			Bold(true)

	chatCodeBlockStyle = lipgloss.NewStyle()

	chatTableBorderStyle = lipgloss.NewStyle().
				Faint(true)

	chatToolRunningStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(chatAnsiYellow))

	chatToolDoneStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(chatAnsiGreen))

	// chatToolMixedStyle marks a tool group with both succeeded and failed
	// calls (partial success). Amber/orange is distinct from green (success),
	// red (failure), and yellow (running).
	chatToolMixedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("208"))

	chatToolOutputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#666666", Dark: "#a0a0a0"})

	chatDiffMetaStyle = lipgloss.NewStyle().
				Faint(true)

	chatDiffFileStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(chatAnsiCyan))

	chatDiffHunkStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(chatAnsiBlue))

	chatDiffAddStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(chatAnsiGreen))

	chatDiffDeleteStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(chatAnsiRed))

	chatErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(chatAnsiRed))

	chatFullAccessStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#9f5f5f", Dark: "#b87373"})

	chatCommandNameStyle = lipgloss.NewStyle()

	chatPickerTextStyle = lipgloss.NewStyle()

	chatPickerTitleStyle = lipgloss.NewStyle().
				Bold(true)

	chatPickerSelectedStyle = lipgloss.NewStyle().
				Bold(true)

	chatPickerMetaStyle = lipgloss.NewStyle().
				Faint(true)

	chatHistoryTitleStyle = lipgloss.NewStyle().
				Bold(true)

	chatHistorySystemRoleStyle = lipgloss.NewStyle().
					Bold(true).
					Faint(true)

	chatHistoryUserRoleStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color(chatAnsiBlue))

	chatHistoryAssistantRoleStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color(chatAnsiYellow))

	chatHistoryToolRoleStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color(chatAnsiGreen))

	chatHistoryLabelStyle = lipgloss.NewStyle().
				Faint(true)

	chatHistoryTextStyle = lipgloss.NewStyle()
)

// chatTheme is the user-facing accent palette. Empty color fields fall back to
// the legacy styling above, so "default" (all empty) renders exactly as before.
type chatTheme struct {
	Name           string
	Accent         string // spinner and other accent moments
	Accent2        string // second spinner tone for a subtle two-tone pulse
	BorderIdle     string // input box border at rest
	BorderRunning  string // input box border while a run is active
	BorderThinking string // input box border while thinking streams
	BorderApproval string // input box border with a pending approval prompt
	Placeholder    string // input placeholder text
}

var chatThemes = map[string]chatTheme{
	"default": {Name: "default"},
	"nord": {
		Name:           "nord",
		Accent:         "#88C0D0", // frost cyan
		Accent2:        "#A3BE8C", // aurora green
		BorderIdle:     "#4C566A", // polar night light
		BorderRunning:  "#EBCB8B", // aurora yellow
		BorderThinking: "#B48EAD", // aurora purple
		BorderApproval: "#BF616A", // aurora red
		Placeholder:    "#616E88",
	},
	"dracula": {
		Name:           "dracula",
		Accent:         "#BD93F9", // purple
		Accent2:        "#50FA7B", // green
		BorderIdle:     "#6272A4", // comment
		BorderRunning:  "#F1FA8C", // yellow
		BorderThinking: "#8BE9FD", // cyan
		BorderApproval: "#FF5555", // red
		Placeholder:    "#6272A4",
	},
	"catppuccin": {
		Name:           "catppuccin",
		Accent:         "#CBA6F7", // mocha mauve
		Accent2:        "#A6E3A1", // mocha green
		BorderIdle:     "#585B70", // mocha surface2
		BorderRunning:  "#F9E2AF", // mocha yellow
		BorderThinking: "#89DCEB", // mocha sky
		BorderApproval: "#F38BA8", // mocha red
		Placeholder:    "#6C7086", // mocha overlay1
	},
}

func chatThemeNames() []string {
	names := make([]string, 0, len(chatThemes))
	for name := range chatThemes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// themeForName resolves a theme by name (case-insensitive); unknown or empty
// names resolve to the default theme.
func themeForName(name string) chatTheme {
	name = strings.ToLower(strings.TrimSpace(name))
	if theme, ok := chatThemes[name]; ok && theme.Name != "" {
		return theme
	}
	return chatThemes["default"]
}

// inputBorderColor picks the border accent for the current model state;
// approval wins, then thinking, then any active run.
func (m chatModel) inputBorderColor() string {
	switch {
	case m.approvalPrompt != nil:
		return m.theme.BorderApproval
	case m.thinking:
		return m.theme.BorderThinking
	case m.running || m.compacting || m.preloadingModel != "":
		return m.theme.BorderRunning
	default:
		return m.theme.BorderIdle
	}
}

func (m chatModel) inputBorderStyle() lipgloss.Style {
	if color := m.inputBorderColor(); color != "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	}
	return chatInputBorderStyle
}

func (m chatModel) inputPlaceholderStyle() lipgloss.Style {
	if m.theme.Placeholder != "" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Placeholder))
	}
	return chatInputPlaceholderStyle
}
