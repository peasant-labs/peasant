package codemap

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/schema"
)

// TestCommitSessionMap reverses the session→commits index into commit→session.
func TestCommitSessionMap(t *testing.T) {
	pd := &projectData{
		commitsByID: map[string][]commitRow{
			"sessA": {{hash: "h1"}, {hash: "h2"}},
			"sessB": {{hash: "h3"}},
		},
	}
	m := commitSessionMap(pd)
	for hash, want := range map[string]string{"h1": "sessA", "h2": "sessA", "h3": "sessB"} {
		if m[hash] != want {
			t.Errorf("commit %s → %q, want %q", hash, m[hash], want)
		}
	}
	if _, ok := m["nope"]; ok {
		t.Errorf("unknown commit should not be in the map")
	}
}

// TestUnusualSignals emits a neutral retry-rate elevation only when the change's
// per-conversation rate runs notably above the project baseline, with enough
// data on both sides.
func TestUnusualSignals(t *testing.T) {
	ri := func(n int) *int { return &n }
	boundTwo := []sessionBinding{
		{sessionID: "s1", binding: schema.ChangeBindingBound},
		{sessionID: "s2", binding: schema.ChangeBindingBound},
	}

	t.Run("elevated", func(t *testing.T) {
		pd := &projectData{metrics: map[string]metricRow{
			"s1":    {retryLoops: ri(5)},
			"s2":    {retryLoops: ri(4)},
			"base1": {retryLoops: ri(0)},
			"base2": {retryLoops: ri(0)},
			"base3": {retryLoops: ri(1)},
		}}
		got := unusualSignals(pd, boundTwo) // change 4.5/conv vs project 2.0/conv
		if len(got) != 1 || got[0].Kind != "retryLoops" {
			t.Fatalf("got %+v, want one retryLoops signal", got)
		}
		if got[0].PerChange <= got[0].PerProject {
			t.Errorf("PerChange %v should exceed PerProject %v", got[0].PerChange, got[0].PerProject)
		}
	})

	t.Run("not elevated when in line with baseline", func(t *testing.T) {
		pd := &projectData{metrics: map[string]metricRow{
			"s1": {retryLoops: ri(2)}, "s2": {retryLoops: ri(2)},
			"base1": {retryLoops: ri(2)}, "base2": {retryLoops: ri(2)}, "base3": {retryLoops: ri(2)},
		}}
		if got := unusualSignals(pd, boundTwo); len(got) != 0 {
			t.Errorf("want no signal when in line with baseline, got %+v", got)
		}
	})

	t.Run("not enough change data", func(t *testing.T) {
		pd := &projectData{metrics: map[string]metricRow{
			"s1": {retryLoops: ri(9)}, "base1": {retryLoops: ri(0)},
			"base2": {retryLoops: ri(0)}, "base3": {retryLoops: ri(0)},
		}}
		one := []sessionBinding{{sessionID: "s1", binding: schema.ChangeBindingBound}}
		if got := unusualSignals(pd, one); len(got) != 0 {
			t.Errorf("want no signal with a single change session, got %+v", got)
		}
	})
}

// TestAttributeHunk picks the recorded session that wrote most of a hunk's
// ADDED lines, tracking the new-line counter and ignoring removed lines.
func TestAttributeHunk(t *testing.T) {
	// new lines: 10 context, 11 add, 12 add, (a removed line), 13 context.
	h := gitops.Hunk{
		NewStart: 10,
		Lines: []gitops.DiffLine{
			{Kind: gitops.DiffLineContext},
			{Kind: gitops.DiffLineAdded},   // new line 11
			{Kind: gitops.DiffLineAdded},   // new line 12
			{Kind: gitops.DiffLineRemoved}, // does not advance the new counter
			{Kind: gitops.DiffLineContext},
		},
	}
	blame := make([]string, 13)
	blame[10] = "cA" // line 11
	blame[11] = "cB" // line 12
	toSession := map[string]string{"cA": "sessA", "cB": "sessB"}

	// Tie (1 line each) → deterministic by sessionID (sessA < sessB).
	if got := attributeHunk(h, blame, toSession); got != "sessA" {
		t.Errorf("tie attribution = %q, want sessA", got)
	}

	// Dominant: both added lines from cA → sessA wins 2-0.
	blame[11] = "cA"
	if got := attributeHunk(h, blame, toSession); got != "sessA" {
		t.Errorf("dominant attribution = %q, want sessA", got)
	}

	// No added line traces to a recorded session → unattributed.
	if got := attributeHunk(h, blame, map[string]string{}); got != "" {
		t.Errorf("unrecorded attribution = %q, want empty", got)
	}

	// Blame shorter than the file (out-of-range new line) is tolerated.
	if got := attributeHunk(h, []string{}, toSession); got != "" {
		t.Errorf("empty blame = %q, want empty", got)
	}
}
