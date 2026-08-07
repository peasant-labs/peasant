package gates_test

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/gates"
)

//go:embed testdata/legacy_allowlist.yaml
var legacyAllowlistData []byte

// --- pure-logic tests (mutation-proof: driven by synthetic content, not the
// real tree, so they prove the scanner and allowlist logic themselves are
// correct independent of whatever internal/tui currently contains) ---------

func TestFindColorMatches_DetectsAllThreePatterns(t *testing.T) {
	t.Parallel()
	content := []byte(`package widget

var bg = lipgloss.Color("#1a1b26")
var fg = lipgloss.ANSIColor(196)
const plain = "no color here"
`)
	matches := gates.FindColorMatches("widget/widget.go", content)
	got := map[string]bool{}
	for _, m := range matches {
		got[m.Pattern] = true
	}
	for _, want := range []string{"hex-literal", "lipgloss.Color-call", "ansi-int-color-literal"} {
		if !got[want] {
			t.Errorf("FindColorMatches did not report pattern %q for content that contains it: %v", want, matches)
		}
	}
}

func TestFindColorMatches_CleanContentReportsNothing(t *testing.T) {
	t.Parallel()
	content := []byte(`package widget

import "github.com/peasant-labs/peasant/internal/tui/theme"

func styles(th theme.Theme) theme.Styles { return th.Styles() }
`)
	if matches := gates.FindColorMatches("widget/widget.go", content); len(matches) != 0 {
		t.Fatalf("FindColorMatches reported %d hits on content with no hardcoded color: %v", len(matches), matches)
	}
}

func TestFindColorMatches_ReportsCorrectLineNumber(t *testing.T) {
	t.Parallel()
	content := []byte("package widget\n\nvar x = 1\nvar bg = lipgloss.Color(\"#123\")\n")
	matches := gates.FindColorMatches("widget/widget.go", content)
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	for _, m := range matches {
		if m.Line != 4 {
			t.Errorf("match %+v has Line=%d, want 4 (the line with the hardcoded value)", m, m.Line)
		}
	}
}

// TestFindColorMatches_DoesNotFlagAWordPrefixedIssueReference is the
// regression proof for a real false positive review mutation-testing found:
// the naive "# followed by 3-8 hex digits" pattern matched an ordinary
// GitHub issue reference like "peasant#166", because decimal digits are
// always valid hex digits too. A hex color literal is never preceded by a
// letter/digit/underscore in real Go source (it is always inside a quoted
// or backtick string), so the pattern requires that boundary.
func TestFindColorMatches_DoesNotFlagAWordPrefixedIssueReference(t *testing.T) {
	t.Parallel()
	content := []byte("// cross-surface naming inconsistency flagged in review (peasant#166 follow-up)\n")
	if matches := gates.FindColorMatches("widget/widget.go", content); len(matches) != 0 {
		t.Fatalf("FindColorMatches flagged an issue reference as a hex color: %v", matches)
	}
}

// TestFindColorMatches_RejectsInvalidHexLengths proves the pattern only
// accepts the four lengths a CSS/terminal hex color actually has (3, 4, 6,
// 8), not the naive "3 to 8" range, which would also match nonsense lengths
// like 5 or 7 that no real hex literal has.
func TestFindColorMatches_RejectsInvalidHexLengths(t *testing.T) {
	t.Parallel()
	if matches := gates.FindColorMatches("w.go", []byte(`x := "#abcde"`)); len(matches) != 0 {
		t.Fatalf("a 5-digit value must not match as a hex literal: %v", matches)
	}
	if matches := gates.FindColorMatches("w.go", []byte(`x := "#abcdefa"`)); len(matches) != 0 {
		t.Fatalf("a 7-digit value must not match as a hex literal: %v", matches)
	}
	if matches := gates.FindColorMatches("w.go", []byte(`x := "#abc"`)); len(matches) == 0 {
		t.Fatal("a valid 3-digit hex literal must still match")
	}
}

