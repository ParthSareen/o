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
o --list               # smoke check
```

## 3. Ship to main

```sh
git add -A && git commit -m "<type>: <what>"
git push origin main
```

Then report: what landed, whether the CLI was rebuilt/installed, anything the
user should try.
