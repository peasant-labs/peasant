package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// LocalContributorID is the one contributor a local Peasant store can
// attribute a session to. Peasant records sessions on this computer for a
// single operator, so every stored session belongs to this contributor; the
// browser adapter already attributes every quality row to the same value, and
// the analytics bundle must not invent a second identity.
const LocalContributorID = "local user"

// AnalyticsFilter constrains the library-wide analytics aggregate. The zero
// value is the whole stored library, which is what the quality page summary
// always requests; a per-project baseline is a later, separately ratified
// feature.
type AnalyticsFilter struct {
	// ProjectHash limits every metric to one project. Empty means every
	// project the library holds.
	ProjectHash schema.ProjectHash
}

// AnalyticsSession is one stored session's analytics inputs. The pure fold and
// the SQL aggregate consume the same shape so the browser-adapter parity
// fixture can exercise both paths against one recorded expectation.
type AnalyticsSession struct {
	StartMs         int64
	ProjectHash     string
	Outcome         string
	DurationMinutes float64
	TotalTokens     int
	TurnCount       int
	ToolCalls       int
}

// analyticsDurationExpr mirrors the authority duration expression in
// sqlAllSessions: a session with an active generation reads the captured stats
// row, every other session reads the legacy analysis record. The same
// expression feeds the quality rows, so a session's duration is one number in
// the page and in the summary.
const analyticsDurationExpr = `COALESCE(CASE WHEN s.active_generation_id IS NOT NULL THEN CAST(c.duration_ms AS REAL) / 60000.0 ELSE m.duration_minutes END, 0)`

// analyticsTokensOutExpr mirrors the authority output-token expression in
// sqlAllSessions.
const analyticsTokensOutExpr = `CASE WHEN s.active_generation_id IS NOT NULL THEN c.tokens_out ELSE m.output_tokens END`

// analyticsToolCallsExpr mirrors the authority tool-call expression in
// sqlAllSessions.
const analyticsToolCallsExpr = `CASE WHEN s.active_generation_id IS NOT NULL THEN c.tool_call_count ELSE m.tool_calls END`

// analyticsWeekExpr renders the UTC Monday-of-week date key as YYYY-MM-DD. The
// browser's weekKey buckets the same way, so a recorded parity fixture pins one
// week rule.
const analyticsWeekExpr = `strftime('%Y-%m-%d', (b.start_ms / 1000) - ((CAST(strftime('%w', b.start_ms / 1000, 'unixepoch') AS INTEGER) + 6) % 7) * 86400, 'unixepoch')`

// analyticsBaseSQL is the one library read both aggregate paths share. It joins
// exactly the tables the quality page rows and the full quality topic join, so
// the summary and the rows describe the same stored set.
const analyticsBaseSQL = `FROM sessions s
JOIN session_metrics m ON m.session_id = s.session_id
JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN session_captured_stats c ON c.session_id = s.session_id`

