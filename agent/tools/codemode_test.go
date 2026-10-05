package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ParthSareen/o/agent"
)

// countingExecutor records nested calls and answers with fixed content.
type countingExecutor struct {
	mu     sync.Mutex
	calls  []nestedCall
	err    error
	result string
}

type nestedCall struct {
	Parent string
	Name   string
	Args   map[string]any
}

func (e *countingExecutor) ExecuteNested(_ context.Context, parentToolCallID, toolName string, args map[string]any) (agent.ToolResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, nestedCall{parentToolCallID, toolName, args})
	e.mu.Unlock()
	if e.err != nil {
		return agent.ToolResult{}, e.err
	}
	return agent.ToolResult{Content: e.result}, nil
}

func codemodeWithExecutor(t *testing.T, exec agent.NestedExecutor, parentID string) (*Codemode, agent.ToolContext) {
	t.Helper()
	tool := &Codemode{}
	tool.SetNestedExecutor(exec)
	return tool, agent.ToolContext{WorkingDir: "/tmp", ToolCallID: parentID}
}

// TestCodemodeComposesNestedCalls verifies the only host binding routes
// through the nested executor with parent correlation, and the summary
// reports the calls.
func TestCodemodeComposesNestedCalls(t *testing.T) {
	exec := &countingExecutor{result: "NESTED-OK"}
	tool, toolCtx := codemodeWithExecutor(t, exec, "comp-1")

	res, err := tool.Execute(context.Background(), toolCtx, map[string]any{
		"code": `
			const one = tool("upper", {text: "a"});
			const two = tool("upper", {text: one.content});
			if (two.toolCalls !== 2) throw new Error("expected 2");
			return one.content + "/" + two.content;
		`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("nested calls = %d", len(exec.calls))
	}
	for _, call := range exec.calls {
		if call.Parent != "comp-1" {
			t.Fatalf("nested call parent = %q, want comp-1", call.Parent)
		}
		if call.Name != "upper" {
			t.Fatalf("nested call name = %q", call.Name)
		}
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(res.Content), &summary); err != nil {
		t.Fatalf("summary not json: %v (%q)", err, res.Content)
	}
	result, _ := summary["result"].(string)
	if !strings.Contains(result, "NESTED-OK/NESTED-OK") {
		t.Fatalf("composed result = %q", result)
	}
}

// TestCodemodeToolErrorsThrow verifies a failing nested call throws into JS
// (catchable), instead of silently continuing.
func TestCodemodeToolErrorsThrow(t *testing.T) {
	exec := &countingExecutor{err: errors.New("external system down")}
	tool, toolCtx := codemodeWithExecutor(t, exec, "comp-1")

	res, err := tool.Execute(context.Background(), toolCtx, map[string]any{
		"code": `
			try {
				tool("bash", {})
				return "unexpected-success"
			} catch (e) {
				return "caught: " + e.message
			}
		`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Content, "external system down") {
		t.Fatalf("caught error not surfaced: %q", res.Content)
	}
}

// TestCodemodeDeniesHostAccess verifies the sandbox has no I/O surface.
func TestCodemodeDeniesHostAccess(t *testing.T) {
	exec := &countingExecutor{}
	tool, toolCtx := codemodeWithExecutor(t, exec, "p")

	res, err := tool.Execute(context.Background(), toolCtx, map[string]any{
		"code": `
			const probes = {
				fetch: typeof fetch,
				require: typeof require,
				process: typeof process,
				setTimeout: typeof setTimeout,
				Deno: typeof Deno,
				Bun: typeof Bun
			};
			return JSON.stringify(probes);
		`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The script returned JSON.stringify(probes): decode the nested string
	// and assert every probe is undefined — no host bindings leak through.
	var summary struct {
		Probes string `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Content), &summary); err != nil {
		t.Fatalf("summary: %v (%q)", err, res.Content)
	}
	var probes map[string]any
	if err := json.Unmarshal([]byte(summary.Probes), &probes); err != nil {
		t.Fatalf("probes: %v (%q)", err, summary.Probes)
	}
	for name, value := range probes {
		if value != "undefined" {
			t.Fatalf("sandbox leaked %q as %v", name, value)
		}
	}
	if len(exec.calls) != 0 {
		t.Fatalf("unexpected nested calls: %+v", exec.calls)
	}
}

// TestCodemodeTimeout verifies the wall-clock limit interrupts a busy script.
func TestCodemodeTimeout(t *testing.T) {
	exec := &countingExecutor{}
	tool, toolCtx := codemodeWithExecutor(t, exec, "p")
	tool.Timeout = 60 * time.Millisecond

	start := time.Now()
	_, err := tool.Execute(context.Background(), toolCtx, map[string]any{
		"code": "while (true) { }",
	})
	if !errors.Is(err, errCodemodeTimeout) {
		t.Fatalf("want timeout error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout did not interrupt promptly: %v", time.Since(start))
	}
}

// TestCodemodeCancelPropagation verifies parent cancellation interrupts the
// script.
func TestCodemodeCancelPropagation(t *testing.T) {
	exec := &countingExecutor{}
	tool, toolCtx := codemodeWithExecutor(t, exec, "p")
	tool.Timeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()
	_, err := tool.Execute(ctx, toolCtx, map[string]any{
		"code": "while (true) { }",
	})
	if !errors.Is(err, errCodemodeCanceled) {
		t.Fatalf("want cancel error, got %v", err)
	}
}

// TestCodemodeNoExecutor verifies the tool refuses to run outside an active
// run (no executor installed).
func TestCodemodeNoExecutor(t *testing.T) {
	tool := &Codemode{}
	if _, err := tool.Execute(context.Background(), agent.ToolContext{ToolCallID: "p"}, map[string]any{"code": "1"}); !errors.Is(err, errCodemodeUnavailable) {
		t.Fatalf("want unavailable error, got %v", err)
	}
}

// TestCodemodeBoundedConsole verifies console output is bounded.
func TestCodemodeBoundedConsole(t *testing.T) {
	exec := &countingExecutor{}
	tool, toolCtx := codemodeWithExecutor(t, exec, "p")
	tool.maxConsoleBytes = 1000

	res, err := tool.Execute(context.Background(), toolCtx, map[string]any{
		"code": `
			for (let i = 0; i < 10000; i++) {
				console.log("spam line aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa " + i)
			}
			return "done"
		`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var summary struct {
		Console string `json:"console"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Content), &summary); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Result != "done" {
		t.Fatalf("result = %q", summary.Result)
	}
	if len(summary.Console) > 1200 {
		t.Fatalf("console not bounded: %d bytes", len(summary.Console))
	}
}
