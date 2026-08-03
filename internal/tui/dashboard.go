package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// DashboardModel displays quality metric cards in labeled sections.
type DashboardModel struct {
	sessions []ingest.Session
	width    int
	height   int
}

// NewDashboard creates a DashboardModel with the given sessions.
func NewDashboard(sessions []ingest.Session) DashboardModel {
	return DashboardModel{sessions: sessions}
}

func (m DashboardModel) Init() tea.Cmd {
	return nil
}

func (m DashboardModel) Update(msg tea.Msg) (DashboardModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

func (m DashboardModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	metrics := m.computeMetrics()
	sections := buildSections(metrics)

	var parts []string
	for _, sec := range sections {
		parts = append(parts, m.renderSection(sec))
	}

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// metricCard holds pre-computed data for a single KPI card.
type metricCard struct {
	title       string
	value       string
	secondary   string    // optional secondary line (e.g., "~24K tokens wasted")
	sparkline   []float64 // 7 values normalized 0.0–1.0
	trendPct    float64
	invertTrend bool           // true if down=good (retries, reverts)
	accentColor defaults.Color // pastel accent for sparkline
}

// metricSection groups cards under a labeled header.
type metricSection struct {
	title      string
	titleStyle lipgloss.Style
	cards      []metricCard
}

// dashMetrics holds all computed values.
type dashMetrics struct {
	tokensAvg       float64
	tokensDailyNorm []float64
	tokensTrend     float64

	costOfPass    float64
	costDailyNorm []float64
	costTrend     float64

	avgTurns       float64
	turnsDailyNorm []float64
	turnsTrend     float64

	retryWeekly    int
	retryWasted    int
	retryDailyNorm []float64
	retryTrend     float64

	signalDensity   float64
	signalDailyNorm []float64
	signalTrend     float64

	revertRate      float64
	revertDailyNorm []float64
	revertTrend     float64

	specScore     float64
	specDailyNorm []float64
	specTrend     float64
}

func (m DashboardModel) computeMetrics() dashMetrics {
	dm := dashMetrics{}
	if len(m.sessions) == 0 {
		return dm
	}

	now := time.Now().UTC()
	days := make(map[string][]ingest.Session)
	for i := range m.sessions {
		key := m.sessions[i].StartTime.Format("2006-01-02")
		days[key] = append(days[key], m.sessions[i])
	}

	// Build 7-day arrays for sparklines.
	type dayAgg struct {
		tokens      int
		costTokens  int
		costCount   int
		turns       int
		turnCount   int
		retryLoops  int
		signalSum   float64
		signalCount int
		revertSum   int
		revertCount int
		specSum     float64
		specCount   int
	}

	dayData := make([]dayAgg, defaults.TrendsDaysLookback)
	for i := 0; i < defaults.TrendsDaysLookback; i++ {
		d := now.AddDate(0, 0, -6+i)
		key := d.Format("2006-01-02")
		for _, s := range days[key] {
			dayData[i].tokens += s.Metadata.TotalTokens
			dayData[i].turns += s.Metadata.TurnCount
			dayData[i].turnCount++
			if s.Metadata.Quality != nil {
				q := s.Metadata.Quality
				if derefOutcome(q.Outcome) == ingest.OutcomeResolved {
					dayData[i].costTokens += s.Metadata.TotalTokens
					dayData[i].costCount++
				}
				dayData[i].retryLoops += derefInt(q.RetryLoops)
				dayData[i].signalSum += derefFloat(q.SignalDensity)
				dayData[i].signalCount++
				dayData[i].revertSum += derefInt(q.WithinSessionReverts)
				dayData[i].revertCount++
				dayData[i].specSum += derefFloat(q.SpecQualityScore)
				dayData[i].specCount++
			}
		}
	}

	// Aggregate totals.
	var totalTokens int
	var totalTurns int
	var resolvedTokens int
	var resolvedCount int
	var totalRetry int
	var totalWasted int
	var signalSum float64
	var signalN int
	var revertSum int
	var revertN int
	var specSum float64
	var specN int

	for _, s := range m.sessions {
		totalTokens += s.Metadata.TotalTokens
		totalTurns += s.Metadata.TurnCount
		if s.Metadata.Quality != nil {
			q := s.Metadata.Quality
			if derefOutcome(q.Outcome) == ingest.OutcomeResolved {
				resolvedTokens += s.Metadata.TotalTokens
				resolvedCount++
			}
			totalRetry += derefInt(q.RetryLoops)
			totalWasted += derefInt(q.RetryTokensWasted)
			signalSum += derefFloat(q.SignalDensity)
			signalN++
			revertSum += derefInt(q.WithinSessionReverts)
			revertN++
			specSum += derefFloat(q.SpecQualityScore)
			specN++
		}
	}

	n := len(m.sessions)
	dm.tokensAvg = float64(totalTokens) / float64(n)
	if resolvedCount > 0 {
		dm.costOfPass = float64(resolvedTokens) / float64(resolvedCount)
	}
	dm.avgTurns = float64(totalTurns) / float64(n)
	dm.retryWeekly = int(math.Round(float64(totalRetry) / float64(n) * 7))
	dm.retryWasted = totalWasted
	if signalN > 0 {
		dm.signalDensity = signalSum / float64(signalN) * 100
	}
	if revertN > 0 {
		dm.revertRate = float64(revertSum) / float64(revertN)
	}
	if specN > 0 {
		dm.specScore = specSum / float64(specN) * 100
	}

	// Normalize daily arrays and compute trends.
	dm.tokensDailyNorm, dm.tokensTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 { return float64(d.tokens) }))
	dm.costDailyNorm, dm.costTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 {
		if d.costCount > 0 {
			return float64(d.costTokens) / float64(d.costCount)
		}
		return 0
	}))
	dm.turnsDailyNorm, dm.turnsTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 {
		if d.turnCount > 0 {
			return float64(d.turns) / float64(d.turnCount)
		}
		return 0
	}))
	dm.retryDailyNorm, dm.retryTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 { return float64(d.retryLoops) }))
	dm.signalDailyNorm, dm.signalTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 {
		if d.signalCount > 0 {
			return d.signalSum / float64(d.signalCount) * 100
		}
		return 0
	}))
	dm.revertDailyNorm, dm.revertTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 {
		if d.revertCount > 0 {
			return float64(d.revertSum) / float64(d.revertCount)
		}
		return 0
	}))
	dm.specDailyNorm, dm.specTrend = normAndTrend(extractFloats(dayData, func(d dayAgg) float64 {
		if d.specCount > 0 {
			return d.specSum / float64(d.specCount) * 100
		}
		return 0
	}))

	return dm
}

