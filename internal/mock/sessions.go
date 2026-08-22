package mock

import (
	"fmt"
	"math"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Seeded PRNG — Park-Miller (same as the TypeScript generator, seed 42)
// ---------------------------------------------------------------------------

type seededRand struct{ s int64 }

func newSeededRand(seed int64) *seededRand { return &seededRand{s: seed} }

// next returns a float64 in [0, 1).
func (r *seededRand) next() float64 {
	r.s = (r.s * 16807) % 2147483647
	return float64(r.s-1) / 2147483646.0
}

func (r *seededRand) intBetween(min, max int) int {
	return int(math.Floor(r.next()*float64(max-min+1))) + min
}

func (r *seededRand) floatBetween(min, max float64) float64 {
	return r.next()*(max-min) + min
}

func (r *seededRand) pick(n int) int {
	return int(math.Floor(r.next() * float64(n)))
}

// ---------------------------------------------------------------------------
// GenerateQualitySessions produces ~90 quality sessions over ~60 days,
// matching the original TypeScript mock-data.ts generator exactly.
// The output is deterministic (seed 42).
// ---------------------------------------------------------------------------

var (
	projectNames = []string{"fortuna", "peasant-api", "data-pipeline", "auth-service", "docs-site"}
	scopeNames   = []string{"Personal", "Platform Team", "Growth Team"}
	taskTitles   = []string{
		"Fix authentication middleware",
		"Add pagination to API",
		"Refactor database queries",
		"Implement search filters",
		"Update user settings page",
		"Fix CORS configuration",
		"Add webhook handlers",
		"Migrate to TypeScript",
		"Optimize image loading",
		"Add rate limiting",
		"Fix session management",
		"Implement dark mode",
		"Add export functionality",
		"Refactor state management",
		"Fix timezone handling",
		"Add input validation",
		"Update dependencies",
		"Fix memory leak",
		"Add logging middleware",
		"Implement caching layer",
		"Fix SQL injection risk",
		"Add unit tests",
		"Refactor API routes",
		"Fix file upload handler",
		"Add notification system",
		"Fix race condition",
		"Implement batch processing",
		"Add health check endpoint",
		"Fix CSS layout issues",
		"Implement retry logic",
	}
)

func generateQualitySession(r *seededRand, index, dayOffset int) api.QualitySession {
	// Mirrors the TS: rand()<0.55 ? "resolved" : rand()<0.6 ? "partial" : "failed"
	// Two independent draws to keep PRNG sequence identical to the original.
	var outcome string
	if r.next() < 0.55 {
		outcome = "resolved"
	} else if r.next() < 0.6 {
		outcome = "partial"
	} else {
		outcome = "failed"
	}

	var baseTurns int
	switch outcome {
	case "resolved":
		baseTurns = r.intBetween(4, 25)
	case "partial":
		baseTurns = r.intBetween(12, 40)
	default:
		baseTurns = r.intBetween(20, 65)
	}
	turnCount := baseTurns

	baseTokens := turnCount * r.intBetween(800, 3500)
	totalTokens := baseTokens + r.intBetween(2000, 15000)
	inputRatio := r.floatBetween(0.91, 0.97)
	inputTokens := int(math.Floor(float64(totalTokens) * inputRatio))
	outputTokens := totalTokens - inputTokens

	var retryLoops int
	switch outcome {
	case "failed":
		retryLoops = r.intBetween(2, 8)
	case "partial":
		retryLoops = r.intBetween(0, 3)
	default:
		retryLoops = r.intBetween(0, 1)
	}
	retryTokensWasted := retryLoops * (totalTokens / max(turnCount, 1))

	var signalDensity float64
	switch outcome {
	case "resolved":
		signalDensity = r.floatBetween(28, 62)
	case "partial":
		signalDensity = r.floatBetween(15, 40)
	default:
		signalDensity = r.floatBetween(8, 25)
	}

	var specScore float64
	switch outcome {
	case "resolved":
		specScore = r.floatBetween(55, 95)
	case "partial":
		specScore = r.floatBetween(30, 65)
	default:
		specScore = r.floatBetween(10, 45)
	}

	explorationRatio := 100 - specScore*r.floatBetween(0.5, 0.9)

	var maxBreadth int
	if outcome == "failed" {
		maxBreadth = 8
	} else {
		maxBreadth = 4
	}
	scopeBreadth := r.intBetween(1, maxBreadth)

	var discoveryTurns int
	if outcome == "resolved" {
		discoveryTurns = r.intBetween(1, 4)
	} else {
		discoveryTurns = r.intBetween(3, 12)
	}

	var maxFiles int
	if outcome == "failed" {
		maxFiles = 15
	} else {
		maxFiles = 8
	}

	var maxLines int
	if outcome == "failed" {
		maxLines = 800
	} else {
		maxLines = 400
	}

	var maxReverts int
	if outcome == "resolved" {
		maxReverts = 2
	} else {
		maxReverts = 6
	}
	var minReverts int
	if outcome == "resolved" {
		minReverts = 0
	} else {
		minReverts = 1
	}

	date := time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)
	date = date.AddDate(0, 0, dayOffset)

	return api.QualitySession{
		ID:                   fmt.Sprintf("sess-%03d", index),
		Date:                 date.Format(time.RFC3339),
		Project:              projectNames[r.pick(len(projectNames))],
		Scope:                scopeNames[r.pick(len(scopeNames))],
		Title:                taskTitles[r.pick(len(taskTitles))],
		TotalTokens:          totalTokens,
		InputTokens:          inputTokens,
		OutputTokens:         outputTokens,
		TurnCount:            turnCount,
		ToolCalls:            turnCount + r.intBetween(0, turnCount*2),
		Outcome:              outcome,
		FilesTouched:         r.intBetween(1, maxFiles),
		LinesChanged:         r.intBetween(10, maxLines),
		DurationMinutes:      float64(int(float64(turnCount) * r.floatBetween(0.8, 2.5))),
		RetryLoops:           retryLoops,
		RetryTokensWasted:    retryTokensWasted,
		WithinSessionReverts: r.intBetween(minReverts, maxReverts),
		SignalDensity:        math.Round(signalDensity*10) / 10,
		SpecQualityScore:     math.Round(specScore),
		ExplorationRatio:     math.Round(explorationRatio),
		ScopeBreadth:         scopeBreadth,
		DiscoveryTurns:       discoveryTurns,
	}
}