// ComputeProjectAnalytics folds per-session analytics inputs into the bundle
// the dashboard renders. It is the pure path used by the mock provider and by
// the parity fixture; the store path folds the grouped aggregate through the
// same helpers.
func ComputeProjectAnalytics(sessions []AnalyticsSession) schema.ProjectAnalytics {
	analytics := emptyProjectAnalytics()
	analytics.TotalSessions = len(sessions)
	if len(sessions) == 0 {
		return analytics
	}

	weekSessions := map[string]int{}
	weekDuration := map[string]float64{}
	projectSet := map[string]struct{}{}
	var durationValues, tokenValues, turnValues, toolValues []float64
	var firstMs, lastMs int64
	firstSet := false

	for _, session := range sessions {
		projectSet[session.ProjectHash] = struct{}{}
		if !firstSet || session.StartMs < firstMs {
			firstMs = session.StartMs
		}
		if !firstSet || session.StartMs > lastMs {
			lastMs = session.StartMs
		}
		firstSet = true

		week := isoWeekKey(session.StartMs)
		weekSessions[week]++
		weekDuration[week] += session.DurationMinutes

		durationValues = append(durationValues, session.DurationMinutes)
		tokenValues = append(tokenValues, float64(session.TotalTokens))
		turnValues = append(turnValues, float64(session.TurnCount))
		toolValues = append(toolValues, float64(session.ToolCalls))

		switch schema.SessionOutcome(session.Outcome) {
		case schema.OutcomeResolved:
			analytics.OutcomeDistribution.Resolved++
		case schema.OutcomePartial:
			analytics.OutcomeDistribution.Partial++
		case schema.OutcomeFailed:
			analytics.OutcomeDistribution.Failed++
		default:
			analytics.OutcomeDistribution.Unknown++
		}
	}

	analytics.TotalProjects = len(projectSet)
	analytics.SessionStats = schema.AnalyticsSessionStats{
		DurationMins:  medianP90(durationValues),
		TotalTokens:   medianP90(tokenValues),
		TurnCount:     medianP90(turnValues),
		ToolCallCount: medianP90(toolValues),
	}
	analytics.OutcomeDistribution.Total = len(sessions)

	weeks := sortedWeekKeys(weekSessions)
	for _, week := range weeks {
		analytics.SessionsPerWeek = append(analytics.SessionsPerWeek, schema.WeekCount{Week: week, Count: weekSessions[week]})
		analytics.WeeklyActiveContributors = append(analytics.WeeklyActiveContributors, schema.WeekContributors{Week: week, Contributors: 1})
		analytics.AvgDurationPerActiveWeek = append(analytics.AvgDurationPerActiveWeek, schema.WeekAvgDuration{
			Week:            week,
			AvgDurationMins: weekDuration[week] / float64(weekSessions[week]),
			Sessions:        weekSessions[week],
		})
	}

	analytics.TotalContributors = 1
	analytics.ReturningContributorRate = returningContributorRate(weeks)
	analytics.LongestStreak = longestStreak(weeks)
	analytics.NewContributorVelocity = []schema.WeekNewContributors{{Week: isoWeekKey(firstMs), NewContributors: 1}}
	// Commit linkage is not part of the session quality read: neither
	// sessions nor session_metrics carries a commit count, and the browser
	// adapter reports the same absent linkage. Report the honest total over
	// the sessions the read covers rather than fabricating a rate.
	analytics.SessionToCommitRate = schema.SessionToCommitRate{Total: len(sessions), WithCommit: 0, Rate: 0}
	analytics.PerContributorBreakdown = []schema.ContributorBreakdown{{
		ContributorID:     LocalContributorID,
		Sessions:          len(sessions),
		ActiveWeeks:       len(weeks),
		TotalDurationMins: int(math.Round(sumWeekDuration(weekDuration))),
		TotalTurns:        sumInts(turnValues),
		TotalToolCalls:    sumInts(toolValues),
		TotalTokens:       sumInts(tokenValues),
		Outcomes:          analytics.OutcomeDistribution,
		FirstSeen:         stringPtr(isoDayKey(firstMs)),
		LastSeen:          stringPtr(isoDayKey(lastMs)),
	}}
	return analytics
}

// emptyProjectAnalytics returns a bundle whose list fields are empty rather
// than nil, because the wire contract forbids a null list.
func emptyProjectAnalytics() schema.ProjectAnalytics {
	return schema.ProjectAnalytics{
		SessionsPerWeek:          []schema.WeekCount{},
		WeeklyActiveContributors: []schema.WeekContributors{},
		NewContributorVelocity:   []schema.WeekNewContributors{},
		AvgDurationPerActiveWeek: []schema.WeekAvgDuration{},
		PerContributorBreakdown:  []schema.ContributorBreakdown{},
		LongestStreak:            schema.LongestStreak{},
	}
}

// analyticsGroup is one (week, outcome) fold of the SQL aggregate. The
// percentile candidates are global ranks carried through whichever group holds
// them, so the whole summary comes from one statement.
type analyticsGroup struct {
	week          string
	outcome       string
	sessions      int
	durationSum   float64
	tokenSum      int64
	turnSum       int64
	toolSum       int64
	firstMs       int64
	lastMs        int64
	totalCount    int
	totalProjects int

	durationLo, durationHi       *float64
	tokenLo, tokenHi             *float64
	turnLo, turnHi               *float64
	toolLo, toolHi               *float64
	durationP90Lo, durationP90Hi *float64
	tokenP90Lo, tokenP90Hi       *float64
	turnP90Lo, turnP90Hi         *float64
	toolP90Lo, toolP90Hi         *float64
}

