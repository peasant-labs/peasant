package kickstart_test

import (
	"context"
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
	for _, row := range loadProgressCompletionDocument(t).Progress {
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
				first := inline.View().Content
				if inline.View().Content != first {
					t.Fatal("rendering alone changed the progress clock or estimate")
				}
			}
		})
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
