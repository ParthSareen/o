package mcpclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ParthSareen/o/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- config ---

func TestLoadConfigCommandAndURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	raw := `{
	  "mcpServers": {
	    "echo":  {"command": "/bin/echo-server", "args": ["-v"], "env": {"A": "1"}},
	    "remote": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer x"}}
	  }
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(cfg.Servers))
	}
	echo := cfg.Servers["echo"]
	if echo.Command != "/bin/echo-server" || echo.Args[0] != "-v" || echo.Env["A"] != "1" {
		t.Fatalf("echo config = %+v", echo)
	}
	if cfg.Servers["remote"].URL != "https://example.com/mcp" {
		t.Fatalf("remote config = %+v", cfg.Servers["remote"])
	}
}

func TestLoadConfigMissingFileIsNil(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || cfg != nil {
		t.Fatalf("cfg=%v err=%v, want nil nil", cfg, err)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"neither": `{"mcpServers": {"x": {}}}`,
		"both":    `{"mcpServers": {"x": {"command": "c", "url": "u"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

// --- naming ---

func TestToolNameSanitizes(t *testing.T) {
	got := toolName("my.server", "do/thing")
	want := "mcp__my_server__do_thing"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := toolName("ok-name", "ok_tool"); got != "mcp__ok-name__ok_tool" {
		t.Fatalf("got %q", got)
	}
}

// --- schema conversion ---

func TestSchemaFromMCPRoundTrip(t *testing.T) {
	input := map[string]any{
		"type":     "object",
		"required": []any{"city"},
		"properties": map[string]any{
			"city": map[string]any{"type": "string", "description": "city name"},
			"days": map[string]any{"type": "integer", "description": "forecast days", "enum": []any{float64(1), float64(3)}},
			"opts": map[string]any{
				"type":     "object",
				"required": []any{"units"},
				"properties": map[string]any{
					"units": map[string]any{"type": "string", "enum": []any{"c", "f"}},
				},
			},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"zone": map[string]any{"type": []any{"string", "null"}},
		},
	}
	params := schemaFromMCP(input)
	if params.Type != "object" {
		t.Fatalf("Type = %q", params.Type)
	}
	if len(params.Required) != 1 || params.Required[0] != "city" {
		t.Fatalf("Required = %v", params.Required)
	}
	city, ok := params.Properties.Get("city")
	if !ok || city.Description != "city name" || len(city.Type) != 1 || city.Type[0] != "string" {
		t.Fatalf("city = %+v ok=%v", city, ok)
	}
	days, _ := params.Properties.Get("days")
	if len(days.Enum) != 2 {
		t.Fatalf("days.Enum = %v", days.Enum)
	}
	opts, _ := params.Properties.Get("opts")
	if opts.Properties == nil {
		t.Fatal("nested properties missing")
	}
	units, ok := opts.Properties.Get("units")
	if !ok || len(units.Enum) != 2 || len(opts.Required) != 1 || opts.Required[0] != "units" {
		t.Fatalf("units = %+v required=%v", units, opts.Required)
	}
	tags, _ := params.Properties.Get("tags")
	if tags.Items == nil {
		t.Fatal("tags items missing")
	}
	zone, _ := params.Properties.Get("zone")
	if len(zone.Type) != 2 || zone.Type[1] != "null" {
		t.Fatalf("zone.Type = %v", zone.Type)
	}
}

func TestSchemaFromMCPGarbage(t *testing.T) {
	// non-object schemas must not panic and fall back to a plain object schema
	for _, input := range []any{nil, "string garbage", 42, []any{1}} {
		p := schemaFromMCP(input)
		if p.Type != "object" {
			t.Fatalf("input %v: Type = %q", input, p.Type)
		}
	}
}

// --- in-memory server plumbing for adapter tests ---

type echoInput struct {
	Text string `json:"text" jsonschema:"text to echo back"`
}

func echoTool(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}},
	}, nil, nil
}

func failTool(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "boom: " + in.Text}},
		IsError: true,
	}, nil, nil
}

