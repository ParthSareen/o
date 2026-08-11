# o

**o** is a workspace for Ollama's agent harness. You can change and test the
agent core, the tools, and the agent TUI here. You do not touch the `main`
branch of the [ollama](https://github.com/ollama/ollama) repo.

## What is here

The code comes from the ollama repo. `sync.sh` copies the code and rewrites
the imports to `github.com/ParthSareen/o`.

| Path | Contains |
| --- | --- |
| `agent/` | The harness core: session, events, tool registry, approvals, compactor, skills |
| `agent/tools/` | bash, file, web, and skill tools |
| `cmd/o/` | The entry point of the agent TUI (`package main`) |
| `cmd/tui/chat/` | The interactive chat UI (bubbletea) |
| `cmd/launch/` | A trimmed shim: the spinner and the types that the TUI uses. The integration runners (claude, codex, …) are not included. |
| `cmd/config/`, `cmd/internal/` | Small support packages for the TUI |
| `api/`, `auth/`, `envconfig/`, `format/`, `progress/`, `version/`, `logutil/` | Public support packages |
| `internal/` | Internal support packages. These must be copies; you cannot import them across modules. |
| `types/model/` | Model capabilities and names |

## Run

You need a local Ollama server. Start it with `ollama serve`.

Install:

```sh
go install ./cmd/o
```

Use:

```sh
o [model]                         # start the interactive TUI (uses the last model)
o [model] "prompt"                # print the answer and exit
echo "prompt" | o --headless <model>
o --resume <id>                   # resume a saved session
o --list                          # list saved sessions
```

In the TUI, use `/sessions` to select a session to resume.

Flags: `--system`, `--allow-all-tools` (no approval prompts), `--no-tools`,
`--multimodal`, `--context-window`, `--headless`, `--resume`, `--list`.

## Sessions

o saves sessions to `~/.o/sessions.db` (SQLite). Each session gets a UUID.
o appends the messages after each run.

## Debug server

If no server answers on the default port, o can start a debug server on port
11433 through [watchy] (`OLLAMA_DEBUG=1`, loopback only). These rules apply:

- o starts the debug server only if watchy and the `ollama` binary are installed.
- o never stops or replaces a server that runs.
- o reuses a debug server from an earlier launch.
- If you set `OLLAMA_HOST`, o uses it as it is.
- If the default server runs, o does nothing.

To manage the debug server:

```sh
watchy logs o-ollama-debug-11433
watchy stop o-ollama-debug-11433
```

## MCP

MCP is removed for now. The code stays on the `mcp` branch. It includes the
OAuth 2.0 authorization-code flow with PKCE, dynamic client registration, and
a loopback callback.

## Differences from upstream

Keep the differences small. Then `sync.sh` can re-sync cleanly:

- `cmd/o/main.go` — new runner. It does the same as `launchInteractiveModel`
  without the `cmd` package plumbing.
- `cmd/o/headless.go` — headless mode, only in o.
- `patches/17295-syntax-highlighting.diff` — copies the changes from
  ollama/ollama#17295 (syntax highlighting in fenced code blocks). `sync.sh`
  applies this patch after each sync. When the PR merges upstream, delete
  the patch.
- `cmd/o/model_helpers.go` — simplified copies of `showOrPullModel`,
  `ensureCloudStub`, and `inferThinkingOption`. The `:cloud` suggestion flow
  is removed.
- `cmd/launch/agent_shim.go` — hand-maintained types that the TUI uses,
  instead of the full `cmd/launch` package.

## Sync from upstream

```sh
./sync.sh                   # copy from ../ollama, rewrite imports, build, test
./sync.sh /path/to/ollama
```

License: MIT (same as ollama).
