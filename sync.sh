#!/usr/bin/env bash
# Re-sync the agent harness bits from an upstream ollama checkout.
# Usage: ./sync.sh [path-to-ollama]   (default: ../ollama)
set -euo pipefail

SRC="${1:-../ollama}"
NEW_MODULE="github.com/ParthSareen/o"

# Full trees that belong to the harness.
for d in agent api auth envconfig format progress version logutil types/model \
         internal/cloud internal/modelref internal/orderedmap \
         cmd/config cmd/tui cmd/internal/filedata cmd/internal/fileutil; do
  rm -rf "$d"
  mkdir -p "$(dirname "$d")"
  cp -R "$SRC/$d" "$d"
done

# Trimmed launch: only the self-contained spinner; types live in the
# hand-maintained agent_shim.go.
cp "$SRC/cmd/launch/spinner.go" cmd/launch/spinner.go

# Agent TUI command, adapted for this repo (see README "Divergence").
cp "$SRC/cmd/agent_tui.go" cmd/o/agent_tui.go
sed -i '' 's/^package cmd$/package main/' cmd/o/agent_tui.go
sed -i '' 's|inferThinkingOption(&info.Capabilities, &runOptions{Model: opts.Model, Think: opts.Think}, thinkExplicit)|inferThinkingOption(info.Capabilities, opts.Think, thinkExplicit)|' cmd/o/agent_tui.go

# Rewrite import paths (quote-anchored: only Go import specs, not URLs.
grep -rl '"github.com/ollama/ollama' --include='*.go' . \
  | xargs sed -i '' 's|"github.com/ollama/ollama|"github.com/ParthSareen/o|g'

# Re-apply sniped unmerged upstream work. When ollama/ollama#17295 merges and
# the files already contain these changes, git apply will fail — delete the
# patch at that point.
if ! git apply --include='cmd/tui/chat/*' patches/17295-syntax-highlighting.diff 2>/dev/null; then
  echo "note: syntax-highlighting patch not applied (already merged upstream? remove patches/17295-syntax-highlighting.diff)"
fi

go mod tidy
go build ./...
go test ./...
echo "synced from $SRC"
