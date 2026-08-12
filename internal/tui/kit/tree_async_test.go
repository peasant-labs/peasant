package kit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// blockingSource blocks Load until release is closed (or ctx is cancelled),
// then returns the fixed forest. It is the async harness's instrument, kept
// local to the kit tests so the tree's own package needs no test-only source.
type blockingSource struct {
	release chan struct{}
	roots   []*kit.TreeNode
}

func newBlockingSource(roots []*kit.TreeNode) *blockingSource {
	return &blockingSource{release: make(chan struct{}), roots: roots}
}

func (b *blockingSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	select {
	case <-b.release:
		return b.roots, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func simpleForest() []*kit.TreeNode {
	return []*kit.TreeNode{{
		ID:    "p",
		Label: "provider",
		Children: []*kit.TreeNode{
			{ID: "a", Label: "session-a"},
			{ID: "b", Label: "session-b"},
		},
	}}
}

// TestTree_SpinnerWhileLoading_InputLive_ThenReplaced is the async harness. It
// asserts (a) a spinner frame renders while the source is blocked, (b) scripted
// input is processed while loading (Update returns without blocking and the
// tree stays in its loading state), and (c) the loaded forest replaces the
// spinner once the source is released.
func TestTree_SpinnerWhileLoading_InputLive_ThenReplaced(t *testing.T) {
	src := newBlockingSource(simpleForest())
	tr := kit.NewTree(darkTheme(), src)
	tr.SetSize(40, 10)

	tr, cmd := tr.Load()
	if !tr.Loading() {
		t.Fatal("tree should be loading immediately after Load")
	}

	// Run the (blocking) load command off the main goroutine.
	msgCh := make(chan tea.Msg, 1)
	go func() { msgCh <- cmd() }()

	// (a) A spinner frame renders while the source is blocked.
	loadingView := tr.View()
	spin := kit.NewSpinner(darkTheme(), "scanning projects")
	spin.SetSize(40, 1)
	if !strings.Contains(loadingView, strings.TrimSpace(spin.CurrentFrame())) {
		t.Fatalf("loading view should render a spinner frame; got %q", loadingView)
	}

	// (b) Scripted input is processed while loading - nav/back/help must not
	// block or panic, and the tree stays loading (no forest to move over yet).
	for _, k := range []string{"down", "up", "esc", "?"} {
		tr, _ = tr.Update(keyPress(t, k))
		if !tr.Loading() {
			t.Fatalf("input %q ended the loading state prematurely", k)
		}
	}
	if _, ok := tr.CurrentNode(); ok {
		t.Fatal("no node should be current while still loading")
	}

	// Release the source and deliver its result.
	src.release <- struct{}{}
	var msg tea.Msg
	select {
	case msg = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("load command never returned after release")
	}
	tr, _ = tr.Update(msg)

	// (c) The loaded forest replaces the spinner.
	if tr.Loading() {
		t.Fatal("tree should not be loading after its result arrives")
	}
	node, ok := tr.CurrentNode()
	if !ok || node.ID != "p" {
		t.Fatalf("loaded tree should have a current node; got %v/%v", node, ok)
	}
	if got := tr.View(); strings.Contains(got, strings.TrimSpace(spin.CurrentFrame())) && !strings.Contains(got, "provider") {
		t.Fatalf("loaded view should render the forest, not the spinner; got %q", got)
	}
}

// TestTree_StaleResultDropped proves the generation guard: a result from an
// earlier Load, delivered after the tree was re-sourced, is dropped, and only
// the current generation's result is accepted.
func TestTree_StaleResultDropped(t *testing.T) {
	firstRoots := []*kit.TreeNode{{ID: "first", Label: "first"}}
	secondRoots := []*kit.TreeNode{{ID: "second", Label: "second"}}

	src1 := newBlockingSource(firstRoots)
	tr := kit.NewTree(darkTheme(), src1)
	tr.SetSize(40, 10)

	// First load (generation 1).
	tr, cmd1 := tr.Load()
	stale := make(chan tea.Msg, 1)
	go func() { stale <- cmd1() }()
	src1.release <- struct{}{}
	staleMsg := <-stale

	// Re-source before delivering the first result (generation 2).
	src2 := newBlockingSource(secondRoots)
	tr = tr.WithSource(src2) // re-point the source
	tr, cmd2 := tr.Load()

	// Deliver the STALE (generation-1) result: it must be dropped - the tree
	// stays loading and shows no forest.
	tr, _ = tr.Update(staleMsg)
	if !tr.Loading() {
		t.Fatal("stale result should have been dropped, leaving the tree loading")
	}
	if _, ok := tr.CurrentNode(); ok {
		t.Fatal("stale result must not populate the forest")
	}

	// Deliver the current (generation-2) result.
	fresh := make(chan tea.Msg, 1)
	go func() { fresh <- cmd2() }()
	src2.release <- struct{}{}
	tr, _ = tr.Update(<-fresh)

	if tr.Loading() {
		t.Fatal("current-generation result should have been accepted")
	}
	node, ok := tr.CurrentNode()
	if !ok || node.ID != "second" {
		t.Fatalf("tree should show the re-sourced forest; got %v/%v", node, ok)
	}
}

// TestTree_CancelledLoadDropped proves a cancelled in-flight load surfaces its
// context error and does not populate the forest.
func TestTree_CancelledLoadDropped(t *testing.T) {
	src := newBlockingSource(simpleForest())
	tr := kit.NewTree(darkTheme(), src)
	tr.SetSize(40, 10)

	tr, cmd := tr.Load()
	// Re-source: Load cancels the previous context, so the first command's
	// blocked Load returns a context error.
	tr2 := tr.WithSource(newBlockingSource(simpleForest()))
	tr2, _ = tr2.Load()

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		// The stale/cancelled result is dropped by generation regardless.
		tr2, _ = tr2.Update(msg)
		if _, ok := tr2.CurrentNode(); ok {
			t.Fatal("cancelled/stale load must not populate the forest")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled load never returned")
	}
}
