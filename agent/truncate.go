package agent

import (
	"fmt"
	"strings"

	"github.com/ParthSareen/o/api"
)

func toolMessageWithLimit(toolName, toolCallID, content string, maxRunes int) api.Message {
	return api.Message{
		Role:       "tool",
		Content:    truncateToolResultContentTo(content, maxRunes),
		ToolName:   toolName,
		ToolCallID: toolCallID,
	}
}

func smallContextToolResultLimitRunes(contextWindow int) int {
	switch {
	case contextWindow > 0 && contextWindow <= tinyContextToolResultTokenWindow:
		return tinyContextToolResultRunes
	case contextWindow > 0 && contextWindow <= smallContextToolResultTokenWindow:
		return smallContextToolResultRunes
	default:
		return 0
	}
}

func toolMessage(toolName, toolCallID, content string) api.Message {
	return toolMessageWithLimit(toolName, toolCallID, content, maxToolResultRunes)
}

func sanitizeMessageForRun(msg api.Message) api.Message {
	if msg.Role == "tool" {
		msg.Content = truncateToolResultContent(msg.Content)
	}
	return msg
}

func sanitizeMessagesForRequest(messages []api.Message) []api.Message {
	if len(messages) == 0 {
		return nil
	}
	sanitized := make([]api.Message, len(messages))
	for i, msg := range messages {
		sanitized[i] = sanitizeMessageForRun(msg)
	}
	return sanitized
}

func truncateToolResultContent(content string) string {
	return truncateToolResultContentTo(content, maxToolResultRunes)
}

func truncateToolResultContentTo(content string, maxRunes int) string {
	return Truncate(content, TruncateConfig{
		MaxRunes:           maxRunes,
		HeadTail:           true,
		HeadPct:            75,
		Label:              "tool output",
		Hint:               "Use a narrower command, line range, or search query if more detail is needed.",
		FullOmissionPrefix: toolOutputFullOmissionPrefix,
	})
}

// TruncateConfig configures content truncation via Truncate.
type TruncateConfig struct {
	MaxRunes           int    // rune limit; <= 0 means full omission
	HeadTail           bool   // true = head + tail split; false = head only
	HeadPct            int    // percentage of MaxRunes for head (e.g. 75); tail gets the rest
	Label              string // e.g. "tool output", "summary", "stdout"
	Hint               string // guidance text appended to marker (optional)
	FullOmissionPrefix string // marker prefix when MaxRunes <= 0
}

// Truncate truncates content to at most cfg.MaxRunes runes. When HeadTail is
// true, it preserves the first HeadPct% and last (100-HeadPct)% of the budget
// with a marker between; otherwise it keeps only the head. MaxRunes <= 0
// triggers full omission using FullOmissionPrefix. All token counts in
// markers use ApproximateTokens.
func Truncate(content string, cfg TruncateConfig) string {
	runes := []rune(content)
	total := len(runes)

	if cfg.MaxRunes <= 0 {
		return fmt.Sprintf("%s omitted ~%d tokens.%s]", cfg.FullOmissionPrefix, ApproximateTokens(total), truncHint(cfg.Hint))
	}
	if total <= cfg.MaxRunes {
		return content
	}

	if !cfg.HeadTail {
		head := cfg.MaxRunes
		omitted := total - head
		return string(runes[:head]) + TruncMarker(cfg.Label, head, 0, omitted, false, cfg.Hint)
	}

	head := cfg.MaxRunes * cfg.HeadPct / 100
	tail := cfg.MaxRunes - head
	omitted := total - head - tail
	return string(runes[:head]) + TruncMarker(cfg.Label, head, tail, omitted, true, cfg.Hint) + string(runes[len(runes)-tail:])
}

func truncHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	if !strings.HasSuffix(hint, ".") {
		hint += "."
	}

	return " " + hint
}

// TruncMarker formats a truncation marker with consistent wording. head and
// tail are rune counts; omitted is the count of runes removed. headTail
// selects the head+tail vs head-only format. hint is optional guidance text.
func TruncMarker(label string, head, tail, omitted int, headTail bool, hint string) string {
	var b strings.Builder
	b.WriteString("\n\n[")
	b.WriteString(label)
	b.WriteString(" truncated: ")
	if headTail {
		fmt.Fprintf(&b, "showing first ~%d tokens and last ~%d tokens; ", ApproximateTokens(head), ApproximateTokens(tail))
	} else {
		fmt.Fprintf(&b, "showing first ~%d tokens; ", ApproximateTokens(head))
	}
	fmt.Fprintf(&b, "omitted ~%d tokens.%s]", ApproximateTokens(omitted), truncHint(hint))
	if headTail {
		b.WriteString("\n\n")
	}
	return b.String()
}

func toolOutputFullyOmitted(content string) bool {
	return strings.HasPrefix(content, toolOutputFullOmissionPrefix)
}

// ApproximateTokens estimates token count from a character/byte count using
// the standard ~4 chars-per-token heuristic. It is intentionally rough; all
// callers use it only for sizing/truncation decisions, not billing.
func ApproximateTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return max(1, (n+3)/4)
}