// GenerateQualitySessions generates ~90 quality sessions over ~60 days with
// a seeded PRNG (seed 42) for deterministic output. This is a faithful port
// of the original web/src/lib/quality/mock-data.ts generator.
func GenerateQualitySessions() []api.QualitySession {
	r := newSeededRand(42)
	var sessions []api.QualitySession
	dayOffset := 0
	sessionIndex := 0

	for dayOffset < 60 {
		// ~15% chance of no sessions (rest day)
		if r.next() < 0.15 {
			dayOffset++
			continue
		}
		// sessions today: 1 (50%), 2 (35%), 3 (15%)
		rv := r.next()
		var sessionsToday int
		switch {
		case rv < 0.50:
			sessionsToday = 1
		case rv < 0.85:
			sessionsToday = 2
		default:
			sessionsToday = 3
		}
		for j := 0; j < sessionsToday; j++ {
			sessions = append(sessions, generateQualitySession(r, sessionIndex, dayOffset))
			sessionIndex++
		}
		dayOffset++
	}
	// Appended after the seeded loop finishes so it draws nothing from r and
	// cannot perturb the deterministic sequence above. See
	// heroTitleFixtureQualitySession.
	sessions = append(sessions, heroTitleFixtureQualitySession())
	return sessions
}

type mockSessionYAML struct {
	ID           string `yaml:"id"`
	Provider     string `yaml:"provider"`
	StartTime    string `yaml:"startTime"`
	DurationMins int    `yaml:"durationMins"`
	TotalTokens  int    `yaml:"totalTokens"`
	TurnCount    int    `yaml:"turnCount"`
	Model        string `yaml:"model"`
	ProjectName  string `yaml:"projectName"`
}

type mockDataYAML struct {
	Sessions []mockSessionYAML `yaml:"sessions"`
}

