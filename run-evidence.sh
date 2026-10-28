#!/bin/bash
cd "$(dirname "$0")"
export HOME="$PWD/fake-home"
sleep 1
echo '================================================================'
echo 'PR #490 - feat(layout): capture the Devin Desktop session layout'
echo 'Synthetic fixtures only (repo testdata + a synthetic state.vscdb)'
echo '================================================================'
sleep 1
echo
echo '--- BEFORE: base tip 883c4b39 (PR #488 branch, no Devin Desktop) ---'
sleep 1
echo '$ peasant-base layout list'
./peasant-base layout list | sed "s|$HOME|~|g"
echo '(no output above: no layout tools are registered at the base)'
sleep 1
echo
echo '$ peasant-base layout capture devin-desktop --path fixtures/... --include-metadata'
./peasant-base layout capture devin-desktop --path fixtures/config/Devin --path fixtures/home/.codeium/windsurf --include-metadata --limit 0 2>&1 | head -1
echo "exit: ${PIPESTATUS[0]}  <- Devin Desktop layout is NOT captured at the base"
sleep 1
echo
echo '--- AFTER: PR head 3ae8910c (rebased onto 883c4b39) ---'
sleep 1
echo '$ peasant-head layout list'
./peasant-head layout list | sed "s|$HOME|~|g"
sleep 1
echo
echo '$ peasant-head layout capture devin-desktop --path fixtures/config/Devin \'
echo '    --path fixtures/home/.codeium/windsurf --include-metadata --limit 0'
./peasant-head layout capture devin-desktop --path fixtures/config/Devin --path fixtures/home/.codeium/windsurf --include-metadata --limit 0 > after.json
echo "exit: $?"
python3 summarize.py after.json
sleep 1
echo
echo '$ leak check: no transcript text, secrets, values or workspace IDs in the report'
for s in FIXTURE-USER-PROMPT FIXTURE-AGENT-REPLY FIXTURE-TOOL-OUTPUT SYNTHETIC-SECRET-VALUE synthetic-ws-1; do
  if grep -q "$s" after.json; then echo "LEAK: $s"; else echo "clean: $s not present"; fi
done
sleep 1
echo
echo '--- TESTS (rebased head) ---'
echo '$ go test -race ./internal/harnesslayout/ -run TestDevinDesktop -v'
export HOME=/home/hatch
cd /home/hatch/workspace/pr-evidence/pr-490
export PATH=$HOME/workspace/tools/go/bin:$PATH GOPATH=$HOME/workspace/tools/gopath GOCACHE=$HOME/workspace/tools/gocache TMPDIR=$HOME/workspace/tools/tmp GOTMPDIR=$HOME/workspace/tools/tmp
go test -race ./internal/harnesslayout/ -run TestDevinDesktop -v 2>&1 | grep -E 'RUN|PASS|FAIL|ok'
echo '$ go test ./internal/harnesslayout/'
go test ./internal/harnesslayout/ 2>&1 | tail -1
sleep 1
echo
echo 'DONE: base = not captured / unknown tool; head = 4 captures, no leaks, tests pass'
sleep 2