// ComputeProjectAnalyticsOnConn runs one grouped aggregate over sessions and
// session_metrics and folds its groups into the full ProjectAnalytics bundle.
// It is one statement: the week bucket, the outcome bucket, the per-week sums,
// the totals, and the median/p90 candidates all come from the same read, so a
// call cannot straddle two snapshots.
func ComputeProjectAnalyticsOnConn(ctx context.Context, conn *sqlite.Conn, filter AnalyticsFilter) (schema.ProjectAnalytics, error) {
	where := ""
	args := []any{}
	if filter.ProjectHash != "" {
		where = "WHERE s.project_hash = ?"
		args = append(args, string(filter.ProjectHash))
	}

	query := `
WITH base AS (
    SELECT
        s.start_ms AS start_ms,
        s.project_hash AS project_hash,
        COALESCE(m.outcome, '') AS outcome,
        ` + analyticsDurationExpr + ` AS duration_mins,
        COALESCE(m.input_tokens, 0) + COALESCE(` + analyticsTokensOutExpr + `, 0) AS total_tokens,
        COALESCE(` + sqlSessionTurnCount + `, 0) AS turn_count,
        COALESCE(` + analyticsToolCallsExpr + `, 0) AS tool_call_count
    ` + analyticsBaseSQL + `
    ` + where + `
),
ranked AS (
    SELECT
        b.*,
        COUNT(*) OVER () AS total_count,
        ROW_NUMBER() OVER (ORDER BY b.duration_mins) AS duration_rank,
        ROW_NUMBER() OVER (ORDER BY b.total_tokens) AS token_rank,
        ROW_NUMBER() OVER (ORDER BY b.turn_count) AS turn_rank,
        ROW_NUMBER() OVER (ORDER BY b.tool_call_count) AS tool_call_rank
    FROM base b
)
SELECT
    ` + analyticsWeekExpr + ` AS week,
    b.outcome,
    COUNT(*) AS sessions,
    SUM(b.duration_mins) AS duration_sum,
    SUM(b.total_tokens) AS token_sum,
    SUM(b.turn_count) AS turn_sum,
    SUM(b.tool_call_count) AS tool_sum,
    MIN(b.start_ms) AS first_ms,
    MAX(b.start_ms) AS last_ms,
    MAX(b.total_count) AS total_count,
    MAX(CASE WHEN b.duration_rank = ((b.total_count - 1) * 5 / 10) + 1 THEN b.duration_mins END) AS duration_lo,
    MAX(CASE WHEN b.duration_rank = (((b.total_count - 1) * 5 + 5) / 10) + 1 THEN b.duration_mins END) AS duration_hi,
    MAX(CASE WHEN b.token_rank = ((b.total_count - 1) * 5 / 10) + 1 THEN b.total_tokens END) AS token_lo,
    MAX(CASE WHEN b.token_rank = (((b.total_count - 1) * 5 + 5) / 10) + 1 THEN b.total_tokens END) AS token_hi,
    MAX(CASE WHEN b.turn_rank = ((b.total_count - 1) * 5 / 10) + 1 THEN b.turn_count END) AS turn_lo,
    MAX(CASE WHEN b.turn_rank = (((b.total_count - 1) * 5 + 5) / 10) + 1 THEN b.turn_count END) AS turn_hi,
    MAX(CASE WHEN b.tool_call_rank = ((b.total_count - 1) * 5 / 10) + 1 THEN b.tool_call_count END) AS tool_lo,
    MAX(CASE WHEN b.tool_call_rank = (((b.total_count - 1) * 5 + 5) / 10) + 1 THEN b.tool_call_count END) AS tool_hi,
    MAX(CASE WHEN b.duration_rank = ((b.total_count - 1) * 9 / 10) + 1 THEN b.duration_mins END) AS duration_p90_lo,
    MAX(CASE WHEN b.duration_rank = (((b.total_count - 1) * 9 + 9) / 10) + 1 THEN b.duration_mins END) AS duration_p90_hi,
    MAX(CASE WHEN b.token_rank = ((b.total_count - 1) * 9 / 10) + 1 THEN b.total_tokens END) AS token_p90_lo,
    MAX(CASE WHEN b.token_rank = (((b.total_count - 1) * 9 + 9) / 10) + 1 THEN b.total_tokens END) AS token_p90_hi,
    MAX(CASE WHEN b.turn_rank = ((b.total_count - 1) * 9 / 10) + 1 THEN b.turn_count END) AS turn_p90_lo,
    MAX(CASE WHEN b.turn_rank = (((b.total_count - 1) * 9 + 9) / 10) + 1 THEN b.turn_count END) AS turn_p90_hi,
    MAX(CASE WHEN b.tool_call_rank = ((b.total_count - 1) * 9 / 10) + 1 THEN b.tool_call_count END) AS tool_p90_lo,
    MAX(CASE WHEN b.tool_call_rank = (((b.total_count - 1) * 9 + 9) / 10) + 1 THEN b.tool_call_count END) AS tool_p90_hi,
    (SELECT COUNT(DISTINCT project_hash) FROM base) AS total_projects
FROM ranked b
GROUP BY week, b.outcome`

	var groups []analyticsGroup
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		groups = append(groups, analyticsGroup{
			week:          stmt.ColumnText(0),
			outcome:       stmt.ColumnText(1),
			sessions:      stmt.ColumnInt(2),
			durationSum:   stmt.ColumnFloat(3),
			tokenSum:      stmt.ColumnInt64(4),
			turnSum:       stmt.ColumnInt64(5),
			toolSum:       stmt.ColumnInt64(6),
			firstMs:       stmt.ColumnInt64(7),
			lastMs:        stmt.ColumnInt64(8),
			totalCount:    stmt.ColumnInt(9),
			durationLo:    nullableFloat(stmt, 10),
			durationHi:    nullableFloat(stmt, 11),
			tokenLo:       nullableFloat(stmt, 12),
			tokenHi:       nullableFloat(stmt, 13),
			turnLo:        nullableFloat(stmt, 14),
			turnHi:        nullableFloat(stmt, 15),
			toolLo:        nullableFloat(stmt, 16),
			toolHi:        nullableFloat(stmt, 17),
			durationP90Lo: nullableFloat(stmt, 18),
			durationP90Hi: nullableFloat(stmt, 19),
			tokenP90Lo:    nullableFloat(stmt, 20),
			tokenP90Hi:    nullableFloat(stmt, 21),
			turnP90Lo:     nullableFloat(stmt, 22),
			turnP90Hi:     nullableFloat(stmt, 23),
			toolP90Lo:     nullableFloat(stmt, 24),
			toolP90Hi:     nullableFloat(stmt, 25),
			totalProjects: stmt.ColumnInt(26),
		})
		return nil
	}})
	if err != nil {
		return schema.ProjectAnalytics{}, fmt.Errorf("store: compute project analytics: %w; no analytics bundle was produced; inspect the store and retry", err)
	}
	return foldAnalyticsGroups(groups), nil
}

