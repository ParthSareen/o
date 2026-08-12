#!/bin/sh
# Build O.app: Go pipe binary (agent core) + SwiftUI shell, assembled into a
# self-contained .app bundle.
#
#   app/scripts/build-app.sh              # build into app/build/O.app
#   app/scripts/build-app.sh --run        # build and open
#   app/scripts/build-app.sh --install    # build and copy to /Applications
#   app/scripts/build-app.sh --dmg        # build and package app/build/O-<version>.dmg
#
# Note: the Go core must keep CGO enabled (sessionstore uses go-sqlite3).
# Signing is ad-hoc; a dmg/zip built this way is fine to share with people
# who can right-click > Open. Public distribution needs a Developer ID
# certificate + notarization (not wired up here).
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
APP_DIR="$ROOT/app/build/O.app"
CONTENTS="$APP_DIR/Contents"
DO_RUN=0; DO_INSTALL=0; DO_DMG=0
for arg in "$@"; do
    case "$arg" in
        --run) DO_RUN=1 ;;
        --install) DO_INSTALL=1 ;;
        --dmg) DO_DMG=1 ;;
        *) echo "unknown flag: $arg" >&2; exit 2 ;;
    esac
done

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

if [ "$DO_INSTALL" = 1 ]; then
    ditto "$APP_DIR" /Applications/O.app
    # remove quarantine in case a previously-downloaded copy landed there
    xattr -dr com.apple.quarantine /Applications/O.app 2>/dev/null || true
    echo "== installed to /Applications/O.app =="
fi

if [ "$DO_DMG" = 1 ]; then
    VERSION=$(/usr/libexec/PlistBuddy -c 'Print CFBundleShortVersionString' "$CONTENTS/Info.plist")
    DMG="$ROOT/app/build/O-$VERSION.dmg"
    STAGE="$(mktemp -d)"
    trap 'rm -rf "$STAGE"' EXIT
    cp -R "$APP_DIR" "$STAGE/O.app"
    ln -s /Applications "$STAGE/Applications"
    rm -f "$DMG"
    hdiutil create -volname "o" -srcfolder "$STAGE" -ov -format UDZO -quiet "$DMG"
    rm -rf "$STAGE"
    trap - EXIT
    codesign --force --sign - "$DMG" 2>/dev/null || true
    echo "== $DMG built =="
fi

if [ "$DO_RUN" = 1 ]; then
    open "$APP_DIR"
fi
