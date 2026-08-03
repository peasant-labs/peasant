#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 <schema-worktree> <fairtrade-worktree> <transcript-browser-worktree> <peasant-worktree> <artifact-output-dir>" >&2
  exit 2
fi

schema=$(realpath "$1")
fairtrade=$(realpath "$2")
transcript_browser=$(realpath "$3")
peasant=$(realpath "$4")
output=$(realpath -m "$5")
tmp=$(mktemp -d "${TMPDIR:-/tmp}/peasant-exact-replay.XXXXXX")
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -INT "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

replay_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)

mkdir -p "$output" "$tmp/pack/fairtrade" "$tmp/pack/transcript-browser" "$tmp/schema" "$tmp/transcript-browser" "$tmp/peasant"
for worktree in "$schema" "$fairtrade" "$transcript_browser" "$peasant"; do
  test -z "$(git -C "$worktree" status --porcelain)"
done

# Exercise the real schema generator in an isolated archive, then byte-compare
# every generator-owned artifact against the committed input. This proves
# freshness without mutating (or falsely declaring) the canonical worktree.
git -C "$schema" archive HEAD | tar -x -C "$tmp/schema"
mkdir -p "$tmp/schema-before/testdata/session-detail"
cp -a "$tmp/schema/generated" "$tmp/schema-before/generated"
cp "$tmp/schema/testdata/session-detail/redactions.yaml" "$tmp/schema-before/testdata/session-detail/redactions.yaml"
(
  cd "$tmp/schema"
  GOWORK=off go run ./cmd/schema-gen
)
diff -qr "$tmp/schema-before/generated" "$tmp/schema/generated"
cmp "$tmp/schema-before/testdata/session-detail/redactions.yaml" "$tmp/schema/testdata/session-detail/redactions.yaml"
schema_generated_sha=$(find "$tmp/schema/generated" -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)

pnpm --dir "$fairtrade" build:lib
pnpm --dir "$fairtrade" pack --pack-destination "$tmp/pack/fairtrade" >/dev/null
fairtrade_packed=$(find "$tmp/pack/fairtrade" -maxdepth 1 -type f -name '*.tgz' -print -quit)
fairtrade_sha=$(sha256sum "$fairtrade_packed" | cut -d' ' -f1)
fairtrade_artifact="$output/fairtrade-${fairtrade_sha}.tgz"
cp "$fairtrade_packed" "$fairtrade_artifact"

git -C "$transcript_browser" archive HEAD | tar -x -C "$tmp/transcript-browser"
mkdir -p "$tmp/transcript-browser/.exact-artifacts"
cp "$fairtrade_artifact" "$tmp/transcript-browser/.exact-artifacts/$(basename "$fairtrade_artifact")"
pnpm --dir "$tmp/transcript-browser" install --offline --ignore-scripts
pnpm --dir "$tmp/transcript-browser" --filter @peasant-labs/transcript-browser add --save-exact --offline "file:../../.exact-artifacts/$(basename "$fairtrade_artifact")"
pnpm --dir "$tmp/transcript-browser" --filter @peasant-labs/types build
pnpm --dir "$tmp/transcript-browser" --filter @peasant-labs/transcript-browser test
pnpm --dir "$tmp/transcript-browser" --filter @peasant-labs/transcript-browser build
pnpm --dir "$tmp/transcript-browser/packages/browser" pack --pack-destination "$tmp/pack/transcript-browser" >/dev/null
transcript_browser_packed=$(find "$tmp/pack/transcript-browser" -maxdepth 1 -type f -name '*.tgz' -print -quit)
transcript_browser_sha=$(sha256sum "$transcript_browser_packed" | cut -d' ' -f1)
transcript_browser_artifact="$output/transcript-browser-${transcript_browser_sha}.tgz"
cp "$transcript_browser_packed" "$transcript_browser_artifact"

git -C "$peasant" archive HEAD | tar -x -C "$tmp/peasant"
mkdir -p "$tmp/peasant/.exact-artifacts"
cp "$fairtrade_artifact" "$transcript_browser_artifact" "$tmp/peasant/.exact-artifacts/"

