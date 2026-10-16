package pullprogress_test

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/pullprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/pullprogress.yaml
var pullProgressYAML []byte

type pullProgressCase struct {
	Name       string `yaml:"name"`
	Done       int    `yaml:"done"`
	Total      int    `yaml:"total"`
	ElapsedSec int    `yaml:"elapsedSec"`
	Canceling  bool   `yaml:"canceling"`
	Canceled   bool   `yaml:"canceled"`
}

type pullProgressDocument struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Cases         []pullProgressCase `yaml:"cases"`
}

func loadPullProgressCases(t *testing.T) []pullProgressCase {
	t.Helper()
	var document pullProgressDocument
	if err := testutil.DecodeNamedFixtureYAML(pullProgressYAML, &document); err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		if row.Name == "" || byName[row.Name] {
			t.Fatalf("pull-progress fixture name %q is empty or duplicated", row.Name)
		}
		byName[row.Name] = true
	}
	for _, name := range document.RequiredNames {
		if !byName[name] {
			t.Fatalf("pull-progress fixture is missing required case %q", name)
		}
	}
	return document.Cases
}

// mount renders one fixture case through the production Model in the given
// theme and width. Height is fixed: the surface is three content rows and
// must never pad to a full screen.
func mount(t *testing.T, c pullProgressCase, mode theme.Mode, width int) string {
	t.Helper()
	model := pullprogress.New(theme.New(mode))
	model.SetTotal(c.Total)
	model.SetDone(c.Done)
	model.SetElapsed(time.Duration(c.ElapsedSec) * time.Second)
	if c.Canceling {
		model.MarkCanceling()
	}
	if c.Canceled {
		model.MarkCanceled()
	}
	model.SetSize(width, 0)
	return model.View()
}

// TestPullProgressMounted proves the mounted surface keeps its counts, its
// elapsed readout, its copy, and its bar in both themes at both terminal
// sizes. It is the mounted screenshot test for the bulk-pull bar: header,
// done/total, elapsed, bar cells, and the interrupt footer must all be
// present, every row must fit the terminal width, and the surface must
// never pad to a full screen.
func TestPullProgressMounted(t *testing.T) {
	t.Parallel()
	for _, c := range loadPullProgressCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
				for _, width := range []int{80, 120} {
					view := mount(t, c, mode, width)
					plain := ansi.Strip(view)
					if !strings.Contains(plain, "collective pull progress") {
						t.Errorf("theme %s width %d: missing header in\n%s", mode, width, plain)
					}
					if c.Total > 0 {
						want := fmt.Sprintf("%d/%d", c.Done, c.Total)
						if !strings.Contains(plain, want) {
							t.Errorf("theme %s width %d: missing count %q in\n%s", mode, width, want, plain)
						}
					}
					if !strings.Contains(plain, "█") && !strings.Contains(plain, "░") {
						t.Errorf("theme %s width %d: bar lost its cells in\n%s", mode, width, plain)
					}
					wantElapsed := fmt.Sprintf("elapsed: %ds", c.ElapsedSec)
					if !strings.Contains(plain, wantElapsed) {
						t.Errorf("theme %s width %d: missing elapsed readout %q in\n%s", mode, width, wantElapsed, plain)
					}
					wantFooter := "ctrl+c to cancel"
					if c.Canceling {
						wantFooter = "canceling pull..."
					}
					if c.Canceled {
						wantFooter = "pull canceled"
					}
					if !strings.Contains(plain, wantFooter) {
						t.Errorf("theme %s width %d: missing footer %q in\n%s", mode, width, wantFooter, plain)
					}
					if c.Canceling && strings.Contains(plain, "ctrl+c to cancel") {
						t.Errorf("theme %s width %d: canceling state kept the steady hint in\n%s", mode, width, plain)
					}
					if c.Canceling && strings.Contains(plain, "waiting for current work") {
						t.Errorf("theme %s width %d: canceling footer kept the long sentence in\n%s", mode, width, plain)
					}
					if c.Canceled && (strings.Contains(plain, "ctrl+c to cancel") || strings.Contains(plain, "canceling pull")) {
						t.Errorf("theme %s width %d: canceled state kept a live hint in\n%s", mode, width, plain)
					}
					lines := strings.Split(plain, "\n")
					if len(lines) != 3 {
						t.Errorf("theme %s width %d: rendered %d rows, want exactly 3 content rows", mode, width, len(lines))
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > width {
							t.Errorf("theme %s width %d: row exceeds terminal width: %q", mode, width, line)
						}
					}
					completed := c.Total > 0 && c.Done == c.Total
					steady := !c.Canceling && !c.Canceled
					if completed && steady {
						if !strings.Contains(plain, "✓") {
							t.Errorf("theme %s width %d: completed bar missing its status icon in\n%s", mode, width, plain)
						}
					}
				}
			}
		})
	}
}
