# o

**o** is a minimal coding agent for [ollama](https://github.com/ollama/ollama):
an agent core, tools, and a TUI.

## Install

Both binaries land in `~/go/bin` — keep it on `PATH`.

**o**, the agent CLI:

```sh
go install github.com/ParthSareen/o/cmd/o@latest
# or from a clone of this repo:
git clone https://github.com/ParthSareen/o && cd o && go install ./cmd/o
```

**watchy**, optional but recommended: o uses it to run its dedicated ollama
server on port 11433 (see [Dedicated server](#dedicated-server)). Without it,
o falls back to a shared server on 11434.

```sh
git clone https://github.com/ParthSareen/watchy && cd watchy && go install ./cmd/watchy
```

Note: `go install github.com/ParthSareen/watchy/cmd/watchy@latest` does not
work — watchy's `go.mod` declares the module as `github.com/parth/watchy`, so
no published version resolves. Install from the clone instead.

## What is here

One module: `github.com/ParthSareen/o`.

| Path | Contains |
| --- | --- |
| `agent/` | The harness core: session, events, tool registry, approvals, compactor, skills |
| `agent/tools/` | bash, file, web, and skill tools |
| `cmd/o/` | The entry point of the agent TUI (`package main`) |
| `cmd/tui/chat/` | The interactive chat UI (bubbletea) |
| `cmd/launch/` | A trimmed shim: the spinner and the types that the TUI uses. The integration runners (claude, codex, …) are not included. |
| `cmd/config/`, `cmd/internal/` | Small support packages for the TUI |
| `sessionstore/` | SQLite-backed session persistence. Only in o. |
| `.agents/skills/` | Project skills. `update-o` runs the build/test/install/ship loop for the CLI. |
| `api/`, `auth/`, `envconfig/`, `format/`, `progress/`, `version/`, `logutil/` | Public support packages |
| `internal/` | Internal support packages. These must be copies; you cannot import them across modules. |
| `types/model/` | Model capabilities and names |

## Run

No manual setup: o starts its own dedicated ollama server on port 11433 via
[watchy] and falls back to a shared server on 11434 only when it must — see
[Dedicated server](#dedicated-server).

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

Flags: `--system`, `--allow-all-tools` (no approval prompts), `--auto`
(on by default: a review model grades tool calls; `--auto=false` falls back
to approval prompts), `--review-model` (grading model for auto mode),
`--no-tools`, `--multimodal`, `--context-window`, `--headless`, `--pipe`,
`--request-id`, `--pipe-approvals`, `--resume`, `--resume-id`, `--list`,
`--name`. Run `o --help` for the full usage text, which includes rules for
headless use by agents.

`--pipe` speaks a machine-readable NDJSON protocol over stdio (prompt/cancel
commands in, the full agent event stream out) for UI frontends.
It implies `--allow-all-tools` unless you set the flag explicitly; pass
`--auto` there for review-model grading. Additive protocol pieces:

- `prompt` commands may carry `requestId`; retrying the same ID with the
  same input replays the committed run (`run_replayed` + `run_finished`
  events, no new model or tool work), while reusing the ID with different
  input is rejected as a request conflict.
- Journaled runs emit `run_admitted` before work begins and `run_committed`
  after the terminal state and history are durably committed (always after
  `run_finished`, which is the live completion event as before).
- `--pipe-approvals` opens an approval channel: approval-needing tool calls
  emit `approval_requested` (`approvalId`, the correlated calls) and the
  frontend answers with `{"cmd":"approval","approvalId":...,"allow":...}`.
  Pending requests and decisions are persisted; disconnect, cancel, and
  timeout settle as expired and never imply permission. Stale or duplicate
  replies are rejected. Without this flag pipe keeps granting full tool
  access (its default), and `--pipe-approvals` drops that implicit grant.

Auto mode is the default starting mode. It sits between review mode (prompt
for every tool call via `--auto=false`) and `--allow-all-tools` (run
everything). Tool calls that would prompt go to a
review model instead of the terminal: reads and known read-only shell
commands skip the model entirely, and everything else is graded with the
same decision contract as the Codex Guardian setup in ollama's compat proxy
(risk level, user authorization, outcome, rationale). Denials carry the
reviewer's rationale back to the agent, critical-risk calls are denied even
when the reviewer allows, and anything malformed fails closed — in the TUI a
failed review falls back to the human prompt, headless runs deny.
`--review-model` (or `O_REVIEW_MODEL`) picks the grading model; the default
`selected` uses the session model. In the TUI, `shift+tab` cycles
auto → full access → review. Every model-graded call leaves a `⟳ auto
review` line in the transcript (outcome, risk, duration; `ctrl+o` expands
the rationale), and headless runs log the same line to stderr.

## The TUI

Slash commands (`/help` shows them in the TUI):

| Command | Action |
| --- | --- |
| `/model` | Switch models |
| `/new` | Start a new chat |
| `/think` | Set thinking mode |
| `/tools` | Toggle tools on or off |
| `/system [on\|off]` | Show or set the built-in system prompt |
| `/skills [import codex\|claude\|pi]` | List or import skills |
| `/compact` | Summarize older context |
| `/copy` | Copy last response to the clipboard |
| `/prompt` | Show full prompt, tools, and messages |
| `/save <filename>` | Save request JSON; saved as `<filename>.json` |
| `/sessions` | List and resume past sessions |
| `/resume [<id\|name>]` | Resume the most recent or a matching session |
| `/name [set <text>]` | Show or set the session name |
| `/help` | Show commands (`/?` is an alias) |
| `/bye` | Exit (`/exit` is an alias) |

Keys:

| Key | Action |
| --- | --- |
| `ctrl+t` | Open nvim in the working directory. `O_NVIM` overrides the command. |
| `ctrl+g` | Open the nvim diff viewer (`nvim -c DiffviewOpen`). `O_NVIM_DIFF` overrides the command. |
| `ctrl+o` | Toggle inline tool output, thinking text, and auto-review rationales. |

Both keys suspend the TUI and come back when you exit nvim. They need nvim in
`PATH`. `/nvim` and `/diffview` do the same but are hidden: they are not in
`/help` or in the completions.

Background shell tasks (`background=true`) are killed when the session exits,
so they cannot outlive o; use watchy for processes that should persist.
Recurring checks should use the `poll` tool instead of background sleep loops:
each tick re-runs the command once, and output that differs from the previous
tick interrupts an in-flight run as a background-task notice (unchanged output
is suppressed). Polls stop when the session exits.

The chat renders markdown: headings, code fences, tables, emphasis, links,
images (alt text only), lists, blockquotes, and horizontal rules.

## Sessions

o saves sessions to `~/.o/sessions.db` (SQLite). Each session gets a UUID,
and o appends the messages after each run.

A session can have a name. Set it with `--name` at launch or with
`/name set <text>` in the TUI. `o --list` prints the ID, Name, Model, and
Title of each session; sessions without a name show `(unnamed)`. o upgrades
an old database when it opens it; no manual step is necessary.

Headless runs print their saved session ID on stderr as `session: <id>`
(background-task logs carry the line too), and `o --resume-id <id>
--headless "follow-up"` continues that conversation — the session's model is
reused, so an agent can follow up on a finished child without restating it.

## Dedicated server

o always runs against its own ollama server on port 11433, started through
[watchy] (`OLLAMA_DEBUG=1`, loopback only) — even when a shared server already
listens on 11434. These rules apply:

- o starts the server only if watchy and the `ollama` binary are installed.
- o never stops or replaces a server that runs.
- o reuses a dedicated server from an earlier launch.
- If you set `OLLAMA_HOST`, o uses it as it is.
- If o cannot start a dedicated server, it falls back to a shared server on
  11434 when one answers.

To manage the dedicated server:

```sh
watchy logs o-ollama-11433
watchy stop o-ollama-11433
```

## Compared to ollama

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
  in-tree.

License: MIT.

[watchy]: https://github.com/ParthSareen/watchy
