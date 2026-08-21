package gates_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/gates"
)

// --- pure-logic tests: synthetic content only, so they prove the scanner
// itself is correct independent of what internal/tui currently holds -------

func TestFindLayoutMatches_DetectsEveryPattern(t *testing.T) {
	t.Parallel()
	content := []byte(`package widget

func view(style lipgloss.Style) string {
	pad := strings.Repeat(" ", 4)
	box := style.Background(bg).Render(pad)
	row := style.Align(lipgloss.Center).Render(box)
	return lipgloss.Place(10, 3, lipgloss.Center, lipgloss.Center, row)
}
`)
	matches := gates.FindLayoutMatches("widget/widget.go", content)
	got := map[string]bool{}
	for _, m := range matches {
		got[m.Pattern] = true
	}
	for _, want := range []string{"hand-rolled-space-pad", "direct-place", "direct-align", "direct-background"} {
		if !got[want] {
			t.Errorf("FindLayoutMatches did not report pattern %q for content that contains it: %v", want, matches)
		}
	}
}

func TestFindLayoutMatches_CleanContentReportsNothing(t *testing.T) {
	t.Parallel()
	content := []byte(`package widget

import "github.com/peasant-labs/peasant/internal/tui/kit"

func view(th theme.Theme) string {
	panel := kit.NewPanel(th)
	panel.Text("one line")
	return panel.View()
}
`)
	if matches := gates.FindLayoutMatches("widget/widget.go", content); len(matches) != 0 {
		t.Fatalf("FindLayoutMatches reported %d hits on content that composes through the kit: %v", len(matches), matches)
	}
}

// TestFindLayoutMatches_IgnoresContextBackground proves the direct-background
// pattern reads the receiver, not the method name alone. context.Background
// is the most common call in the scanned tree and has nothing to do with a
// painted cell; flagging it would bury every real hit.
func TestFindLayoutMatches_IgnoresContextBackground(t *testing.T) {
	t.Parallel()
	content := []byte("ctx := context.Background()\n")
	if matches := gates.FindLayoutMatches("widget/widget.go", content); len(matches) != 0 {
		t.Fatalf("FindLayoutMatches flagged context.Background as a painted background: %v", matches)
	}
}

func TestFindLayoutMatches_ReportsCorrectLineNumber(t *testing.T) {
	t.Parallel()
	content := []byte("package widget\n\nvar x = 1\nvar pad = strings.Repeat(\" \", 3)\n")
	matches := gates.FindLayoutMatches("widget/widget.go", content)
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	for _, m := range matches {
		if m.Line != 4 {
			t.Errorf("match %+v has Line=%d, want 4 (the line with the hand-rolled pad)", m, m.Line)
		}
	}
}

// TestFindLayoutMatches_CountsPerOccurrenceNotPerLine keeps the count-pinned
// allowlist able to see a SECOND hand-rolled pad appended to a line that
// already carried one.
func TestFindLayoutMatches_CountsPerOccurrenceNotPerLine(t *testing.T) {
	t.Parallel()
	content := []byte(`row := strings.Repeat(" ", left) + text + strings.Repeat(" ", right)` + "\n")
	matches := gates.FindLayoutMatches("widget/widget.go", content)
	count := 0
	for _, m := range matches {
		if m.Pattern == "hand-rolled-space-pad" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("hand-rolled-space-pad occurrences on one line = %d, want 2", count)
	}
}

// --- the real gate: scans the actual tree ----------------------------------

// TestLayoutGate_MatchesAllowlistCounts is the gate itself. Every production
// surface under internal/tui (plus internal/push/wizard.go) must compose its
// lines through the kit layout primitives. A hit that no allowlist entry
// covers fails as a new violation, and a covered entry whose pinned count no
// longer matches the tree fails in either direction.
func TestLayoutGate_MatchesAllowlistCounts(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist(legacyAllowlistData)
	if err != nil {
		t.Fatalf("load testdata/legacy_allowlist.yaml: %v", err)
	}

	root := testutil.ModuleRoot(t)
	matches, err := gates.ScanForHandRolledLayout(root)
	if err != nil {
		t.Fatalf("scan for hand-rolled layout: %v", err)
	}
	if err := checkLayoutAllowlistCounts(allowlist, matches); err != nil {
		t.Fatal(err.Error())
	}
}