// connectInMemory starts an in-process MCP server with the echo and fail
// tools and returns a connected client session.
func connectInMemory(t *testing.T) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text"}, echoTool)
	mcp.AddTool(server, &mcp.Tool{Name: "fail", Description: "always fails"}, failTool)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func listServerTools(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestAdapterExecuteEcho(t *testing.T) {
	session := connectInMemory(t)
	tools := listServerTools(t, session)

	a := newAdapter(ServerTool{Server: "test", Tool: tools["echo"], Session: session})
	if a.Name() != "mcp__test__echo" {
		t.Fatalf("Name = %q", a.Name())
	}
	if !a.RequiresApproval(nil) {
		t.Fatal("MCP tools must require approval by default")
	}
	res, err := a.Execute(context.Background(), agent.ToolContext{}, map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echo:hi" {
		t.Fatalf("Content = %q", res.Content)
	}

	s := a.Schema()
	if s.Name != "mcp__test__echo" || s.Description != "echo text" {
		t.Fatalf("schema = %+v", s)
	}
	if len(s.Parameters.Required) != 1 || s.Parameters.Required[0] != "text" {
		t.Fatalf("required = %v", s.Parameters.Required)
	}
	prop, ok := s.Parameters.Properties.Get("text")
	if !ok || len(prop.Type) != 1 || prop.Type[0] != "string" {
		t.Fatalf("text property = %+v", prop)
	}
}

func TestAdapterExecuteErrorSurfacesContent(t *testing.T) {
	session := connectInMemory(t)
	tools := listServerTools(t, session)

	a := newAdapter(ServerTool{Server: "test", Tool: tools["fail"], Session: session})
	res, err := a.Execute(context.Background(), agent.ToolContext{}, map[string]any{"text": "x"})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "boom: x") {
		t.Fatalf("err = %v", err)
	}
	// error content is also surfaced in the result for the model
	if res.Content != "boom: x" {
		t.Fatalf("Content = %q", res.Content)
	}
}

// --- Manager ---

func TestManagerRegisterAllAndWarnings(t *testing.T) {
	// one dead server in config: should warn, not fail the connect
	m := NewManager(&mcp.Implementation{Name: "test", Version: "v0"})
	defer m.Close()
	m.Connect(context.Background(), &Config{Servers: map[string]ServerConfig{
		"dead": {Command: "/definitely/not/a/real/binary-xyz"},
	}})
	if len(m.Warnings()) != 1 {
		t.Fatalf("warnings = %v", m.Warnings())
	}
	if got := m.RegisterAll(&agent.Registry{}); got != 0 {
		t.Fatalf("registered = %d", got)
	}
}

func TestManagerRegisterSkipsNameCollision(t *testing.T) {
	session := connectInMemory(t)
	tools := listServerTools(t, session)

	// two managers worth of the same server tools -> second registration collides
	m1 := NewManager(&mcp.Implementation{Name: "t", Version: "v0"})
	defer m1.Close()
	r := &agent.Registry{}
	r.Register(newAdapter(ServerTool{Server: "dup", Tool: tools["echo"], Session: session}))

	for _, st := range []ServerTool{{Server: "dup", Tool: tools["echo"], Session: session}} {
		m1.mu.Lock()
		m1.tools = append(m1.tools, st)
		m1.mu.Unlock()
	}
	if n := m1.RegisterAll(r); n != 0 {
		t.Fatalf("registered = %d, want 0 (collision skipped)", n)
	}
	if len(m1.Warnings()) != 1 || !strings.Contains(m1.Warnings()[0], "mcp__dup__echo") {
		t.Fatalf("warnings = %v", m1.Warnings())
	}
}

// --- real stdio transport, using this test binary as the child process ---

// TestHelperProcess is not a test; it runs when the test binary re-executes
// itself as an MCP stdio server.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_MCP_HELPER") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "helper", Version: "v0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text"}, echoTool)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStdioTransportEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "GO_MCP_HELPER=1")

	m := NewManager(&mcp.Implementation{Name: "test", Version: "v0"})
	defer m.Close()
	m.Connect(context.Background(), &Config{Servers: map[string]ServerConfig{
		"helper": {Command: cmd.Path, Args: cmd.Args[1:], Env: map[string]string{"GO_MCP_HELPER": "1"}},
	}})
	if len(m.Warnings()) != 0 {
		t.Fatalf("warnings = %v", m.Warnings())
	}
	if len(m.Tools()) == 0 {
		t.Fatal("no tools discovered over stdio")
	}

	r := &agent.Registry{}
	if n := m.RegisterAll(r); n == 0 {
		t.Fatal("no tools registered")
	}
	got, ok := r.Get("mcp__helper__echo")
	if !ok {
		t.Fatalf("registry names = %v", r.Names())
	}
	res, err := got.Execute(context.Background(), agent.ToolContext{}, map[string]any{"text": "stdio-works"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "echo:stdio-works" {
		t.Fatalf("Content = %q", res.Content)
	}
}
