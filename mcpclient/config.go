// Package mcpclient connects the agent harness to MCP (Model Context
// Protocol) servers and exposes their tools to the agent tool registry.
//
// Config files are Claude-Code compatible:
//
//	{
//	  "mcpServers": {
//	    "echo":  {"command": "/path/to/server", "args": ["--flag"], "env": {"K": "v"}},
//	    "remote": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer x"}}
//	  }
//	}
package mcpclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ServerConfig describes one MCP server. Exactly one of Command (stdio
// transport) or URL (streamable HTTP transport) must be set.
type ServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Config is the top-level mcp.json document.
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// DefaultConfigPath is ~/.ollama/mcp.json (co-located with the rest of the
// ollama-adjacent client config this binary already uses).
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ollama", "mcp.json")
}

// LoadConfig reads and validates an MCP config file. A missing file is not an
// error; it returns a nil Config and nil error.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mcp config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse mcp config %s: %w", path, err)
	}
	for name, sc := range cfg.Servers {
		if err := sc.validate(); err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", name, err)
		}
	}
	return &cfg, nil
}

func (c ServerConfig) validate() error {
	switch {
	case c.Command != "" && c.URL != "":
		return fmt.Errorf("set either command or url, not both")
	case c.Command == "" && c.URL == "":
		return fmt.Errorf("one of command or url is required")
	}
	return nil
}
