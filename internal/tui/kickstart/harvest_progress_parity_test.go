package kickstart_test

import (
	"context"
	_ "embed"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Reuse the wizard's named YAML scenarios, including revised totals, bursts,
// stalls, concurrent stages, errors and completed clocks. Both production
// parents receive the same sequence; parity must hold beyond a static mount.
func TestHarvestAndKickstartProgressParity(t *testing.T) {
	cases := append(loadProgressCompletionDocument(t).Progress, loadHarvestTimingFixtures(t)...)
	for _, row := range cases {
		t.Run(row.Name, func(t *testing.T) {
			clock := &fixtureClock{now: time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)}
			progress := &fixtureProgressSource{}
			var tick func(time.Time) tea.Msg
			wizard, _, _ := newProgressProgram(t, progress, clock, func(context.Context) (*ftue.IngestResult, error) {
				return &ftue.IngestResult{New: 1}, nil
			}, nil, &tick)
			inline := ingestprogress.NewModel(progress, animation.IngestAnimation(), theme.New(theme.ModeDark), clock.Now(), nil)
			for _, observation := range row.Observations {
				clock.Advance(observation.AdvanceSeconds)
				progress.Set(observation.Stages)
				wizard, _ = wizard.Update(tick(clock.Now()))
				updated, _ := inline.Update(ingestprogress.TickMsg(clock.Now()))
				inline = updated.(ingestprogress.Model)
				want := progressMatrixLines(wizard.View())
				got := progressMatrixLines(inline.View().Content)
				if len(want) == 0 || !reflect.DeepEqual(got, want) {
					t.Fatalf("mounted progress differs at %s\nharvest: %v\nkickstart: %v", clock.Now(), got, want)
				}
				assertHarvestTiming(t, "kickstart", want, observation)
				assertHarvestTiming(t, "harvest", got, observation)
				first := inline.View().Content
				if inline.View().Content != first {
					t.Fatal("rendering alone changed the progress clock or estimate")
				}
			}
		})
	}
}

//go:embed testdata/guided/harvest_timing.yaml
var harvestTimingYAML []byte

func loadHarvestTimingFixtures(t *testing.T) []progressFixture {
	t.Helper()
	var doc struct {
		Required []string          `yaml:"required_cases"`
		Cases    []progressFixture `yaml:"cases"`
	}
	decodeSingleKnownFieldsDocument(t, "harvest timing", harvestTimingYAML, &doc)
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || len(c.Observations) == 0 {
			t.Fatalf("invalid timing fixture %q", c.Name)
		}
		seen[c.Name] = true
		for _, observation := range c.Observations {
			if len(observation.WantContains) == 0 || len(observation.WantElapsed) == 0 {
				t.Fatalf("timing fixture %q lacks independent expectations", c.Name)
			}
		}
	}
	if len(doc.Required) == 0 {
		t.Fatal("timing fixtures need required names")
	}
	for _, name := range doc.Required {
		if !seen[name] {
			t.Fatalf("missing timing fixture %q", name)
		}
	}
	return doc.Cases
}

func assertHarvestTiming(t *testing.T, surface string, lines []string, observation progressObservationFixture) {
	t.Helper()
	text := strings.Join(lines, "\n")
	for _, want := range observation.WantContains {
		if !strings.Contains(text, want) {
			t.Errorf("%s missing %q:\n%s", surface, want, text)
		}
	}
	for _, missing := range observation.WantMissing {
		if strings.Contains(text, missing) {
			t.Errorf("%s unexpectedly contains %q:\n%s", surface, missing, text)
		}
	}
	for stage, elapsed := range observation.WantElapsed {
		found := false
		for _, line := range lines {
			if strings.Contains(line, strings.ToLower(stage.String())) {
				found = true
				if !strings.HasSuffix(line, "  "+elapsed) {
					t.Errorf("%s %s elapsed should be %s: %s", surface, stage, elapsed, line)
				}
			}
		}
		if !found {
			t.Errorf("%s missing timed stage %s", surface, stage)
		}
	}
}

func progressMatrixLines(view string) []string {
	var result []string
	for _, line := range strings.Split(stripRender(view), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "total elapsed:") || strings.Contains(line, "estimate") {
			result = append(result, line)
			continue
		}
		for _, stage := range ingest.StageOrder {
			if strings.Contains(line, strings.ToLower(stage.String())) {
				result = append(result, line)
				break
			}
		}
	}
	return result
}
