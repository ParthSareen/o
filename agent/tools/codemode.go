package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// Failure modes for the codemode sandbox.
var (
	errCodemodeUnavailable = errors.New("codemode is unavailable outside an active run")
	errCodemodeNoCode      = errors.New("codemode requires a non-empty \"code\" argument")
	errCodemodeNoName      = errors.New("tool(name, args) requires a non-empty tool name string")
	errCodemodeArgs        = errors.New("tool(name, args) requires args to be a plain object of JSON-compatible values")
	errCodemodeTimeout     = errors.New("codemode script exceeded its time limit")
	errCodemodeCanceled    = errors.New("codemode script was canceled")
)

// Codemode composes multiple tool calls inside one bounded JavaScript
// sandbox (goja). The sandbox gets no filesystem, network, process, or timer
// access by construction — the only host binding is tool(name, args), which
// routes through the session's nested-call path and receives the same
// approval, journaling, blocking, and cancellation rules as top-level calls.
// A failed script never rolls back the external side effects of calls that
// already ran; their outcomes stay journaled.
type Codemode struct {
	mu sync.Mutex
	// exec is installed per-run by the session (agent.NestedExecutorSetter).
	exec agent.NestedExecutor
	// Timeout bounds one script's wall clock. Zero selects the default
	// (overridable via O_CODEMODE_TIMEOUT_MS).
	Timeout time.Duration
	// Optional test overrides for the output caps.
	maxConsoleBytes int
	maxResultBytes  int
}

const (
	codemodeDefaultTimeout = 30 * time.Second
	codemodeMaxConsole     = 16 << 10
	codemodeMaxResult      = 64 << 10
)

// SetNestedExecutor implements agent.NestedExecutorSetter.
func (c *Codemode) SetNestedExecutor(executor agent.NestedExecutor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exec = executor
}

func (c *Codemode) Name() string { return "codemode" }

func (c *Codemode) Description() string {
	return "Compose multiple tool calls in one sandboxed JavaScript snippet. Call tool(name, argsObject) synchronously for each tool call; tool() throws on failure. No filesystem, network, process, or timer access is available. Prefer single direct tool calls; reach for codemode only when several calls must be combined."
}

func (c *Codemode) Schema() api.ToolFunction {
	props := api.NewToolPropertiesMap()
	props.Set("code", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "JavaScript to run. Use tool(name, argsObject) to invoke a registered tool; it returns {content, toolCalls} or throws.",
	})
	return api.ToolFunction{
		Name:        c.Name(),
		Description: c.Description(),
		Parameters: api.ToolFunctionParameters{
			Type:       "object",
			Properties: props,
			Required:   []string{"code"},
		},
	}
}

// ReplaySafe is false: a script's nested calls can reach the outside world,
// so re-running an interrupted script is not automatically safe.
func (c *Codemode) ReplaySafe() bool { return false }

func (c *Codemode) codemodeTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	if ms := strings.TrimSpace(os.Getenv("O_CODEMODE_TIMEOUT_MS")); ms != "" {
		if parsed, err := time.ParseDuration(ms + "ms"); err == nil && parsed > 0 {
			return parsed
		}
		if parsed, err := time.ParseDuration(ms); err == nil && parsed > 0 {
			return parsed
		}
	}
	return codemodeDefaultTimeout
}

// codemodeRun is the per-Execute sandbox state. The console buffer stops
// accepting lines once its cap is hit, so a spamming loop cannot grow memory.
type codemodeRun struct {
	console     strings.Builder
	consoleCap  int
	consoleFull bool
	toolCalls   int
}

func (cr *codemodeRun) writeLog(line string) {
	if cr.consoleFull {
		return
	}
	if cr.console.Len()+len(line) > cr.consoleCap {
		cr.console.WriteString("[codemode console output truncated]\n")
		cr.consoleFull = true
		return
	}
	cr.console.WriteString(line)
}

