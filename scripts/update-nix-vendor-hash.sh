#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
flake="${repo_root}/flake.nix"

current_hash="$(
  sed -nE 's/^[[:space:]]*vendorHash = "([^"]+)";[[:space:]]*$/\1/p' "${flake}" | head -n1
)"

if [[ -z "${current_hash}" ]]; then
  echo "could not find quoted vendorHash in ${flake}" >&2
  exit 1
fi

restore_current_hash() {
  # NOTE: the delimiter is | because vendor hashes are base64 and may contain
  # / (e.g. sha256-iFnqXnC+/ql…) — a / inside the replacement of an s/// ends
  # the substitution early and the trailing hash characters become bogus
  # regexp modifiers, which previously corrupted a tag-time hash update.
  perl -0pi -e "s|vendorHash = nixpkgs\\.lib\\.fakeHash;|vendorHash = \"${current_hash}\";|" "${flake}"
}

perl -0pi -e 's/vendorHash = "[^"]+";/vendorHash = nixpkgs.lib.fakeHash;/' "${flake}"
trap restore_current_hash EXIT

set +e
build_output="$(cd "${repo_root}" && nix build .#peasant --no-link 2>&1)"
build_status=$?
set -e

new_hash="$(
  printf '%s\n' "${build_output}" |
    sed -nE 's/^[[:space:]]*got:[[:space:]]*(sha256-[A-Za-z0-9+/=]+)[[:space:]]*$/\1/p' |
    tail -n1
)"

if [[ -z "${new_hash}" ]]; then
  printf '%s\n' "${build_output}" >&2
  echo "nix did not report a replacement vendorHash" >&2
  exit "${build_status}"
fi

trap - EXIT
perl -0pi -e "s|vendorHash = nixpkgs\\.lib\\.fakeHash;|vendorHash = \"${new_hash}\";|" "${flake}"

echo "updated vendorHash: ${current_hash} -> ${new_hash}"
