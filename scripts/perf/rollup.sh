#!/usr/bin/env bash
# Roll up a `go tool pprof -top` listing into subsystem family shares.
#
# Usage: scripts/perf/rollup.sh <pprof-top.txt> [<pprof-top.txt> ...]
#
# A committed family-share number comes from running this script, never from
# reading a profile by eye. Each input is one `-top` text (flat and cumulative
# seconds per symbol); the script prints one family table per file.
#
# Matcher rule (the published rule; literal symbol prefixes):
#   detector        flat-sum of the exact race/checkptr boundary symbols
#   adjacent        flat-sum of the exact checkptr-adjacent runtime symbols
#   engine          flat-sum of rows under the modernc.org/ module prefix
#                   (the "..." form: the module tree including its root-package
#                   dot form, e.g. modernc.org/libc.Xmemcpy)
#   driver          flat-sum of rows under the zombiezen.com/go/sqlite prefix
#                   (module prefix; root-package dot forms included)
#   application     flat-sum of rows under the repo module prefix
#   runtime-other   flat-sum of the remaining runtime. rows
#   other           flat-sum of every row not matched above
# Shares divide each flat-sum by the run's total sampled seconds, taken from
# the file's own "Total samples = Ns" header.
set -euo pipefail

for f in "$@"; do
  echo "== $f"
  awk '
    /Total samples =/ {
      for (i = 1; i <= NF; i++)
        if ($i == "samples" && $(i+1) == "=") { total = $(i+2); sub(/s$/, "", total) }
    }
    /^[[:space:]]*([0-9.]+(ms|s)?[[:space:]]+)?[0-9.]+%/ {
      # flat field: $1 is either "Ns"/"Nms" or bare 0, or the row starts at the pct
      flat = $1
      if (flat ~ /%$/) { flatsec = 0; sym0 = 1 }
      else {
        sym0 = 6
        if (flat ~ /ms$/) { sub(/ms$/, "", flat); flatsec = flat / 1000 }
        else { sub(/s$/, "", flat); flatsec = flat + 0 }
      }
      sym = ""
      for (i = sym0; i <= NF; i++) sym = (sym == "" ? $i : sym " " $i)
      sub(/ \(inline\)$/, "", sym)
      if (sym == "") next
      n++
      if (sym == "racecall" || sym == "racecalladdr" || \
          sym == "runtime._ExternalCode" || sym == "runtime.checkptrBase" || \
          sym == "runtime.checkptrStraddles" || sym == "runtime.checkptrAlignment" || \
          sym == "runtime.checkptrArithmetic") { d += flatsec; dn++ }
      else if (sym == "runtime.spanOf" || sym == "runtime.findObject" || \
               sym == "runtime.activeModules") { a += flatsec; an++ }
      else if (sym ~ /^modernc\.org\//) { e += flatsec; en++ }
      else if (sym ~ /^zombiezen\.com\/go\/sqlite/) { s += flatsec; sn++ }
      else if (sym ~ /^github\.com\/peasant-labs\/peasant/) { p += flatsec; pn++ }
      else if (sym ~ /^runtime\./) { r += flatsec; rn++ }
      else { o += flatsec; on++ }
    }
    END {
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "detector", dn, d, 100*d/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "adjacent", an, a, 100*a/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "runtime-other", rn, r, 100*r/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "engine", en, e, 100*e/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "driver", sn, s, 100*s/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "application", pn, p, 100*p/total
      printf "%-14s %4d rows %9.2f s %6.2f%%\n", "other", on, o, 100*o/total
      printf "%-14s %4d rows %9.2f s %6.2f%%  (total sampled %.2fs)\n", \
        "accounted", dn+an+rn+en+sn+pn+on, d+a+r+e+s+p+o, 100*(d+a+r+e+s+p+o)/total, total
    }
  ' "$f"
done