// foldAnalyticsGroups turns the grouped statement's rows into the bundle.
func foldAnalyticsGroups(groups []analyticsGroup) schema.ProjectAnalytics {
	analytics := emptyProjectAnalytics()
	if len(groups) == 0 {
		return analytics
	}

	type weekAccumulator struct {
		sessions int
		duration float64
	}
	weeks := map[string]*weekAccumulator{}
	var firstMs, lastMs int64
	firstSet := false
	var totalCount, totalProjects int
	var outcome schema.OutcomeDistribution
	var tokenLo, tokenHi, durationLo, durationHi, turnLo, turnHi, toolLo, toolHi *float64
	var tokenP90Lo, tokenP90Hi, durationP90Lo, durationP90Hi, turnP90Lo, turnP90Hi, toolP90Lo, toolP90Hi *float64
	var totalTokens, totalTurns, totalTools int64
	var durationSum float64

	for _, group := range groups {
		totalCount = group.totalCount
		totalProjects = group.totalProjects
		if !firstSet || group.firstMs < firstMs {
			firstMs = group.firstMs
		}
		if !firstSet || group.lastMs > lastMs {
			lastMs = group.lastMs
		}
		firstSet = true

		if _, ok := weeks[group.week]; !ok {
			weeks[group.week] = &weekAccumulator{}
		}
		weeks[group.week].sessions += group.sessions
		weeks[group.week].duration += group.durationSum
		durationSum += group.durationSum
		totalTokens += group.tokenSum
		totalTurns += group.turnSum
		totalTools += group.toolSum

		switch schema.SessionOutcome(group.outcome) {
		case schema.OutcomeResolved:
			outcome.Resolved += group.sessions
		case schema.OutcomePartial:
			outcome.Partial += group.sessions
		case schema.OutcomeFailed:
			outcome.Failed += group.sessions
		default:
			outcome.Unknown += group.sessions
		}

		tokenLo = firstNonNil(tokenLo, group.tokenLo)
		tokenHi = firstNonNil(tokenHi, group.tokenHi)
		durationLo = firstNonNil(durationLo, group.durationLo)
		durationHi = firstNonNil(durationHi, group.durationHi)
		turnLo = firstNonNil(turnLo, group.turnLo)
		turnHi = firstNonNil(turnHi, group.turnHi)
		toolLo = firstNonNil(toolLo, group.toolLo)
		toolHi = firstNonNil(toolHi, group.toolHi)
		tokenP90Lo = firstNonNil(tokenP90Lo, group.tokenP90Lo)
		tokenP90Hi = firstNonNil(tokenP90Hi, group.tokenP90Hi)
		durationP90Lo = firstNonNil(durationP90Lo, group.durationP90Lo)
		durationP90Hi = firstNonNil(durationP90Hi, group.durationP90Hi)
		turnP90Lo = firstNonNil(turnP90Lo, group.turnP90Lo)
		turnP90Hi = firstNonNil(turnP90Hi, group.turnP90Hi)
		toolP90Lo = firstNonNil(toolP90Lo, group.toolP90Lo)
		toolP90Hi = firstNonNil(toolP90Hi, group.toolP90Hi)
	}

	analytics.TotalSessions = totalCount
	analytics.TotalProjects = totalProjects
	outcome.Total = totalCount
	analytics.OutcomeDistribution = outcome
	analytics.SessionStats = schema.AnalyticsSessionStats{
		DurationMins:  schema.MedianP90{Count: totalCount, Median: percentileCandidate(durationLo, durationHi, totalCount, 0.5), P90: percentileCandidate(durationP90Lo, durationP90Hi, totalCount, 0.9)},
		TotalTokens:   schema.MedianP90{Count: totalCount, Median: percentileCandidate(tokenLo, tokenHi, totalCount, 0.5), P90: percentileCandidate(tokenP90Lo, tokenP90Hi, totalCount, 0.9)},
		TurnCount:     schema.MedianP90{Count: totalCount, Median: percentileCandidate(turnLo, turnHi, totalCount, 0.5), P90: percentileCandidate(turnP90Lo, turnP90Hi, totalCount, 0.9)},
		ToolCallCount: schema.MedianP90{Count: totalCount, Median: percentileCandidate(toolLo, toolHi, totalCount, 0.5), P90: percentileCandidate(toolP90Lo, toolP90Hi, totalCount, 0.9)},
	}

	weekKeys := make([]string, 0, len(weeks))
	for week := range weeks {
		weekKeys = append(weekKeys, week)
	}
	sort.Strings(weekKeys)
	for _, week := range weekKeys {
		accumulator := weeks[week]
		analytics.SessionsPerWeek = append(analytics.SessionsPerWeek, schema.WeekCount{Week: week, Count: accumulator.sessions})
		analytics.WeeklyActiveContributors = append(analytics.WeeklyActiveContributors, schema.WeekContributors{Week: week, Contributors: 1})
		analytics.AvgDurationPerActiveWeek = append(analytics.AvgDurationPerActiveWeek, schema.WeekAvgDuration{
			Week:            week,
			AvgDurationMins: accumulator.duration / float64(accumulator.sessions),
			Sessions:        accumulator.sessions,
		})
	}

	analytics.TotalContributors = 1
	analytics.ReturningContributorRate = returningContributorRate(weekKeys)
	analytics.LongestStreak = longestStreak(weekKeys)
	analytics.NewContributorVelocity = []schema.WeekNewContributors{{Week: isoWeekKey(firstMs), NewContributors: 1}}
	analytics.SessionToCommitRate = schema.SessionToCommitRate{Total: totalCount, WithCommit: 0, Rate: 0}
	analytics.PerContributorBreakdown = []schema.ContributorBreakdown{{
		ContributorID:     LocalContributorID,
		Sessions:          totalCount,
		ActiveWeeks:       len(weekKeys),
		TotalDurationMins: int(math.Round(durationSum)),
		TotalTurns:        int(totalTurns),
		TotalToolCalls:    int(totalTools),
		TotalTokens:       int(totalTokens),
		Outcomes:          outcome,
		FirstSeen:         stringPtr(isoDayKey(firstMs)),
		LastSeen:          stringPtr(isoDayKey(lastMs)),
	}}
	return analytics
}