// TestFindColorMatches_CountsPerOccurrenceNotPerLine is the regression proof
// for the second same-file blindness review mutation-testing found: counting
// per matching LINE (regexp.MatchString) collapses two same-kind colors on
// one line into a single hit, so a SECOND color appended onto an
// already-matching line would be invisible to the count-pinned allowlist -
// the exact same-file blindness the count pin exists to close, just
// relocated to same-line. TestFindColorMatches_ReportsCorrectLineNumber
// above already covers one-hit-per-line; this covers TWO hits on one line,
// which that test cannot (its line has exactly one pattern occurrence).
func TestFindColorMatches_CountsPerOccurrenceNotPerLine(t *testing.T) {
	t.Parallel()
	content := []byte(`var a, b = lipgloss.Color("#111111"), lipgloss.Color("#222222")` + "\n")
	matches := gates.FindColorMatches("widget/widget.go", content)

	byPattern := map[string]int{}
	for _, m := range matches {
		byPattern[m.Pattern]++
	}
	if got := byPattern["lipgloss.Color-call"]; got != 2 {
		t.Errorf("lipgloss.Color-call occurrences on one line = %d, want 2 (two distinct calls, not one line-level hit)", got)
	}
	if got := byPattern["hex-literal"]; got != 2 {
		t.Errorf("hex-literal occurrences on one line = %d, want 2 (two distinct hex values, not one line-level hit)", got)
	}
}

func TestPathMatchesEntry_ExactFile(t *testing.T) {
	t.Parallel()
	if !gates.PathMatchesEntry("internal/push/wizard.go", "internal/push/wizard.go") {
		t.Fatal("exact path should match itself")
	}
	if gates.PathMatchesEntry("internal/push/wizard.go", "internal/push/other.go") {
		t.Fatal("exact path should not match a different file")
	}
}

func TestPathMatchesEntry_DirectoryGlob(t *testing.T) {
	t.Parallel()
	if !gates.PathMatchesEntry("internal/tui/ftue/*", "internal/tui/ftue/styles.go") {
		t.Fatal("dir/* should match a direct child")
	}
	if !gates.PathMatchesEntry("internal/tui/ftue/*", "internal/tui/ftue/sub/nested.go") {
		t.Fatal("dir/* should match a nested descendant")
	}
	if gates.PathMatchesEntry("internal/tui/ftue/*", "internal/tui/other/styles.go") {
		t.Fatal("dir/* must not match a sibling directory")
	}
	if gates.PathMatchesEntry("internal/tui/ftue/*", "internal/tui/ftue-2/styles.go") {
		t.Fatal("dir/* must not match a directory that merely shares a prefix")
	}
}

func TestCountHitsByPath(t *testing.T) {
	t.Parallel()
	counts := gates.CountHitsByPath([]string{"a.go", "b.go", "a.go", "a.go"})
	if counts["a.go"] != 3 || counts["b.go"] != 1 || len(counts) != 2 {
		t.Fatalf("CountHitsByPath = %v, want a.go=3 b.go=1", counts)
	}
}

func TestAllowlistEntry_ActualHits_SumsAcrossADirectoryGlob(t *testing.T) {
	t.Parallel()
	entry := gates.AllowlistEntry{Path: "internal/tui/ftue/*"}
	counts := map[string]int{
		"internal/tui/ftue/styles.go": 13,
		"internal/tui/ftue/wizard.go": 2,
		"internal/tui/other.go":       5,
	}
	if got := entry.ActualHits(counts); got != 15 {
		t.Fatalf("ActualHits = %d, want 15 (13+2, excluding the sibling file)", got)
	}
}

func TestLoadAllowlist_RejectsUnknownGateKind(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colours
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "typo'd kind"
`))
	if err == nil || !strings.Contains(err.Error(), "unknown gate") {
		t.Fatalf("error = %v, want rejection of an unknown gate kind", err)
	}
}

func TestLoadAllowlist_RejectsMissingRetiredBy(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: ""
    reason: "no owner named"
`))
	if err == nil || !strings.Contains(err.Error(), "no retiredBy") {
		t.Fatalf("error = %v, want rejection of a blank retiredBy", err)
	}
}

func TestLoadAllowlist_RejectsCountMismatch(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 2
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "one entry, declared as two"
`))
	if err == nil || !strings.Contains(err.Error(), "expectedEntryCount=2") {
		t.Fatalf("error = %v, want rejection of a declared/actual count mismatch", err)
	}
}

func TestLoadAllowlist_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "ok"
    typoField: "oops"
`))
	if err == nil {
		t.Fatal("expected rejection of an unknown field, got nil error")
	}
}

func TestLoadAllowlist_RejectsDuplicatePath(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 2
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "first"
  - path: internal/tui/x.go
    gates:
      - gate: keys
        expectedHits: 1
    retiredBy: "some other epoch"
    reason: "duplicate path"
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("error = %v, want rejection of a duplicate path", err)
	}
}

