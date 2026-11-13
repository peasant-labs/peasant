#!/usr/bin/env bash
# Keep every analyzer for owned code and the driver's higher-level packages.
# The native driver retains upstream's audited modernc uintptr conversions,
# which Go's unsafeptr analyzer cannot distinguish from Go heap pointers. The
# audited conversion set is pinned by internal/sqliteforkaudit.
set -euo pipefail

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
# -unsafeptr only: the native package's modernc address conversions are audited
# upstream source, not a defect we can fix without forking them.
go vet -unsafeptr=false "${native_package}"

# One list of opt-in tags. Their tests are compiled/vetted, never run here.
opt_in_tags=(
  astgrep capability_scan_negative dry_run_capability_negative e2e
  evidence_negative_dedup evidence_negative_indexer evidence_negative_suppression
  experimental guided_screenshots origin_audit projection_negative_fold
  projection_negative_scope projection_negative_seed
)

# Search owned files, including untracked ones; git grep exits 1 for no matches.
grep_owned() {
  git grep --untracked --exclude-standard "$@" -- '*.go' \
    ':(exclude)third_party/**' ':(exclude)**/vendor/**' \
    ':(exclude)testdata/**' ':(exclude)**/testdata/**' || [ "$?" -eq 1 ]
}

# Treat constraint expressions as tokens, not a Boolean syntax to interpret.
# Every custom tag must be declared above before the gate can pass.
constraints="$(grep_owned -h -e '^//go:build[[:space:]]')"
platforms="$(go tool dist list)"
builtins="$(printf '%s\n' "${platforms}" | tr '/\n' '  ') unix cgo gc gccgo race msan asan ignore"
arches="$(printf '%s\n' "${platforms}" | cut -d/ -f2 | tr '\n' ' ')"
tokens="$(printf '%s\n' "${constraints}" | sed 's@^//go:build[[:space:]]*@@' | grep -oE '[[:alnum:]_.]+' | sort -u)"
for tag in ${tokens}; do
  case " ${opt_in_tags[*]} ${builtins} " in *" ${tag} "*) continue ;; esac
  if [[ "${tag}" =~ ^go[0-9]+\.[0-9]+$ || "${tag}" == goexperiment.* ]]; then continue; fi
  if [[ "${tag}" == *.* ]]; then
    case " ${arches} " in *" ${tag%%.*} "*) continue ;; esac
  fi
  printf 'vet-go: undeclared opt-in build tag: %s; add it to opt_in_tags in scripts/vet-go.sh and rerun make lint\n' "${tag}" >&2
  exit 1
done

# Vet each declared tag on the packages carrying it. Files constrained to a
# different platform (e.g. e2e && !linux on Linux) are not selected here.
# Tags are enabled one at a time, so files requiring multiple opt-ins are not
# selected by these runs either.
# The Windows cross-vet step in .github/workflows/tests.yml covers Windows;
# other excluded platforms need their own vet run.
for tag in "${opt_in_tags[@]}"; do
  escaped_tag="${tag//./\\.}"
  files="$(grep_owned -l -E -e "^//go:build.*[^[:alnum:]_.]${escaped_tag}([^[:alnum:]_.]|$)")"
  packages=()
  while IFS= read -r file; do
    [ -n "${file}" ] || continue
    dir="${file%/*}"
    if [ "${dir}" = "${file}" ]; then dir='.'; else dir="./${dir}"; fi
    case " ${packages[*]} " in *" ${dir} "*) ;; *) packages+=("${dir}") ;; esac
  done <<< "${files}"
  if [ "${#packages[@]}" -gt 0 ]; then
    printf 'vet-go: vetting tag %s: %s\n' "${tag}" "${packages[*]}"
    go vet -tags="${tag}" "${packages[@]}"
  fi
done