func buildSections(dm dashMetrics) []metricSection {
	return []metricSection{
		{
			title:      "Cost & Efficiency",
			titleStyle: SectionHeaderCost,
			cards: []metricCard{
				{
					title:       "Tokens Trend",
					value:       formatTokensK(dm.tokensAvg),
					secondary:   "avg per session",
					sparkline:   dm.tokensDailyNorm,
					trendPct:    dm.tokensTrend,
					invertTrend: true, // lower tokens = better
					accentColor: defaults.ColorPastelPink,
				},
				{
					title:       "Cost-of-Pass",
					value:       formatTokensK(dm.costOfPass),
					secondary:   "per resolved task",
					sparkline:   dm.costDailyNorm,
					trendPct:    dm.costTrend,
					invertTrend: true,
					accentColor: defaults.ColorPastelPeach,
				},
			},
		},
		{
			title:      "Behavioral Patterns",
			titleStyle: SectionHeaderBehavioral,
			cards: []metricCard{
				{
					title:       "Avg Turns",
					value:       fmt.Sprintf("%.0f", dm.avgTurns),
					secondary:   "per session",
					sparkline:   dm.turnsDailyNorm,
					trendPct:    dm.turnsTrend,
					invertTrend: true,
					accentColor: defaults.ColorPastelSky,
				},
				{
					title:       "Retry Loops",
					value:       fmt.Sprintf("%d/wk", dm.retryWeekly),
					secondary:   fmt.Sprintf("~%s wasted", formatTokensK(float64(dm.retryWasted))),
					sparkline:   dm.retryDailyNorm,
					trendPct:    dm.retryTrend,
					invertTrend: true,
					accentColor: defaults.ColorPastelLemon,
				},
				{
					title:       "Signal Density",
					value:       fmt.Sprintf("%.0f%%", dm.signalDensity),
					secondary:   "of context is signal",
					sparkline:   dm.signalDailyNorm,
					trendPct:    dm.signalTrend,
					invertTrend: false,
					accentColor: defaults.ColorPastelLilac,
				},
			},
		},
		{
			title:      "Quality Outcomes",
			titleStyle: SectionHeaderQuality,
			cards: []metricCard{
				{
					title:       "Revert Rate",
					value:       fmt.Sprintf("%.1f/session", dm.revertRate),
					secondary:   "within-session reverts",
					sparkline:   dm.revertDailyNorm,
					trendPct:    dm.revertTrend,
					invertTrend: true,
					accentColor: defaults.ColorPastelCoral,
				},
				{
					title:       "Spec Score",
					value:       fmt.Sprintf("%.0f", dm.specScore),
					secondary:   "composite 0–100",
					sparkline:   dm.specDailyNorm,
					trendPct:    dm.specTrend,
					invertTrend: false,
					accentColor: defaults.ColorPastelMint,
				},
			},
		},
	}
}