func TestLoadAllowlist_RejectsDuplicateGateWithinAnEntry(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 1
      - gate: colors
        expectedHits: 2
    retiredBy: "some epoch"
    reason: "colors named twice"
`))
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("error = %v, want rejection of a gate named twice in one entry", err)
	}
}

func TestLoadAllowlist_RejectsNegativeExpectedHits(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: -1
    retiredBy: "some epoch"
    reason: "negative count"
`))
	if err == nil || !strings.Contains(err.Error(), "expectedHits must be at least 1") {
		t.Fatalf("error = %v, want rejection of a negative expectedHits", err)
	}
}

// TestLoadAllowlist_RejectsZeroExpectedHits is the mutation proof that a
// fully-retired entry cannot linger pinned at zero: an entry pinned at
// expectedHits: 0 covers nothing and would never trip the up/down count
// comparison (0 pinned, 0 actual both match), so it would never be forced
// out - a fully-retired gate entry must be DELETED, never pinned at zero.
func TestLoadAllowlist_RejectsZeroExpectedHits(t *testing.T) {
	t.Parallel()
	_, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/x.go
    gates:
      - gate: colors
        expectedHits: 0
    retiredBy: "some epoch"
    reason: "fully retired but left pinned at zero instead of being deleted"
`))
	if err == nil {
		t.Fatal("LoadAllowlist accepted expectedHits: 0, want rejection - a zero-count entry must be deleted, not pinned")
	}
	for _, want := range []string{"expectedHits must be at least 1", "DELETE this fully-retired gate entry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q (an actionable delete-not-pin instruction)", err, want)
		}
	}
}

// --- the real gate: scans the actual tree ----------------------------------

// TestColorGate_MatchesAllowlistCounts is the gate itself, in both
// directions the count-pinned schema exists for:
//
//  1. every hardcoded-color hit path must be covered by SOME allowlist
//     entry (an entirely unlisted file is the "add an entry" failure), and
//  2. every colors-gated entry's ACTUAL current hit count (summed across a
//     directory glob) must equal its pinned expectedHits - not merely "the
//     path is listed". A rise means a NEW hardcoded color was introduced
//     since the count was pinned (a real regression a file-granular-only
//     check would miss - proved by
//     TestColorGate_MatchesAllowlistCounts_CatchesANewHitInAnAllowlistedFile
//     below); a fall means a hit was removed without updating the entry,
//     which folds in what a separate "is this entry still needed" check
//     would otherwise have to do (expectedHits reaching 0 with a real
//     actual of 0 is exactly the "stale entry, delete it" case).
func TestColorGate_MatchesAllowlistCounts(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist(legacyAllowlistData)
	if err != nil {
		t.Fatalf("load testdata/legacy_allowlist.yaml: %v", err)
	}

	root := testutil.ModuleRoot(t)
	matches, err := gates.ScanForHardcodedColors(root)
	if err != nil {
		t.Fatalf("scan for hardcoded colors: %v", err)
	}
	if err := checkAllowlistCounts(allowlist, matches); err != nil {
		t.Fatal(err.Error())
	}
}

// TestColorGate_MatchesAllowlistCounts_CatchesANewHitInAnAllowlistedFile is
// the mutation proof review asked for: it reproduces the exact probe that
// found the file-granular gap (appending a new lipgloss.Color(...) call to
// an already-allowlisted file) and asserts the count-pinned check now FAILS
// on it, using synthetic matches so it does not require mutating a real
// source file on disk.
func TestColorGate_MatchesAllowlistCounts_CatchesANewHitInAnAllowlistedFile(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/dashboard.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "one pre-existing color"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}

	// Reproduces today's real state: exactly the 1 pinned hit.
	before := []gates.ColorMatch{{Path: "internal/tui/dashboard.go", Line: 42, Pattern: "lipgloss.Color-call"}}
	if err := checkAllowlistCounts(allowlist, before); err != nil {
		t.Fatalf("the pinned count must pass against the matching actual count: %v", err)
	}

	// The mutation: a SECOND hardcoded color appended to the same
	// already-allowlisted file (review's probe: appending
	// lipgloss.Color("#abcdef") to internal/tui/dashboard.go).
	after := append(append([]gates.ColorMatch{}, before...),
		gates.ColorMatch{Path: "internal/tui/dashboard.go", Line: 99, Pattern: "hex-literal"})
	err = checkAllowlistCounts(allowlist, after)
	if err == nil {
		t.Fatal("a NEW hardcoded color in an already-allowlisted file must fail the count-pinned gate, but it passed")
	}
	if !strings.Contains(err.Error(), "NEW") || !strings.Contains(err.Error(), "internal/tui/dashboard.go") {
		t.Fatalf("error does not clearly identify a new hit in the file: %v", err)
	}
}

// TestColorGate_MatchesAllowlistCounts_CatchesARemovedHitWithoutUpdatingTheEntry
// is the other mutation direction: a hit disappearing (e.g. migrated onto
// theme.Theme) without lowering or deleting its allowlist entry must also
// fail, not silently pass - an overstated count is exactly as wrong as an
// understated one, and is what makes the entry's "shrinks to zero, then gets
// deleted" lifecycle checkable.
func TestColorGate_MatchesAllowlistCounts_CatchesARemovedHitWithoutUpdatingTheEntry(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/dashboard.go
    gates:
      - gate: colors
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "one pre-existing color"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}

	// The hit named by the entry no longer exists (e.g. migrated to
	// theme.Theme), but the entry was left at expectedHits: 1.
	err = checkAllowlistCounts(allowlist, nil)
	if err == nil {
		t.Fatal("an entry overstating its actual hit count (1 pinned, 0 actual) must fail, but it passed")
	}
	if !strings.Contains(err.Error(), "removed") && !strings.Contains(err.Error(), "lower") {
		t.Fatalf("error does not explain the count went down: %v", err)
	}
}

// checkAllowlistCounts is the production logic behind
// TestColorGate_MatchesAllowlistCounts, factored out so the mutation-proof
// tests above can drive it with synthetic matches instead of the real tree.
func checkAllowlistCounts(allowlist gates.Allowlist, matches []gates.ColorMatch) error {
	paths := make([]string, len(matches))
	byPathLine := map[string][]gates.ColorMatch{}
	for i, m := range matches {
		paths[i] = m.Path
		byPathLine[m.Path] = append(byPathLine[m.Path], m)
	}
	counts := gates.CountHitsByPath(paths)

	var problems strings.Builder

	for path, n := range counts {
		if _, ok := allowlist.Covers(path, gates.GateColors); !ok {
			fmt.Fprintf(&problems, "  %s: %d hardcoded-color hit(s), no allowlist entry covers this path at all.\n", path, n)
			for _, m := range byPathLine[path] {
				fmt.Fprintf(&problems, "    %s:%d: [%s] %s\n", m.Path, m.Line, m.Pattern, m.Text)
			}
		}
	}

	for _, entry := range allowlist.Entries {
		expected, ok := entry.ExpectedHits(gates.GateColors)
		if !ok {
			continue
		}
		actual := entry.ActualHits(counts)
		switch {
		case actual > expected:
			fmt.Fprintf(&problems,
				"  %s: NEW hardcoded color(s) found - expectedHits=%d but actual=%d.\n"+
					"    A hit count going UP means a hardcoded color was added since this entry was pinned.\n"+
					"    fix: use theme.New(mode).Styles() (or a Palette token) instead of a literal color; if this really "+
					"is more pre-existing legacy code, raise expectedHits and say why in reason.\n",
				entry.Path, expected, actual)
		case actual < expected:
			fmt.Fprintf(&problems,
				"  %s: STALE allowlist entry - expectedHits=%d but actual=%d.\n"+
					"    A hit count going DOWN means a hardcoded color was removed (e.g. migrated onto theme.Theme) "+
					"without updating the entry.\n"+
					"    fix: lower expectedHits to %d, or delete the entry entirely if it reached 0 (and decrement "+
					"expectedEntryCount).\n",
				entry.Path, expected, actual, actual)
		}
	}

	if problems.Len() == 0 {
		return nil
	}
	return fmt.Errorf(
		"color grep gate: allowlist count mismatch(es):\n%s"+
			"what: a hardcoded-color hit's path has no covering allowlist entry, or a covering entry's pinned "+
			"expectedHits no longer matches the actual current hit count.\n"+
			"where: internal/tui/gates (colors.go scanner, allowlist.go count check), against "+
			"testdata/legacy_allowlist.yaml.\n"+
			"when: TestColorGate_MatchesAllowlistCounts.\n"+
			"means: either a component picked a hardcoded color instead of deriving it from a theme.Theme, or the "+
			"allowlist's accounting of pre-existing legacy colors is out of date.",
		problems.String())
}