// Sessions returns sessions parsed from schema.SessionsYAML.
func Sessions() []ingest.Session {
	var data mockDataYAML
	if err := yaml.Unmarshal(schema.SessionsYAML, &data); err != nil {
		return nil
	}

	// mockOutcomes cycles through the SessionOutcome values so mock session
	// detail carries a visible, deterministic outcome chip.
	mockOutcomes := []schema.SessionOutcome{
		schema.OutcomeResolved,
		schema.OutcomePartial,
		schema.OutcomeFailed,
	}

	sessions := make([]ingest.Session, len(data.Sessions)+1, len(data.Sessions)+1+len(heroTitleFixtureSessions()))
	sessions[0] = canonicalStrikeMockFixture.session()
	for i, s := range data.Sessions {
		startTime, _ := time.Parse(time.RFC3339, s.StartTime)
		duration := time.Duration(s.DurationMins) * time.Minute

		outcome := mockOutcomes[i%len(mockOutcomes)]
		sessions[i+1] = ingest.Session{
			ID:        schema.SessionID(s.ID),
			Project:   s.ProjectName,
			Harness:   schema.Harness(s.Provider),
			StartTime: startTime,
			EndTime:   startTime.Add(duration),
			Turns:     MockTurns(s.TurnCount, startTime),
			Metadata: ingest.SessionMetadata{
				TotalTokens:   s.TotalTokens,
				Duration:      duration,
				TurnCount:     s.TurnCount,
				ToolCallCount: s.TurnCount / 3, // Synthesis
				Quality:       mockScorecardMetrics(i, s.TotalTokens, outcome),
			},
		}
	}
	// Additive capture fixtures for the session hero heading (peasant#175) —
	// appended after every yaml-driven session so they never shift an
	// existing index or ID. See heroTitleFixtureSessions for what each one
	// demonstrates.
	sessions = append(sessions, heroTitleFixtureSessions()...)
	return sessions
}

// heroTitleGeneratedSessionID and heroTitleUntitledSessionID name the two
// additive mock sessions the visual-review harness captures for the session
// hero heading (peasant#175, web/scripts/visual/README.md "session-detail"
// boot surface). heroTitleFixtureProject reuses the existing "fortuna" mock
// project so no new project-summary/routing wiring is needed.
const (
	heroTitleGeneratedSessionID = "sess-hero-title-generated"
	heroTitleUntitledSessionID  = "sess-hero-title-untitled"
	heroTitleFixtureProject     = "fortuna"
)

// heroTitleFixtureSessions returns two additive mock sessions:
//   - heroTitleGeneratedSessionID has a matching heroTitleFixtureQualitySession
//     row, so the hero renders that generated title.
//   - heroTitleUntitledSessionID has no quality row and opens with the same
//     local-command harness markup the SessionDetailV2 hero-title regression
//     test pins (testdata/hero_title.yaml), so the hero renders the
//     "Untitled session" placeholder instead of that markup.
//
// Neither entry touches an existing session, index, or the seeded-PRNG draw
// sequence used elsewhere in this file.
func heroTitleFixtureSessions() []ingest.Session {
	generatedStart := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	untitledStart := generatedStart.Add(time.Hour)
	return []ingest.Session{
		{
			ID:        schema.SessionID(heroTitleGeneratedSessionID),
			Project:   heroTitleFixtureProject,
			Harness:   schema.HarnessClaudeCode,
			StartTime: generatedStart,
			EndTime:   generatedStart.Add(6 * time.Minute),
			Turns:     MockTurns(4, generatedStart),
			Metadata: ingest.SessionMetadata{
				TotalTokens:   4200,
				Duration:      6 * time.Minute,
				TurnCount:     4,
				ToolCallCount: 1,
			},
		},
		{
			ID:        schema.SessionID(heroTitleUntitledSessionID),
			Project:   heroTitleFixtureProject,
			Harness:   schema.HarnessClaudeCode,
			StartTime: untitledStart,
			EndTime:   untitledStart.Add(6 * time.Minute),
			Turns: []ingest.Turn{
				{
					Index:     0,
					Role:      ingest.RoleUser,
					Content:   "<local-command-caveat>Caveat: the messages below were generated while running local commands.</local-command-caveat>",
					Timestamp: untitledStart,
				},
				{
					Index:     1,
					Role:      ingest.RoleAssistant,
					Content:   "the branch has no staged changes",
					Timestamp: untitledStart.Add(2 * time.Minute),
				},
			},
			Metadata: ingest.SessionMetadata{
				TotalTokens:   900,
				Duration:      6 * time.Minute,
				TurnCount:     2,
				ToolCallCount: 0,
			},
		},
	}
}

// heroTitleFixtureQualitySession is the quality row matched by ID to
// heroTitleGeneratedSessionID above, so the mock quality channel supplies a
// generated title for that one fixture session. heroTitleUntitledSessionID
// deliberately has no matching row.
func heroTitleFixtureQualitySession() api.QualitySession {
	return api.QualitySession{
		ID:      heroTitleGeneratedSessionID,
		Date:    "2026-08-01T09:00:00Z",
		Project: heroTitleFixtureProject,
		Scope:   scopeNames[0],
		Title:   "refactor the ingest pipeline",
		Outcome: "resolved",
	}
}

