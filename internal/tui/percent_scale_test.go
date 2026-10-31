package tui_test

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui"
	"github.com/peasant-labs/schema"
)

// qualitySession is testSession with the two metrics the metrics engine
// stores on a 0–100 scale: signal density 90% and spec score 60.
func qualitySession() ingest.Session {
	s := testSession()
	density, spec := 90.0, 60.0
	s.Metadata.Quality = &schema.QualityMetrics{SignalDensity: &density, SpecQualityScore: &spec}
	return s
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(view string) string { return ansi.ReplaceAllString(view, "") }

// TestPercentMetricsAreNotScaledTwice guards issue 684: signal density and spec
// score are already 0–100, and the detail view, dashboard and trends
// multiplied them by 100 again (9000%, 6000 / 100).
func TestPercentMetricsAreNotScaledTwice(t *testing.T) {
	t.Parallel()
	sessions := []ingest.Session{qualitySession()}

	detail := tui.NewSession(sessions)
	detail, _ = detail.Update(tea.WindowSizeMsg{Width: 160, Height: 400})
	detail, _ = detail.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	dash, _ := tui.NewDashboard(sessions).Update(tea.WindowSizeMsg{Width: 160, Height: 400})
	trends, _ := tui.NewTrends(sessions).Update(tea.WindowSizeMsg{Width: 160, Height: 400})

	for name, c := range map[string]struct {
		view string
		want []string
	}{
		"detail":    {plain(detail.View()), []string{"Signal Density:  90%", "Spec Score:  60 / 100"}},
		"dashboard": {plain(dash.View()), []string{"90%"}},
		"trends":    {plain(trends.View()), []string{"  90%"}},
	} {
		for _, want := range c.want {
			if !strings.Contains(c.view, want) {
				t.Errorf("%s view misses %q", name, want)
			}
		}
		for _, bad := range []string{"9000%", "6000"} {
			if strings.Contains(c.view, bad) {
				t.Errorf("%s view shows the double-scaled %q:\n%s", name, bad, c.view)
			}
		}
	}
}
