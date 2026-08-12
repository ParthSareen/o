# o

**o** is a workspace for Ollama's agent harness: the agent core, the tools,
and the agent TUI, in one place you can change and test without touching the
[ollama](https://github.com/ollama/ollama) repo.

## What is here

Most of the code is adapted from the ollama repo, with imports rewritten to
`github.com/ParthSareen/o`.

| Path | Contains |
| --- | --- |
| `agent/` | The harness core: session, events, tool registry, approvals, compactor, skills |
| `agent/tools/` | bash, file, web, and skill tools |
| `cmd/o/` | The entry point of the agent TUI (`package main`) |
| `cmd/tui/chat/` | The interactive chat UI (bubbletea) |
| `cmd/launch/` | A trimmed shim: the spinner and the types that the TUI uses. The integration runners (claude, codex, …) are not included. |
| `cmd/config/`, `cmd/internal/` | Small support packages for the TUI |
| `sessionstore/` | SQLite-backed session persistence. Only in o. |
| `app/` | Native macOS app (SwiftUI). Bundles the agent core and talks to it over `o --pipe`. See `app/README.md`. |
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
o [model]                # start the interactive TUI (uses the last model)
o [model] "prompt"       # print the answer and exit
o --headless <model>     # headless, prompt from args or stdin
o --resume               # resume the most recent session
o --resume-id <id>       # resume a session by ID
o --list                 # list saved sessions
o --name <text> [model]  # start a new session with a name
```

Flags: `--system`, `--allow-all-tools` (no approval prompts), `--no-tools`,
`--multimodal`, `--context-window`, `--headless`, `--pipe`, `--resume`,
`--resume-id`, `--list`, `--name`. Run `o --help` for the full usage text,
which includes rules for headless use by agents.

`--pipe` speaks a machine-readable NDJSON protocol over stdio (prompt/cancel
commands in, the full agent event stream out) for UI frontends like `app/`.
It implies `--allow-all-tools` and `--rlm` unless you set them explicitly.

## The TUI

Slash commands: `/sessions` selects a session to resume, `/name [set <text>]`
shows or sets the session name, `/help` lists all commands.

Keys:

| Key | Action |
| --- | --- |
| `ctrl+t` | Open nvim in the working directory. `O_NVIM` overrides the command. |
| `ctrl+g` | Open the nvim diff viewer (`nvim -c DiffviewOpen`). `O_NVIM_DIFF` overrides the command. |

Both keys suspend the TUI and come back when you exit nvim. They need nvim in
`PATH`. `/nvim` and `/diffview` do the same but are hidden: they are not in
`/help` or in the completions.

The chat renders markdown: headings, code fences, tables, emphasis, links,
images (alt text only), lists, blockquotes, and horizontal rules.

## Sessions

o saves sessions to `~/.o/sessions.db` (SQLite). Each session gets a UUID,
and o appends the messages after each run.

A session can have a name. Set it with `--name` at launch or with
`/name set <text>` in the TUI. `o --list` prints the ID, Name, Model, and
Title of each session; sessions without a name show `(unnamed)`. o upgrades
an old database when it opens it; no manual step is necessary.

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

## Differences from upstream

o adds these on top of the ollama code:

- `sessionstore/` — session persistence, only in o.
- `cmd/o/main.go` — new runner. It does the same as `launchInteractiveModel`
  without the `cmd` package plumbing.
- `cmd/o/headless.go` — headless mode, only in o.
- `cmd/o/model_helpers.go` — simplified copies of `showOrPullModel`,
  `ensureCloudStub`, and `inferThinkingOption`. The `:cloud` suggestion flow
  is removed.
- `cmd/launch/agent_shim.go` — hand-maintained types that the TUI uses,
  instead of the full `cmd/launch` package.
- `cmd/tui/chat/` — session names (`--name`, `/name`), the nvim keys
  (`ctrl+t`, `ctrl+g`), and the extended markdown renderer.
- `patches/17295-syntax-highlighting.diff` — the changes from
  ollama/ollama#17295 (syntax highlighting in fenced code blocks), applied
  in-tree. When the PR merges upstream, delete the patch.

License: MIT (same as ollama).

[watchy]: https://github.com/ParthSareen/watchy
