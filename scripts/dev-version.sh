#!/usr/bin/env sh

set -eu

git_bin="${GIT_BIN:-git}"

fail() {
  what="$1"
  why="$2"
  fix="$3"

  {
    printf '%s\n' 'peasant dev version failed'
    printf 'what: %s\n' "$what"
    printf 'why: %s\n' "$why"
    printf '%s\n' 'where: scripts/dev-version.sh'
    printf '%s\n' 'when: deriving VERSION for make build or make go'
    printf '%s\n' 'means: Peasant would otherwise stamp an unordered development version'
    printf 'fix: %s\n' "$fix"
  } >&2
  exit 1
}

base_tag="$("$git_bin" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null)" ||
  fail 'latest reachable release tag could not be found' \
    'local development versions must be ordered against a release tag' \
    'fetch repository tags, then retry; release builds can pass VERSION=<release tag>'

case "$base_tag" in
  v[0-9]*) ;;
  *)
    fail "latest reachable release tag $base_tag is not a Peasant release tag" \
      'development versions must use a v-prefixed release tag as their base' \
      'retag from a Peasant release, fetch tags, or pass VERSION=<release tag> for release builds'
    ;;
esac

distance="$("$git_bin" rev-list --count "${base_tag}..HEAD" 2>/dev/null)" ||
  fail "commit distance from $base_tag could not be counted" \
    'the repository history is not available enough to order this development build' \
    'fetch full history for this checkout, then retry'

case "$distance" in
  '' | *[!0-9]*)
    fail "commit distance $distance is not numeric" \
      'development versions need a numeric build distance' \
      'check the git command output and retry from a normal repository checkout'
    ;;
esac

hash="$("$git_bin" rev-parse --short=9 HEAD 2>/dev/null)" ||
  fail 'current commit hash could not be read' \
    'development versions need a source commit hash' \
    'retry from a normal repository checkout with a valid HEAD'

case "$hash" in
  '' | *[!0-9a-fA-F]*)
    fail "commit hash $hash is not a hexadecimal short hash" \
      'development versions need a source commit hash' \
      'check the git command output and retry from a normal repository checkout'
    ;;
esac

printf '%s-dev.%s+%s\n' "$base_tag" "$distance" "$hash"
