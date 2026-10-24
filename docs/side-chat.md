# Side chat (split pane)

Status: **implemented, not landed**. Branch `side-chat`, worktree
`~/.worktrees/o/side-chat`, tip `0051145` (based on `main` @ `676ca4d`).
`go test ./...` and `go vet ./...` are green. One end-to-end check was run
against a live ollama server (see [Verification](#verification)).

## What it does

`/side` forks the conversation into a right-hand pane. Both panes are real
agent chats: they stream and run tools at the same time, and neither writes
into the other's history.

- `/side` opens the pane (and focuses it). `/side close` closes it.
- `opt+]` or `ctrl+]` opens the pane, or switches focus between panes.
- Clicking a pane focuses it; the wheel scrolls the pane under the pointer.
- `esc` twice, `ctrl+c` twice, or `ctrl+d` twice closes an idle, unfocused-by-
  then pane. While a pane run is active the first press cancels it.
- In-pane commands: `/model` (fork only), `/clear` (back to the inherited
  snapshot), `/close`, `/help`, and skills. Anything else replies with a
  notice instead of being sent to the model.
- The pane header shows `side`, the fork's model, its context estimate, a
  running spinner, and `approval needed` when a tool call is waiting.
- Approval prompts and metrics are per pane; permission *mode* is shared.

## Design

### Fork semantics

- The fork inherits a **snapshot** of the root's committed messages at open
  time, plus the system prompt, working directory, model, thinking options,
  compactor, and permission mode. Later root turns do not appear in the fork.
- The fork is **ephemeral**: no `sessions` row, no journal, no prompt-history
  writes, and it is never listed by `/sessions`. Closing the pane discards it.
- History inheritance is budgeted: if the snapshot exceeds ~60% of the side
  model's context window the oldest messages are dropped, starting on a clean
  turn (never an orphaned tool result). A notice entry says history was
  trimmed. If even the newest exchange exceeds the budget, the latest
  exchange is kept (a tool result is paired back to its assistant call).

### Two runs, one terminal

- A side pane is a nested `chatModel` with `pane = true`. It reuses the whole
  chat machinery (streaming events, tools, approvals, rendering) instead of a
  parallel implementation.
- Async work from the pane is wrapped by `sideMessageRoute` into
  `chatSideMsg`, so the root update loop can tell whose message it is. Only
  `waitEvents` and `scheduleTick` need the route; everything else is
  synchronous or explicitly tagged (`chatSideCloseMsg`).
- The root owns the terminal. Panes never print to scrollback: all
  flow-transcript paths are suppressed while `pane` or `side != nil`, and the
  close path runs a full repaint so the transcript is rewritten once.
- Split layout is inline (no alt-screen): left column = root transcript tail +
  root input, divider, right column = fork header + transcript + input. Width
  is 60/40, clamped to 44/34 columns, and the pane is refused below 90
  columns. Both columns are padded/truncated to exactly `height` lines.

### Tools and approvals

- Each pane gets its **own registry** via `Options.ToolRegistryForModel`.
  Sharing the root registry would be wrong: `BackgroundSource` promises
  exactly-once delivery of background completions, so two runs draining it
  would steal each other's task output. Closing a pane closes its registry
  (killing its background shell/poll tasks), and `/model` in a pane closes
  the previous registry before building a new one.
- `ApprovalState` is **shared** across the tree (it is mutex-protected), so
  "always allow" applies to both panes. Approval *prompts* and their
  controllers are per run; a pane's prompt renders in its own column. Prompts
  do not steal focus: the unfocused pane shows `approval needed`.
- The fork's `Session` runs with `Journal: nil` and an empty chat ID, which is
  what keeps it out of the SQLite journal.

### Keys and mouse

- `opt+]` works because the terminal sends `ESC ]`, which the input parser
  reads as alt+`]`.
- `opt+[` is deliberately **not bound**: it sends `ESC [`, the CSI
  introducer. bubbletea buffers it waiting for a sequence and swallows the
  next keypress. `ctrl+]` (`KeyCtrlCloseBracket`) is the unambiguous alias.
- The program now passes `tea.WithMouseCellMotion()` so panes are clickable.
  Consequence: the terminal's native drag-selection is captured, so the app
  copies drag selections on **every** platform (previously Windows-only).
  Option-drag still selects natively in iTerm2/Terminal. A drag keeps the
  pane it started in across the divider, and a release always finishes that
  selection.

## Files

| Path | Change |
| --- | --- |
| `cmd/tui/chat/side.go` | New: pane state, fork creation, inherited-history trimming, run/routing, pane commands, split layout, mouse/key routing |
| `cmd/tui/chat/side_test.go` | New: 17 behaviour tests |
| `cmd/tui/chat/chat.go` | Pane fields, `chatSideMsg`/`chatSideCloseMsg` cases, split `View`, focus routing, mouse ownership, pane `esc`/`ctrl+c`/`ctrl+d`, quit teardown, mouse capture, flow-print guards |
| `cmd/tui/chat/events.go` | `waitEvents` (route-aware) and routed `scheduleTick` |
| `cmd/tui/chat/render.go` | `transcriptWidth`, split-aware `transcriptLayout`/`transcriptHeight`, pane header hook |
| `cmd/tui/chat/input.go` | `/side` command + help, pane submit branch, pane completions |
| `cmd/tui/chat/modals.go` | `/model` in a pane selects for the fork only |
| `cmd/tui/chat/session.go` | Resume drops the pane |
| `cmd/tui/chat/render_test.go` | Selection-copy tests reflect app-owned copy |
| `README.md`, `AGENTS.md` | Commands, keys, side-chat paragraph |

## Verification

- `go test ./...`, `go vet ./...`.
- Tests cover: fork/tool isolation, width math, render invariants (line count,
  divider placement, resize), scrollback suppression, registry teardown on
  close and on `/model`, focus routing (`opt+]`, `ctrl+]`), pane commands and
  rejections, per-pane runs with fork history, budget trimming, orphaned tool
  results, cross-pane drag ownership, clicking the pane input, and `ctrl+d`.
- Live pty run (expect + `llama3.2`): split rendered with mouse capture on;
  typing landed in the focused pane; `ctrl+]` toggled focus; `ctrl+c` twice
  closed the pane and repainted; the side chat answered a question using the
  inherited history **while the root waited on an approval prompt**. Pane
  slash completion and `/clear` were also checked live.

## Not finished

### Before landing

- Land on `main` and reinstall the CLI (`update-o` skill or
  `go install ./cmd/o`). The branch is complete but unmerged.
- Re-read `AGENTS.md`/`README.md` wording once more; both now describe the
  pane, but `AGENTS.md` is the terse one and drifted during review.
- No Windows check was run for the new mouse/selection behaviour.

### Known limitations (accepted for v1)

- **One fork at a time.** No `/side` inside a pane. The state is shaped as a
  tree (`fork []api.Message` is the parent edge) so nesting can be added
  without reworking the pane.
- **Ephemeral only.** No child session rows, no `parent_id`, no resume of a
  side chat. A tree of persisted forks needs a store migration and a picker
  that can show/nest forks.
- **No `/compact` in a pane.** A side chat that fills its window has only
  `/clear` (and `/model` to a larger window) as recourse. Automatic
  compaction inside the pane run was not exercised.
- **Pane background work is pane-local.** Its background completions are
  drained by the pane's next run and never wake the root. They die with the
  pane; there is no idle auto-wake for a pane.
- **No `/copy` in a pane** (selection copy only) and no `/sessions`,
  `/resume`, `/reviewer`, `/system`, `/tools`, or `/compact`.
- **Scrollback gap while split.** Root lines streamed while the split is open
  are only in the rendered view; on close the tail is repainted, but older
  lines are not replayed into the terminal's scrollback. Consistent with the
  existing `scroll > 0` behaviour, but worth revisiting if it bites.
- **Narrow terminals.** Below 90 columns the split refuses to open; if the
  window shrinks after opening, an already-open pane shows a "widen the
  window" notice while still accepting keystrokes in its (invisible) input.
- **Pane pickers render inline** (compact model list) rather than full-screen,
  and the approval/modal focus rule is "focused pane wins; the other pane
  shows a marker".
- **Pipe/headless frontends** (`--pipe`) do not see side-chat events; the pane
  intentionally forwards no `EventSinks`.

### Nice-to-haves

- A key to hand the fork's last answer back to the root input (`/side to-main`).
- Side-of-side forks, and a `side · <parent>` naming scheme once forks persist.
- `opt+{`/`opt+}` as extra focus/close aliases if Hammerspoon eats `opt+]`.
- Show a per-pane "focus" hint on first open, and confirm before closing a
  pane that has a finished conversation.
