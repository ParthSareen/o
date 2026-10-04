---
name: update-o
description: Build, test, install, and ship the o CLI after code changes. Use when the user asks to rebuild/reinstall the o CLI, update the o binary, or "ship"/land changes to main.
---

# Update o (CLI) end to end

The repo is the agent harness (`cmd/o`, `agent/`) — no separate frontend.

## 1. Verify

```sh
go test ./...
```

CGO must stay enabled for the Go core (sessionstore uses go-sqlite3);
never set `CGO_ENABLED=0`.

## 2. Install the CLI

```sh
go install ./cmd/o     # lands at ~/go/bin/o (on PATH)
o --list               # smoke check: binary launches, DB opens
```

## 3. Smoke-test a real run (after runtime changes)

`o --list` and `go test ./...` can't catch broken server wiring — the server
tests mock watchy and model resolution. After touching `cmd/o/server.go`, the
watchy spawn, model handling, or tools, run one real headless exchange:

    o --headless glm-5.3-flash:cloud "Reply with exactly one word: works"

Expect: a dedicated ollama server on 127.0.0.1:11433 spawned via watchy
(`watchy list` shows task `o-ollama-11433`). If watchy is missing, o falls
back to the shared server on 11434 — that's the documented fallback, not a
failure, but call it out in the report.

## 4. Ship to main

```sh
git add -A && git commit -m "<type>: <what>"
git push origin main
```

Then report: what landed, whether the CLI was rebuilt/installed, anything the
user should try.
