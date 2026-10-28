#!/bin/bash
# PR #714 before/after demo — issue #671.
# SAFETY: every PID planted in a fake PID file below is a `sleep 300`
# started by THIS script. No other process is ever named.
set -u
BIN_BEFORE="$HOME/workspace/pr-evidence/pr-714/evidence/bin/peasant-before"
BIN_AFTER="$HOME/workspace/pr-evidence/pr-714/evidence/bin/peasant-after"
ROOT="$HOME/workspace/pr-evidence/pr-714/evidence/demo-scratch"
case "$ROOT" in *pr-714/evidence/demo-scratch) ;; *) echo "refusing unsafe ROOT"; exit 1;; esac
rm -rf "$ROOT"; mkdir -p "$ROOT"
pause(){ sleep 1; }

echo '$ # PR #714 — peasant web stop must only signal its own web server (issue #671)'
echo '$ # A stale web-<port>.pid can name ANY reused PID. Plant a harmless'
echo '$ # stand-in (sleep 300, started by this script) in a fake PID file.'
pause

echo
echo '================ BEFORE: develop (be2bc57f) ================'
export HOME="$ROOT/before-home"
export XDG_CONFIG_HOME="$HOME/.config" XDG_DATA_HOME="$HOME/.local/share" XDG_STATE_HOME="$HOME/.local/state"
mkdir -p "$XDG_STATE_HOME/peasant"
sleep 300 & SLEEP_PID=$!
echo "$SLEEP_PID" > "$XDG_STATE_HOME/peasant/web-18693.pid"
echo "\$ sleep 300 &  # stand-in PID $SLEEP_PID, planted in web-18693.pid"
ps -o pid,comm,args -p "$SLEEP_PID" | cat
echo "\$ peasant-before web stop --port 18693"
"$BIN_BEFORE" web stop --port 18693; echo "exit=$?"
pause
if kill -0 "$SLEEP_PID" 2>/dev/null; then echo "stand-in $SLEEP_PID is STILL ALIVE"; else echo ">>> stand-in $SLEEP_PID was KILLED by web stop on develop (BUG, #671)"; fi
wait "$SLEEP_PID" 2>/dev/null

echo
echo '================ AFTER: PR #714 head (47b14407, rebased) ================'
export HOME="$ROOT/after-home"
export XDG_CONFIG_HOME="$HOME/.config" XDG_DATA_HOME="$HOME/.local/share" XDG_STATE_HOME="$HOME/.local/state"
mkdir -p "$XDG_STATE_HOME/peasant"
sleep 300 & SLEEP_PID=$!
echo "$SLEEP_PID" > "$XDG_STATE_HOME/peasant/web-18694.pid"
echo "\$ sleep 300 &  # stand-in PID $SLEEP_PID, planted in web-18694.pid"
ps -o pid,comm,args -p "$SLEEP_PID" | cat
echo "\$ peasant-after web stop --port 18694"
"$BIN_AFTER" web stop --port 18694; echo "exit=$?"
pause
if kill -0 "$SLEEP_PID" 2>/dev/null; then echo ">>> stand-in $SLEEP_PID is STILL ALIVE — nothing was signalled (FIXED)"; else echo ">>> stand-in $SLEEP_PID was KILLED (FIX FAILED)"; fi
echo "\$ ls \$XDG_STATE_HOME/peasant/  # stale PID file was removed"
ls "$XDG_STATE_HOME/peasant/" 2>&1 | cat
echo "# now killing my own stand-in myself:"
kill "$SLEEP_PID"; wait "$SLEEP_PID" 2>/dev/null; echo "stand-in cleaned up"

echo
echo '================ LEGIT PATH: real server start/stop (PR binary) ================'
export HOME="$ROOT/legit-home"
export XDG_CONFIG_HOME="$HOME/.config" XDG_DATA_HOME="$HOME/.local/share" XDG_STATE_HOME="$HOME/.local/state"
mkdir -p "$HOME"
echo "\$ peasant-after web start --no-browser --port 18695"
"$BIN_AFTER" web start --no-browser --port 18695; echo "start exit=$?"
sleep 1
curl -s -o /dev/null -w "health check HTTP %{http_code}\n" http://localhost:18695/api/v1/health || echo "health check failed"
ls "$XDG_STATE_HOME/peasant/" 2>&1 | cat
echo "\$ peasant-after web stop --port 18695"
"$BIN_AFTER" web stop --port 18695; echo "stop exit=$?"
sleep 1
if curl -s -m 2 -o /dev/null http://localhost:18695/api/v1/health; then echo ">>> server STILL answering (unexpected)"; else echo ">>> server stopped — port 18695 no longer answers"; fi
echo
echo '$ # DONE: before = stand-in killed; after = stand-in untouched, real server still stops.'
