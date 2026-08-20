# o.app — native macOS UI for the o agent harness

SwiftUI shell in front of the agent core. No terminal emulation: the app
spawns `o --pipe` (bundled) and drives the session over NDJSON on stdio.

## Build & run

```sh
app/scripts/build-app.sh            # builds Go core + Swift shell -> app/build/O.app
app/scripts/build-app.sh --run      # build and open
app/scripts/build-app.sh --install  # build and copy to /Applications
app/scripts/build-app.sh --dmg      # build and package app/build/O-<version>.dmg
```

The bundle is ad-hoc signed. A dmg/zip from it is fine to share with anyone
who can right-click → Open; public distribution still needs a Developer ID
certificate + notarization (not wired up).

Dev loop without bundling:

```sh
cd app && O_BINARY=/path/to/o swift run OApp      # or: export O_BINARY=..., open from Xcode
```

Binary lookup order: app bundle → `$O_BINARY` → PATH-style locations
(`/opt/homebrew/bin/o`, `~/go/bin/o`, …).

## Architecture

```
OApp (SwiftUI)
  └─ SessionController  @MainActor @Observable — per window
       │   folds events -> transcript blocks, coalesces deltas ~30fps
       └─ OProcess      actor — owns the child process + pipes
            └─ o --pipe (Go)     — agent session; owns canonical history
                                        └─ ~/.o/sessions.db (canonical store)
```

- **History ownership**: Go `agent.Session` owns the conversation and
  persistence. The app holds only the display transcript — rebuilt from the
  `session_opened` event's `messages` on resume, then folded from live events.
  One Swift `SessionController` + one `o --pipe` process per window.
- **Sidebar** reads `~/.o/sessions.db` read-only via SQLite (schema lives in
  `sessionstore/`). Deletes go through direct DB writes; everything else goes
  through the process.
- **Settings** live in `~/.o/ui.json` (model, full-access toggle, context
  window, extra system prompt, default working directory); the model
  choice also syncs into `~/.ollama/config.json:last_model`.
- **Model selector** (composer bar): switching reloads this window's session
  via `--resume-id <id> <model>`; `o` persists the override (`SetModel`).
- **Skills palette**: `session_opened` carries the skill catalog; typing `/`
  filters, picking inserts `/name`, and send maps it to
  `{"cmd":"prompt","skill":"name","text":"..."}` (→ `RunOptions.SkillName`).
- **Changes inspector** (toolbar toggle): `git status`/`git diff HEAD` of the
  session working directory, refreshed after every completed run.
- **New chat mid-run doesn't kill the run**: the old process detaches — its
  stdin closes, the run finishes and persists, then the process exits on its
  own (`--pipe` treats stdin EOF as "finish, then exit").
- **Working directory**: the folder toolbar button (or Settings default)
  respawns the session in that directory; sidebar resumes reuse the stored
  `working_dir` of the session.

## Pipe protocol

Commands on stdin, one JSON object per line:

```json
{"cmd":"prompt","text":"...","skill":"optional-skill-name"}
{"cmd":"cancel"}
```

Events on stdout: everything `agent.Event` emits (`message_delta`,
`thinking_delta`, `tool_call_detected/started/finished`, `compaction_*`,
`run_finished`, `error`), preceded by:

```json
{"type":"session_opened","chatId":"…","model":"…","name":"…","workingDir":"…","messages":[...],"skills":[{"name":"…","description":"…"}]}
```

Pipe mode grants full tool access by default
(override with `--allow-all-tools=false`).

Hand-test without the app:

```sh
echo '{"cmd":"prompt","text":"hello"}' | o --pipe <model>
```

## Speed notes

- Token deltas are decoded off-main and coalesced into live text at ~30 fps;
  completed blocks render markdown once (code fences split out, plain and
  monospaced) and are never reparsed.
- Transcript rows use stable `UUID` identities in a `LazyVStack`.
- Session list is a direct SQLite read — no IPC.

## Layout

| Path | |
| --- | --- |
| `Sources/OApp/Model` | wire types (`AgentEvent`, `AgentCommand`, `JSONValue`, blocks) |
| `Sources/OApp/Bridge` | `OProcess` actor, binary locator |
| `Sources/OApp/State` | `SessionController`, session list, settings |
| `Sources/OApp/Views` | sidebar, chat/transcript, tool cards, composer, settings |
| `scripts/build-app.sh` | assemble + ad-hoc sign `O.app` (bundles `o-core`) |