// mockScorecardMetrics builds deterministic per-session quality signals so the
// Highlights self-assessment card renders in mock mode. Values cycle through
// healthy / amber / red bands across sessions so all three states are visible.
func mockScorecardMetrics(i, totalTokens int, outcome schema.SessionOutcome) *schema.QualityMetrics {
	band := i % 3 // 0=healthy, 1=amber, 2=red

	contextPct := []float64{42, 68, 88}[band]
	survivalPct := []float64{82, 58, 38}[band]
	specScore := []float64{72, 45, 28}[band]
	signalDensity := []float64{58, 36, 22}[band]
	consecErrors := []int{1, 3, 5}[band]
	reverts := []int{0, 2, 4}[band]
	retryShare := []float64{0.05, 0.18, 0.34}[band]
	hasExamples := band == 0
	hasConstraints := band != 2

	retryWasted := int(math.Round(float64(totalTokens) * retryShare))
	cost := math.Round(float64(totalTokens)/1000.0*3.0*100) / 100

	return &schema.QualityMetrics{
		Outcome:                 &outcome,
		TotalTokens:             &totalTokens,
		M5ContextUtilizationPct: &contextPct,
		M6OutputSurvivalPct:     &survivalPct,
		SpecQualityScore:        &specScore,
		SignalDensity:           &signalDensity,
		M4ConsecutiveErrorMax:   &consecErrors,
		WithinSessionReverts:    &reverts,
		RetryTokensWasted:       &retryWasted,
		CostTotalUSD:            &cost,
		M7SpecHasExamples:       &hasExamples,
		M7SpecHasConstraints:    &hasConstraints,
	}
}

// QualitySessions returns quality sessions parsed from schema.QualitySessionsYAML.
func QualitySessions() []api.QualitySession {
	fixtures, err := schema.LoadQualityFixtures()
	if err != nil {
		return nil
	}

	return fixtures.QualitySessions()
}

// MockTurns generates a slice of turns for a mock session.
func MockTurns(count int, base time.Time) []ingest.Turn {
	toolNames := []string{"Read", "Write", "Bash", "Grep", "Glob"}
	userContents := []string{
		"Implement the auth module",
		"Fix the database connection issue",
		"Add unit tests for the parser",
		"Refactor the API handlers",
		"Update the configuration loader",
		"Create the migration script",
		"Debug the WebSocket handler",
		"Optimize the query performance",
		"Set up CI pipeline",
		"Add error handling to ingest",
		"Review the security middleware",
		"Implement session caching",
		"Update the README documentation",
		"Fix the race condition in store",
		"Add metrics endpoint",
	}
	assistantContents := []string{
		"Here is the implementation of the auth module...",
		"I've identified the connection issue and fixed it...",
		"I've added comprehensive tests for the parser...",
		"The API handlers have been refactored to use...",
		"The configuration loader now supports YAML and...",
		"Here's the migration script for the schema...",
		"The WebSocket handler bug was caused by...",
		"Query performance improved by adding an index...",
		"CI pipeline configured with build, test, lint...",
		"Error handling added with proper error wrapping...",
		"Security middleware reviewed and patched for...",
		"Session caching implemented using in-memory LRU...",
		"README updated with installation and usage...",
		"Race condition fixed using a sync.Mutex on...",
		"Metrics endpoint added at /api/v1/metrics...",
	}

	turns := make([]ingest.Turn, count)
	for i := 0; i < count; i++ {
		role := ingest.RoleUser
		content := userContents[i%len(userContents)]
		if i%2 == 1 {
			role = ingest.RoleAssistant
			content = assistantContents[i%len(assistantContents)]
		}

		var toolCalls []ingest.ToolCall
		if role == ingest.RoleAssistant && i%3 == 1 {
			numCalls := (i % 3) + 1
			toolCalls = make([]ingest.ToolCall, numCalls)
			for j := 0; j < numCalls; j++ {
				toolCalls[j] = ingest.ToolCall{
					ID:        fmt.Sprintf("tc_%d_%d", i, j),
					Name:      toolNames[(i+j)%len(toolNames)],
					Arguments: fmt.Sprintf(`{"path": "internal/file_%d.go"}`, j),
					Result:    "success",
				}
			}
		}

		turns[i] = ingest.Turn{
			Index:     i,
			Role:      role,
			Content:   content,
			ToolCalls: toolCalls,
			Timestamp: base.Add(time.Duration(i*2) * time.Minute),
		}
	}
	return turns
}
