---
name: update-o
description: Build, test, package, install, and ship the o app + CLI after code changes. Use when the user asks to rebuild/reinstall the O macOS app, update the o CLI, produce a DMG, or "ship"/land UI changes to main.
---

# Update o (app + CLI) end to end

The repo is the agent harness (`cmd/o`, `agent/`) plus the macOS app (`app/`, SwiftUI).
The harness embeds in the app bundle as `o-core` — always rebuild both together.

## 1. Verify

```sh
go test ./...
cd app && swift test && cd ..
```

## 2. Build + install the app

```sh
sh app/scripts/build-app.sh             # bundle at app/build/O.app
sh app/scripts/build-app.sh --install   # also copy to /Applications/O.app
sh app/scripts/build-app.sh --dmg       # also produce app/build/O-<version>.dmg
```

Notes:
- CGO must stay enabled for the Go core (sessionstore uses go-sqlite3) — the script handles it.
- Signing uses the machine's Apple Development cert so TCC grants persist across
  rebuilds; override with `O_SIGN_IDENTITY`. Icon compiles from
  `app/Resources/o_icon.icon` via actool — do not hand-edit the icns.
- `--install` overwrites /Applications/O.app in place.

## 3. Update the CLI

```sh
go install ./cmd/o     # lands at ~/go/bin/o (on PATH)
o --list               # smoke check
```

## 4. Relaunch the running app

```sh
pkill -x OApp; open /Applications/O.app   # or: app/build/O.app for dev runs
```

## 5. Ship to main

Work in the `ui` worktree; pushes land on main directly (ui and main move together):

```sh
git add -A && git commit -m "<type>: <what>"
git fetch origin main
git merge origin/main -m "Merge branch 'main' into ui"   # if ui isn't current
git push origin ui ui:main
```

Then report: commits landed, whether the app was rebuilt/relaunched, anything the
user should verify by clicking.