manifest_before=$(sha256sum "$tmp/peasant/web/package.json" | cut -d' ' -f1)
lock_before=$(sha256sum "$tmp/peasant/web/pnpm-lock.yaml" | cut -d' ' -f1)
pnpm --dir "$tmp/peasant/web" add --save-exact --offline \
  "file:../.exact-artifacts/$(basename "$fairtrade_artifact")" \
  "file:../.exact-artifacts/$(basename "$transcript_browser_artifact")"
manifest_after=$(sha256sum "$tmp/peasant/web/package.json" | cut -d' ' -f1)
lock_after=$(sha256sum "$tmp/peasant/web/pnpm-lock.yaml" | cut -d' ' -f1)

installed_fairtrade="$tmp/peasant/web/node_modules/@peasant-labs/fairtrade"
source_graph=$(sha256sum "$fairtrade/dist/lib/graph.js" | cut -d' ' -f1)
installed_graph=$(sha256sum "$installed_fairtrade/dist/lib/graph.js" | cut -d' ' -f1)
source_graph_css=$(sha256sum "$fairtrade/dist/lib/graph.css" | cut -d' ' -f1)
installed_graph_css=$(sha256sum "$installed_fairtrade/dist/lib/graph.css" | cut -d' ' -f1)
test "$source_graph" = "$installed_graph"
test "$source_graph_css" = "$installed_graph_css"

installed_browser="$tmp/peasant/web/node_modules/@peasant-labs/transcript-browser"
source_browser_js=$(sha256sum "$tmp/transcript-browser/packages/browser/dist/index.js" | cut -d' ' -f1)
installed_browser_js=$(sha256sum "$installed_browser/dist/index.js" | cut -d' ' -f1)
source_browser_css=$(sha256sum "$tmp/transcript-browser/packages/browser/dist/styles.css" | cut -d' ' -f1)
installed_browser_css=$(sha256sum "$installed_browser/dist/styles.css" | cut -d' ' -f1)
test "$source_browser_js" = "$installed_browser_js"
test "$source_browser_css" = "$installed_browser_css"

pnpm --dir "$tmp/peasant/web" exec vitest run \
  'src/app/page.test.tsx' \
  'src/app/map/[[...segments]]/MapRouter.test.tsx' \
  'src/app/map/[[...segments]]/MapShell.realCodeMap.test.tsx' \
  'src/app/map/[[...segments]]/MapShell.test.tsx' \
  'src/app/review/[[...segments]]/ReviewSurface.test.tsx' \
  'src/app/review/[[...segments]]/lifted-surface.test.tsx' \
  'src/components/command/CommandPalette.test.tsx' \
  'src/components/session-detail/v2/SessionDetailV2.realViewer.test.tsx' \
  'src/components/session-detail/v2/lib/scopeTurns.test.ts' \
  'src/contexts/__tests__/WebSocketContext.test.tsx' \
  'src/lib/api/map.test.ts' \
  'src/test/strictYaml.test.ts'
pnpm --dir "$tmp/peasant/web" test:transcript-position:mutations
TRANSCRIPT_INPUT_FIXTURE_ONLY=1 pnpm --dir "$tmp/peasant/web" exec node scripts/visual/transcript-input-gate.mjs
pnpm --dir "$tmp/peasant/web" build

# Embed an exact-run provenance marker in the newly built export before the Go
# binary is compiled. The harness later waits for this exact file from the
# server process it launched, so an unrelated listener cannot satisfy the gate.
schema_head=$(git -C "$schema" rev-parse HEAD)
fairtrade_head=$(git -C "$fairtrade" rev-parse HEAD)
transcript_browser_head=$(git -C "$transcript_browser" rev-parse HEAD)
peasant_head=$(git -C "$peasant" rev-parse HEAD)
printf '{"schema":"%s","fairtrade":"%s","transcriptBrowser":"%s","peasant":"%s"}\n' \
  "$schema_head" "$fairtrade_head" "$transcript_browser_head" "$peasant_head" \
  >"$tmp/peasant/web/out/exact-replay.json"
marker_sha=$(sha256sum "$tmp/peasant/web/out/exact-replay.json" | cut -d' ' -f1)

mkdir -p "$tmp/peasant/bin"
(
  cd "$tmp/peasant"
  go mod edit -replace "github.com/peasant-labs/schema=$tmp/schema"
  GOWORK=off go build -buildvcs=false \
    -ldflags "-X github.com/peasant-labs/peasant/internal/defaults.version=exact-replay" \
    -o bin/peasant ./cmd/peasant
)
peasant_binary_sha=$(sha256sum "$tmp/peasant/bin/peasant" | cut -d' ' -f1)
peasant_binary="$output/peasant-${peasant_binary_sha}"
cp "$tmp/peasant/bin/peasant" "$peasant_binary"
grep -aq 'gmp-changes-root' "$peasant_binary"