// AnalyticsAggregates computes the page summary and the personal medians over
// the library from one connection, so the two cached values describe the same
// stored set. Callers cache the result on the page revision.
func (s *Store) AnalyticsAggregates(ctx context.Context, filter AnalyticsFilter) (schema.ProjectAnalytics, schema.PersonalMedians, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return schema.ProjectAnalytics{}, schema.PersonalMedians{}, fmt.Errorf("store: take a connection for the analytics aggregate: %w; no analytics bundle was produced; restore database access and retry", err)
	}
	defer s.pool.Put(conn)

	analytics, err := ComputeProjectAnalyticsOnConn(ctx, conn, filter)
	if err != nil {
		return schema.ProjectAnalytics{}, schema.PersonalMedians{}, err
	}
	medians, err := ComputePersonalMediansOnConn(ctx, conn, filter)
	if err != nil {
		return schema.ProjectAnalytics{}, schema.PersonalMedians{}, err
	}
	return analytics, medians, nil
}

// MedianInput is one session's personal-medians inputs. It is the exported
// corpus shape the Go aggregate and the browser baseline both read.
type MedianInput struct {
	TotalTokens          int
	RetryTokensWasted    int
	SpecQualityScore     float64
	WithinSessionReverts int
}

// ComputePersonalMedians returns the library-wide baseline the transcript
// scorecard compares one session against. A zero-token session contributes no
// retry share, a non-positive spec score contributes no score, a negative
// revert count contributes no revert value, and an empty sample returns an
// empty baseline rather than a fabricated zero.
func ComputePersonalMedians(sessions []MedianInput) schema.PersonalMedians {
	var retryShares []float64
	var scores []float64
	var reverts []float64
	for _, session := range sessions {
		if session.TotalTokens > 0 {
			retryShares = append(retryShares, float64(session.RetryTokensWasted)/float64(session.TotalTokens))
		}
		if session.SpecQualityScore > 0 {
			scores = append(scores, session.SpecQualityScore)
		}
		if session.WithinSessionReverts >= 0 {
			reverts = append(reverts, float64(session.WithinSessionReverts))
		}
	}
	return schema.PersonalMedians{
		RetryShare:           percentile(retryShares, 0.5),
		SpecQualityScore:     percentile(scores, 0.5),
		WithinSessionReverts: percentile(reverts, 0.5),
	}
}

