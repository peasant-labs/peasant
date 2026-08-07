package gates_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/gates"
)

// This file is the HERMETIC layer of the key grep gate: it drives
// checkKeyAllowlistCounts - the count-comparison/allowlist-enforcement logic
// against the shared testdata/legacy_allowlist.yaml - with hand-built
// []gates.KeyMatch values. None of it invokes ast-grep or touches the real
// tree, so it runs under plain `go test ./...` with no external dependency,
// and proves the up/down/stale/zero-count enforcement fails closed
// independent of whatever internal/tui currently contains or whether the
// ast-grep binary is even installed.
//
// The REAL gate - real matches sourced from `ast-grep scan --json` against
// the real tree, checked against these same allowlist entries - lives in
// keys_astgrep_test.go (`//go:build astgrep`), which calls the exact same
// checkKeyAllowlistCounts defined below.

func TestKeyGate_RejectsUnknownGateKind(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: keys
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "sanity: keys is a recognized gate kind"
`))
	if err != nil {
		t.Fatalf("gate kind %q must be recognized: %v", gates.GateKeys, err)
	}
}

// TestKeyGate_MatchesAllowlistCounts_CatchesANewHitInAnAllowlistedFile is the
// same mutation proof colors_test.go carries for the color gate, reproduced
// for the key gate: a NEW raw key comparison appended to an
// already-allowlisted file must fail the count-pinned check. Hermetic: the
// "matches" are hand-built, not sourced from any scanner.
func TestKeyGate_MatchesAllowlistCounts_CatchesANewHitInAnAllowlistedFile(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/session.go
    gates:
      - gate: keys
        expectedHits: 2
    retiredBy: "some epoch"
    reason: "two pre-existing raw switches"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}

	before := []gates.KeyMatch{
		{Path: "internal/tui/session.go", Line: 10, Pattern: "no-raw-key-string-switch"},
		{Path: "internal/tui/session.go", Line: 20, Pattern: "no-raw-key-string-switch"},
	}
	if err := checkKeyAllowlistCounts(allowlist, before); err != nil {
		t.Fatalf("the pinned count must pass against the matching actual count: %v", err)
	}

	after := append(append([]gates.KeyMatch{}, before...),
		gates.KeyMatch{Path: "internal/tui/session.go", Line: 30, Pattern: "no-raw-key-string-equality"})
	err = checkKeyAllowlistCounts(allowlist, after)
	if err == nil {
		t.Fatal("a NEW raw key comparison in an already-allowlisted file must fail the count-pinned gate, but it passed")
	}
	if !strings.Contains(err.Error(), "NEW") || !strings.Contains(err.Error(), "internal/tui/session.go") {
		t.Fatalf("error does not clearly identify a new hit in the file: %v", err)
	}
}

// TestKeyGate_MatchesAllowlistCounts_CatchesARemovedHitWithoutUpdatingTheEntry
// is the other mutation direction: a hit disappearing (e.g. migrated onto
// keymap.Match) without lowering or deleting its allowlist entry must also
// fail, not silently pass. Hermetic: no scanning involved.
func TestKeyGate_MatchesAllowlistCounts_CatchesARemovedHitWithoutUpdatingTheEntry(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/annotation_editor.go
    gates:
      - gate: keys
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "one pre-existing raw switch"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}

	err = checkKeyAllowlistCounts(allowlist, nil)
	if err == nil {
		t.Fatal("an entry overstating its actual hit count (1 pinned, 0 actual) must fail, but it passed")
	}
	if !strings.Contains(err.Error(), "removed") && !strings.Contains(err.Error(), "lower") {
		t.Fatalf("error does not explain the count went down: %v", err)
	}
}

// TestKeyGate_MatchesAllowlistCounts_UnlistedPathFails is the third
// mutation direction: a hit whose path has NO covering allowlist entry at
// all (as opposed to a covered entry with the wrong count) must fail too -
// the "add an allowlist entry" failure mode, distinct from "update the
// pinned count".
func TestKeyGate_MatchesAllowlistCounts_UnlistedPathFails(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/session.go
    gates:
      - gate: keys
        expectedHits: 2
    retiredBy: "some epoch"
    reason: "two pre-existing raw switches"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}

	matches := []gates.KeyMatch{
		{Path: "internal/tui/session.go", Line: 10, Pattern: "no-raw-key-string-switch"},
		{Path: "internal/tui/session.go", Line: 20, Pattern: "no-raw-key-string-switch"},
		{Path: "internal/tui/dashboard.go", Line: 5, Pattern: "no-raw-key-string-equality"},
	}
	err = checkKeyAllowlistCounts(allowlist, matches)
	if err == nil {
		t.Fatal("a hit in a completely unlisted file must fail, but it passed")
	}
	if !strings.Contains(err.Error(), "internal/tui/dashboard.go") || !strings.Contains(err.Error(), "no allowlist entry covers this path at all") {
		t.Fatalf("error does not clearly identify the unlisted path: %v", err)
	}
}

// checkKeyAllowlistCounts mirrors colors_test.go's checkAllowlistCounts, the
// same production logic reused for the "keys" gate kind instead of
// "colors" - shared by this file's hermetic mutation-proof tests AND
// keys_astgrep_test.go's real-tree gate (which calls this exact function
// with matches sourced from ast-grep instead of hand-built).
func checkKeyAllowlistCounts(allowlist gates.Allowlist, matches []gates.KeyMatch) error {
	paths := make([]string, len(matches))
	byPathLine := map[string][]gates.KeyMatch{}
	for i, m := range matches {
		paths[i] = m.Path
		byPathLine[m.Path] = append(byPathLine[m.Path], m)
	}
	counts := gates.CountHitsByPath(paths)

	var problems strings.Builder

	for path, n := range counts {
		if _, ok := allowlist.Covers(path, gates.GateKeys); !ok {
			fmt.Fprintf(&problems, "  %s: %d raw key-string hit(s), no allowlist entry covers this path at all.\n", path, n)
			for _, m := range byPathLine[path] {
				fmt.Fprintf(&problems, "    %s:%d: [%s] %s\n", m.Path, m.Line, m.Pattern, m.Text)
			}
		}
	}

	for _, entry := range allowlist.Entries {
		expected, ok := entry.ExpectedHits(gates.GateKeys)
		if !ok {
			continue
		}
		actual := entry.ActualHits(counts)
		switch {
		case actual > expected:
			fmt.Fprintf(&problems,
				"  %s: NEW raw key-string comparison(s) found - expectedHits=%d but actual=%d.\n"+
					"    A hit count going UP means a new msg.String() comparison or raw key-string switch was added "+
					"since this entry was pinned.\n"+
					"    fix: dispatch through keymap.Match against a named keymap.ActionID instead; if this really "+
					"is more pre-existing legacy code, raise expectedHits and say why in reason.\n",
				entry.Path, expected, actual)
		case actual < expected:
			fmt.Fprintf(&problems,
				"  %s: STALE allowlist entry - expectedHits=%d but actual=%d.\n"+
					"    A hit count going DOWN means a raw key comparison was removed (e.g. migrated onto "+
					"keymap.Match) without updating the entry.\n"+
					"    fix: lower expectedHits to %d, or delete the entry entirely if it reached 0 (and decrement "+
					"expectedEntryCount).\n",
				entry.Path, expected, actual, actual)
		}
	}

	if problems.Len() == 0 {
		return nil
	}
	return fmt.Errorf(
		"key grep gate: allowlist count mismatch(es):\n%s"+
			"what: a raw key-string hit's path has no covering allowlist entry, or a covering entry's pinned "+
			"expectedHits no longer matches the actual current hit count.\n"+
			"where: internal/tui/gates (astrules/ ast-grep rules via keys_astgrep_test.go, allowlist.go count "+
			"check), against testdata/legacy_allowlist.yaml.\n"+
			"when: the key grep gate (go test -tags=astgrep ./internal/tui/gates/...).\n"+
			"means: either a component compared/switched on tea.KeyPressMsg.String() directly instead of dispatching "+
			"through keymap.Match, or the allowlist's accounting of pre-existing legacy key comparisons is out of "+
			"date.",
		problems.String())
}
