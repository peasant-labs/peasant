#!/usr/bin/env bash
# Fail if any copyleft (or unclassifiable) license enters the peasant binary's
# Go-module set. This is the ongoing guard that keeps THIRD_PARTY_NOTICES honest:
# every module it ships text for must be a permissive license, forever.
#
# WHY fail on "unknown", not just forbidden/restricted: go-licenses classifies
# by matching license TEXT, and it cannot recognize every permissive variant
# (the modernc.org SQLite stack is the known case). If "unknown" were allowed,
# a FUTURE dependency whose license the classifier cannot read -- which could
# genuinely be GPL/AGPL under a nonstandard filename -- would pass the guard
# silently. So the guard fails closed on "unknown" and allowlists ONLY the
# vetted, known-permissive modules the classifier cannot match today. When a new
# "unknown" trips this guard, vet the module's actual license, then add it to
# IGNORE below with a note -- do NOT widen --disallowed_types.
#
# Allowlist (vetted permissive, classifier cannot match the text):
#   modernc.org/mathutil -- BSD-3-Clause ("The mathutil Authors"); its text ships
#     in THIRD_PARTY_NOTICES, so the notice obligation is still met.
#
# The check runs across every shipped platform (CGO_ENABLED=0, the release build
# mode) so a platform-specific dependency cannot evade it, matching the module
# set enumerated by gen-third-party-notices.sh.
#
# Requirements: run inside the Nix devShell.
set -euo pipefail

pkg="./cmd/peasant"
pinned_go_licenses="github.com/google/go-licenses@v1.6.0"
IGNORE="modernc.org/mathutil"

# Build the tool once for the host. `go run <pkg>@ver` under a cross GOOS/GOARCH
# would build the TOOL for that target and fail to exec it on the host, so the
# host binary is built once and only the analyzed package set is cross-loaded.
gobin="$(mktemp -d)"
trap 'rm -rf "${gobin}"' EXIT
GOBIN="${gobin}" go install "${pinned_go_licenses}"

status=0
for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  if ! CGO_ENABLED=0 GOOS="${pair%/*}" GOARCH="${pair#*/}" \
      "${gobin}/go-licenses" check "${pkg}" \
        --disallowed_types=forbidden,restricted,unknown \
        --ignore "${IGNORE}"; then
    echo "check-dep-licenses: disallowed or unclassifiable license for ${pair}" >&2
    status=1
  fi
done

if [ "${status}" -eq 0 ]; then
  echo "check-dep-licenses: no copyleft or unclassifiable dependency (all targets)"
fi
exit "${status}"
