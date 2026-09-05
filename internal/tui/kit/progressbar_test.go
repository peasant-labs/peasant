package kit_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// TestProgressBarMatchesHarvestFormat pins the single-row rendering: status
// icon, name padded to 13, 24-cell bar, raw count, no duration column.
func TestProgressBarMatchesHarvestFormat(t *testing.T) {
	t.Parallel()
	line := kit.ProgressBar("DISCOVER", 1, 4, false, false)
	filled, empty := strings.Count(line, "█"), strings.Count(line, "░")
	if filled != 6 || empty != 18 {
		t.Errorf("bar cells filled/empty=%d/%d, want 6/18 in %q", filled, empty, line)
	}
	for _, want := range []string{"●", "DISCOVER", "1/4"} {
		if !strings.Contains(line, want) {
			t.Errorf("bar row omits %q in %q", want, line)
		}
	}
	if strings.HasSuffix(line, " ") {
		t.Errorf("bar row without duration must not pad trailing cells in %q", line)
	}
}

// TestProgressMatrixAlignsDurations proves the matrix pads every count cell
// to the widest count in the set, so trailing durations start in one column,
// and that rows without a duration stay clean.
func TestProgressMatrixAlignsDurations(t *testing.T) {
	t.Parallel()
	lines := kit.ProgressMatrix([]kit.ProgressRow{
		{Label: "discover", Done: 1, Total: 4, Elapsed: "0s"},
		{Label: "diff"},
		{Label: "extract+write", Done: 15602, Total: 15602, Ended: true, Elapsed: "8s"},
	})
	if len(lines) != 3 {
		t.Fatalf("matrix rendered %d rows, want 3", len(lines))
	}
	first := strings.Index(lines[0], "0s")
	last := strings.Index(lines[2], "8s")
	if first < 0 || first != last {
		t.Errorf("durations start at columns %d and %d, want one aligned column:\n%s", first, last, strings.Join(lines, "\n"))
	}
	// Duration-less rows pad the count cell for alignment; trimming that
	// pad must recover the legacy single-row rendering exactly.
	if wantDiff := kit.ProgressBar("diff", 0, 0, false, false); strings.TrimRight(lines[1], " ") != strings.TrimRight(wantDiff, " ") {
		t.Errorf("duration-less row %q does not match the legacy row %q", lines[1], wantDiff)
	}
	for _, line := range lines {
		if !strings.Contains(line, "░") && !strings.Contains(line, "█") {
			t.Errorf("matrix row lost its bar cells in %q", line)
		}
	}
}
