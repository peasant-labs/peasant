package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// TrendsModel displays token, session, and quality trends as horizontal bar charts.
type TrendsModel struct {
	sessions []ingest.Session
	viewport viewport.Model
	ready    bool
	width    int
	height   int
}

// NewTrends creates a TrendsModel with the given sessions.
func NewTrends(sessions []ingest.Session) TrendsModel {
	return TrendsModel{sessions: sessions}
}

func (m TrendsModel) Init() tea.Cmd {
	return nil
}

func (m TrendsModel) Update(msg tea.Msg) (TrendsModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentHeight := m.height - 4
		if contentHeight < 3 {
			contentHeight = 3
		}
		if !m.ready {
			m.viewport = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(contentHeight))
			m.ready = true
		} else {
			m.viewport.SetWidth(m.width)
			m.viewport.SetHeight(contentHeight)
		}
		m.viewport.SetContent(m.renderContent())
		return m, nil
	}

	if m.ready {
		m.viewport, cmd = m.viewport.Update(msg)
	}
	return m, cmd
}

func (m TrendsModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if !m.ready {
		return "Loading..."
	}
	return m.viewport.View()
}

// dayStats aggregates per-day data for all chart types.
type dayStats struct {
	date           time.Time
	tokens         int
	sessions       int
	retryLoops     int
	signalDensityN int
	signalDensityS float64
	revertN        int
	revertS        int
	specScoreN     int
	specScoreS     float64
}

func (m TrendsModel) aggregateDays() []*dayStats {
	dayMap := make(map[string]*dayStats)
	for _, s := range m.sessions {
		key := s.StartTime.Format("2006-01-02")
		d, ok := dayMap[key]
		if !ok {
			d = &dayStats{
				date: time.Date(s.StartTime.Year(), s.StartTime.Month(), s.StartTime.Day(), 0, 0, 0, 0, time.UTC),
			}
			dayMap[key] = d
		}
		d.tokens += s.Metadata.TotalTokens
		d.sessions++
		if s.Metadata.Quality != nil {
			q := s.Metadata.Quality
			d.retryLoops += derefInt(q.RetryLoops)
			d.signalDensityS += derefFloat(q.SignalDensity)
			d.signalDensityN++
			d.revertS += derefInt(q.WithinSessionReverts)
			d.revertN++
			d.specScoreS += derefFloat(q.SpecQualityScore)
			d.specScoreN++
		}
	}

	// Fill missing days for the last 7 days.
	now := time.Now().UTC()
	days := make([]*dayStats, defaults.TrendsDaysLookback)
	for i := 0; i < defaults.TrendsDaysLookback; i++ {
		date := now.AddDate(0, 0, -6+i)
		key := date.Format("2006-01-02")
		if ds, ok := dayMap[key]; ok {
			days[i] = ds
		} else {
			days[i] = &dayStats{date: date}
		}
	}

	sort.Slice(days, func(i, j int) bool {
		return days[i].date.Before(days[j].date)
	})

	return days
}