// ComputePersonalMediansOnConn reads every stored session's four median inputs
// in one statement and folds them through the same rule as the exported corpus.
func ComputePersonalMediansOnConn(ctx context.Context, conn *sqlite.Conn, filter AnalyticsFilter) (schema.PersonalMedians, error) {
	where := ""
	args := []any{}
	if filter.ProjectHash != "" {
		where = "WHERE s.project_hash = ?"
		args = append(args, string(filter.ProjectHash))
	}
	query := `SELECT
    COALESCE(m.input_tokens, 0) + COALESCE(` + analyticsTokensOutExpr + `, 0) AS total_tokens,
    COALESCE(m.retry_tokens_wasted, 0) AS retry_tokens_wasted,
    COALESCE(m.spec_quality_score, 0) AS spec_quality_score,
    COALESCE(m.within_session_reverts, 0) AS within_session_reverts
` + analyticsBaseSQL + `
` + where

	var inputs []MedianInput
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		inputs = append(inputs, MedianInput{
			TotalTokens:          stmt.ColumnInt(0),
			RetryTokensWasted:    stmt.ColumnInt(1),
			SpecQualityScore:     stmt.ColumnFloat(2),
			WithinSessionReverts: stmt.ColumnInt(3),
		})
		return nil
	}})
	if err != nil {
		return schema.PersonalMedians{}, fmt.Errorf("store: compute personal medians: %w; no medians were produced; inspect the store and retry", err)
	}
	return ComputePersonalMedians(inputs), nil
}

