package chat

import (
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

func TestPromptDebugRendersCompactionSummary(t *testing.T) {
	m := chatModel{
		opts: Options{Model: "test", Client: chatTestClient{}},
		messages: append([]api.Message{
			{Role: "user", Content: "recent question"},
			{Role: "assistant", Content: "recent answer"},
		}, coreagent.CompactionSummaryMessages("Earlier we discussed X and decided Y.", true)...),
	}
	m.handlePromptCommand("")
	plain := stripANSI(strings.Join(m.promptDebugLines(100), "\n"))
	for _, want := range []string{
		"tool call 1: summary",
		"tool_call_id: " + coreagent.CompactionToolCallID,
		coreagent.CompactionSummaryMessagePrefix,
		"Earlier we discussed X and decided Y.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("prompt debug missing %q", want)
		}
	}
	// recent turns survive compaction alongside the summary pair
	if !strings.Contains(plain, "recent question") {
		t.Error("prompt debug missing recent turns")
	}
}