func (m DashboardModel) renderSection(sec metricSection) string {
	header := sec.titleStyle.Render("  " + sec.title)

	var cardStrs []string
	for _, c := range sec.cards {
		cardStrs = append(cardStrs, m.renderMetricCard(c))
	}

	var content string
	if m.width >= defaults.WideLayoutBreakpoint {
		content = lipgloss.JoinHorizontal(lipgloss.Top, cardStrs...)
	} else {
		content = lipgloss.JoinVertical(lipgloss.Left, cardStrs...)
	}

	sectionBorder := SectionStyle.Width(m.width - 4)
	return sectionBorder.Render(lipgloss.JoinVertical(lipgloss.Left, header, content)) + "\n"
}

func (m DashboardModel) renderMetricCard(c metricCard) string {
	cardWidth := m.cardWidth(c)

	titleStr := DimStyle.Render(c.title)
	valueStr := BrightStyle.Render(c.value)
	secondaryStr := DimStyle.Render(c.secondary)

	spark := renderSparkline(c.sparkline, c.accentColor)
	trend := renderTrend(c.trendPct, c.invertTrend)
	sparkRow := spark + "  " + trend

	content := lipgloss.JoinVertical(lipgloss.Left, titleStr, valueStr, secondaryStr, sparkRow)

	return CardStyle.Width(cardWidth).Render(content)
}

func (m DashboardModel) cardWidth(c metricCard) int {
	// Determine card width based on how many cards are in each section.
	// The section has already been laid out, so we use a simple heuristic.
	if m.width < defaults.WideLayoutBreakpoint {
		return m.width - 8
	}
	// Use the section's inner width minus padding, divided by the number of cards
	// We'll default to ~1/3 of available width for 3-card sections and ~1/2 for 2-card.
	_ = c
	innerWidth := m.width - 8 // section padding
	perCard := innerWidth / 3
	if perCard < defaults.MinCardWidth {
		perCard = defaults.MinCardWidth
	}
	return perCard
}

// renderSparkline renders 7 normalized values as Unicode block characters.
func renderSparkline(data []float64, color defaults.Color) string {
	blocks := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	var sb strings.Builder
	for _, v := range data {
		idx := int(v * 7)
		if idx > 7 {
			idx = 7
		}
		if idx < 0 {
			idx = 0
		}
		sb.WriteRune(blocks[idx])
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(color.String())).
		Background(baseBg).
		Render(sb.String())
}

// renderTrend renders a trend indicator with appropriate color.
// invertTrend means down=good (green) and up=bad (red).
func renderTrend(pct float64, invert bool) string {
	if math.Abs(pct) < 0.5 {
		return TrendNeutralStyle.Render("─ 0%")
	}
	if pct > 0 {
		arrow := "↑"
		style := TrendUpStyle
		if invert {
			style = TrendDownStyle
		}
		return style.Render(fmt.Sprintf("%s %.0f%%", arrow, pct))
	}
	arrow := "↓"
	style := TrendDownStyle
	if invert {
		style = TrendUpStyle
	}
	return style.Render(fmt.Sprintf("%s %.0f%%", arrow, math.Abs(pct)))
}

// extractFloats extracts a float64 slice from dayAgg using a selector.
func extractFloats[T any](data []T, fn func(T) float64) []float64 {
	out := make([]float64, len(data))
	for i, d := range data {
		out[i] = fn(d)
	}
	return out
}

// normAndTrend normalizes values to 0–1 and computes trend (recent 3 vs prior 4).
func normAndTrend(vals []float64) ([]float64, float64) {
	norm := make([]float64, len(vals))
	var max float64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	if max > 0 {
		for i, v := range vals {
			norm[i] = v / max
		}
	}

	// Trend: average of last 3 vs first 4
	var trend float64
	if len(vals) >= 7 {
		var recent, prior float64
		for i := 0; i < 4; i++ {
			prior += vals[i]
		}
		for i := 4; i < 7; i++ {
			recent += vals[i]
		}
		prior /= 4
		recent /= 3
		if prior > 0 {
			trend = (recent - prior) / prior * 100
		}
	}

	return norm, trend
}

// formatTokensK formats a token count as K/M.
func formatTokensK(n float64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", n/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", n/1_000)
	}
	return fmt.Sprintf("%.0f", n)
}
