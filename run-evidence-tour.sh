#!/bin/bash
export PATH=$HOME/workspace/tools/go/bin:$PATH
export GOPATH=$HOME/workspace/tools/gopath GOCACHE=$HOME/workspace/tools/gocache
export TMPDIR=$HOME/workspace/tools/tmp GOTMPDIR=$HOME/workspace/tools/tmp
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
E=$HOME/workspace/pr-evidence/evidence-758
cd ~/workspace/pr-evidence/pr-758
clear
echo "PR #758 - perf(check): overlap the single no-race pass and pin its attribution"
echo "=========================================================================="
echo "Rebased head (was b3b48849, 15 behind develop; now on develop be2bc57f):"
git log --oneline -2
echo
echo "MEASURED - testgate subset run, RACE=0 single no-race pass, same 6 packages both sides:"
echo "  pkgs: coveragemap, teststream, fsdecorator, metrics, auth, defaults (-p = GOMAXPROCS = 2)"
echo
echo "BEFORE (develop be2bc57f, SerialPassB forced concurrency=1 for the single pass):"
grep "^rep" $E/before-summary.log | sed 's/.*pass_wall=/  pass wall: /; s/ basis=.*serialized/  basis: serialized/; s/ screen=/  screen: /; s/ result=/  /; s/ load=.*//'
echo
echo "AFTER (PR head d1f78111, single pass concurrent, attribution pinned pass-level):"
grep "^rep" $E/after-all.log | sed 's/.*pass_wall=/  pass wall: /; s/ basis=.*pass-level/  basis: pass-level/; s/ screen=/  screen: /; s/ result=/  /; s/ load=.*//'
echo
echo "Median pass wall: BEFORE ~195s -> AFTER ~75s  (about 2.6x faster; VM heavily shared, see caveat in PR comment)"
echo
echo "FUNCTIONAL OVERLAP PROOF - the PR's rendezvous probe packages, live:"
echo "  left and right tests each wait for the other's file; a serialized pass fails after 20s."
rm -rf $E/overlap-dir-video; mkdir -p $E/overlap-dir-video
export PEASANT_TESTGATE_OVERLAP_DIR=$E/overlap-dir-video
$E/testgate-after run -race=false -pkgs ./internal/testkit/testgate/overlapprobe/left,./internal/testkit/testgate/overlapprobe/right -out $E/out-overlap-video 2>&1 | grep -vE "^github.com|^UNIT|^CLASS" | tail -32
echo
echo "Rendezvous files written by the two concurrently running packages:"
ls -l $E/overlap-dir-video
echo
echo "PR's own tests: go test -race -count=1 ./cmd/testgate ./internal/testkit/testgate"
cat $E/own-tests.log
echo "=== end of evidence tour ==="
sleep 2
