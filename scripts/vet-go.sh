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

# Discover opt-in tags and the directories that use them from owned Go sources,
# including untracked files. This keeps a new tag or package from needing a
# separate, easily forgotten list update. Tests are compiled/vetted, not run.
# Platform, toolchain, and the conventional "ignore" tag are not opt-ins here.
if constraints="$(git grep --untracked --exclude-standard -n \
  -e '^//go:build[[:space:]]' -- '*.go' \
  ':(exclude)third_party/**' ':(exclude)**/vendor/**' \
  ':(exclude)testdata/**' ':(exclude)**/testdata/**')"; then
  :
else
  status=$?
  if [ "${status}" -ne 1 ]; then
    echo 'vet-go: could not discover Go build constraints' >&2
    exit "${status}"
  fi
fi

platforms="$(go tool dist list)"
tagged_packages="$(printf '%s\n' "${constraints}" | awk -F: -v platforms="${platforms}" '
  BEGIN {
    count = split(platforms, targets, /[[:space:]]+/)
    for (i = 1; i <= count; i++) {
      split(targets[i], target, "/")
      builtin[target[1]] = 1
      builtin[target[2]] = 1
      arch[target[2]] = 1
    }
    split("unix cgo gc gccgo race msan asan ignore", special, " ")
    for (i in special) builtin[special[i]] = 1
    split("cgo gc gccgo race msan asan ignore", conditional, " ")
    for (i in conditional) special_condition[conditional[i]] = 1
  }
  function is_builtin(tag, parts) {
    split(tag, parts, ".")
    return builtin[tag] || tag ~ /^go[0-9]+\.[0-9]+$/ ||
      parts[1] == "goexperiment" || (parts[1] in arch && parts[2] != "")
  }
  NF >= 3 {
    file = $1
    expression = $0
    sub(/^[^:]+:[0-9]+:\/\/go:build[[:space:]]+/, "", expression)
    # Platform-only expressions need no opt-in vet, including negated groups.
    all_count = split(expression, all_words, /[^[:alnum:]_.]+/)
    custom = 0
    for (i = 1; i <= all_count; i++) {
      if (all_words[i] != "" && !is_builtin(all_words[i])) custom++
    }
    # With compound negation or two positive opt-ins joined by AND, vetting
    # one tag at a time may never select the file. Refuse to silently miss it.
    if (expression ~ /![[:space:]]*[!(]/ && custom > 0) {
      printf "vet-go: cannot vet compound negation in %s; update tag discovery\n", file > "/dev/stderr"
      exit 1
    }
    positive = expression
    gsub(/![[:space:]]*[[:alnum:]_.]+/, " ", positive)
    n = split(positive, words, /[^[:alnum:]_.]+/)
    found = 0
    tool_condition = 0
    for (i = 1; i <= n; i++) {
      tag = words[i]
      if (tag == "") continue
      if (is_builtin(tag)) {
        if (special_condition[tag] || tag ~ /^go[0-9]+\.[0-9]+$/ || tag ~ /^goexperiment\./) tool_condition = 1
        continue
      }
      if (seen[NR, tag]++) continue
      found++
      dir = file
      if (sub(/\/[^/]*$/, "", dir)) dir = "./" dir
      else dir = "."
      print tag "\t" dir
    }
    if (found > 1 && expression ~ /&&/) {
      printf "vet-go: multiple positive opt-in tags with AND in %s; update tag discovery\n", file > "/dev/stderr"
      exit 1
    }
    if (found > 0 && tool_condition && expression ~ /&&/) {
      printf "vet-go: opt-in tag requires a toolchain condition in %s; update tag discovery\n", file > "/dev/stderr"
      exit 1
    }
  }
' | sort -u)"

vet_tagged_packages() {
  printf 'vet-go: vetting tag %s: %s\n' "${current_tag}" "${packages[*]}"
  go vet -tags="${current_tag}" "${packages[@]}"
}

current_tag=''
packages=()
while IFS=$'\t' read -r tag package; do
  [ -n "${tag}" ] || continue
  if [ "${tag}" != "${current_tag}" ]; then
    if [ -n "${current_tag}" ]; then vet_tagged_packages; fi
    current_tag="${tag}"
    packages=()
  fi
  packages+=("${package}")
done <<< "${tagged_packages}"
if [ -n "${current_tag}" ]; then vet_tagged_packages; fi
