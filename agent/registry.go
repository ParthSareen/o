package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ParthSareen/o/api"
)

type ToolContext struct {
	WorkingDir string
	// EventSinks are the parent session's event sinks, passed so tools that
	// spawn child sessions (e.g. the RLM subagents tool) can forward child
	// events to the parent TUI.
	EventSinks []EventSink
	// ToolCallID is the ID of the tool call that triggered this execution,
	// used to tag forwarded child events with their parent tool call.
	ToolCallID string
}

type ToolResult struct {
	Content    string
	WorkingDir string
}

type Tool interface {
	Name() string
	Description() string
	Schema() api.ToolFunction
	Execute(context.Context, ToolContext, map[string]any) (ToolResult, error)
}

type ApprovalRequired interface {
	RequiresApproval(map[string]any) bool
}

// ScopedTool is implemented by tools that need per-invocation approval
// scoping beyond the tool name (e.g. shell commands scoped to the exact
// command string). Tools that don't implement this are scoped by name only.
type ScopedTool interface {
	ApprovalScope(args map[string]any) string
}

type Registry struct {
	tools map[string]Tool
	// Background surfaces completions from background tool work (e.g.
	// tools.BackgroundManager, populated by the shell tool's background=true
	// flag) into runs built from this registry. Child registries (RLM
	// sub-agents) deliberately leave it unset: only the root run may consume
	// completions, or a child's drain would steal notices that belong in the
	// parent's history.
	Background BackgroundSource
}

// BackgroundCompletion is one finished background task, reported to runs
// via BackgroundSource.
type BackgroundCompletion struct {
	ID       string
	Command  string
	ExitCode int
	Killed   bool
	// Failure is the process start/wait error, if any; ExitCode is not
	// meaningful when Failure is set.
	Failure  string
	Duration time.Duration
	LogPath  string
	// Tail is a bounded trailing log excerpt, set only for failed tasks so
	// the notice carries enough context to act on without an extra read.
	Tail string
}

// BackgroundSource reports finished background work. A session drains it at
// run start (work that completed while the session was idle) and again
// before finishing a run (work that completed mid-run), injecting results
// into the conversation so the model can react. Implementations must report
// each completion exactly once across all drains.
type BackgroundSource interface {
	DrainCompletions() []BackgroundCompletion
}

// BackgroundSource returns the registry's background completion source, or
// nil. Nil-receiver safe.
func (r *Registry) BackgroundSource() BackgroundSource {
	if r == nil {
		return nil
	}
	return r.Background
}

func (r *Registry) Register(tool Tool) {
	if r == nil || tool == nil {
		return
	}
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	r.tools[tool.Name()] = tool
}

func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Tools() api.Tools {
	if r == nil {
		return nil
	}
	names := r.Names()
	apiTools := make(api.Tools, 0, len(names))
	for _, name := range names {
		tool := r.tools[name]
		apiTools = append(apiTools, api.Tool{
			Type:     "function",
			Function: tool.Schema(),
		})
	}
	return apiTools
}

func (r *Registry) Execute(ctx context.Context, toolCtx ToolContext, call api.ToolCall) (ToolResult, error) {
	tool, ok := r.Get(call.Function.Name)
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool: %s", call.Function.Name)
	}
	return tool.Execute(ctx, toolCtx, call.Function.Arguments.ToMap())
}

func ToolRequiresApproval(tool Tool, args map[string]any) bool {
	if tool == nil {
		return false
	}
	if t, ok := tool.(ApprovalRequired); ok {
		return t.RequiresApproval(args)
	}
	return false
}
