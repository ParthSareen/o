package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// fakeClient plays scripted chat-response chunks.
type fakeClient struct {
	calls     int
	responses [][]api.ChatResponse
	requests  []*api.ChatRequest
}

func (c *fakeClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	c.requests = append(c.requests, req)
	if c.calls >= len(c.responses) {
		return nil
	}
	for _, r := range c.responses[c.calls] {
		if err := fn(r); err != nil {
			return err
		}
	}
	c.calls++
	return nil
}

var _ coreagent.ChatClient = (*fakeClient)(nil)

// upperTool is an approval-free test tool echoing args uppercased.
type upperTool struct{ called int }

func (t *upperTool) Name() string        { return "upper" }
func (t *upperTool) Description() string { return "uppercase text" }
func (t *upperTool) Schema() api.ToolFunction {
	props := api.NewToolPropertiesMap()
	props.Set("text", api.ToolProperty{Type: api.PropertyType{"string"}})
	return api.ToolFunction{Name: t.Name(), Description: t.Description(), Parameters: api.ToolFunctionParameters{
		Type: "object", Properties: props, Required: []string{"text"},
	}}
}
func (t *upperTool) Execute(_ context.Context, _ coreagent.ToolContext, args map[string]any) (coreagent.ToolResult, error) {
	t.called++
	text, _ := args["text"].(string)
	return coreagent.ToolResult{Content: strings.ToUpper(text)}, nil
}

// riskyTool requires approval.
type riskyTool struct{ called int }

func (t *riskyTool) Name() string        { return "risky" }
func (t *riskyTool) Description() string { return "needs approval" }
func (t *riskyTool) Schema() api.ToolFunction {
	return api.ToolFunction{Name: t.Name(), Parameters: api.ToolFunctionParameters{Type: "object", Properties: api.NewToolPropertiesMap()}}
}
func (t *riskyTool) RequiresApproval(map[string]any) bool { return true }
func (t *riskyTool) Execute(_ context.Context, _ coreagent.ToolContext, _ map[string]any) (coreagent.ToolResult, error) {
	t.called++
	return coreagent.ToolResult{Content: "risky ran"}, nil
}

func textChunks(text string) []api.ChatResponse {
	return []api.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: text}, Done: true},
	}
}

func toolCallChunks(name string, kv map[string]any) []api.ChatResponse {
	args := api.NewToolCallFunctionArguments()
	for k, v := range kv {
		args.Set(k, v)
	}
	return []api.ChatResponse{
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID:       "call_1",
			Function: api.ToolCallFunction{Name: name, Arguments: args},
		}}}, Done: true},
	}
}

func runFake(t *testing.T, fc *fakeClient, registry *coreagent.Registry, allowAll bool) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: allowAll, Options: map[string]any{}}
	code = runHeadlessSession(context.Background(), fc, opts, nil, registry, "system prompt", "do the thing", t.TempDir(), &out, &errb)
	return out.String(), errb.String(), code
}

func TestHeadlessPlainResponse(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("hello world")}}
	stdout, _, code := runFake(t, fc, &coreagent.Registry{}, false)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if stdout != "hello world\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestHeadlessToolRound(t *testing.T) {
	tool := &upperTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)

	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("upper", map[string]any{"text": "hi"}),
		textChunks("done: tool said HI"),
	}}
	stdout, stderr, code := runFake(t, fc, registry, false)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr)
	}
	if tool.called != 1 {
		t.Fatalf("tool called %d times", tool.called)
	}
	if !strings.Contains(stdout, "done: tool said HI") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "→ upper") || !strings.Contains(stderr, "✓ upper done") {
		t.Fatalf("stderr = %q", stderr)
	}
	// second model request must include the tool result for the model
	if len(fc.requests) != 2 {
		t.Fatalf("requests = %d", len(fc.requests))
	}
	last := fc.requests[1].Messages[len(fc.requests[1].Messages)-1]
	if last.Role != "tool" || last.Content != "HI" {
		t.Fatalf("last message = %+v", last)
	}
}

func TestHeadlessApprovalDeniedWithoutFlag(t *testing.T) {
	tool := &riskyTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)

	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("risky", nil),
	}}
	_, stderr, code := runFake(t, fc, registry, false)
	if tool.called != 0 {
		t.Fatal("risky tool must not execute without approval")
	}
	if !strings.Contains(stderr, "✗ risky denied") || !strings.Contains(stderr, "--allow-all-tools") {
		t.Fatalf("stderr = %q (want denial + actionable reason)", stderr)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (denied)", code)
	}
}

func TestHeadlessApprovalGrantedWithFlag(t *testing.T) {
	tool := &riskyTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)

	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("risky", nil),
		textChunks("risky ran fine"),
	}}
	stdout, stderr, code := runFake(t, fc, registry, true)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr)
	}
	if tool.called != 1 {
		t.Fatal("risky tool should execute with --allow-all-tools")
	}
	if strings.Contains(stderr, "denied") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stdout, "risky ran fine") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestHeadlessPromptBecomesUserMessage(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("ok")}}
	runFake(t, fc, &coreagent.Registry{}, false)
	if len(fc.requests) == 0 {
		t.Fatal("no request")
	}
	msgs := fc.requests[0].Messages
	if msgs[len(msgs)-1].Role != "user" || msgs[len(msgs)-1].Content != "do the thing" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestHeadlessUnknownToolDoesNotPanic(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("nonexistent", nil),
		textChunks("that tool does not exist"),
	}}
	_, _, code := runFake(t, fc, &coreagent.Registry{}, true)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
}