// TestLayoutGate_CatchesADeliberatelyHandRolledSurface is the mutation proof
// the epic asks for: a surface that pads and paints its own line, in a file
// no entry covers, must turn the gate red.
func TestLayoutGate_CatchesADeliberatelyHandRolledSurface(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist(legacyAllowlistData)
	if err != nil {
		t.Fatalf("load testdata/legacy_allowlist.yaml: %v", err)
	}
	handRolled := gates.FindLayoutMatches("internal/tui/settings/newsurface.go", []byte(
		"line += strings.Repeat(\" \", width-lipgloss.Width(line))\n"+
			"return styles.Muted.Background(canvas).Render(line)\n"))
	if len(handRolled) != 2 {
		t.Fatalf("the synthetic hand-rolled surface produced %d hits, want 2: %v", len(handRolled), handRolled)
	}
	err = checkLayoutAllowlistCounts(allowlist, handRolled)
	if err == nil {
		t.Fatal("a hand-rolled pad and background in an uncovered file must fail the layout gate, but it passed")
	}
	if !strings.Contains(err.Error(), "internal/tui/settings/newsurface.go") {
		t.Fatalf("error does not name the offending file: %v", err)
	}
}

// TestLayoutGate_CatchesARemovedHitWithoutUpdatingTheEntry is the other
// direction: migrating a legacy surface onto the kit without lowering its pin
// must fail too, so the allowlist shrinks to empty instead of going stale.
func TestLayoutGate_CatchesARemovedHitWithoutUpdatingTheEntry(t *testing.T) {
	t.Parallel()
	allowlist, err := gates.LoadAllowlist([]byte(`expectedEntryCount: 1
entries:
  - path: internal/tui/trends.go
    gates:
      - gate: layout
        expectedHits: 1
    retiredBy: "some epoch"
    reason: "one pre-existing hand-rolled background"
`))
	if err != nil {
		t.Fatalf("load synthetic allowlist: %v", err)
	}
	if err := checkLayoutAllowlistCounts(allowlist, nil); err == nil {
		t.Fatal("an entry overstating its actual hit count (1 pinned, 0 actual) must fail, but it passed")
	}
}

// checkLayoutAllowlistCounts is the production logic behind
// TestLayoutGate_MatchesAllowlistCounts, factored out so the mutation-proof
// tests above can drive it with synthetic matches instead of the real tree.
func checkLayoutAllowlistCounts(allowlist gates.Allowlist, matches []gates.LayoutMatch) error {
	paths := make([]string, len(matches))
	byPath := map[string][]gates.LayoutMatch{}
	for i, m := range matches {
		paths[i] = m.Path
		byPath[m.Path] = append(byPath[m.Path], m)
	}
	counts := gates.CountHitsByPath(paths)

	var problems strings.Builder
	for path, n := range counts {
		if _, ok := allowlist.Covers(path, gates.GateLayout); !ok {
			fmt.Fprintf(&problems, "  %s: %d hand-rolled layout hit(s), no allowlist entry covers this path at all.\n", path, n)
			for _, m := range byPath[path] {
				fmt.Fprintf(&problems, "    %s:%d: [%s] %s\n", m.Path, m.Line, m.Pattern, m.Text)
			}
		}
	}
	for _, entry := range allowlist.Entries {
		expected, ok := entry.ExpectedHits(gates.GateLayout)
		if !ok {
			continue
		}
		actual := entry.ActualHits(counts)
		switch {
		case actual > expected:
			fmt.Fprintf(&problems,
				"  %s: NEW hand-rolled layout found - expectedHits=%d but actual=%d.\n"+
					"    A hit count going UP means a surface padded, placed, aligned, or painted a background by hand "+
					"since this entry was pinned.\n"+
					"    fix: compose the lines through kit.Panel (or kit.FitLine, kit.FitCell, kit.ScrollStrip, "+
					"kit.Center, kit.Indent); if this really is more pre-existing legacy code, raise expectedHits and "+
					"say why in reason.\n",
				entry.Path, expected, actual)
		case actual < expected:
			fmt.Fprintf(&problems,
				"  %s: STALE allowlist entry - expectedHits=%d but actual=%d.\n"+
					"    A hit count going DOWN means hand-rolled layout was removed (e.g. migrated onto kit.Panel) "+
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
		"layout grep gate: allowlist count mismatch(es):\n%s"+
			"what: a hand-rolled layout hit's path has no covering allowlist entry, or a covering entry's pinned "+
			"expectedHits no longer matches the actual current hit count.\n"+
			"where: internal/tui/gates (layout.go scanner, allowlist.go count check), against "+
			"testdata/legacy_allowlist.yaml.\n"+
			"when: TestLayoutGate_MatchesAllowlistCounts.\n"+
			"means: either a surface re-implemented the pad-and-paint rule instead of composing through the kit "+
			"layout primitives, or the allowlist's accounting of pre-existing legacy layout code is out of date.",
		problems.String())
}