func (c *Codemode) Execute(ctx context.Context, toolCtx agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	c.mu.Lock()
	exec := c.exec
	c.mu.Unlock()
	if exec == nil {
		return agent.ToolResult{}, errCodemodeUnavailable
	}

	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return agent.ToolResult{}, errCodemodeNoCode
	}

	parentCallID := toolCtx.ToolCallID
	state := &codemodeRun{consoleCap: c.consoleCap()}
	rt := goja.New()
	c.installBindings(rt, ctx, exec, parentCallID, state)

	timer := time.AfterFunc(c.codemodeTimeout(), func() { rt.Interrupt(errCodemodeTimeout) })
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			rt.Interrupt(errCodemodeCanceled)
		case <-cancelWatch:
		}
	}()
	defer func() {
		close(cancelWatch)
		timer.Stop()
		rt.ClearInterrupt()
	}()

	// Scripts return a value; the sandbox wraps the snippet in an IIFE so a
	// top-level return is allowed (RunString evaluates statements, not a
	// function body).
	value, runErr := rt.RunString("(function(){\n" + code + "\n})()")
	if runErr != nil {
		if interruptedBy(runErr, errCodemodeTimeout) {
			return agent.ToolResult{}, errCodemodeTimeout
		}
		if interruptedBy(runErr, errCodemodeCanceled) {
			return agent.ToolResult{}, errCodemodeCanceled
		}
		return agent.ToolResult{}, fmt.Errorf("codemode: %s", capRunesUTF8(runErr.Error(), 2000))
	}

	return agent.ToolResult{Content: c.summarize(rt, value, state)}, nil
}

func (c *Codemode) summarize(rt *goja.Runtime, value goja.Value, state *codemodeRun) string {
	out := marshalForJSON(map[string]any{"console": state.console.String()})
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return out
	}
	// Export directly into the summary: marshaling an intermediate []byte
	// would base64-encode it.
	if wrapped := marshalForJSON(map[string]any{"console": state.console.String(), "result": value.Export()}); wrapped != "" {
		out = wrapped
	}
	return capRunesUTF8(out, c.resultCap())
}

// marshalForJSON marshals and never fails the run over a summary encode.
func marshalForJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (c *Codemode) consoleCap() int {
	if c.maxConsoleBytes > 0 {
		return c.maxConsoleBytes
	}
	return codemodeMaxConsole
}

func (c *Codemode) resultCap() int {
	if c.maxResultBytes > 0 {
		return c.maxResultBytes
	}
	return codemodeMaxResult
}

// installBindings sets up the sandbox surface: console plus tool(). Nothing
// else is added, so require/process/network/timers do not exist by
// construction.
func (c *Codemode) installBindings(rt *goja.Runtime, ctx context.Context, exec agent.NestedExecutor, parentCallID string, state *codemodeRun) {
	logFn := func(kind string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, arg := range call.Arguments {
				parts = append(parts, arg.String())
			}
			state.writeLog("[" + kind + "] " + strings.Join(parts, " ") + "\n")
			return goja.Undefined()
		}
	}
	consoleObj := rt.NewObject()
	_ = consoleObj.Set("log", logFn("log"))
	_ = consoleObj.Set("info", logFn("info"))
	_ = consoleObj.Set("warn", logFn("warn"))
	_ = consoleObj.Set("error", logFn("error"))
	rt.Set("console", consoleObj)

	toolFn := func(call goja.FunctionCall) goja.Value {
		nameVal := call.Argument(0)
		if nameVal == nil || goja.IsUndefined(nameVal) || goja.IsNull(nameVal) {
			panic(rt.NewGoError(errCodemodeNoName))
		}
		name, ok := nameVal.Export().(string)
		if !ok || strings.TrimSpace(name) == "" {
			panic(rt.NewGoError(errCodemodeNoName))
		}
		var args map[string]any
		if arg := call.Argument(1); arg != nil && !goja.IsUndefined(arg) && !goja.IsNull(arg) {
			exported, ok := arg.Export().(map[string]any)
			if !ok {
				panic(rt.NewGoError(errCodemodeArgs))
			}
			args = exported
		}
		state.toolCalls++
		result, err := exec.ExecuteNested(ctx, parentCallID, name, args)
		if err != nil {
			panic(rt.NewGoError(err))
		}
		obj := rt.NewObject()
		_ = obj.Set("content", result.Content)
		_ = obj.Set("toolCalls", state.toolCalls)
		return rt.ToValue(obj)
	}
	rt.Set("tool", toolFn)
}

// interruptedBy reports whether err is a goja interrupt caused by target.
func interruptedBy(err, target error) bool {
	var interrupted *goja.InterruptedError
	if !errors.As(err, &interrupted) {
		return false
	}
	if cause, ok := interrupted.Value().(error); ok {
		return errors.Is(cause, target)
	}
	return false
}

// capRunesUTF8 truncates to max bytes on a rune boundary.
func capRunesUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut, size := 0, 0
	for i, r := range s {
		size += utf8.RuneLen(r)
		if size > max {
			break
		}
		cut = i + utf8.RuneLen(r)
	}
	return s[:cut]
}