chrome=${CHROME_PATH:-}
if [[ -z "$chrome" ]]; then
  chrome=$(command -v google-chrome || command -v chromium || command -v chromium-browser || true)
fi
if [[ -z "$chrome" || ! -x "$chrome" ]]; then
  echo "exact replay failed: CHROME_PATH does not name an executable Chromium browser" >&2
  echo "fix: set CHROME_PATH to the Chrome/Chromium executable and rerun" >&2
  exit 1
fi

port=${EXACT_REPLAY_PORT:-18799}
origin="http://127.0.0.1:$port"
if curl -fsS --max-time 1 "$origin/api/v1/health" >/dev/null 2>&1; then
  echo "exact replay failed: port $port already serves a Peasant health response" >&2
  echo "fix: stop the unrelated listener or choose an unused EXACT_REPLAY_PORT" >&2
  exit 1
fi

mkdir -p "$output/runtime/data" "$output/runtime/config" "$output/browser-captures"
server_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
"$peasant_binary" \
  --data-dir "$output/runtime/data" \
  --config-dir "$output/runtime/config" \
  web start --port "$port" --foreground --no-browser --mock-data-store=web,map,review \
  >"$output/server.log" 2>&1 &
server_pid=$!
server_executable=$(readlink -f "/proc/$server_pid/exe")
server_executable_sha=$(sha256sum "$server_executable" | cut -d' ' -f1)
test "$server_executable_sha" = "$peasant_binary_sha"

served_marker="$output/served-exact-replay.json"
marker_ready=false
for _ in $(seq 1 80); do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "exact replay failed: the exact Peasant server exited before serving its provenance marker" >&2
    tail -40 "$output/server.log" >&2
    exit 1
  fi
  if curl -fsS --max-time 1 "$origin/exact-replay.json" -o "$served_marker" 2>/dev/null \
    && cmp "$tmp/peasant/web/out/exact-replay.json" "$served_marker"; then
    marker_ready=true
    break
  fi
  sleep 0.25
done
if [[ "$marker_ready" != true ]]; then
  echo "exact replay failed: the launched PID did not serve the newly embedded exact-replay marker" >&2
  echo "fix: inspect $output/server.log and verify the built export is mounted by this binary" >&2
  exit 1
fi
served_marker_sha=$(sha256sum "$served_marker" | cut -d' ' -f1)
test "$served_marker_sha" = "$marker_sha"

browser_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
set +e
PEASANT_ORIGIN="$origin" \
CHROME_PATH="$chrome" \
TRANSCRIPT_INPUT_OUTPUT="$output/browser-captures" \
  pnpm --dir "$tmp/peasant/web" exec node scripts/visual/transcript-input-gate.mjs \
  >"$output/browser.log" 2>&1
browser_exit=$?
set -e
if [[ "$browser_exit" -ne 0 ]]; then
  echo "exact replay failed: Chrome transcript input gate exited $browser_exit" >&2
  tail -80 "$output/browser.log" >&2
  exit "$browser_exit"
fi
browser_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)

kill -INT "$server_pid"
set +e
wait "$server_pid"
server_exit=$?
set -e
server_stopped=$(date -u +%Y-%m-%dT%H:%M:%SZ)
server_pid_record=$server_pid
server_pid=""
if [[ "$server_exit" -ne 0 ]]; then
  echo "exact replay failed: exact Peasant server exited $server_exit after the browser gate" >&2
  tail -80 "$output/server.log" >&2
  exit "$server_exit"
fi

find "$output/browser-captures" -type f -name '*.png' -print0 | sort -z | xargs -0 sha256sum >"$output/browser-captures.sha256"
browser_log_sha=$(sha256sum "$output/browser.log" | cut -d' ' -f1)
server_log_sha=$(sha256sum "$output/server.log" | cut -d' ' -f1)
captures_manifest_sha=$(sha256sum "$output/browser-captures.sha256" | cut -d' ' -f1)
replay_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)

for worktree in "$schema" "$fairtrade" "$transcript_browser" "$peasant"; do
  test -z "$(git -C "$worktree" status --porcelain)"
