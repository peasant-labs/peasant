package codemap

import (
	"reflect"
	"testing"
)

// TestLabelIsInformative table-tests the server-side task-chip filter:
// path-like values, the session outcome, and negative-signal detector values
// are dropped; human annotations and positive signals pass.
func TestLabelIsInformative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		value   string
		outcome string
		want    bool
	}{
		// Filesystem-path-looking values.
		{"absolute path", "/tmp", "resolved", false},
		{"deep absolute path", "/Users/sampleuser/Documents/Projects/peasant/internal/", "resolved", false},
		{"home-relative path", "~/projects/peasant", "resolved", false},
		{"users path not at start", "file:/Users/someone/x", "resolved", false},

		// Outcome duplicate (already its own chip).
		{"equals outcome", "resolved", "resolved", false},
		{"equals outcome case-insensitive", "Resolved", "resolved", false},
		{"differs from outcome", "failed", "resolved", true},
		{"outcome value with empty outcome", "resolved", "", true},

		// Negative-signal detector values.
		{"not_detected", "not_detected", "resolved", false},
		{"unknown", "unknown", "resolved", false},
		{"none", "none", "resolved", false},
		{"negative signal uppercased", "NOT_DETECTED", "resolved", false},

		// Degenerate values.
		{"empty", "", "resolved", false},
		{"whitespace only", "   ", "resolved", false},

		// Genuinely informative values pass.
		{"scope feature", "feature", "resolved", true},
		{"scope bug", "bug", "resolved", true},
		{"positive frustration signal", "detected", "resolved", true},
		{"human annotation", "needs-follow-up", "resolved", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := labelIsInformative(tc.value, tc.outcome); got != tc.want {
				t.Errorf("labelIsInformative(%q, %q) = %v, want %v", tc.value, tc.outcome, got, tc.want)
			}
		})
	}
}

// TestInformativeLabels: the slice filter preserves input order and returns
// an empty (non-nil-safe via caller append) slice when everything filters.
func TestInformativeLabels(t *testing.T) {
	t.Parallel()

	got := informativeLabels(
		[]string{"/tmp", "feature", "resolved", "not_detected", "detected"},
		"resolved",
	)
	if want := []string{"feature", "detected"}; !reflect.DeepEqual(got, want) {
		t.Errorf("informativeLabels = %v, want %v", got, want)
	}

	if got := informativeLabels([]string{"/tmp", "not_detected"}, "resolved"); len(got) != 0 {
		t.Errorf("all-filtered labels = %v, want empty", got)
	}
}
