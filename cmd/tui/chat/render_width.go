package chat

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

func allBlankLines(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(stripChatANSI(l)) != "" {
			return false
		}
	}
	return true
}

var chatANSISequencePattern = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func stripChatANSI(s string) string {
	return chatANSISequencePattern.ReplaceAllString(s, "")
}

func displayColumnToRuneIndex(line string, col int) int {
	if col <= 0 {
		return 0
	}
	width := 0
	for i, r := range []rune(line) {
		next := width + runewidth.RuneWidth(r)
		if col < next {
			return i
		}
		width = next
	}
	return len([]rune(line))
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func wrapChatText(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		for runewidth.StringWidth(line) > width {
			cut := chatDisplayWidthCut(line, width)
			out = append(out, strings.TrimSpace(line[:cut]))
			line = strings.TrimSpace(line[cut:])
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func chatDisplayWidthCut(line string, width int) int {
	hardCut := 0
	currentWidth := 0
	spaceCut := 0
	spaceWidth := 0
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		nextWidth := currentWidth + runewidth.RuneWidth(r)
		if nextWidth > width {
			break
		}
		currentWidth = nextWidth
		hardCut = i + size
		if (r == ' ' || r == '\t') && currentWidth > width/2 {
			spaceCut = i
			spaceWidth = currentWidth
		}
		i += size
	}
	if spaceCut > 0 && spaceWidth > 0 {
		return spaceCut
	}
	if hardCut > 0 {
		return hardCut
	}
	_, size := utf8.DecodeRuneInString(line)
	return size
}
