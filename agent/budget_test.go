package agent

import (
	"strings"
	"testing"

	"github.com/ParthSareen/o/api"
)

func TestPromptBudgetZeroValueDisablesSizing(t *testing.T) {
	var b PromptBudget
	if b.ContextWindow() != 0 || b.Threshold() != 0 {
		t.Fatalf("zero-value budget = (%d,%d), want (0,0)", b.ContextWindow(), b.Threshold())
	}
	// Short content is returned unchanged; no ceiling is enforced.
	msg := b.FitToolResult("bash", "c1", "hello", 0, CeilingThreshold)
	if msg.Content != "hello" || msg.Role != "tool" || msg.ToolName != "bash" || msg.ToolCallID != "c1" {
		t.Fatalf("FitToolResult = %+v, want unchanged short tool message", msg)
	}
}

func TestPromptBudgetRuneCapAppliesWithoutCeiling(t *testing.T) {
	var b PromptBudget
	large := strings.Repeat("x", maxToolResultRunes+10_000)
	msg := b.FitToolResult("bash", "c1", large, 0, CeilingThreshold)
	if !strings.Contains(msg.Content, "tool output truncated") {
		t.Fatalf("expected rune-cap truncation marker, got len=%d", len([]rune(msg.Content)))
	}
	if len([]rune(msg.Content)) >= len(large) {
		t.Fatalf("rune-cap did not shorten content: got %d runes", len([]rune(msg.Content)))
	}
}

func TestNewPromptBudgetComputesThreshold(t *testing.T) {
	b := NewPromptBudget(10_000, 0.5, "", nil, "")
	if b.ContextWindow() != 10_000 {
		t.Fatalf("ContextWindow = %d, want 10000", b.ContextWindow())
	}
	if b.Threshold() != 5_000 {
		t.Fatalf("Threshold = %d, want 5000", b.Threshold())
	}
}

func TestNewPromptBudgetZeroWindowYieldsZeroBudget(t *testing.T) {
	b := NewPromptBudget(0, 0.8, "", nil, "")
	if b.ContextWindow() != 0 || b.Threshold() != 0 {
		t.Fatalf("NewPromptBudget(0,...) = (%d,%d), want (0,0)", b.ContextWindow(), b.Threshold())
	}
}

func TestFitToolResultFitsUnchanged(t *testing.T) {
	b := NewPromptBudget(10_000, 0.5, "", nil, "") // threshold 5000, window 10000
	// baseTokens 0 + a tiny message easily fits under either ceiling.
	msg := b.FitToolResult("bash", "c1", "hello world", 0, CeilingThreshold)
	if msg.Content != "hello world" {
		t.Fatalf("expected unchanged content, got %q", msg.Content)
	}
}

// TestFitToolResultCeilingSelection verifies that CeilingThreshold produces a
// tighter fit than CeilingContextWindow for the same baseTokens: the threshold
// ceiling leaves less room than the full context window.
func TestFitToolResultCeilingSelection(t *testing.T) {
	b := NewPromptBudget(10_000, 0.5, "", nil, "") // threshold 5000, window 10000
	const baseTokens = 4000
	large := strings.Repeat("x", maxToolResultRunes+5_000)

	thresholdMsg := b.FitToolResult("bash", "c1", large, baseTokens, CeilingThreshold)
	windowMsg := b.FitToolResult("bash", "c1", large, baseTokens, CeilingContextWindow)

	if !strings.Contains(thresholdMsg.Content, "tool output truncated") {
		t.Fatalf("CeilingThreshold should truncate oversized output, got len=%d", len([]rune(thresholdMsg.Content)))
	}
	if !strings.Contains(windowMsg.Content, "tool output truncated") {
		t.Fatalf("CeilingContextWindow should truncate oversized output, got len=%d", len([]rune(windowMsg.Content)))
	}
	// The threshold ceiling (5000) is tighter than the window ceiling (10000),
	// so the threshold result must hold fewer runes than the window result.
	thresholdRunes := len([]rune(thresholdMsg.Content))
	windowRunes := len([]rune(windowMsg.Content))
	if thresholdRunes >= windowRunes {
		t.Fatalf("CeilingThreshold (%d runes) should be smaller than CeilingContextWindow (%d runes)", thresholdRunes, windowRunes)
	}
}

func TestFitToolResultFullOmissionWhenBudgetExhausted(t *testing.T) {
	b := NewPromptBudget(100, 0.8, "", nil, "") // threshold 80, window 100
	large := strings.Repeat("x", 50_000)
	// baseTokens leaves almost no room under the threshold: the reserve + the
	// message overhead push the available-rune budget to zero, triggering full
	// omission rather than a head/tail split.
	msg := b.FitToolResult("bash", "c1", large, 78, CeilingThreshold)
	if !strings.HasPrefix(msg.Content, toolOutputFullOmissionPrefix) {
		t.Fatalf("expected full-omission marker, got %q", msg.Content)
	}
}

func TestPromptBudgetEstimateNonZero(t *testing.T) {
	b := NewPromptBudget(10_000, 0.5, "you are helpful", nil, "")
	n := b.Estimate([]api.Message{{Role: "user", Content: "hello there"}})
	if n <= 0 {
		t.Fatalf("Estimate = %d, want > 0", n)
	}
}

// TestPromptBudgetEstimateBindsShape verifies that the bound system prompt
// counts toward the estimate: a budget bound with a system prompt estimates
// larger than one bound with an empty system prompt for the same messages.
func TestPromptBudgetEstimateBindsShape(t *testing.T) {
	messages := []api.Message{{Role: "user", Content: "hello there"}}
	without := NewPromptBudget(10_000, 0.5, "", nil, "").Estimate(messages)
	with := NewPromptBudget(10_000, 0.5, strings.Repeat("you are helpful ", 100), nil, "").Estimate(messages)
	if with <= without {
		t.Fatalf("bound system prompt should raise estimate: with=%d without=%d", with, without)
	}
}
