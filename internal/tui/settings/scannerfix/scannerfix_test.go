package scannerfix_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
)

// countNodes walks a forest counting every node.
func countNodes(roots []*kit.TreeNode) int {
	n := 0
	var visit func(*kit.TreeNode)
	visit = func(node *kit.TreeNode) {
		n++
		for _, c := range node.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return n
}

// findState reports whether any node in the forest carries the given state.
func hasState(roots []*kit.TreeNode, st kit.TriState) bool {
	found := false
	var visit func(*kit.TreeNode)
	visit = func(node *kit.TreeNode) {
		if node.State == st {
			found = true
		}
		for _, c := range node.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return found
}

// TestNames_CoversTheCapturedFixtures pins the fixture set: the three captured
// scenarios must all be present, so dropping one fails here rather than
// silently shrinking downstream coverage.
func TestNames_CoversTheCapturedFixtures(t *testing.T) {
	names, err := scannerfix.Names()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"standard": false, "conflict": false, "large": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("expected fixture %q to be present; got %v", n, names)
		}
	}
}

// TestLoad_AllFixturesParseAndGuard proves every fixture parses and its
// row-count guard passes (Load returns an error when the declared count is
// wrong), then spot-checks the shape each fixture exists to capture.
func TestLoad_AllFixturesParseAndGuard(t *testing.T) {
	names, err := scannerfix.Names()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			roots, err := scannerfix.Load(name)
			if err != nil {
				t.Fatalf("load %q: %v", name, err)
			}
			if len(roots) == 0 {
				t.Fatalf("fixture %q has no roots", name)
			}
			if countNodes(roots) == 0 {
				t.Fatalf("fixture %q has no nodes", name)
			}
		})
	}
}

// TestLoad_ConflictScenario asserts the conflict fixture actually carries a
// Conflict node - the deleted-worktree case the tree renders distinctly.
func TestLoad_ConflictScenario(t *testing.T) {
	roots, err := scannerfix.Load("conflict")
	if err != nil {
		t.Fatal(err)
	}
	if !hasState(roots, kit.Conflict) {
		t.Fatal("conflict fixture must contain a Conflict node")
	}
}

// TestLoad_LargeScenario asserts the large fixture is genuinely large so it
// exercises scroll windowing and deep-indent truncation.
func TestLoad_LargeScenario(t *testing.T) {
	roots, err := scannerfix.Load("large")
	if err != nil {
		t.Fatal(err)
	}
	if got := countNodes(roots); got < 100 {
		t.Fatalf("large fixture has %d nodes; want a genuinely large tree (>=100)", got)
	}
}

// TestLoad_ReturnsIndependentCopies proves a mutation of one load's nodes does
// not leak into the next load - the property that lets the tree toggle state in
// place without corrupting the shared fixture.
func TestLoad_ReturnsIndependentCopies(t *testing.T) {
	first, err := scannerfix.Load("standard")
	if err != nil {
		t.Fatal(err)
	}
	first[0].State = kit.Checked
	first[0].Label = "MUTATED"
	second, err := scannerfix.Load("standard")
	if err != nil {
		t.Fatal(err)
	}
	if second[0].State == kit.Checked || second[0].Label == "MUTATED" {
		t.Fatal("Load must return an independent copy; a prior mutation leaked")
	}
}

// TestFixtureTreeSource_Load drives the deterministic source through the
// kit.TreeSource interface.
func TestFixtureTreeSource_Load(t *testing.T) {
	src := scannerfix.NewFixtureTreeSource("standard")
	roots, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("expected roots from FixtureTreeSource")
	}
}

// TestFixtureTreeSource_HonorsCancellation proves a cancelled context short-
// circuits Load rather than scanning.
func TestFixtureTreeSource_HonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := scannerfix.NewFixtureTreeSource("standard")
	if _, err := src.Load(ctx); err == nil {
		t.Fatal("expected a cancelled context to fail Load")
	}
}

// TestDelayedTreeSource_BlocksUntilRelease proves the wrapper holds Load until
// Release, then returns the inner forest.
func TestDelayedTreeSource_BlocksUntilRelease(t *testing.T) {
	d := scannerfix.NewDelayedTreeSource(scannerfix.NewFixtureTreeSource("standard"))
	done := make(chan []*kit.TreeNode, 1)
	go func() {
		roots, err := d.Load(context.Background())
		if err != nil {
			t.Errorf("delayed load: %v", err)
		}
		done <- roots
	}()
	select {
	case <-done:
		t.Fatal("Load returned before Release")
	default:
	}
	d.Release()
	roots := <-done
	if len(roots) == 0 {
		t.Fatal("expected roots after Release")
	}
}

// TestDelayedTreeSource_HonorsCancellation proves a blocked Load returns when
// its context is cancelled, without ever being released.
func TestDelayedTreeSource_HonorsCancellation(t *testing.T) {
	d := scannerfix.NewDelayedTreeSource(scannerfix.NewFixtureTreeSource("standard"))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := d.Load(ctx)
		errCh <- err
	}()
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("expected a cancelled blocked Load to return an error")
	}
}
