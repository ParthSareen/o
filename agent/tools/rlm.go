package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// RLMQuery is a recursive language model tool: it spawns a child agent session
// (depth=1) with the full tool suite to answer a sub-query over a provided
// context string. The root model calls this tool to decompose large-context
// tasks and avoid context rot. It is registered only when --rlm is passed.
//
// The child gets all tools except subagents itself (enforcing max depth=1),
// so it can run bash, edit files, search the web, etc. to answer the query.
//
// Child session events are forwarded to the parent TUI's event sinks, tagged
// with SubagentID (the parent tool call ID) so the TUI can render the child's
// activity — tool calls, messages, etc. — nested under the subagents entry.
//
// For experiments, the tool name is configurable via ToolName so we can test
// which name models call most naturally (e.g., "subagents" vs "rlm_query").
type RLMQuery struct {
	Client    agent.ChatClient
	Model     string
	Options   map[string]any
	Think     *api.ThinkValue
	KeepAlive *api.Duration

	// ToolName overrides the tool name (default: "subagents"). Used for
	// A/B testing which name the model calls most naturally.
	ToolName string

	// MaxDepth caps recursion depth. Depth=1 children get the full tool suite
	// but cannot spawn their own sub-agents.
	MaxDepth int
	Depth    int

	// Timeout per recursive call.
	Timeout time.Duration

	// Registry is set after registration to enable tool-using child sessions.
	// When nil, Execute falls back to a raw chat request without tools.
	Registry *agent.Registry
}

func (t *RLMQuery) name() string {
	if t.ToolName != "" {
		return t.ToolName
	}
	return "subagents"
}

func (t *RLMQuery) Name() string { return t.name() }

func (t *RLMQuery) Description() string {
	return "Launch a sub-agent with full tool access (bash, file editing, web search, etc.) to answer a query over a given context. Use this to decompose large-context tasks: the sub-agent can inspect files, run commands, and search the web to answer the query. Only one level of recursion is allowed — the sub-agent cannot spawn its own sub-agents."
}

func (t *RLMQuery) Schema() api.ToolFunction {
	props := api.NewToolPropertiesMap()
	props.Set("query", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "The question or instruction for the sub-agent.",
	})
	props.Set("context", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "The text context the sub-agent should analyze to answer the query.",
	})
	return api.ToolFunction{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: api.ToolFunctionParameters{
			Type:       "object",
			Properties: props,
			Required:   []string{"query", "context"},
		},
	}
}

func (t *RLMQuery) RequiresApproval(map[string]any) bool { return true }

func (t *RLMQuery) Execute(ctx context.Context, toolCtx agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return agent.ToolResult{}, errors.New("query parameter is required")
	}
	contextText, ok := args["context"].(string)
	if !ok || strings.TrimSpace(contextText) == "" {
		return agent.ToolResult{}, errors.New("context parameter is required")
	}

	if t.Client == nil {
		return agent.ToolResult{}, errors.New("no chat client available for recursive call")
	}

	if t.Depth >= t.MaxDepth && t.MaxDepth > 0 {
		return agent.ToolResult{Content: fmt.Sprintf("Error: max recursion depth %d reached", t.MaxDepth)}, nil
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// When a registry is available, run the child as a full agent session
	// with all tools except subagents (depth=1 cap). The child can use bash,
	// file editing, web search, etc. to answer the query.
	if t.Registry != nil {
		return t.executeWithTools(callCtx, toolCtx, query, contextText)
	}

	// Fallback: raw chat request without tools.
	return t.executeRaw(callCtx, query, contextText)
}

// subagentEventSink wraps the parent session's event sinks, tagging each
// forwarded event with SubagentID so the TUI can nest child activity under
// the subagents tool entry.
type subagentEventSink struct {
	parentSinks []agent.EventSink
	subagentID  string
}