// CountQualitySessions returns the number of stored sessions the quality read
// covers: the same sessions/metrics/projects join the page rows use.
func (s *Store) CountQualitySessions(ctx context.Context) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take a connection to count quality sessions: %w; no total was produced; restore database access and retry", err)
	}
	defer s.pool.Put(conn)
	return CountQualitySessionsOnConn(ctx, conn)
}

// CountQualitySessionsOnConn is CountQualitySessions on a caller-supplied
// connection. It is one statement.
func CountQualitySessionsOnConn(ctx context.Context, conn *sqlite.Conn) (int, error) {
	var total int
	err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) `+analyticsBaseSQL, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		total = stmt.ColumnInt(0)
		return nil
	}})
	if err != nil {
		return 0, fmt.Errorf("store: count quality sessions: %w; no total was produced; inspect the store and retry", err)
	}
	return total, nil
}

// QualityPageRead is one offset read of quality rows: the requested page
// followed by the rows of the next page, filled from one statement.
type QualityPageRead struct {
	Rows []SessionRow
}

// QualityPageRows reads qne page of quality rows plus the rows of the page
// after it. It reads only the rows the page needs; a page never walks the whole
// library.
func (s *Store) QualityPageRows(ctx context.Context, page, perPage int) (QualityPageRead, error) {
	if page < 1 || perPage < 1 {
		return QualityPageRead{Rows: []SessionRow{}}, fmt.Errorf("store: quality page %d with %d rows is outside the bounded read; no query executed; supply a one-based page and a positive page size", page, perPage)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return QualityPageRead{Rows: []SessionRow{}}, fmt.Errorf("store: take a connection to read the quality page: %w; no rows were returned; restore database access and retry", err)
	}
	defer s.pool.Put(conn)
	return QualityPageRowsOnConn(ctx, conn, page, perPage)
}

// QualityPageRowsOnConn is QualityPageRows on a caller-supplied connection. It
// is one statement bounded to two pages.
func QualityPageRowsOnConn(ctx context.Context, conn *sqlite.Conn, page, perPage int) (QualityPageRead, error) {
	query := sqlAllSessions + ` ORDER BY s.start_ms DESC, s.session_id ASC LIMIT ? OFFSET ?`
	var rows []SessionRow
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: []any{2 * perPage, (page - 1) * perPage},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			rows = append(rows, scanSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return QualityPageRead{Rows: []SessionRow{}}, fmt.Errorf("store: read the quality page: %w; no rows were returned; inspect the store and retry", err)
	}
	if rows == nil {
		rows = []SessionRow{}
	}
	return QualityPageRead{Rows: rows}, nil
}

// percentile returns the linear-interpolated percentile of values, matching the
// browser's stats helper. A non-finite value is dropped; an empty sample has no
// value, so the caller renders no baseline instead of a fabricated zero.
func percentile(values []float64, p float64) *float64 {
	finite := make([]float64, 0, len(values))
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		finite = append(finite, value)
	}
	if len(finite) == 0 {
		return nil
	}
	sort.Float64s(finite)
	if len(finite) == 1 {
		return floatPtr(finite[0])
	}
	clamped := math.Min(1, math.Max(0, p))
	rank := clamped * float64(len(finite)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return floatPtr(finite[lo])
	}
	fraction := rank - float64(lo)
	return floatPtr(finite[lo] + (finite[hi]-finite[lo])*fraction)
}

// medianP90 is the typical/tail pair for one metric.
func medianP90(values []float64) schema.MedianP90 {
	return schema.MedianP90{
		Count:  len(values),
		Median: percentile(values, 0.5),
		P90:    percentile(values, 0.9),
	}
}

// percentileCandidate interpolates the two global ranks the SQL aggregate
// carried for one percentile. It matches percentile over the same sample.
func percentileCandidate(lo, hi *float64, count int, p float64) *float64 {
	if count == 0 || lo == nil {
		return nil
	}
	if hi == nil {
		return floatPtr(*lo)
	}
	rank := p * float64(count-1)
	loIndex := int(math.Floor(rank))
	fraction := rank - float64(loIndex)
	if fraction == 0 {
		return floatPtr(*lo)
	}
	return floatPtr(*lo + (*hi-*lo)*fraction)
}

// nullableFloat reads a nullable float column.
func nullableFloat(stmt *sqlite.Stmt, column int) *float64 {
	if stmt.ColumnIsNull(column) {
		return nil
	}
	return floatPtr(stmt.ColumnFloat(column))
}

// firstNonNil returns the first non-nil value; the SQL aggregate carries each
// percentile candidate in exactly one group and leaves the others NULL.
func firstNonNil(current, candidate *float64) *float64 {
	if current != nil {
		return current
	}
	return candidate
}

// isoWeekKey returns the UTC Monday-of-week date key the browser's weekKey
// produces.
func isoWeekKey(ms int64) string {
	instant := time.UnixMilli(ms).UTC()
	daysSinceMonday := (int(instant.Weekday()) + 6) % 7
	monday := time.Date(instant.Year(), instant.Month(), instant.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -daysSinceMonday)
	return monday.Format("2006-01-02")
}

// isoDayKey returns the UTC day key the browser's dayKey produces.
func isoDayKey(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// sortedWeekKeys returns the week keys in ascending order.
func sortedWeekKeys(weeks map[string]int) []string {
	keys := make([]string, 0, len(weeks))
	for week := range weeks {
		keys = append(keys, week)
	}
	sort.Strings(keys)
	return keys
}

// returningContributorRate reports the share of contributors active in two or
// more distinct weeks. The local store has one contributor.
func returningContributorRate(weeks []string) schema.ReturningContributorRate {
	if len(weeks) == 0 {
		return schema.ReturningContributorRate{}
	}
	returning := 0
	if len(weeks) >= 2 {
		returning = 1
	}
	return schema.ReturningContributorRate{Total: 1, Returning: returning, Rate: float64(returning)}
}

// longestStreak reports the longest run of consecutive active weeks.
func longestStreak(weeks []string) schema.LongestStreak {
	if len(weeks) == 0 {
		return schema.LongestStreak{}
	}
	bestLength, bestStart, bestEnd := 1, weeks[0], weeks[0]
	currentLength, currentStart := 1, weeks[0]
	for i := 1; i < len(weeks); i++ {
		previous, previousErr := time.Parse("2006-01-02", weeks[i-1])
		current, currentErr := time.Parse("2006-01-02", weeks[i])
		if previousErr == nil && currentErr == nil && current.Sub(previous) == 7*24*time.Hour {
			currentLength++
		} else {
			currentLength = 1
			currentStart = weeks[i]
		}
		if currentLength > bestLength {
			bestLength = currentLength
			bestStart = currentStart
			bestEnd = weeks[i]
		}
	}
	return schema.LongestStreak{Weeks: bestLength, StartWeek: stringPtr(bestStart), EndWeek: stringPtr(bestEnd)}
}

// sumWeekDuration totals the per-week duration sums.
func sumWeekDuration(weeks map[string]float64) float64 {
	var total float64
	for _, value := range weeks {
		total += value
	}
	return total
}

// sumInts totals a float sample as an integer.
func sumInts(values []float64) int {
	var total float64
	for _, value := range values {
		total += value
	}
	return int(math.Round(total))
}

func floatPtr(value float64) *float64 { return &value }

func stringPtr(value string) *string { return &value }
