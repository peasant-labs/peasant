package mock

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

func init() {
	api.MockProviderFactory = func() api.DataProvider {
		return NewProvider()
	}
}

// Provider implements api.DataProvider with static mock data.
type Provider struct {
	sessions        []ingest.Session
	qualitySessions []api.QualitySession
}

// NewProvider creates a MockProvider backed by the default mock sessions.
func NewProvider() *Provider {
	return &Provider{
		sessions:        Sessions(),
		qualitySessions: GenerateQualitySessions(),
	}
}

func (p *Provider) Sessions(_ context.Context) ([]ingest.Session, error) {
	return p.sessions, nil
}

func (p *Provider) SessionByID(_ context.Context, id string) (*ingest.Session, error) {
	for i := range p.sessions {
		if string(p.sessions[i].ID) == id {
			return &p.sessions[i], nil
		}
	}
	return nil, fmt.Errorf("session not found: %s", id)
}

func (p *Provider) DashboardMetrics(_ context.Context) (*api.DashboardPayload, error) {
	total := len(p.sessions)
	if total == 0 {
		return &api.DashboardPayload{
			HarnessBreakdown: make(map[ingest.Harness]int),
			AcceptanceRate:   0.0,
		}, nil
	}

	var totalTokens, totalTurns int
	var totalDuration float64
	providers := make(map[ingest.Harness]int)

	for _, s := range p.sessions {
		totalTokens += s.Metadata.TotalTokens
		totalDuration += s.Metadata.Duration.Minutes()
		totalTurns += s.Metadata.TurnCount
		providers[s.Harness]++
	}

	return &api.DashboardPayload{
		TotalSessions:      total,
		TotalTokens:        totalTokens,
		AvgDurationMins:    totalDuration / float64(total),
		HarnessBreakdown:   providers,
		AvgTurnsPerSession: float64(totalTurns) / float64(total),
		AcceptanceRate:     78.3,
	}, nil
}

func (p *Provider) TrendsData(_ context.Context) (*api.TrendsPayload, error) {
	type dayAgg struct {
		date     time.Time
		tokens   int
		sessions int
	}

	dayMap := make(map[string]*dayAgg)
	for _, s := range p.sessions {
		key := s.StartTime.Format("2006-01-02")
		if d, ok := dayMap[key]; ok {
			d.tokens += s.Metadata.TotalTokens
			d.sessions++
		} else {
			dayMap[key] = &dayAgg{
				date:     time.Date(s.StartTime.Year(), s.StartTime.Month(), s.StartTime.Day(), 0, 0, 0, 0, time.UTC),
				tokens:   s.Metadata.TotalTokens,
				sessions: 1,
			}
		}
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := make([]api.DayStats, 7)
	var totalTokens, totalSessions int

	for i := range 7 {
		d := today.AddDate(0, 0, -6+i)
		key := d.Format("2006-01-02")
		ds := api.DayStats{Date: key}
		if agg, ok := dayMap[key]; ok {
			ds.Tokens = agg.tokens
			ds.Sessions = agg.sessions
		}
		days[i] = ds
		totalTokens += ds.Tokens
		totalSessions += ds.Sessions
	}

	sort.Slice(days, func(i, j int) bool {
		return days[i].Date < days[j].Date
	})

	return &api.TrendsPayload{
		Days:          days,
		TotalTokens:   totalTokens,
		TotalSessions: totalSessions,
	}, nil
}

func (p *Provider) SessionSummaries(_ context.Context) ([]api.SessionSummary, error) {
	summaries := make([]api.SessionSummary, len(p.sessions))
	for i, s := range p.sessions {
		summaries[i] = api.SessionSummary{
			ID:            string(s.ID),
			Harness:       s.Harness,
			StartTime:     s.StartTime,
			DurationMins:  s.Metadata.Duration.Minutes(),
			TotalTokens:   s.Metadata.TotalTokens,
			TurnCount:     s.Metadata.TurnCount,
			ToolCallCount: s.Metadata.ToolCallCount,
			Project:       s.Project,
			ProjectHash:   mockProjectHash(s.Project),
			Preview:       firstUserTurnContent(s.Turns),
		}
	}
	return summaries, nil
}

// SessionSummariesByID resolves links against the mock corpus with the same
// contract the real provider honours: exactly the named sessions, in the order
// named, with an unknown identifier omitted rather than reported as an error.
func (p *Provider) SessionSummariesByID(ctx context.Context, ids []string) ([]api.SessionSummary, error) {
	all, err := p.SessionSummaries(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]api.SessionSummary, len(all))
	for _, summary := range all {
		byID[summary.ID] = summary
	}
	resolved := make([]api.SessionSummary, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if summary, ok := byID[id]; ok {
			resolved = append(resolved, summary)
		}
	}
	return resolved, nil
}

// firstUserTurnContent returns the content of the earliest user turn, used as
// the session preview. Returns "" when there is no user turn.
func firstUserTurnContent(turns []ingest.Turn) string {
	for _, t := range turns {
		if t.Role == schema.RoleUser {
			return t.Content
		}
	}
	return ""
}

