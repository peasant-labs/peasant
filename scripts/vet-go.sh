#!/usr/bin/env bash
# Keep every analyzer for owned code and the driver's higher-level packages.
# The native driver retains upstream's audited modernc uintptr conversions,
# which Go's unsafeptr analyzer cannot distinguish from Go heap pointers.
set -euo pipefail
python3 scripts/check-sqlite-fork.py
native_package='github.com/peasant-labs/peasant/third_party/zombiezen-sqlite'
listed="$(go list ./...)"
default_packages=()
found_native=false
while IFS= read -r package; do
  if [ "${package}" = "${native_package}" ]; then
    found_native=true
  else
    default_packages+=("${package}")
  fi
done <<< "${listed}"
if [ "${found_native}" != true ] || [ "${#default_packages[@]}" -eq 0 ]; then
  echo 'vet-go: expected native driver and owned packages were not discovered' >&2
  exit 1
fi
go vet "${default_packages[@]}"
go vet -unsafeptr=false "${native_package}"
