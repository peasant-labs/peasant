#!/usr/bin/env bash
# Prepare a safe, repeatable village push profile evidence run.
#
# The harness only works under /tmp/opencode. It never reads the live Peasant data
# directory, and it records structural profile evidence instead of requiring a
# maximum elapsed time.
#
# Usage:
#   scripts/profile-push-copy.sh --dry-run --profile-output /tmp/opencode/push-profile.json --summary-output /tmp/opencode/push-profile.summary.log
#   scripts/profile-push-copy.sh --work /tmp/opencode/peasant-push-profile-run --profile-output /tmp/opencode/push-profile.json --trace-output /tmp/opencode/push-profile.jsonl --summary-output /tmp/opencode/push-profile.summary.log -- bash -c 'go run ./cmd/peasant --data-dir "$PROFILE_WORK/data-home" --config-dir "$PROFILE_WORK/config-home" --state-dir "$PROFILE_WORK/state-home" village push --profile-output "$PROFILE_JSON" --profile-trace "$PROFILE_TRACE"'

set -uo pipefail

WORK="/tmp/opencode/peasant-push-profile-run"
PROFILE_JSON=""
PROFILE_TRACE=""
SUMMARY_OUTPUT=""
DRY_RUN=0
CLEAN=0

usage() {
  awk 'NR >= 2 && NR <= 11 { sub(/^# ?/, ""); print }' "$0"
}

fatal() {
  printf 'FATAL %s\n' "$*" >&2
  exit 1
}

require_absolute_tmp_opencode_file() {
  local flag=$1 value=$2
  [ -n "$value" ] || fatal "$flag is required"
  case "$value" in
    /tmp/opencode/*) ;;
    *) fatal "$flag must be a file under /tmp/opencode" ;;
  esac
  local parent=${value%/*}
  [ -d "$parent" ] || fatal "$flag parent does not exist: $parent"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --work)
      [ "$#" -ge 2 ] || fatal "--work needs a path"
      WORK=$2
      shift 2
      ;;
    --profile-output)
      [ "$#" -ge 2 ] || fatal "--profile-output needs a path"
      PROFILE_JSON=$2
      shift 2
      ;;
    --trace-output)
      [ "$#" -ge 2 ] || fatal "--trace-output needs a path"
      PROFILE_TRACE=$2
      shift 2
      ;;
    --summary-output)
      [ "$#" -ge 2 ] || fatal "--summary-output needs a path"
      SUMMARY_OUTPUT=$2
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --clean)
      CLEAN=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    *)
      fatal "unknown flag: $1"
      ;;
  esac
done

case "$WORK" in
  /tmp/opencode/peasant-push-profile-*) ;;
  *) fatal "--work must start with /tmp/opencode/peasant-push-profile-" ;;
esac
require_absolute_tmp_opencode_file "--profile-output" "$PROFILE_JSON"
require_absolute_tmp_opencode_file "--summary-output" "$SUMMARY_OUTPUT"
if [ -n "$PROFILE_TRACE" ]; then
  require_absolute_tmp_opencode_file "--trace-output" "$PROFILE_TRACE"
fi

if [ "$CLEAN" -eq 1 ]; then
  case "$PROFILE_JSON:$PROFILE_TRACE:$SUMMARY_OUTPUT" in
    *"$WORK"*) fatal "outputs must not be inside --work when --clean is set" ;;
  esac
fi

cleanup() {
  if [ "$CLEAN" -eq 1 ]; then
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

if [ "$DRY_RUN" -eq 1 ]; then
  {
    printf 'push profile dry run: ok\n'
    printf 'work: %s\n' "$WORK"
    printf 'profile json: %s\n' "$PROFILE_JSON"
    if [ -n "$PROFILE_TRACE" ]; then
      printf 'profile trace: %s\n' "$PROFILE_TRACE"
    fi
    printf 'summary: %s\n' "$SUMMARY_OUTPUT"
    printf 'timing gate: structural assertions only\n'
  } >"$SUMMARY_OUTPUT"
  printf 'push profile dry run: ok\n'
  printf 'profile summary: %s\n' "$SUMMARY_OUTPUT"
  exit 0
fi

[ "$#" -gt 0 ] || fatal "a command after -- is required unless --dry-run is set"

rm -rf "$WORK"
mkdir -p "$WORK/data-home" "$WORK/config-home" "$WORK/state-home"
: >"$SUMMARY_OUTPUT"

export PROFILE_WORK="$WORK"
export PROFILE_JSON
export PROFILE_TRACE
export PROFILE_SUMMARY="$SUMMARY_OUTPUT"

START_SECONDS=$(date +%s)
if "$@"; then
  STATUS=0
else
  STATUS=$?
fi
END_SECONDS=$(date +%s)

{
  printf 'profile status: %d\n' "$STATUS"
  printf 'wall seconds: %d\n' "$((END_SECONDS - START_SECONDS))"
  printf 'profile json exists: '
  if [ -f "$PROFILE_JSON" ]; then printf 'yes\n'; else printf 'no\n'; fi
  if [ -n "$PROFILE_TRACE" ]; then
    printf 'profile trace exists: '
    if [ -f "$PROFILE_TRACE" ]; then printf 'yes\n'; else printf 'no\n'; fi
  fi
  printf 'timing gate: structural assertions only\n'
  printf 'next check: validate JSON keys, stage names, counters, outcomes, safe subject IDs, and forbidden-string absence\n'
} >>"$SUMMARY_OUTPUT"

printf 'profile status: %d\n' "$STATUS"
printf 'profile summary: %s\n' "$SUMMARY_OUTPUT"
exit "$STATUS"