func (p *Provider) QualitySessions(_ context.Context, f api.QualityFilter) ([]api.QualitySession, error) {
	if len(f.Projects) == 0 {
		return p.qualitySessions, nil
	}

	var filtered []api.QualitySession
	projects := make(map[string]bool)
	for _, proj := range f.Projects {
		projects[proj] = true
	}

	for _, s := range p.qualitySessions {
		if projects[s.Project] {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

func (p *Provider) AnnotationsForSession(_ context.Context, sessionID string) ([]schema.AnnotationSummary, error) {
	// Mock returns annotations from quality sessions that match the requested session ID.
	for _, qs := range p.qualitySessions {
		if qs.ID == sessionID {
			return qs.EffectiveAnnotations, nil
		}
	}
	return nil, nil
}

// ProjectFamiliarity returns realistic mock familiarity data for a project.
// Uses the same seeded PRNG pattern (seed 42) as other mock generators.
func (p *Provider) ProjectFamiliarity(_ context.Context, projectHash schema.ProjectHash) (*schema.FamiliarityPayload, error) {
	r := newSeededRand(42)
	now := time.Now().UTC()

	// Mock file paths representing a typical Go project.
	mockFiles := []string{
		"cmd/main.go",
		"internal/api/server.go",
		"internal/api/handler.go",
		"internal/api/middleware.go",
		"internal/store/store.go",
		"internal/store/reader.go",
		"internal/store/writer.go",
		"internal/store/schema.go",
		"internal/config/config.go",
		"internal/ingest/pipeline.go",
		"internal/ingest/adapter.go",
		"internal/ingest/types.go",
		"internal/api/messages.go",
		"internal/api/store_adapter.go",
		"go.mod",
		"go.sum",
		"Makefile",
		"README.md",
		".gitignore",
		"web/src/App.tsx",
	}

	files := make([]schema.FileFamiliarity, len(mockFiles))
	for i, fp := range mockFiles {
		depth := r.intBetween(0, 3)
		sessionCount := 0
		totalTurns := 0
		humanTurns := 0
		var lastEngaged *string
		var ds *int
		decay := schema.DecayUnexplored

		if depth > 0 {
			sessionCount = r.intBetween(1, 8)
			totalTurns = r.intBetween(1, 20)
			humanTurns = r.intBetween(0, totalTurns)

			daysAgo := r.intBetween(0, 60)
			engaged := now.AddDate(0, 0, -daysAgo).Format(time.RFC3339)
			lastEngaged = &engaged
			ds = &daysAgo

			if daysAgo < 7 {
				decay = schema.DecayFresh
			} else if daysAgo <= 30 {
				decay = schema.DecayFading
			} else {
				decay = schema.DecayStale
			}
		}

		files[i] = schema.FileFamiliarity{
			Path:          fp,
			Depth:         depth,
			SessionCount:  sessionCount,
			TotalTurns:    totalTurns,
			HumanTurns:    humanTurns,
			LastEngagedAt: lastEngaged,
			DaysSince:     ds,
			DecayLevel:    decay,
			IsSourceFile:  isSourceFileMock(fp),
		}
	}

	// Build mock trails.
	trails := []schema.WalkthroughTrail{
		{
			SessionID: "mock-sess-001",
			Title:     "Add API endpoint for health checks",
			Date:      now.AddDate(0, 0, -2).Format("2006-01-02"),
			TurnCount: 12,
			Steps: []schema.WalkthroughStep{
				{File: "internal/api/server.go", Excerpt: "Read"},
				{File: "internal/api/handler.go", Excerpt: "Edit"},
				{File: "internal/store/store.go", Excerpt: "Read"},
			},
			IsCoherent: true,
		},
		{
			SessionID: "mock-sess-002",
			Title:     "Debug pipeline crash on empty input",
			Date:      now.AddDate(0, 0, -5).Format("2006-01-02"),
			TurnCount: 24,
			Steps: []schema.WalkthroughStep{
				{File: "internal/ingest/pipeline.go", Excerpt: "Read"},
				{File: "internal/ingest/adapter.go", Excerpt: "Read"},
				{File: "internal/ingest/pipeline.go", Excerpt: "Edit"},
				{File: "internal/store/writer.go", Excerpt: "Read"},
				{File: "internal/ingest/pipeline.go", Excerpt: "Edit"},
			},
			IsCoherent: false,
		},
	}

	// Build mock suggestions.
	suggestions := []schema.ReviewSuggestion{
		{
			Path:            "internal/store/schema.go",
			LastEngaged:     now.AddDate(0, 0, -45).Format(time.RFC3339),
			DaysSince:       45,
			SuggestedPrompt: "Review schema.go — it was last discussed 45 days ago. What has changed since then?",
		},
		{
			Path:            "internal/config/config.go",
			LastEngaged:     now.AddDate(0, 0, -35).Format(time.RFC3339),
			DaysSince:       35,
			SuggestedPrompt: "Review config.go — it was last discussed 35 days ago. What has changed since then?",
		},
	}

	// Compute aggregate stats.
	familiarityPct := 0.0
	unexploredCount := 0
	sourceCount := 0
	engagedCount := 0
	for _, f := range files {
		if f.IsSourceFile {
			sourceCount++
			if f.Depth > 0 {
				engagedCount++
			} else {
				unexploredCount++
			}
		}
	}
	if sourceCount > 0 {
		familiarityPct = float64(engagedCount) / float64(sourceCount) * 100
	}
	freshnessDays := 2

	return &schema.FamiliarityPayload{
		ProjectHash:     projectHash,
		FamiliarityPct:  familiarityPct,
		UnexploredCount: unexploredCount,
		FreshnessDays:   &freshnessDays,
		Files:           files,
		Trails:          trails,
		Suggestions:     suggestions,
	}, nil
}

// ChildSessionsForParent returns nil — mock data has no parent/child relationships.
func (p *Provider) ChildSessionsForParent(_ context.Context, _ string) ([]schema.ChildSessionRef, error) {
	return nil, nil
}

// isSourceFileMock is a simplified source file check for mock data.
func isSourceFileMock(path string) bool {
	excludeSuffixes := []string{".sum", ".mod", ".lock"}
	for _, s := range excludeSuffixes {
		if len(path) > len(s) && path[len(path)-len(s):] == s {
			return false
		}
	}
	return true
}

// Verify MockProvider implements DataProvider at compile time.
var _ api.DataProvider = (*Provider)(nil)