done

repo_line() {
  local name=$1
  local path=$2
  local base=$3
  local diff_basis
  local diff_sha

  if git -C "$path" merge-base "$base" HEAD >/dev/null 2>&1; then
    diff_basis="$base...HEAD"
    diff_sha=$(git -C "$path" diff --binary "$base"...HEAD | sha256sum | cut -d' ' -f1)
  elif [[ "$(git -C "$path" rev-list --count HEAD)" == 1 ]] \
    && [[ -z "$(git -C "$path" show -s --format=%P HEAD)" ]]; then
    diff_basis=root-commit
    diff_sha=$(git -C "$path" diff-tree --root --no-commit-id --binary -p HEAD | sha256sum | cut -d' ' -f1)
  else
    printf >&2 'exact replay failed: %s HEAD has no merge base with %s and is not a one-commit parentless root.\n' "$name" "$base"
    printf >&2 'fix: pass the expected branch checkout or the verified parentless public-root worktree.\n'
    return 1
  fi

  printf '%s_branch=%s\n' "$name" "$(git -C "$path" branch --show-current)"
  printf '%s_head=%s\n' "$name" "$(git -C "$path" rev-parse HEAD)"
  printf '%s_commit_time=%s\n' "$name" "$(git -C "$path" show -s --format=%cI HEAD)"
  printf '%s_status=clean\n' "$name"
  printf '%s_diff_basis=%s\n' "$name" "$diff_basis"
  printf '%s_diff_sha256=%s\n' "$name" "$diff_sha"
}

{
repo_line schema "$schema" origin/develop
repo_line fairtrade "$fairtrade" origin/main
repo_line transcript_browser "$transcript_browser" origin/main
repo_line peasant "$peasant" origin/develop
printf '%s\n' \
  "fairtrade_artifact=$fairtrade_artifact" \
  "fairtrade_artifact_sha256=$fairtrade_sha" \
  "transcript_browser_artifact=$transcript_browser_artifact" \
  "transcript_browser_artifact_sha256=$transcript_browser_sha" \
  "peasant_binary=$peasant_binary" \
  "peasant_binary_sha256=$peasant_binary_sha" \
  "peasant_binary_surface_marker=PASS" \
  "schema_generated_sha256=$schema_generated_sha" \
  "schema_generation_command=go run ./cmd/schema-gen" \
  "schema_generation_zero_diff=PASS" \
  "served_marker=$served_marker" \
  "served_marker_sha256=$served_marker_sha" \
  "served_marker_matches_new_build=PASS" \
  "server_pid=$server_pid_record" \
  "server_executable=$server_executable" \
  "server_executable_sha256=$server_executable_sha" \
  "server_started_utc=$server_started" \
  "server_stopped_utc=$server_stopped" \
  "server_exit=$server_exit" \
  "browser_started_after_exact_marker=PASS" \
  "browser_started_utc=$browser_started" \
  "browser_finished_utc=$browser_finished" \
  "browser_exit=$browser_exit" \
  "browser_log_sha256=$browser_log_sha" \
  "server_log_sha256=$server_log_sha" \
  "browser_captures_manifest_sha256=$captures_manifest_sha" \
  "replay_started_utc=$replay_started" \
  "replay_finished_utc=$replay_finished" \
  "source_graph_sha256=$source_graph" \
  "installed_graph_sha256=$installed_graph" \
  "source_graph_css_sha256=$source_graph_css" \
  "installed_graph_css_sha256=$installed_graph_css" \
  "source_browser_js_sha256=$source_browser_js" \
  "installed_browser_js_sha256=$installed_browser_js" \
  "source_browser_css_sha256=$source_browser_css" \
  "installed_browser_css_sha256=$installed_browser_css" \
  "temp_manifest_before_sha256=$manifest_before" \
  "temp_manifest_after_sha256=$manifest_after" \
  "temp_lock_before_sha256=$lock_before" \
  "temp_lock_after_sha256=$lock_after" \
  "exact_package_tests=PASS" \
  "transcript_input_fixture_validation=PASS" \
  "canonical_state=clean_unchanged" \
  "result=PASS"
} | tee "$output/manifest.txt"
printf 'manifest_sha256=%s\n' "$(sha256sum "$output/manifest.txt" | cut -d' ' -f1)"
