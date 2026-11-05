#!/usr/bin/env bash
# Fail when the committed CLI reference is not what the command tree generates.
#
# The pages under docs/cli are committed, so a command change must ship its
# regenerated pages. This check answers one question: does the docs directory
# have uncommitted changes after regeneration? It uses `git status` rather than
# `git diff` because a new command produces a new, untracked page that a diff
# would miss. A checkout where git cannot answer fails closed: an unreadable
# status looks exactly like a clean one.
#
# Usage: scripts/check-docs-fresh.sh [docs-dir]     (default: docs/cli)
# GIT_BIN overrides the git binary so a test can drive the unreadable branch.
set -uo pipefail

docs_dir="${1:-docs/cli}"
git_bin="${GIT_BIN:-git}"

if ! status="$("${git_bin}" status --porcelain -- "${docs_dir}" 2>/dev/null)"; then
  echo "::error::cannot verify ${docs_dir} freshness: git status failed in this checkout" >&2
  exit 1
fi

if [ -n "${status}" ]; then
  echo "::error::${docs_dir} is stale; run 'make docs-cli' and commit the regenerated pages" >&2
  printf '%s\n' "${status}" >&2
  exit 1
fi
