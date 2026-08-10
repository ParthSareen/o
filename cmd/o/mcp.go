package main

// MCP wiring: cmd/o/mcp.go owns the o-side integration between the copied
// upstream agent code and the mcpclient package. Upstream's
// agentToolsRegistry is renamed to agentToolsRegistryBase by sync.sh; the
// wrapper below adds MCP tools for both the TUI and headless paths (both
// funnel through this function).

import (
	"context"
	"fmt"
	"io"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/mcpclient"
	"github.com/ParthSareen/o/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpManager is connected once in main before any registry is built.
var mcpManager *mcpclient.Manager

// setupMCP loads the MCP config (missing file = no-op) and connects servers.
// Failures are non-fatal: they surface as warnings printed by the caller.
func setupMCP(ctx context.Context, configPath string, stderr io.Writer) error {
	if configPath == "" {
		configPath = mcpclient.DefaultConfigPath()
	}
	cfg, err := mcpclient.LoadConfig(configPath)
	if err != nil {
		// invalid config is a hard error: the user explicitly asked for MCP
		return err
	}
	if cfg == nil {
		return nil
	}
	mcpManager = mcpclient.NewManager(&mcp.Implementation{Name: "o", Version: version.Version})
	mcpManager.Connect(ctx, cfg)
	for _, w := range mcpManager.Warnings() {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return nil
}

func closeMCP() {
	if mcpManager != nil {
		_ = mcpManager.Close()
	}
}

// agentToolsRegistry wraps the upstream-built registry with MCP tools.
func agentToolsRegistry(ctx context.Context, client *api.Client, modelName string, skillCatalog *coreagent.SkillCatalog) *coreagent.Registry {
	r := agentToolsRegistryBase(ctx, client, modelName, skillCatalog)
	if mcpManager != nil {
		mcpManager.RegisterAll(r)
	}
	return r
}
