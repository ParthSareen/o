# Test plan — headless mode & MCP support

Status as of 2026-08-10: headless items all pass (automated or live with
glm-5.2:cloud). MCP was removed pending redesign; see `git log mcp`.

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

Removed for now — parked on the `mcp` branch (config, transports, tool
adapter, full OAuth consent flow; last live gap: Mintlify issues a second
consent window, likely the standalone-SSE connection re-challenging).

## 3. Regression

`go test ./...` — full suite green; `./sync.sh ../ollama` re-run end-to-end
(confirms the upstream-copy seam, including the `agentToolsRegistryBase`
rename sed, still applies on fresh copies).
