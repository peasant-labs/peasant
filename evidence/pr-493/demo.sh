#!/bin/bash
# PR #493 before/after demo — synthetic fixture data only (repo testdata).
export PATH=$HOME/workspace/tools/go/bin:$PATH
export GOPATH=$HOME/workspace/tools/gopath GOCACHE=$HOME/workspace/tools/gocache
export TMPDIR=$HOME/workspace/tools/tmp GOTMPDIR=$HOME/workspace/tools/tmp
export PS1='$ '
export GIT_PAGER=cat PAGER=cat TERM=dumb
cd ~/workspace/pr-evidence/pr-493 || exit 1
BEFORE=../pr-493-media/peasant-before2
AFTER=../pr-493-media/peasant-after3
TD=internal/harnesslayout/testdata
sum() { python3 -c "import json,sys; r=json.load(sys.stdin); roots=sum(x.get('sessions',0) for x in r.get('roots') or []); print(f\"  -> tool={r.get('tool')}  sessions discovered={roots}  captured={len(r.get('captures') or [])}  failures={len(r.get('failures') or [])}\")"; }
sleep 1
echo "================================================================"
echo " PR #493 — capture the Kiro, Kiro CLI, and Kiro Crew layouts"
echo "================================================================"
sleep 1
echo
echo "### BEFORE — base branch tip 92d67881 (#488 framework, no tools yet)"
sleep 1
echo "\$ peasant layout list"
$BEFORE layout list
echo "(no output: zero layouts are registered)"
sleep 1
for t in kiro kiro-cli kirocrew; do
  echo "\$ peasant layout capture $t"
  $BEFORE layout capture $t 2>&1 | head -1
  sleep 0.7
done
echo "\$ ls internal/harnesslayout/  (in the base checkout) | grep -i kiro"
ls ../pr-493-base2/internal/harnesslayout/ | grep -i kiro || echo "(no kiro files on base)"
sleep 1.5
echo
echo "### AFTER — PR head 9118db77 (rebased onto 92d67881 + grammar & SQLite-fork fixes)"
sleep 1
echo "\$ peasant layout list  (tool lines)"
$AFTER layout list | grep -E '^[A-Za-z].*\('
sleep 1
echo "\$ peasant layout capture kiro --path <fixture sessions> --path <fixture kiroagent> --limit 0"
$AFTER layout capture kiro --path $TD/kiro/sessions --path $TD/kiro/kiroagent --limit 0 2>/dev/null | sum
echo "   (the 1 failure is the deliberately truncated legacy .chat fixture)"
sleep 1
echo "\$ peasant layout capture kiro-cli --path <fixture cli sessions> --limit 0"
$AFTER layout capture kiro-cli --path $TD/kiro/sessions/cli --limit 0 2>/dev/null | sum
sleep 1
echo "\$ peasant layout capture kirocrew --path <fixture kirocrew root> --limit 0"
$AFTER layout capture kirocrew --path $TD/kirocrew --limit 0 2>/dev/null | sum
sleep 1.5
echo
echo "### Fixture tests (the sqlite conversation DB case is covered here)"
echo "\$ go test -race ./internal/harnesslayout/ -run 'TestKiro|TestKiroCrew' -count=1"
go test -race ./internal/harnesslayout/ -run 'TestKiro|TestKiroCrew' -count=1
sleep 1
echo "\$ go test -race ./internal/harnesslayout/ -count=1   (whole package)"
go test -race ./internal/harnesslayout/ -count=1
sleep 1
echo
echo "### Stack after rebase"
echo "\$ git log --oneline -7"
git log --oneline -7
sleep 2
echo
echo "Done."
sleep 1
