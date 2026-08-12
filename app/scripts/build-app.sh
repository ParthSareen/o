#!/bin/sh
# Build O.app: Go pipe binary (agent core) + SwiftUI shell, assembled into a
# self-contained .app bundle. Usage:
#   app/scripts/build-app.sh          # build into app/build/O.app
#   app/scripts/build-app.sh --run    # build and open
#
# Note: the Go core must keep CGO enabled (sessionstore uses go-sqlite3).
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
APP_DIR="$ROOT/app/build/O.app"
CONTENTS="$APP_DIR/Contents"

echo "== building agent core (go, cgo on) =="
cd "$ROOT"
mkdir -p "$CONTENTS/MacOS" "$CONTENTS/Resources"
go build -trimpath -ldflags "-s -w" -o "$CONTENTS/Resources/o-core" ./cmd/o

echo "== building app shell (swift, release) =="
cd "$ROOT/app"
swift build -c release 2>&1 | tail -1
cp "$ROOT/app/.build/release/OApp" "$CONTENTS/MacOS/OApp"
cp "$ROOT/app/Info.plist" "$CONTENTS/Info.plist"

# Ad-hoc sign so Gatekeeper treats it consistently across launches.
codesign --force --sign - "$APP_DIR" 2>/dev/null || true

echo "== $APP_DIR built =="
if [ "${1:-}" = "--run" ]; then
    open "$APP_DIR"
fi