func (m TrendsModel) renderContent() string {
	days := m.aggregateDays()

	labelWidth := 14
	valueWidth := 18
	barMax := m.width - labelWidth - valueWidth
	if barMax < 10 {
		barMax = 10
	}

	var b strings.Builder

	// 1. Tokens per Day
	b.WriteString(SectionHeaderCost.Render("Tokens per Day (Last 7 Days)"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelSky,
		func(d *dayStats) float64 { return float64(d.tokens) },
		func(d *dayStats) string {
			if d.tokens > 0 {
				return fmt.Sprintf("  %s tokens", formatNumber(d.tokens))
			}
			return ""
		},
	)

	// 2. Sessions per Day
	b.WriteString("\n")
	b.WriteString(SectionHeaderBehavioral.Render("Sessions per Day"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelSky,
		func(d *dayStats) float64 { return float64(d.sessions) },
		func(d *dayStats) string {
			if d.sessions > 0 {
				return fmt.Sprintf("  %d sessions", d.sessions)
			}
			return ""
		},
	)

	// 3. Retry Loops per Day
	b.WriteString("\n")
	b.WriteString(SectionHeaderBehavioral.Render("Retry Loops per Day"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelLemon,
		func(d *dayStats) float64 { return float64(d.retryLoops) },
		func(d *dayStats) string {
			if d.retryLoops > 0 {
				return fmt.Sprintf("  %d loops", d.retryLoops)
			}
			return ""
		},
	)

	// 4. Signal Density per Day (avg %)
	b.WriteString("\n")
	b.WriteString(SectionHeaderBehavioral.Render("Signal Density per Day"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelLilac,
		func(d *dayStats) float64 {
			if d.signalDensityN > 0 {
				return d.signalDensityS / float64(d.signalDensityN) * 100
			}
			return 0
		},
		func(d *dayStats) string {
			if d.signalDensityN > 0 {
				avg := d.signalDensityS / float64(d.signalDensityN) * 100
				return fmt.Sprintf("  %.0f%%", avg)
			}
			return ""
		},
	)

	// 5. Revert Rate per Day (avg per session)
	b.WriteString("\n")
	b.WriteString(SectionHeaderQuality.Render("Reverts per Day"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelCoral,
		func(d *dayStats) float64 { return float64(d.revertS) },
		func(d *dayStats) string {
			if d.revertS > 0 {
				return fmt.Sprintf("  %d reverts", d.revertS)
			}
			return ""
		},
	)

	// 6. Spec Score per Day (avg 0–100)
	b.WriteString("\n")
	b.WriteString(SectionHeaderQuality.Render("Spec Score per Day"))
	b.WriteString("\n\n")
	renderBarChart(&b, days, barMax, labelWidth, defaults.ColorPastelMint,
		func(d *dayStats) float64 {
			if d.specScoreN > 0 {
				return d.specScoreS / float64(d.specScoreN) * 100
			}
			return 0
		},
		func(d *dayStats) string {
			if d.specScoreN > 0 {
				avg := d.specScoreS / float64(d.specScoreN) * 100
				return fmt.Sprintf("  %.0f", avg)
			}
			return ""
		},
	)

	// Summary footer
	var totalTokens, totalSessions int
	for _, d := range days {
		totalTokens += d.tokens
		totalSessions += d.sessions
	}
	b.WriteString("\n")
	b.WriteString(DimStyle.Render(fmt.Sprintf("7-day total: %s tokens across %d sessions",
		formatNumber(totalTokens), totalSessions)))

	return b.String()
}

// renderBarChart renders a horizontal bar chart for the given data.
func renderBarChart(
	b *strings.Builder,
	days []*dayStats,
	barMax int,
	labelWidth int,
	color defaults.Color,
	valueFn func(*dayStats) float64,
	labelFn func(*dayStats) string,
) {
	blocks := []rune("▏▎▍▌▋▊▉█")

	var maxVal float64
	for _, d := range days {
		v := valueFn(d)
		if v > maxVal {
			maxVal = v
		}
	}

	barStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(color.String())).Background(baseBg)

	for _, d := range days {
		label := DimStyle.Render(d.date.Format("Mon Jan 02") + "  ")

		var bar string
		v := valueFn(d)
		if maxVal > 0 && v > 0 {
			fullWidth := v / maxVal * float64(barMax)
			fullBlocks := int(fullWidth)
			remainder := fullWidth - float64(fullBlocks)

			bar = strings.Repeat("█", fullBlocks)
			if remainder > 0 && fullBlocks < barMax {
				idx := int(remainder * float64(len(blocks)))
				if idx >= len(blocks) {
					idx = len(blocks) - 1
				}
				bar += string(blocks[idx])
			}
			bar = barStyle.Render(bar)
		}

		value := labelFn(d)
		if value != "" {
			value = BrightStyle.Render(value)
		}

		b.WriteString(label + bar + value + "\n")
	}
}

func formatNumber(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
