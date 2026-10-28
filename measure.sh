#!/bin/bash
# measure.sh <label> <binary> <pkgs> <reps>
set -u
LABEL="$1"; BIN="$2"; PKGS="$3"; REPS="$4"
export PATH=$HOME/workspace/tools/go/bin:$PATH
export GOPATH=$HOME/workspace/tools/gopath GOCACHE=$HOME/workspace/tools/gocache
export TMPDIR=$HOME/workspace/tools/tmp GOTMPDIR=$HOME/workspace/tools/tmp
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
cd ~/workspace/pr-evidence/pr-758
echo "label=$LABEL ref=$(git rev-parse --abbrev-ref HEAD)@$(git rev-parse --short HEAD) pkgs=$PKGS"
for i in $(seq 1 $REPS); do
  OUT=$HOME/workspace/pr-evidence/evidence-758/out-$LABEL-rep$i
  LOG=$HOME/workspace/pr-evidence/evidence-758/$LABEL-rep$i.log
  S=$(date +%s)
  "$BIN" run -race=false -pkgs "$PKGS" -out "$OUT" > "$LOG" 2>&1; RC=$?
  E=$(date +%s)
  echo "rep$i rc=$RC shell_wall=$((E-S))s pass_wall=$(grep 'single no-race wall:' $LOG) basis=$(grep -E 'serialized-per-invocation|pass-level-aggregate' $LOG | head -1) screen=$(grep -A1 '=== screen ===' $LOG | tail -1) result=$(grep 'testgate: PASS\|testgate: FAIL' $LOG | head -1) load=$(cat /proc/loadavg | cut -d' ' -f1-3)"
done
echo DONE-$LABEL
