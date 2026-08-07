package kit_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// recordingSource is a deterministic kit.ContentSource for the async tests: it
// returns "body:<id>" for each id and records every id it was asked for, in
// order, so a test can prove the left pane's highlight moved (and thus that
// input was not blocked) during an in-flight load. It is mutex-guarded so the
// race detector is satisfied even though the load commands run synchronously
// in the test goroutine.
type recordingSource struct {
	mu    sync.Mutex
	calls []string
}

func (s *recordingSource) Content(id string, _ int) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, id)
	s.mu.Unlock()
	return "body:" + id, nil
}

func (s *recordingSource) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// newPreviewSplit builds a PreviewSplit over a fixed three-item list and the
// given source, sized to a region that renders both panes.
func newPreviewSplit(t *testing.T, src kit.ContentSource) kit.PreviewSplit {
	t.Helper()
	items := []kit.ListItem{
		kit.StringItem("alpha"),
		kit.StringItem("bravo"),
		kit.StringItem("charlie"),
	}
	ps := kit.NewPreviewSplit(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), src)
	ps.SetSize(40, 8)
	ps.Focus()
	return ps
}

// TestPreviewSplit_SpinnerWhileLoading proves a spinner (its label) shows in
// the preview pane while a load is in flight, and disappears once the result
// is applied.
func TestPreviewSplit_SpinnerWhileLoading(t *testing.T) {
	ps := newPreviewSplit(t, &recordingSource{})
	load := ps.Load()
	if !ps.Loading() {
		t.Fatal("split should be loading immediately after Load()")
	}
	if got := ps.View(); !strings.Contains(got, "loading preview") {
		t.Fatalf("preview pane should show the spinner label while loading; view:\n%s", got)
	}
	for _, msg := range collectMsgs(load) {
		ps, _ = ps.Update(msg)
	}
	if ps.Loading() {
		t.Fatal("split should not be loading after the result is applied")
	}
	if got := ps.View(); strings.Contains(got, "loading preview") {
		t.Fatalf("spinner label should be gone once loaded; view:\n%s", got)
	}
	if got := ps.View(); !strings.Contains(got, "body:alpha") {
		t.Fatalf("preview pane should show the loaded content; view:\n%s", got)
	}
}

// TestPreviewSplit_InputNeverBlockedDuringLoad drives navigation key presses
// while a preview load is still in flight and proves the highlight advances
// each time (so the source is asked for the newly-highlighted ids) - list
// input is never blocked on the preview load.
func TestPreviewSplit_InputNeverBlockedDuringLoad(t *testing.T) {
	src := &recordingSource{}
	ps := newPreviewSplit(t, src)

	// Kick the first load for alpha. Run its load command (which is what
	// calls the source) but DO NOT apply the result to the model: the load is
	// in flight. Input must still move the cursor immediately.
	_ = collectMsgs(ps.Load())
	if id, _ := ps.HighlightedID(); id != "alpha" {
		t.Fatalf("initial highlight = %q, want alpha", id)
	}

	next, cmd1 := ps.Update(keyPress(t, "down"))
	ps = next
	_ = collectMsgs(cmd1)
	if id, _ := ps.HighlightedID(); id != "bravo" {
		t.Fatalf("highlight after down = %q, want bravo (input blocked?)", id)
	}
	next, cmd2 := ps.Update(keyPress(t, "down"))
	ps = next
	_ = collectMsgs(cmd2)
	if id, _ := ps.HighlightedID(); id != "charlie" {
		t.Fatalf("highlight after second down = %q, want charlie (input blocked?)", id)
	}

	// Each highlight change requested a load for the newly-highlighted id.
	want := []string{"alpha", "bravo", "charlie"}
	if got := src.requested(); !equalStrings(got, want) {
		t.Fatalf("source requests = %v, want %v", got, want)
	}
}

// TestPreviewSplit_StaleResultDropped proves the stale-result guard: a late
// result for a since-de-highlighted item is dropped and never overwrites the
// current preview. The two load commands are captured before either result is
// applied, then applied in stale-first order.
func TestPreviewSplit_StaleResultDropped(t *testing.T) {
	ps := newPreviewSplit(t, &recordingSource{})

	// Load for alpha (seq 1), capturing its result command without applying.
	staleMsgs := collectMsgs(ps.Load())

	// Move to bravo (seq 2), capturing its result command.
	next, downCmd := ps.Update(keyPress(t, "down"))
	ps = next
	freshMsgs := collectMsgs(downCmd)

	if id, _ := ps.HighlightedID(); id != "bravo" {
		t.Fatalf("highlight = %q, want bravo", id)
	}

	// Apply the STALE alpha result first: it must be dropped.
	for _, msg := range staleMsgs {
		ps, _ = ps.Update(msg)
	}
	if got := ps.View(); strings.Contains(got, "body:alpha") {
		t.Fatalf("stale alpha result must be dropped, but preview shows it; view:\n%s", got)
	}
	if !ps.Loading() {
		t.Fatal("split should still be loading bravo after dropping the stale alpha result")
	}

	// Apply the FRESH bravo result: it must land.
	for _, msg := range freshMsgs {
		ps, _ = ps.Update(msg)
	}
	if got := ps.View(); !strings.Contains(got, "body:bravo") {
		t.Fatalf("fresh bravo result should be shown; view:\n%s", got)
	}
	if ps.Loading() {
		t.Fatal("split should not be loading after the fresh result lands")
	}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
