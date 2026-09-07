package kit_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/tui/kit"
	"gopkg.in/yaml.v3"
)

type progressBarCase struct {
	Name     string `yaml:"name"`
	Label    string `yaml:"label"`
	Done     int    `yaml:"done"`
	Total    int    `yaml:"total"`
	Ended    bool   `yaml:"ended"`
	HasError bool   `yaml:"hasError"`
	Filled   int    `yaml:"filled"`
	Empty    int    `yaml:"empty"`
	Icon     string `yaml:"icon"`
	Count    string `yaml:"count"`
}

type progressBarDocument struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []progressBarCase `yaml:"cases"`
}

//go:embed testdata/progressbar.yaml
var progressBarFixtureData []byte

func loadProgressBarCases(t *testing.T) []progressBarCase {
	t.Helper()
	var document progressBarDocument
	if err := yaml.Unmarshal(progressBarFixtureData, &document); err != nil {
		t.Fatalf("decode testdata/progressbar.yaml: %v", err)
	}
	byName := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		if row.Name == "" || byName[row.Name] {
			t.Fatalf("progress-bar fixture name %q is empty or duplicated", row.Name)
		}
		byName[row.Name] = true
	}
	for _, name := range document.RequiredNames {
		if !byName[name] {
			t.Fatalf("progress-bar fixture is missing required case %q", name)
		}
	}
	return document.Cases
}

// TestProgressBarMatchesHarvestFormat pins the single-row rendering: status
// icon, name padded to 13, 24-cell bar, raw count, no duration column.
func TestProgressBarMatchesHarvestFormat(t *testing.T) {
	t.Parallel()
	for _, testCase := range loadProgressBarCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			line := kit.ProgressBar(testCase.Label, testCase.Done, testCase.Total, testCase.Ended, testCase.HasError)
			filled, empty := strings.Count(line, "█"), strings.Count(line, "░")
			if filled != testCase.Filled || empty != testCase.Empty {
				t.Errorf("bar cells filled/empty=%d/%d, want %d/%d in %q", filled, empty, testCase.Filled, testCase.Empty, line)
			}
			for _, want := range []string{testCase.Icon, testCase.Label, testCase.Count} {
				if !strings.Contains(line, want) {
					t.Errorf("bar row omits %q in %q", want, line)
				}
			}
			if strings.Contains(line, "\x1b[") {
				t.Errorf("raw progress bar contains ANSI styling: %q", line)
			}
			if strings.HasSuffix(line, " ") {
				t.Errorf("bar row without duration must not pad trailing cells in %q", line)
			}
		})
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
	first := lines[0].Bar + "  " + lines[0].Elapsed
	last := lines[2].Bar + "  " + lines[2].Elapsed
	if a, b := strings.Index(first, "0s"), strings.Index(last, "8s"); a < 0 || a != b {
		t.Errorf("durations start at columns %d and %d, want one aligned column:\n%s\n%s", a, b, first, last)
	}
	if lines[1].Elapsed != "" {
		t.Errorf("duration-less row carries %q", lines[1].Elapsed)
	}
	// Duration-less rows pad the count cell for alignment; trimming that
	// pad must recover the legacy single-row rendering exactly.
	if wantDiff := kit.ProgressBar("diff", 0, 0, false, false); strings.TrimRight(lines[1].Bar, " ") != strings.TrimRight(wantDiff, " ") {
		t.Errorf("duration-less row %q does not match the legacy row %q", lines[1].Bar, wantDiff)
	}
	for _, line := range lines {
		if !strings.Contains(line.Bar, "░") && !strings.Contains(line.Bar, "█") {
			t.Errorf("matrix row lost its bar cells in %q", line.Bar)
		}
	}
}