func (s subagentEventSink) Emit(event agent.Event) error {
	event.SubagentID = s.subagentID
	var errs []error
	for _, sink := range s.parentSinks {
		if sink == nil {
			continue
		}
		if err := sink.Emit(event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// executeWithTools runs a child agent session with the full tool suite
// (minus subagents) so the sub-agent can inspect files, run commands, and
// search the web to answer the query.
func (t *RLMQuery) executeWithTools(ctx context.Context, toolCtx agent.ToolContext, query, contextText string) (agent.ToolResult, error) {
	// Build a child registry with all tools except subagents itself,
	// enforcing the depth=1 recursion cap.
	childRegistry := &agent.Registry{}
	for _, name := range t.Registry.Names() {
		if name == t.Name() {
			continue
		}
		if tool, ok := t.Registry.Get(name); ok {
			childRegistry.Register(tool)
		}
	}

	// Auto-approve child tool calls: the parent already approved the
	// subagents call, and the child is bounded by MaxToolRounds and Timeout.
	approvalState := &agent.ApprovalState{}
	approvalState.GrantAll()

	// Forward child session events to the parent TUI, tagged with the
	// parent tool call ID so the TUI can render the child's activity
	// (tool calls, messages, etc.) nested under the subagents entry.
	var childEventSinks []agent.EventSink
	if len(toolCtx.EventSinks) > 0 && toolCtx.ToolCallID != "" {
		childEventSinks = []agent.EventSink{subagentEventSink{
			parentSinks: toolCtx.EventSinks,
			subagentID:  toolCtx.ToolCallID,
		}}
	}

	childSession := &agent.Session{
		Client:        t.Client,
		Tools:         childRegistry,
		ApprovalState: approvalState,
		WorkingDir:    toolCtx.WorkingDir,
		EventSinks:    childEventSinks,
	}

	systemPrompt := "You are a sub-agent answering a query about the provided context. " +
		"You have access to tools (bash, file editing, web search, etc.) — use them when they help. " +
		"Be concise and direct. Base your answer on the provided context."

	runOpts := agent.RunOptions{
		Model:        t.Model,
		SystemPrompt: systemPrompt,
		NewMessages: []api.Message{{
			Role:    "user",
			Content: fmt.Sprintf("Context:\n%s\n\nQuery:\n%s", contextText, query),
		}},
		Options:      t.Options,
		Think:        t.Think,
		MaxToolRounds: 300,
	}
	if t.KeepAlive != nil {
		runOpts.KeepAlive = t.KeepAlive
	}

	result, err := childSession.Run(ctx, runOpts)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: %v", err)}, nil
	}

	// Find the last assistant message with content.
	for i := len(result.Messages) - 1; i >= 0; i-- {
		msg := result.Messages[i]
		if msg.Role == "assistant" && strings.TrimSpace(msg.Content) != "" {
			return agent.ToolResult{Content: strings.TrimSpace(msg.Content)}, nil
		}
	}
	return agent.ToolResult{Content: "(sub-agent returned no answer)"}, nil
}

// executeRaw is the fallback path: a plain LM call without tools, used when
// no registry is available (e.g., in minimal test setups).
func (t *RLMQuery) executeRaw(ctx context.Context, query, contextText string) (agent.ToolResult, error) {
	req := &api.ChatRequest{
		Model: t.Model,
		Messages: []api.Message{
			{
				Role: "system",
				Content: "You are a sub-agent answering a query about the provided context. " +
					"Be concise and direct. Answer based only on the provided context.",
			},
			{
				Role:    "user",
				Content: fmt.Sprintf("Context:\n%s\n\nQuery:\n%s", contextText, query),
			},
		},
		Options: t.Options,
		Think:   t.Think,
	}
	if t.KeepAlive != nil {
		req.KeepAlive = t.KeepAlive
	}

	var answer strings.Builder
	err := t.Client.Chat(ctx, req, func(response api.ChatResponse) error {
		if response.Message.Content != "" {
			answer.WriteString(response.Message.Content)
		}
		return nil
	})
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: %v", err)}, nil
	}

	result := strings.TrimSpace(answer.String())
	if result == "" {
		return agent.ToolResult{Content: "(sub-agent returned no answer)"}, nil
	}
	return agent.ToolResult{Content: result}, nil
}
