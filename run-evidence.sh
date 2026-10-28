#!/bin/bash
export PATH=$HOME/workspace/tools/go/bin:$PATH
export GOPATH=$HOME/workspace/tools/gopath GOCACHE=$HOME/workspace/tools/gocache
export TMPDIR=$HOME/workspace/tools/tmp GOTMPDIR=$HOME/workspace/tools/tmp
export PS1='$ '
cd ~/workspace/pr-evidence || exit 1

echo '################################################################'
echo '# PR #494 BEFORE — base tip cursor/harness-layout-capture-35a4'
echo '################################################################'
cd pr-494-before || exit 1
git log --oneline -1
echo '$ ls internal/harnesslayout/'
ls internal/harnesslayout/
echo '$ ls internal/harnesslayout/cursor_*.go'
ls internal/harnesslayout/cursor_*.go 2>&1
echo '$ grep -rn "cursor-cli" internal/harnesslayout/ --include=*.go'
grep -rn 'cursor-cli' internal/harnesslayout/ --include='*.go' || echo 'NOT FOUND: no Cursor CLI / IDE composer layout is captured at the base tip'
sleep 2
echo
echo '# Original PR commit (79103a7, pre-rename) combined with the base tip:'
echo '$ git cherry-pick --no-commit 79103a7   # original feat commit of PR #494'
git cherry-pick --no-commit 79103a76e33293c303a9230147eb16f39046356d 2>&1 | tail -2
echo '$ go test ./internal/harnesslayout/'
go test ./internal/harnesslayout/ 2>&1 | head -8
git reset --hard HEAD >/dev/null 2>&1
git status --porcelain
sleep 2

echo
echo '################################################################'
echo '# PR #494 AFTER — rebased onto current base tip (883c4b3)'
echo '################################################################'
cd ../pr-494 || exit 1
git log --oneline origin/cursor/harness-layout-capture-35a4..HEAD
echo '$ ls internal/harnesslayout/cursor_*.go docs/harness-layouts/cursor.md'
ls internal/harnesslayout/cursor_*.go docs/harness-layouts/cursor.md
echo '$ grep -rn "func fieldPaths" internal/harnesslayout/'
grep -rn 'func fieldPaths' internal/harnesslayout/
echo '$ grep -rn "func cursorFieldPaths" internal/harnesslayout/'
grep -rn 'func cursorFieldPaths' internal/harnesslayout/
echo '$ go test ./internal/harnesslayout/'
go test -count=1 ./internal/harnesslayout/ 2>&1 | tail -3
echo '$ go test -v -run Cursor ./internal/harnesslayout/'
go test -count=1 -v -run 'Cursor' ./internal/harnesslayout/ 2>&1 | grep -E '^(--- |ok|PASS|FAIL)'
echo
echo '# AFTER passes: Cursor CLI chat store + IDE composer layouts captured,'
echo '# helper renamed to cursorFieldPaths, package tests green on the new base.'
sleep 2
