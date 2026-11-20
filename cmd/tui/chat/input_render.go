package chat

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	chatPromptPrefix          = ""
	inputBoxHorizontalPadding = 1
	inputCursorGlyph          = "█"
	inputCursorMarker         = "\x00"
	maxInputBoxBodyLines      = 12
)

func inputWithCursor(input []rune, cursor int) string {
	cursor = clamp(cursor, 0, len(input))
	next := make([]rune, 0, len(input)+1)
	next = append(next, input[:cursor]...)
	next = append(next, []rune(inputCursorMarker)...)
	next = append(next, input[cursor:]...)
	return string(next)
}

func (m chatModel) emptyInputPlaceholder() string {
	if m.promptDebug != nil || m.modelPicker != nil || m.thinkPicker != nil || m.approvalPrompt != nil || m.cloudAuthPrompt != nil {
		return ""
	}
	if len(m.entries) > 0 || len(m.messages) > 0 || len(m.input) > 0 || len(m.inputAttachments) > 0 || len(m.inputPastedTexts) > 0 {
		return ""
	}
	return m.emptyChatHint()
}

func renderInputBoxLines(input string, cursor int, width, maxBodyLines int, placeholder string) []string {
	if width < 12 {
		width = 12
	}
	if maxBodyLines < 1 {
		maxBodyLines = 1
	}

	prefix := chatPromptPrefix
	continuationPrefix := strings.Repeat(" ", lipgloss.Width(prefix))
	prefixWidth := lipgloss.Width(prefix)
	innerWidth := max(1, width-2)
	contentWidth := max(1, innerWidth-inputBoxHorizontalPadding*2)
	bodyWidth := max(1, contentWidth-prefixWidth)

	var raw []string
	placeholderMode := input == "" && strings.TrimSpace(placeholder) != ""
	if input == "" && strings.TrimSpace(placeholder) != "" {
		raw = renderInputPromptRawLines(inputWithCursor([]rune(placeholder), 0), prefix, continuationPrefix, bodyWidth)
	} else {
		if cursor >= 0 {
			input = inputWithCursor([]rune(input), cursor)
		}
		raw = renderInputPromptRawLines(input, prefix, continuationPrefix, bodyWidth)
	}
	if len(raw) > maxBodyLines {
		raw = slices.Clone(raw[len(raw)-maxBodyLines:])
		raw[0] = truncateInputLine(continuationPrefix+trimInputPromptPrefix(raw[0]), contentWidth)
	}

	lines := make([]string, 0, len(raw)+2)
	lines = append(lines, chatInputBorderStyle.Render(inputBoxTopBorderLine(width)))
	for i, line := range raw {
		rendered := line
		if placeholderMode {
			if i == 0 && strings.HasPrefix(line, prefix) {
				rest := strings.TrimPrefix(line, prefix)
				if strings.HasPrefix(rest, inputCursorMarker) {
					rendered = chatUserStyle.Render(prefix) + renderInputTextWithCursorStyle(rest, chatInputPlaceholderStyle)
					lines = append(lines, renderInputBoxBodyLine(rendered, contentWidth))
					continue
				}
				rendered = chatUserStyle.Render(prefix) + chatInputPlaceholderStyle.Render(rest)
				lines = append(lines, renderInputBoxBodyLine(rendered, contentWidth))
				continue
			}
			if strings.HasPrefix(line, continuationPrefix) {
				rendered = chatInputPlaceholderStyle.Render(continuationPrefix + strings.TrimPrefix(line, continuationPrefix))
				lines = append(lines, renderInputBoxBodyLine(rendered, contentWidth))
				continue
			}
			rendered = chatInputPlaceholderStyle.Render(line)
			lines = append(lines, renderInputBoxBodyLine(rendered, contentWidth))
			continue
		}
		lines = append(lines, renderInputBoxBodyLine(renderInputTextWithCursor(rendered), contentWidth))
	}
	lines = append(lines, chatInputBorderStyle.Render(inputBoxBottomBorderLine(width)))
	return lines
}

func renderInputTextWithCursor(line string) string {
	return renderInputTextWithCursorStyle(line, chatUserStyle)
}

func renderInputTextWithCursorStyle(line string, style lipgloss.Style) string {
	before, after, ok := strings.Cut(line, inputCursorMarker)
	if !ok {
		return style.Render(line)
	}
	cell, rest := inputCursorCell(after)
	return style.Render(before) + renderInputCursorCell(cell) + style.Render(rest)
}

func inputCursorCell(after string) (string, string) {
	if after == "" {
		return "", ""
	}
	r, size := utf8.DecodeRuneInString(after)
	if r == '\n' {
		return "", after
	}
	return after[:size], after[size:]
}

func renderInputCursorCell(cell string) string {
	if cell == "" {
		return chatBlankCursorStyle.Render(inputCursorGlyph)
	}
	return chatCursorStyle.Render(cell)
}

func renderInputPromptRawLines(text, prefix, continuationPrefix string, width int) []string {
	if width <= 0 {
		width = 1
	}
	text = string(normalizeInputRunes([]rune(text)))
	body := wrapChatText(text, width)
	for i, line := range body {
		if i == 0 {
			body[i] = prefix + line
			continue
		}
		body[i] = continuationPrefix + line
	}
	if len(body) == 0 {
		return []string{prefix}
	}
	return body
}

func trimInputPromptPrefix(line string) string {
	for _, prefix := range []string{chatPromptPrefix, strings.Repeat(" ", lipgloss.Width(chatPromptPrefix))} {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return strings.TrimSpace(line)
}

func inputBoxTopBorderLine(width int) string {
	return inputBoxBorderLine(width, "╭", "╮")
}

func inputBoxBottomBorderLine(width int) string {
	return inputBoxBorderLine(width, "╰", "╯")
}

func inputBoxBorderLine(width int, left, right string) string {
	if width < 4 {
		width = 4
	}
	return left + strings.Repeat("─", max(0, width-2)) + right
}

func renderInputBoxBodyLine(line string, width int) string {
	padding := strings.Repeat(" ", inputBoxHorizontalPadding)
	return chatInputBorderStyle.Render("│") + padding + padRenderedLine(line, width) + padding + chatInputBorderStyle.Render("│")
}

func padRenderedLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	if renderedWidth := lipgloss.Width(line); renderedWidth < width {
		return line + strings.Repeat(" ", width-renderedWidth)
	}
	return line
}

func truncateInputLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	if runewidth.StringWidth(line) <= width {
		return line
	}
	return runewidth.Truncate(line, width, "")
}

func inputBoxTextIndent() string {
	return strings.Repeat(" ", inputBoxHorizontalPadding+1)
}
