# o

Ollama Agent Harness — a standalone workspace for iterating on Ollama's agent
harness (agent core, tools, and agent TUI) without touching `main` of the
[ollama](https://github.com/ollama/ollama) repo.

## What's here

Copied from the ollama repo (see `sync.sh`), imports rewritten to
`github.com/ParthSareen/o`:

| Path | What |
| --- | --- |
| `agent/` | harness core: session, events, tool registry, approvals, compactor, skills |
| `agent/tools/` | bash, file, web, skill tools |
| `cmd/o/` | the agent TUI entry point (`ollama`'s `cmd/agent_tui.go` as `package main`) |
| `cmd/tui/`, `cmd/tui/chat/` | interactive chat UI (bubbletea) |
| `cmd/launch/` | **trimmed shim** — spinner + the ~10 types the TUI references; the integration runners (claude/codex/…) are intentionally excluded |
| `cmd/config/`, `cmd/internal/filedata/`, `cmd/internal/fileutil/` | small support packages used by the TUI |
| `api/`, `auth/`, `envconfig/`, `format/`, `progress/`, `version/`, `logutil/` | public support packages |
| `internal/cloud/`, `internal/modelref/`, `internal/orderedmap/` | `internal/` packages — must be copied (not importable across modules) |
| `types/model/` | model capabilities, names |

## Run

```sh
go install ./cmd/o
o [model]                          # interactive TUI (remembers last model)
o [model] "prompt"                 # headless: answer and exit
echo "prompt" | o --headless <model>
```

Flags: `--system`, `--allow-all-tools` (skip approval prompts), `--no-tools`,
`--multimodal`, `--context-window`, `--headless`, `--mcp-config`.

Requires a local Ollama server (`ollama serve`) like the normal CLI.

## MCP

MCP servers are configured in `~/.ollama/mcp.json` (Claude-compatible;
override with `--mcp-config`). Tools appear as `mcp__<server>__<tool>` in both
the TUI and headless mode, and always require approval unless
`--allow-all-tools` is set. Dead servers warn on stderr and are skipped. See
`TEST_PLAN.md` for the config schema and a runnable example.

## Divergence from upstream

Kept intentionally small so `sync.sh` re-syncs cleanly:

- `cmd/o/main.go` — new runner, mirrors `launchInteractiveModel` without
  the `cmd` package plumbing
- `cmd/o/mcp.go`, `cmd/o/headless.go` — o-only features: MCP wiring (upstream
  `agentToolsRegistry` is renamed to `agentToolsRegistryBase` by sync.sh and
  wrapped) and headless mode
- `mcpclient/` — o-only MCP client (config, session manager, tool adapter)
- `cmd/o/model_helpers.go` — simplified copies of `showOrPullModel`,
  `ensureCloudStub`, `inferThinkingOption` from `cmd/cmd.go` (drops the
  `:cloud` suggestion flow; thinking inference takes capabilities directly)
- `cmd/launch/agent_shim.go` — hand-maintained types (`LauncherState`,
  `SelectionItem`, `PlanSatisfies`, `OpenBrowser`, …) instead of the full
  `cmd/launch` package

## Syncing from upstream

```sh
./sync.sh               # pulls from ../ollama, rewrites imports, builds, tests
./sync.sh /path/to/ollama
```

License: MIT (same as ollama).
