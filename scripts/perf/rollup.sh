#!/usr/bin/env bash
# Roll up a `go tool pprof -top` listing into subsystem family shares.
#
# Usage: scripts/perf/rollup.sh <pprof-top.txt> [<pprof-top.txt> ...]
#
# A committed family-share number comes from running this script, never from
# reading a profile by eye. Each input is one `-top` text (flat and cumulative
# values per symbol); the script prints one family table per file.
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
# the file's own "Total samples = ..." header.
#
# Units: every flat value and the header total are normalised to seconds. The
# parser accepts ns, us (and the micro sign spellings), ms, and s, each
# optionally carrying a k or M prefix. Any other unit fails the run loudly
# instead of being read as a bare number of seconds.
set -euo pipefail

for f in "$@"; do
  echo "== $f"
  awk '
    BEGIN {
      # The two micro spellings pprof can emit (MICRO SIGN, GREEK SMALL MU),
      # built locale-aware so the comparison does not depend on byte layout.
      microUnit = sprintf("%c", 181) "s"
      microAltUnit = sprintf("%c", 956) "s"
    }
    function fail(msg) {
      printf "rollup.sh: %s\n", msg > "/dev/stderr"
      fatal = 1
      exit 2
    }
    # secondsInUnit returns the seconds carried by one unit suffix. A suffix
    # outside the accepted set is a fatal error: the earlier parser silently
    # read "400ms" as 400 seconds, which produced shares off by orders of
    # magnitude for any profile whose largest value was under a second.
    function secondsInUnit(u, what,   prefix, rest, p) {
      rest = u
      prefix = 1
      if (rest ~ /^[kMG]/) {
        p = substr(rest, 1, 1)
        if (p == "k") prefix = 1000
        else if (p == "M") prefix = 1000000
        else prefix = 1000000000
        rest = substr(rest, 2)
      }
      if (rest == "ns") return prefix * 1e-9
      if (rest == "us" || rest == microUnit || rest == microAltUnit) return prefix * 1e-6
      if (rest == "ms") return prefix * 1e-3
      if (rest == "s") return prefix
      fail("unknown time unit " u " in " what)
      return 0
    }
    # toSeconds parses one pprof value ("313.19s", "400ms", "1.50ks", "0") into
    # seconds.
    function toSeconds(tok, what,   v, u) {
      if (tok ~ /^[0-9]*\.?[0-9]+$/) return tok + 0
      if (match(tok, /^[0-9]*\.?[0-9]+/)) {
        v = substr(tok, 1, RLENGTH) + 0
        u = substr(tok, RLENGTH + 1)
      } else {
        fail("cannot parse a time value in " what ": " tok)
      }
      return v * secondsInUnit(u, what)
    }
    /Total samples[[:space:]]*=/ {
      for (i = 1; i <= NF; i++)
        if ($i == "samples" && $(i+1) == "=") {
          total = toSeconds($(i+2), "the Total samples header")
          totalSeen = 1
        }
    }
    /^[[:space:]]*[0-9]/ {
      if ($0 !~ /%/) next
      # flat field: $1 is either "Ns"/"Nms"/... or bare 0, or the row starts at the pct
      flat = $1
      if (flat ~ /%$/) { flatsec = 0; sym0 = 1 }
      else {
        sym0 = 6
        flatsec = toSeconds(flat, "the flat column")
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
      if (fatal) exit 2
      if (!totalSeen) fail("no \"Total samples = ...\" header found")
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
