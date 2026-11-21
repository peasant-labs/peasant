#!/usr/bin/env bash
# Assert every tracked Go file is gofmt-clean without rewriting any file, with
# the building toolchain's own gofmt. `make fmt-check` (and so `make check`)
# runs this; `make fmt` rewrites files instead.
set -uo pipefail

gofmt="$(go env GOROOT)/bin/gofmt"
if ! unformatted=$(git ls-files -z -- '*.go' | xargs -0 "${gofmt}" -l -e); then
  echo "gofmt could not read every Go file (see errors above); fix or remove the file it names, then rerun make fmt-check" >&2
  exit 1
fi
if [ -n "${unformatted}" ]; then
  echo "Go files not gofmt-clean (run make fmt):"
  echo "${unformatted}"
  exit 1
fi
