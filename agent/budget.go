package agent

import "github.com/ParthSareen/o/api"

// BudgetCeiling selects which token limit a tool result is sized against.
type BudgetCeiling int

const (
	// CeilingThreshold sizes a tool result against the compaction threshold so
	// the next round triggers compaction rather than overflowing the context.
	CeilingThreshold BudgetCeiling = iota
	// CeilingContextWindow sizes a tool result against the hard context-window
	// limit, used when refitting tool results after a compaction.
	CeilingContextWindow
)

// PromptBudget owns the context window, compaction threshold, and prompt shape
// (system prompt, tools, format) for a run, centralizing all "does it fit /
// size this to fit / how big is the prompt" decisions that were previously
// scattered across Session methods (compactionThresholdTokens,
// contextWindowTokens, toolMessageForContext, toolMessageForPostCompactionContext,
// estimateRunPromptTokens, check*PromptBudget) and the CompactionRequest's
// estimate-only fields.
//
// A zero-value PromptBudget (contextWindow <= 0) disables sizing: estimates
// are still computed but no ceiling is enforced, matching the previous
// behavior when the compactor or context window was unknown.
type PromptBudget struct {
	contextWindow int
	threshold     int
	systemPrompt  string
	tools         api.Tools
	format        string
}

// NewPromptBudget builds a PromptBudget from a resolved context window (in
// tokens), a threshold fraction of the window (e.g. 0.8 means compact at 80%
// capacity), and the run's prompt shape (system prompt, tools, format) used
// for prompt-size estimation. A non-positive context window yields a
// zero-value budget that disables sizing. A non-positive computed threshold
// keeps the window but disables threshold-based fitting (CeilingThreshold
// falls back to the small-context rune cap only), matching the former
// compactionThresholdTokens guard.
func NewPromptBudget(contextWindow int, thresholdFraction float64, systemPrompt string, tools api.Tools, format string) PromptBudget {
	if contextWindow <= 0 {
		return PromptBudget{systemPrompt: systemPrompt, tools: tools, format: format}
	}
	threshold := int(float64(contextWindow) * thresholdFraction)
	return PromptBudget{contextWindow: contextWindow, threshold: threshold, systemPrompt: systemPrompt, tools: tools, format: format}
}

// ContextWindow returns the effective context window in tokens, or 0 if
// unknown/disabled.
func (b PromptBudget) ContextWindow() int { return b.contextWindow }

// Threshold returns the compaction threshold in tokens (window * fraction),
// or 0 if disabled. CeilingThreshold fitting falls back to the small-context
// rune cap when this is 0.
func (b PromptBudget) Threshold() int { return b.threshold }

// Estimate approximates the token count of the chat prompt composed of the
// budget's bound system prompt, tools, and format plus the given messages. It
// mirrors the marshal + chars/4 heuristic used by the compactor's due-ness
// check. The prompt shape is bound at construction so callers need only pass
// the messages that change per estimate.
func (b PromptBudget) Estimate(messages []api.Message) int {
	return estimatePromptTokens(b.systemPrompt, messages, b.tools, b.format)
}

// FitToolResult sizes a tool-result message to fit under the selected ceiling
// given baseTokens already present in history. It preserves head and tail of
// oversized output. A zero-value budget, or a ceiling whose limit is 0,
// applies only the small-context rune cap.
//
// baseTokens is the pre-computed estimate of everything before this message;
// the ceiling selects the compaction threshold (pre-overflow) or the context
// window (post-compaction refit) as the budget.
func (b PromptBudget) FitToolResult(toolName, toolCallID, content string, baseTokens int, ceiling BudgetCeiling) api.Message {
	maxRunes := maxToolResultRunes
	if limit := smallContextToolResultLimitRunes(b.contextWindow); limit > 0 {
		maxRunes = min(maxRunes, limit)
	}

	budgetTokens := 0
	switch ceiling {
	case CeilingThreshold:
		budgetTokens = b.threshold
	case CeilingContextWindow:
		budgetTokens = b.contextWindow
	}

	if budgetTokens <= 0 {
		return toolMessageWithLimit(toolName, toolCallID, content, maxRunes)
	}

	msg := toolMessageWithLimit(toolName, toolCallID, content, maxRunes)
	projectedTokens := baseTokens + estimateMessagesTokens([]api.Message{msg})
	if projectedTokens < budgetTokens {
		return msg
	}

	overheadTokens := estimateMessagesTokens([]api.Message{{
		Role:       "tool",
		ToolName:   toolName,
		ToolCallID: toolCallID,
	}})
	// Keep oversized tool output below the budget before it is appended to
	// history. This is especially important for <=8k contexts: the next step
	// must still have enough room to compact and continue the same user
	// request instead of asking the user to prompt again.
	availableRunes := (budgetTokens - baseTokens - overheadTokens - toolTruncationMarkerReserveTokens) * 4
	maxRunes = min(maxRunes, max(0, availableRunes))
	msg.Content = truncateToolResultContentTo(content, maxRunes)
	return msg
}
