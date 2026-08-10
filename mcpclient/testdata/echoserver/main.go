// Command echoserver is a minimal MCP stdio server used by the live
// verification steps in TEST_PLAN.md. It exposes a single "echo" tool.
package main

import (
	"context"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text" jsonschema:"text to echo back"`
}

func echo(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}},
	}, nil, nil
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "v0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo the given text back prefixed with 'echo:'"}, echo)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
