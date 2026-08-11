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
o --resume <id>                  # resume a saved session
o --list                          # list saved sessions
o /sessions                       # in TUI: pick a session to resume
```

Flags: `--system`, `--allow-all-tools` (skip approval prompts), `--no-tools`,
`--multimodal`, `--context-window`, `--headless`, `--resume`, `--list`.

Sessions are persisted to `~/.o/sessions.db` (SQLite). Each conversation gets
a UUID; messages are appended after each run. Use `/sessions` in the TUI or
`o --list` from the shell to browse and resume past sessions.

Requires a local Ollama server (`ollama serve`) like the normal CLI. If none
answers on the default port, `o` starts a **debug server on port 11433** via
[watchy] (`OLLAMA_DEBUG=1`, loopback-only) — only when watchy and the `ollama`
binary are installed, and never by restarting or preempting a server that's
already running. It reuses a debug server from an earlier launch, honors an
explicit `OLLAMA_HOST` as-is, and is a quiet no-op when the default server is
up. Manage it with `watchy logs o-ollama-debug-11433` / `watchy stop`.

## MCP

Removed for now; the work (including OAuth 2.0 authorization-code+PKCE flow
with dynamic client registration and a loopback callback) is parked on the
`mcp` branch.

## Divergence from upstream

Kept intentionally small so `sync.sh` re-syncs cleanly:

- `cmd/o/main.go` — new runner, mirrors `launchInteractiveModel` without
  the `cmd` package plumbing
- `cmd/o/headless.go` — o-only headless mode
- `patches/17295-syntax-highlighting.diff` — sniped unmerged PR
  ollama/ollama#17295 (syntax-highlighted fenced code blocks in the TUI);
  sync.sh re-applies it after each sync — delete the patch once it merges
  upstream
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
