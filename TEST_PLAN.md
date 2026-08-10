# Test plan — headless mode & MCP support

Status as of 2026-08-10: all items below pass (automated) or were verified live
with `glm-5.2:cloud` (marked LIVE).

## 1. Headless mode

Usage: `o <model> "prompt"` · `prompt | o --headless <model>` · add
`--allow-all-tools` to bypass approval prompts (required for tools headless).

| # | Case | Type | Result |
|---|------|------|--------|
| H1 | Plain response streams to stdout, trailing newline, exit 0 | unit (`TestHeadlessPlainResponse`) + LIVE | pass |
| H2 | Positional prompt implies headless | LIVE (`o glm-5.2:cloud "…29*47…"` → `1363`) | pass |
| H3 | `--headless` reads prompt from stdin | LIVE (`echo … \| o --headless`) | pass |
| H4 | Tool round: model→tool→model, result visible on stdout, tool activity on stderr | unit + LIVE (`bash echo HEADLESS_MARKER`) | pass |
| H5 | Approval-required tool without `-y`: not executed, stderr shows denial + reason, exit 1 | unit + LIVE | pass |
| H6 | Approval-required tool with `-y`: executes, exit 0 | unit + LIVE | pass |
| H7 | Prompt becomes the final user message of the first request | unit | pass |
| H8 | Unknown tool names don't crash the run | unit | pass |
| H9 | Thinking deltas → stderr, assistant content → stdout (pipe-safe) | LIVE (observed glm thinking on stderr) | pass |
| H10 | Missing model arg falls back to last used; none → clear error | prior feature, retested in suite | pass |

Exit codes: 0 finished · 1 error / denied / canceled.

Not covered (manual): Ctrl-C mid-run (context is signal-wired), compaction
under long tool loops, multi-round runs at the local-model tool-round guard.

## 2. MCP support

Config: `~/.ollama/mcp.json` (override with `--mcp-config`), Claude-compatible:

```json
{
  "mcpServers": {
    "echo":   {"command": "/path/to/server", "args": [], "env": {}},
    "remote": {"url": "https://example.com/mcp", "headers": {"Authorization": "Bearer …"}}
  }
}
```

Tools appear as `mcp__<server>__<tool>` in the registry (TUI and headless —
one shared injection point). All MCP tools require approval by default.

| # | Case | Type | Result |
|---|------|------|--------|
| M1 | Config parse: command & URL servers, env, headers | unit | pass |
| M2 | Missing config file = no-op (nil config, no error) | unit | pass |
| M3 | Config validation: neither/both of command+url → error | unit | pass |
| M4 | Tool naming: `mcp__server__tool`, sanitizes `.` `/` etc. | unit | pass |
| M5 | JSON Schema → api params round-trip: required, nested objects, enums, array items, type unions | unit | pass |
| M6 | Garbage/non-object schemas fall back to a plain object schema, no panic | unit | pass |
| M7 | Tool call round-trip over in-memory transport | unit | pass |
| M8 | MCP `IsError` results surface content and a Go error | unit | pass |
| M9 | Dead/failed server: warning recorded, harness continues without it | unit | pass |
| M10 | Registry name collision: skipped with warning | unit | pass |
| M11 | Real stdio JSON-RPC end-to-end (spawns test binary as MCP server) | unit/integration | pass |
| M12 | LIVE: discover → register → glm-5.2:cloud calls `mcp__echo__echo` → result relayed, exit 0 | LIVE | pass |

Live reproduction for M12:

```sh
go build -o /tmp/echo-mcp ./mcpclient/testdata/echoserver
echo '{"mcpServers": {"echo": {"command": "/tmp/echo-mcp"}}}' > ~/.ollama/mcp.json
o --allow-all-tools glm-5.2:cloud \
  "Use the mcp__echo__echo tool with text 'hello-o'. Then reply with exactly what the tool returned."
# expect stdout: echo:hello-o
```

Not covered yet: streamable-HTTP servers against a real remote endpoint
(unit covers transport construction only), OAuth flows, servers with >1 page
of tools (pagination loop untested), MCP in the interactive TUI (code path is
shared and unit-covered; needs a TTY manual check), tool-name collisions
between two servers exposing identical sanitized names for *different* tools.

## 3. Regression

`go test ./...` — full suite green; `./sync.sh ../ollama` re-run end-to-end
(confirms the upstream-copy seam, including the `agentToolsRegistryBase`
rename sed, still applies on fresh copies).
