#!/usr/bin/env bash
# Profile INDEX against the reusable copied Peasant corpus, without copying it.
#
# The default corpus is a pre-existing real-data copy. This script never reads
# ~/.local/share/peasant. It creates only a small control directory with config,
# state, logs, and a peasant symlink that points at the copied corpus.
#
# Usage:
#   scripts/profile-index-copy.sh
#   scripts/profile-index-copy.sh --work /tmp/opencode/peasant-index-profile-control-develop
#   scripts/profile-index-copy.sh --corpus /tmp/opencode/peasant-index-profile-live-source --clean

set -uo pipefail

CORPUS="/tmp/opencode/peasant-index-profile-live-source"
WORK="/tmp/opencode/peasant-index-profile-control"
CLEAN=0

usage() {
  sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
}

fatal() {
  printf 'FATAL %s\n' "$*" >&2
  exit 1
}

count_log_pattern() {
  local label=$1 pattern=$2 count
  count="$(rg -c "$pattern" "$LOG" || true)"
  if [ -z "$count" ]; then
    count=0
  fi
  printf '%s: %s\n' "$label" "$count"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --corpus)
      [ "$#" -ge 2 ] || fatal "--corpus needs a path"
      CORPUS=$2
      shift 2
      ;;
    --work)
      [ "$#" -ge 2 ] || fatal "--work needs a path"
      WORK=$2
      shift 2
      ;;
    --clean)
      CLEAN=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      fatal "unknown flag: $1"
      ;;
  esac
done

command -v go >/dev/null 2>&1 || fatal "go is required"
command -v node >/dev/null 2>&1 || fatal "node is required"
command -v rg >/dev/null 2>&1 || fatal "rg is required"

case "$CORPUS" in
  /tmp/opencode/peasant-index-profile-*) ;;
  *) fatal "corpus must be a copied profile corpus under /tmp/opencode/peasant-index-profile-*" ;;
esac
case "$WORK" in
  /tmp/opencode/peasant-index-profile-control*) ;;
  *) fatal "work directory must start with /tmp/opencode/peasant-index-profile-control" ;;
esac

[ -d "$CORPUS" ] || fatal "copied corpus does not exist: $CORPUS"
[ -f "$CORPUS/peasant.db" ] || fatal "copied corpus has no peasant.db: $CORPUS"
[ -d "$CORPUS/peasant-sync" ] || fatal "copied corpus has no peasant-sync/: $CORPUS"
[ "$WORK" != "$CORPUS" ] || fatal "work directory must not be the copied corpus"

cleanup() {
  if [ "$CLEAN" -eq 1 ]; then
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

rm -rf "$WORK"
mkdir -p "$WORK/data-home" "$WORK/config-home/peasant" "$WORK/state-home"
ln -s "$CORPUS" "$WORK/data-home/peasant"

CORPUS="$CORPUS" WORK="$WORK" node <<'NODE'
const fs = require('fs');
const path = require('path');

const corpus = process.env.CORPUS;
const work = process.env.WORK;
const syncRoot = path.join(corpus, 'peasant-sync');
const configPath = path.join(work, 'config-home/peasant/config.yaml');
let rewritten = 0;
let alreadyCorrect = 0;
let missingTranscript = 0;
let invalid = 0;

function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(p);
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith('--metadata.json')) {
      continue;
    }

    let doc;
    try {
      doc = JSON.parse(fs.readFileSync(p, 'utf8'));
    } catch (_) {
      invalid++;
      continue;
    }

    const sessionId = doc.sessionId || entry.name.slice(0, -'--metadata.json'.length);
    const format = doc.source && doc.source.format;
    if (!sessionId || !format) {
      invalid++;
      continue;
    }

    const transcript = path.join(path.dirname(p), `${sessionId}--transcript.${format}`);
    if (!fs.existsSync(transcript)) {
      missingTranscript++;
      continue;
    }

    if (doc.source.filePath === transcript) {
      alreadyCorrect++;
      continue;
    }
    doc.source.filePath = transcript;
    fs.writeFileSync(p, JSON.stringify(doc) + '\n');
    rewritten++;
  }
}

walk(syncRoot);
fs.writeFileSync(configPath, `version: 1
sources:
  claude-code: {enabled: false}
  opencode: {enabled: false}
  codex: {enabled: false}
  cursor: {enabled: false}
  strike: {enabled: false}
output:
  basePath: ${syncRoot}
`);

console.log(JSON.stringify({ syncRoot, configPath, rewritten, alreadyCorrect, missingTranscript, invalid }));
if (missingTranscript || invalid) {
  process.exit(1);
}
NODE
PREP_STATUS=$?
[ "$PREP_STATUS" -eq 0 ] || fatal "copied-corpus preparation failed; control directory kept at $WORK"

LOG="$WORK/profile.log"
START_SECONDS=$(date +%s)
if go run ./cmd/peasant \
  --data-dir "$WORK/data-home" \
  --config-dir "$WORK/config-home" \
  --state-dir "$WORK/state-home" \
  harvest index --all --profile-index >"$LOG" 2>&1; then
  STATUS=0
else
  STATUS=$?
fi
END_SECONDS=$(date +%s)

printf 'profile status: %d\n' "$STATUS"
printf 'wall seconds: %d\n' "$((END_SECONDS - START_SECONDS))"
printf 'copied corpus: %s\n' "$CORPUS"
printf 'control directory: %s\n' "$WORK"
printf 'profile log: %s\n' "$LOG"
printf '\nprofile lines:\n'
PROFILE_LINE_RE="^(INDEX profile|  batch sizes|  work items|  write txs|  write causes:|  annotation target repair timing:|  annotation detail:|    (hash matches|hash misses|fallback compares|skipped by hash|skipped by compare|rewrites|projection repair rewrites|annotation rollback failures|annotation targets carried|annotation targets preserved|annotation targets remapped|annotation targets unresolved|annotation targets superseded|annotation target repair errors|read targets:|match anchors:|restore target rows:|anchor upserts:|note:|list entries:|get metrics:|classifier run:|results:|id cache:|batch persistence:|batch persistence detail:|annotation results by type:|dedup lookup:|create session annotation:|create entry annotation:|update content hash:|supersede annotation:|dedup decisions:|[A-Z][A-Z ]+:)|      (mutex wait:|connection checkout:|savepoint SQL:|dedup lookup:|insert annotation row:|insert target row:|update content hash:|supersede annotation:|commit:|type=)|  parse|  stage timings:|peasant harvest|  index:|  warning:)"
rg "$PROFILE_LINE_RE" "$LOG" || true
printf '\nwarning counts:\n'
count_log_pattern "database is locked" "database is locked"
count_log_pattern "annotation target carry failures" "preserve annotation_target_entries"
count_log_pattern "missing provider roots" "harness stores entries under a provider root"

exit "$STATUS"
