#!/usr/bin/env bash
set -euo pipefail
# Run from the feature checkout; the detached base must remain otherwise clean.
base=${1:?usage: capture.sh BASE_WORKTREE NEW_OUTPUT_DIRECTORY}
out=${2:?usage: capture.sh BASE_WORKTREE NEW_OUTPUT_DIRECTORY}
source_root=$(git rev-parse --show-toplevel)
expected=ed9984369d8bafb4b5552123b9b4f56f93600569
test "$(git -C "$base" rev-parse HEAD)" = "$expected"
test -z "$(git -C "$base" status --porcelain)"
test ! -e "$base/internal/testkit/contentparity"
test ! -e "$base/scripts/parity-oracle"
cleanup() {
  rm -rf -- "$base/internal/testkit/contentparity" "$base/scripts/parity-oracle"
}
trap cleanup EXIT HUP INT TERM
mkdir -p "$base/internal/testkit/contentparity" "$base/scripts/parity-oracle"
cp "$source_root/internal/testkit/contentparity/oracle.go" "$base/internal/testkit/contentparity/"
cp "$source_root/scripts/parity-oracle/main.go" "$base/scripts/parity-oracle/"
cd "$base"
nix develop -c go build -o "$base/scripts/parity-oracle/peasant" ./cmd/peasant
nix develop -c go run ./scripts/parity-oracle \
  --sources "$source_root/internal/store/testdata/content_model_parity.yaml" \
  --manifest "$source_root/internal/store/testdata/content_model_parity.manifest.yaml" \
  --out "$out"
