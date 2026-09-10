# o

**o** is a standalone workspace for Ollama's agent harness: the agent core,
the tools, and the agent TUI, in one place you can change and test without
touching the [ollama](https://github.com/ollama/ollama) repo. Most code is
adapted from ollama with imports rewritten to `github.com/ParthSareen/o`.

This is NOT the ollama repo. Do not reach for ollama build/test commands here.

## Build & run

You need the `ollama` binary on PATH. o runs against its own dedicated ollama
server on port 11433 (started via [watchy]; see "Dedicated server" below).

Install the CLI (lands at `~/go/bin/o`, must be on PATH):

```sh
go install ./cmd/o
o --list          # smoke check
```

Run:

```sh
o [model]                # interactive TUI (remembers last model)
o [model] "prompt"       # headless: answer once and exit
o --headless [model]     # headless, prompt from args or stdin
o --resume               # resume most recent session
o --resume-id <id>       # resume a session by ID
o --list                 # list saved sessions
o --name <text> [model]  # new session with a name
```

Flags: `--system`, `--allow-all-tools`, `--auto`, `--review-model`,
`--no-tools`, `--multimodal`, `--context-window`, `--headless`, `--pipe`,
`--resume`, `--resume-id`, `--list`, `--name`. Run `o --help` for the full
usage text.

`--pipe` speaks a machine-readable NDJSON protocol over stdio (prompt/cancel/
compact/set_think/set_tools/inspect in, full agent event stream out) for UI
frontends like `app/`. It implies `--allow-all-tools` unless set explicitly.

## Test

```sh
go test ./...
cd app && swift test && cd ..    # macOS app (SwiftUI)
```

CGO must stay enabled for the Go core — `sessionstore/` uses go-sqlite3.
The app build script handles this; never set `CGO_ENABLED=0`.

## macOS app

Native SwiftUI shell over the agent core. Spawns bundled `o --pipe` and
drives the session over NDJSON. See `app/README.md`.

```sh
sh app/scripts/build-app.sh             # bundle at app/build/O.app
sh app/scripts/build-app.sh --install   # also copy to /Applications/O.app
sh app/scripts/build-app.sh --dmg       # also produce app/build/O-<version>.dmg
```

Dev loop without bundling:

```sh
cd app && O_BINARY=/path/to/o swift run OApp
```

Slash commands in the TUI: `/sessions` selects a session to resume,
`/resume [<id|name>]` resumes the most recent or a matching session,
`/name [set <text>]` shows or sets the session name, `/think` sets thinking
mode, `/tools` toggles tools, `/compact` summarizes older context, `/copy`
copies the last reply, `/help` lists all commands. The app composer supports
`/copy`, `/compact`, `/think`, `/tools`, mid-line `/<skill>` tokens, and ↑/↓
prompt-history recall; Esc or ⌘. cancels a run.

The `update-o` skill (`.agents/skills/update-o/`) runs the full
build/test/package/install/ship loop end to end.

## Worktrees

Branch worktrees live under `~/.herdr/worktrees/o/<branch>/` — the `herdr`
convention. The macOS app's binary lookup checks `~/.herdr/worktrees/o/ui/o`
first, so keep a built `o` there for dev runs:

```sh
git worktree add ~/.herdr/worktrees/o/ui ui      # one-time
cd ~/.herdr/worktrees/o/ui && go build -o o ./cmd/o
```

`ui` is the primary dev branch — it and `main` move together. Per `update-o`,
work in the `ui` worktree, then land on both:

```sh
git fetch origin main
git merge origin/main -m "Merge branch 'main' into ui"   # if ui isn't current
git push origin ui ui:main
```

List current worktrees: `git worktree list`.

## Layout

| Path | Contains |
| --- | --- |
| `agent/` | Harness core: session, events, tool registry, approvals, compactor, skills |
| `agent/tools/` | bash, file, web, and skill tools |
| `cmd/o/` | Entry point of the agent TUI (`package main`) |
| `cmd/tui/chat/` | Interactive chat UI (bubbletea) |
| `cmd/launch/` | Trimmed shim: spinner + types the TUI uses. Integration runners are not included. |
| `cmd/config/`, `cmd/internal/` | Small support packages for the TUI |
| `sessionstore/` | SQLite-backed session persistence. Only in o. |
| `app/` | Native macOS app (SwiftUI). Bundles the agent core, talks over `o --pipe`. See `app/README.md`. |
| `.agents/skills/` | Project skills (`update-o`). |
| `api/`, `auth/`, `envconfig/`, `format/`, `progress/`, `version/`, `logutil/` | Public support packages |
| `internal/` | Internal support packages. These must be copies; you cannot import them across modules. |
| `types/model/` | Model capabilities and names |

## Sessions

o saves sessions to `~/.o/sessions.db` (SQLite). Each session gets a UUID; o
appends messages after each run. A session can have a name (`--name` at
launch or `/name set <text>` in the TUI). o upgrades an old database on open.

## TUI

Slash commands and app-composer parity are listed under [Build & run].

Keys: `ctrl+t` opens nvim in the working directory (`O_NVIM` overrides);
`ctrl+g` opens the nvim diff viewer (`nvim -c DiffviewOpen`, `O_NVIM_DIFF`
overrides). Both suspend the TUI and resume when nvim exits. Need nvim in PATH.

The chat renders markdown: headings, code fences, tables, emphasis, links,
images (alt text only), lists, blockquotes, horizontal rules.

## Code style

- Concise commit messages with scope prefix (e.g. `cmd/o: add headless flag`).
- No co-author tags, no sign-offs, no "Generated by" metadata.
- No AI-slop comments — only leave comments where genuinely helpful.
- Don't mention the assistant in commits or code.
- `internal/` packages are copies, not cross-module imports.

## Dedicated server

o always runs against its own ollama server on port 11433, started through
[watchy] (`OLLAMA_DEBUG=1`, loopback only) — even when a shared server already
listens on 11434. Rules: o reuses a dedicated server from an earlier launch;
never stops or replaces a running server; honors `OLLAMA_HOST`. If watchy or
the `ollama` binary is missing, o falls back to a shared server on 11434 if
one answers.

```sh
watchy logs o-ollama-11433
watchy stop o-ollama-11433
```

## Differences from upstream

o adds on top of the ollama code: `sessionstore/`, `cmd/o/main.go` (new
runner), `cmd/o/headless.go`, `cmd/o/model_helpers.go` (simplified copies),
`cmd/launch/agent_shim.go` (hand-maintained types), `cmd/tui/chat/` (session
names, nvim keys, extended markdown renderer), and
`patches/17295-syntax-highlighting.diff` (ollama/ollama#17295 applied in-tree;
delete the patch when the PR merges upstream).

License: MIT (same as ollama).

[watchy]: https://github.com/ParthSareen/watchy