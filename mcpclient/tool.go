package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// adapter exposes one MCP tool as an agent.Tool.
type adapter struct {
	server   string
	mcpName  string // tool name on the server
	desc     string
	params   api.ToolFunctionParameters
	session  *mcp.ClientSession
	fullName string
}

func newAdapter(st ServerTool) *adapter {
	return &adapter{
		server:   st.Server,
		mcpName:  st.Tool.Name,
		desc:     st.Tool.Description,
		params:   schemaFromMCP(st.Tool.InputSchema),
		session:  st.Session,
		fullName: toolName(st.Server, st.Tool.Name),
	}
}

// toolName builds the registry name mcp__<server>__<tool>, sanitized to the
// character set model APIs accept for function names.
func toolName(server, tool string) string {
	return "mcp__" + sanitize(server) + "__" + sanitize(tool)
}

func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (a *adapter) Name() string        { return a.fullName }
func (a *adapter) Description() string { return a.desc }

func (a *adapter) Schema() api.ToolFunction {
	return api.ToolFunction{
		Name:        a.fullName,
		Description: a.desc,
		Parameters:  a.params,
	}
}

// RequiresApproval marks every MCP tool as needing approval so nothing
// outside the harness runs silently; --allow-all-tools bypasses this.
func (a *adapter) RequiresApproval(map[string]any) bool { return true }

func (a *adapter) Execute(ctx context.Context, _ agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	res, err := a.session.CallTool(ctx, &mcp.CallToolParams{Name: a.mcpName, Arguments: args})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("mcp call %s: %w", a.mcpName, err)
	}
	text := resultText(res)
	if res.IsError {
		if text == "" {
			text = "mcp tool returned an error"
		}
		return agent.ToolResult{Content: text}, errors.New(text)
	}
	return agent.ToolResult{Content: text}, nil
}

// resultText flattens a CallToolResult for the model: text content blocks are
// joined verbatim; if there is no text but structured content exists it is
// rendered as JSON.
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		default:
			raw, err := json.Marshal(c)
			if err == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err == nil {
			parts = append(parts, string(raw))
		}
	}
	return strings.Join(parts, "\n")
}

// schemaFromMCP converts an MCP inputSchema (arbitrary JSON Schema) into the
// ollama API's typed tool parameters. Unknown schema keywords are dropped;
// the structure models actually consume (type/properties/required/enum/items)
// is preserved.
func schemaFromMCP(input any) api.ToolFunctionParameters {
	raw, err := json.Marshal(input)
	if err != nil {
		return api.ToolFunctionParameters{Type: "object", Properties: api.NewToolPropertiesMap()}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return api.ToolFunctionParameters{Type: "object", Properties: api.NewToolPropertiesMap()}
	}
	return objectSchemaFromMap(m)
}

func objectSchemaFromMap(m map[string]any) api.ToolFunctionParameters {
	out := api.ToolFunctionParameters{Type: "object", Properties: api.NewToolPropertiesMap()}
	if t, ok := m["type"].(string); ok && t != "" {
		out.Type = t
	}
	out.Required = stringSlice(m["required"])
	if defs, ok := m["$defs"]; ok {
		out.Defs = defs
	}
	props, _ := m["properties"].(map[string]any)
	for name, p := range props {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		out.Properties.Set(name, propertyFromMap(pm))
	}
	return out
}

// propertyFromMap converts one JSON Schema property into api.ToolProperty.
func propertyFromMap(m map[string]any) api.ToolProperty {
	var tp api.ToolProperty

	// type may be a string or an array (["string","null"]).
	switch t := m["type"].(type) {
	case string:
		tp.Type = api.PropertyType{t}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				tp.Type = append(tp.Type, s)
			}
		}
	}

	if d, ok := m["description"].(string); ok {
		tp.Description = d
	}
	if e, ok := m["enum"].([]any); ok {
		tp.Enum = e
	}
	if items, ok := m["items"]; ok {
		tp.Items = items
	}
	tp.Required = stringSlice(m["required"])

	switch ao := m["anyOf"].(type) {
	case []any:
		for _, v := range ao {
			if vm, ok := v.(map[string]any); ok {
				tp.AnyOf = append(tp.AnyOf, propertyFromMap(vm))
			}
		}
	}

	if props, ok := m["properties"].(map[string]any); ok && len(props) > 0 {
		nested := api.NewToolPropertiesMap()
		for name, p := range props {
			if pm, ok := p.(map[string]any); ok {
				nested.Set(name, propertyFromMap(pm))
			}
		}
		tp.Properties = nested
	}

	return tp
}

func stringSlice(v any) []string {
	var out []string
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	}
	return out
}
