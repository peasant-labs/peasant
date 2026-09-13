#!/usr/bin/env bash
# Prepare a safe, repeatable village push profile evidence run.
#
# The harness only works under /tmp/opencode. It never reads the live Peasant data
# directory. Work and output paths must be fresh, physical paths (no aliases).
# --clean clears owned contents but retains the empty workspace root for safety.
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
WORK_IDENTITY=""
WORK_FD=""

usage() {
  awk 'NR >= 2 && NR <= 11 { sub(/^# ?/, ""); print }' "$0"
}

fatal() {
  printf 'FATAL %s\n' "$*" >&2
  exit 1
}

physical_path() {
  local value=$1 resolved
  case "$value" in
    /tmp/opencode/*) ;;
    *) return 1 ;;
  esac
  case "$value/" in
    *//* | */./* | */../*) return 1 ;;
  esac
  resolved=$(realpath -m -- "$value") || return 1
  [ "$resolved" = "$value" ]
}

require_fresh_path() {
  local flag=$1 value=$2
  [ -n "$value" ] || fatal "$flag is required; supply a fresh path under /tmp/opencode"
  physical_path "$value" || fatal "$flag path validation failed: $value; use a physical path under /tmp/opencode without traversal or symlinks"
  [ ! -e "$value" ] && [ ! -L "$value" ] || fatal "$flag already exists: $value; nothing was overwritten; choose a fresh path"
  local parent=${value%/*}
  [ -d "$parent" ] || fatal "$flag parent does not exist: $parent; create the parent directory before running the harness"
}

# Record ancestors as well as the leaf; replacing a parent must fail closed even
# if the command moves the original workspace back beneath the replacement.
ancestor_identity() {
  local path=$1
  while [ "$path" != / ]; do
    stat -Lc '%d:%i' -- "$path" || return 1
    path=${path%/*}
    [ -n "$path" ] || path=/
  done
}

open_summary() {
  local reserved_fd
  exec {reserved_fd}>"$SUMMARY_OUTPUT" || fatal "cannot create summary: $SUMMARY_OUTPUT; choose a fresh writable file"
  # Reopen the pinned inode with O_APPEND, so child-written summary context is
  # retained without resolving the caller's pathname again after reservation.
  exec {SUMMARY_FD}>>"/proc/$$/fd/$reserved_fd" || fatal "cannot pin summary: $SUMMARY_OUTPUT; check Linux /proc availability"
  exec {reserved_fd}>&-
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
require_fresh_path "--work" "$WORK"
require_fresh_path "--profile-output" "$PROFILE_JSON"
require_fresh_path "--summary-output" "$SUMMARY_OUTPUT"
if [ -n "$PROFILE_TRACE" ]; then
  require_fresh_path "--trace-output" "$PROFILE_TRACE"
fi

[ "$PROFILE_JSON" != "$SUMMARY_OUTPUT" ] &&
  [ "$PROFILE_JSON" != "$PROFILE_TRACE" ] &&
  [ "$SUMMARY_OUTPUT" != "$PROFILE_TRACE" ] || fatal "output paths overlap; choose distinct fresh files for profile, trace, and summary"
for output in "$PROFILE_JSON" "$PROFILE_TRACE" "$SUMMARY_OUTPUT"; do
  [ -n "$output" ] || continue
  case "$output" in
    "$WORK" | "$WORK"/*) fatal "output overlaps --work: $output; choose a file outside the workspace" ;;
  esac
done

if [ "$DRY_RUN" -eq 0 ] && [ "$#" -eq 0 ]; then
  fatal "a command after -- is required unless --dry-run is set"
fi

cleanup() {
  local status=$1
  if [ "$CLEAN" -eq 1 ] && [ -n "${WORK_IDENTITY:-}" ]; then
    if ! physical_path "$WORK" || [ ! "$WORK" -ef "/proc/$$/fd/$WORK_FD" ] ||
      [ "$(ancestor_identity "$WORK" 2>/dev/null)" != "$WORK_IDENTITY" ]; then
      printf 'WARNING cleanup refused: workspace or ancestors changed at %s; inspect retained data manually\n' "$WORK" >&2
    else
      # Pin the cleanup root in the kernel, not via a pathname that the command
      # can replace. GNU rm does not follow symlinks in these owned contents.
      # Do not rmdir WORK: Bash cannot atomically check identity and unlink it.
      (
        cd -P -- "/proc/$$/fd/$WORK_FD" || exit 1
        shopt -s dotglob nullglob
        entries=(./*)
        if [ "${#entries[@]}" -gt 0 ]; then
          rm -rf -- "${entries[@]}" || exit 1
        fi
      ) || printf 'WARNING cleanup incomplete at %s; inspect retained data manually\n' "$WORK" >&2
      printf 'cleanup retained workspace root: %s; inspect before removing it manually\n' "$WORK"
    fi
  fi
  return "$status"
}
if [ "$DRY_RUN" -eq 0 ]; then
  trap 'cleanup "$?"' EXIT
fi

# Noclobber reserves a new summary atomically. Keep its descriptor open so a
# child replacing the summary pathname cannot redirect our final write.
umask 077
set -C

if [ "$DRY_RUN" -eq 1 ]; then
  open_summary
  {
    printf 'push profile dry run: ok\n'
    printf 'work: %s\n' "$WORK"
    printf 'profile json: %s\n' "$PROFILE_JSON"
    if [ -n "$PROFILE_TRACE" ]; then
      printf 'profile trace: %s\n' "$PROFILE_TRACE"
    fi
    printf 'summary: %s\n' "$SUMMARY_OUTPUT"
    printf 'timing gate: structural assertions only\n'
  } >&"$SUMMARY_FD"
  printf 'push profile dry run: ok\n'
  printf 'profile summary: %s\n' "$SUMMARY_OUTPUT"
  exit 0
fi

mkdir -m 700 -- "$WORK" || fatal "cannot exclusively create --work: $WORK; choose a fresh writable directory"
exec {WORK_FD}<"$WORK" || fatal "cannot pin created workspace: $WORK; inspect it manually before retrying"
WORK_IDENTITY=$(ancestor_identity "$WORK") || fatal "cannot identify created workspace: $WORK; inspect it manually before retrying"
mkdir -- "$WORK/data-home" "$WORK/config-home" "$WORK/state-home" || fatal "cannot prepare workspace: $WORK; check directory permissions"
open_summary

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
} >&"$SUMMARY_FD"

printf 'profile status: %d\n' "$STATUS"
printf 'profile summary: %s\n' "$SUMMARY_OUTPUT"
trap - EXIT
cleanup "$STATUS"
exit "$STATUS"
