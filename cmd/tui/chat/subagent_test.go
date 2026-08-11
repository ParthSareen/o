package chat

import (
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
)

// TestApplySubagentEventRoutesChildToolCalls tests that events forwarded from
// a child sub-agent session are nested under the parent subagents tool entry.
func TestApplySubagentEventRoutesChildToolCalls(t *testing.T) {
	parentCallID := "parent-call-1"
	m := chatModel{running: true}

	// Parent tool call starts (the subagents tool itself).
	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		ToolCallID: parentCallID,
		ToolName:   "subagents",
		Args:       map[string]any{"query": "what files exist?", "context": "check the repo"},
	})

	// Verify the parent entry exists.
	parentIdx := m.findToolEntry(parentCallID)
	if parentIdx < 0 {
		t.Fatal("parent subagents tool entry not found")
	}
	if m.entries[parentIdx].tools != nil && len(m.entries[parentIdx].tools) != 0 {
		t.Fatalf("expected no nested children initially, got %d", len(m.entries[parentIdx].tools))
	}

	// Child sub-agent starts a bash tool call.
	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		SubagentID: parentCallID,
		ToolCallID: "child-call-1",
		ToolName:   "bash",
		Args:       map[string]any{"command": "ls"},
	})

	parentIdx = m.findToolEntry(parentCallID)
	if len(m.entries[parentIdx].tools) != 1 {
		t.Fatalf("expected 1 nested child, got %d", len(m.entries[parentIdx].tools))
	}
	child := m.entries[parentIdx].tools[0]
	if child.toolID != "child-call-1" || child.detail != "bash" || child.status != "running" {
		t.Fatalf("unexpected child entry: %+v", child)
	}

	// Child sub-agent finishes the bash tool call.
	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolFinished,
		SubagentID: parentCallID,
		ToolCallID: "child-call-1",
		ToolName:   "bash",
		ToolStatus: coreagent.ToolStatusDone,
		Args:       map[string]any{"command": "ls"},
		Content:    "file1.go\nfile2.go",
	})

	parentIdx = m.findToolEntry(parentCallID)
	if len(m.entries[parentIdx].tools) != 1 {
		t.Fatalf("expected 1 nested child, got %d", len(m.entries[parentIdx].tools))
	}
	child = m.entries[parentIdx].tools[0]
	if child.status != "done" || child.content != "file1.go\nfile2.go" {
		t.Fatalf("unexpected child entry after finish: %+v", child)
	}

	// Child sends a second tool call (e.g., web search).
	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		SubagentID: parentCallID,
		ToolCallID: "child-call-2",
		ToolName:   "web_search",
		Args:       map[string]any{"query": "golang tips"},
	})

	parentIdx = m.findToolEntry(parentCallID)
	if len(m.entries[parentIdx].tools) != 2 {
		t.Fatalf("expected 2 nested children, got %d", len(m.entries[parentIdx].tools))
	}

	// Child streams an assistant message (its answer).
	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventMessageDelta,
		SubagentID: parentCallID,
		Content:    "I found 2 files and searched the web.",
	})

	parentIdx = m.findToolEntry(parentCallID)
	if m.entries[parentIdx].content != "I found 2 files and searched the web." {
		t.Fatalf("expected child message in parent content, got %q", m.entries[parentIdx].content)
	}
}

// TestApplySubagentEventIgnoresUnknownParent tests that subagent events with
// no matching parent tool entry are silently dropped.
func TestApplySubagentEventIgnoresUnknownParent(t *testing.T) {
	m := chatModel{running: true}

	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		SubagentID: "nonexistent",
		ToolCallID: "child-call-1",
		ToolName:   "bash",
		Args:       map[string]any{"command": "ls"},
	})

	if len(m.entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(m.entries))
	}
}

// TestSubagentToolEntryExpandable tests that a tool entry with nested
// subagent children is marked as expandable.
func TestSubagentToolEntryExpandable(t *testing.T) {
	parentCallID := "parent-call-1"
	m := chatModel{running: true}

	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		ToolCallID: parentCallID,
		ToolName:   "subagents",
		Args:       map[string]any{"query": "test", "context": "ctx"},
	})

	m.applyAgentEvent(coreagent.Event{
		Type:       coreagent.EventToolStarted,
		SubagentID: parentCallID,
		ToolCallID: "child-call-1",
		ToolName:   "bash",
		Args:       map[string]any{"command": "ls"},
	})

	parentIdx := m.findToolEntry(parentCallID)
	if !entryHasExpandableOutput(m.entries[parentIdx]) {
		t.Fatal("expected subagents tool entry with children to be expandable")
	}
	if !entryHasToolOutputMode(m.entries[parentIdx]) {
		t.Fatal("expected subagents tool entry with children to have tool output mode")
	}
}
