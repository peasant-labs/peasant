package metrics_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// helper to build entries with tokens.
func tokEntry(sid ingest.SessionID, idx int, role ingest.Role, tokIn, tokOut int) schema.SessionEntry {
	e := schema.SessionEntry{
		SessionID:  sid,
		EntryIndex: idx,
		Role:       role,
		EntryType:  ingest.EntryTypeText,
	}
	if tokIn > 0 {
		e.TokensIn = &tokIn
	}
	if tokOut > 0 {
		e.TokensOut = &tokOut
	}
	return e
}

// runMetrics creates an engine, seeds entries, computes, and returns the saved metrics.
func runMetrics(t *testing.T, sid ingest.SessionID, entries []schema.SessionEntry) *ingest.SessionMetrics {
	t.Helper()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	store.IndexedEntries[sid] = entries

	n, err := engine.ComputeMetrics(context.Background(), []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 computed, got %d", n)
	}

	m := store.SavedMetrics[sid]
	if m == nil {
		t.Fatal("expected saved metrics")
	}
	return m
}

func TestCompute_Tokens(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	entries := []schema.SessionEntry{
		tokEntry(sid, 0, ingest.RoleUser, 100, 0),
		tokEntry(sid, 1, ingest.RoleAssistant, 200, 50),
		tokEntry(sid, 2, ingest.RoleAssistant, 300, 150),
	}

	m := runMetrics(t, sid, entries)

	// InputTokens = peak context (MAX), not SUM.
	if m.InputTokens == nil || *m.InputTokens != 300 {
		t.Errorf("input_tokens (peak): expected 300, got %v", m.InputTokens)
	}
	if m.OutputTokens == nil || *m.OutputTokens != 200 {
		t.Errorf("output_tokens: expected 200, got %v", m.OutputTokens)
	}
	// TotalTokens = peak context window usage.
	if m.TotalTokens == nil || *m.TotalTokens != 300 {
		t.Errorf("total_tokens (peak context): expected 300, got %v", m.TotalTokens)
	}
}

func TestCompute_Tokens_NoTokenData(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
	}

	m := runMetrics(t, sid, entries)

	// Tokens should still get 0 values from turns computation but nil from tokens func.
	// The merged result depends on which func runs first; tokens func returns nil,
	// but turns/other funcs may set ToolCalls etc.
	// TotalTokens specifically comes from computeTokens which returns nil when no tokens.
	if m.TotalTokens != nil {
		t.Errorf("total_tokens: expected nil (no token data), got %v", m.TotalTokens)
	}
}

func TestCompute_Turns(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	toolCSV := "Read,Grep"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeToolUse, HasToolUse: true, ToolNamesCSV: &toolCSV},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
		{SessionID: sid, EntryIndex: 3, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
	}

	m := runMetrics(t, sid, entries)

	// TurnCount = depth=0 user + assistant entries = 4.
	if m.TurnCount == nil || *m.TurnCount != 4 {
		t.Errorf("turn_count: expected 4, got %v", m.TurnCount)
	}
	if m.ToolCalls == nil || *m.ToolCalls != 2 {
		t.Errorf("tool_calls: expected 2, got %v", m.ToolCalls)
	}
}

func TestCompute_Title(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	preview := "Help me fix the authentication bug"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &preview},
	}

	m := runMetrics(t, sid, entries)

	if m.TitleGenerated == nil || *m.TitleGenerated != preview {
		t.Errorf("title: expected %q, got %v", preview, m.TitleGenerated)
	}
}

func TestCompute_Title_Truncation_NoSpaces(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// No spaces → falls back to hard cut at 80.
	longPreview := strings.Repeat("A", 200)
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &longPreview},
	}

	m := runMetrics(t, sid, entries)

	if m.TitleGenerated == nil {
		t.Fatal("title: expected non-nil")
	}
	if len(*m.TitleGenerated) != 80 {
		t.Errorf("title length: expected 80, got %d", len(*m.TitleGenerated))
	}
}

func TestCompute_Title_Truncation_WordBoundary(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// "word " repeated → 100 chars. Word boundary truncation should cut at a space before 80.
	longPreview := strings.Repeat("word ", 20) // 100 chars
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &longPreview},
	}

	m := runMetrics(t, sid, entries)

	if m.TitleGenerated == nil {
		t.Fatal("title: expected non-nil")
	}
	if len(*m.TitleGenerated) > 80 {
		t.Errorf("title length: expected <= 80, got %d", len(*m.TitleGenerated))
	}
	// Should end at a word boundary (last space at or before position 80).
	// "word " * 16 = 80 chars, last space is at index 79.
	if strings.HasSuffix(*m.TitleGenerated, " ") {
		// Trimmed at space boundary — correct behavior.
	}
	if len(*m.TitleGenerated) != 79 {
		t.Errorf("title length: expected 79 (word boundary), got %d", len(*m.TitleGenerated))
	}
}

func TestCompute_Outcome_Resolved(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant},
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomeResolved {
		t.Errorf("outcome: expected %q, got %v", ingest.OutcomeResolved, m.Outcome)
	}
}

func TestCompute_Outcome_Failed(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// >40% errors + last assistant IsError=true → failed.
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, IsError: true},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, IsError: true},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleUser, IsError: true},
		{SessionID: sid, EntryIndex: 3, Role: ingest.RoleAssistant, IsError: true},
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomeFailed {
		t.Errorf("outcome: expected %q, got %v", ingest.OutcomeFailed, m.Outcome)
	}
}

func TestCompute_Outcome_Partial(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Last assistant IsError=true, but ≤40% errors → partial.
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 3, Role: ingest.RoleAssistant},
		{SessionID: sid, EntryIndex: 4, Role: ingest.RoleAssistant, IsError: true},
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomePartial {
		t.Errorf("outcome: expected %q, got %v", ingest.OutcomePartial, m.Outcome)
	}
}

func TestCompute_Outcome_ConsecutiveErrors(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// 5 consecutive errors in a 20-entry session → maxConsecutiveErrors ≥ 5 → failed.
	var entries []schema.SessionEntry
	for i := 0; i < 10; i++ {
		entries = append(entries, schema.SessionEntry{SessionID: sid, EntryIndex: i, Role: ingest.RoleUser})
		entries = append(entries, schema.SessionEntry{SessionID: sid, EntryIndex: i + 100, Role: ingest.RoleAssistant})
	}
	// Overwrite entries 10-14 (assistant entries at indices 100-104) with errors.
	// Place 5 consecutive errors in the middle: indices 5-9 as a block.
	entries = entries[:0]
	idx := 0
	for i := 0; i < 20; i++ {
		role := ingest.RoleUser
		if i%2 == 1 {
			role = ingest.RoleAssistant
		}
		e := schema.SessionEntry{SessionID: sid, EntryIndex: idx, Role: role}
		// Make entries 10-14 all errors (a consecutive block of 5).
		if i >= 10 && i < 15 {
			e.IsError = true
		}
		entries = append(entries, e)
		idx++
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomeFailed {
		t.Errorf("outcome: expected %q (5 consecutive errors), got %v", ingest.OutcomeFailed, m.Outcome)
	}
}

func TestCompute_Outcome_TailErrorConcentration(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// 20 entries, last 5 (tail quarter) are all errors → tailErrorRate = 1.0 → failed.
	var entries []schema.SessionEntry
	for i := 0; i < 20; i++ {
		role := ingest.RoleUser
		if i%2 == 1 {
			role = ingest.RoleAssistant
		}
		e := schema.SessionEntry{SessionID: sid, EntryIndex: i, Role: role}
		if i >= 15 {
			e.IsError = true
		}
		entries = append(entries, e)
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomeFailed {
		t.Errorf("outcome: expected %q (tail errors concentrated at end), got %v", ingest.OutcomeFailed, m.Outcome)
	}
}

func TestCompute_Outcome_LargeSessionDilution(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// 40 entries total, 3 errors clustered in the last 10 (tail quarter).
	// Overall errorRate = 3/40 = 0.075 (≥ 0.05) → partial (not resolved).
	// This catches the "dilution" bug: errors are few overall but concentrated at the tail.
	var entries []schema.SessionEntry
	for i := 0; i < 40; i++ {
		role := ingest.RoleUser
		if i%2 == 1 {
			role = ingest.RoleAssistant
		}
		e := schema.SessionEntry{SessionID: sid, EntryIndex: i, Role: role}
		// 3 errors in positions 35, 37, 39 (tail quarter of 40 = last 10).
		if i == 35 || i == 37 || i == 39 {
			e.IsError = true
		}
		entries = append(entries, e)
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome == ingest.OutcomeResolved {
		t.Errorf("outcome: expected partial or failed (diluted errors at tail), got %v", m.Outcome)
	}
}

func TestCompute_Outcome_ModerateTailErrors(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// 20 entries, 2 errors in last 5 (tail quarter) → tailErrorRate = 0.4 ≥ 0.2 → partial.
	// Overall errorRate = 2/20 = 0.1 ≥ 0.05 → also triggers partial via errorRate.
	var entries []schema.SessionEntry
	for i := 0; i < 20; i++ {
		role := ingest.RoleUser
		if i%2 == 1 {
			role = ingest.RoleAssistant
		}
		e := schema.SessionEntry{SessionID: sid, EntryIndex: i, Role: role}
		if i == 16 || i == 18 {
			e.IsError = true
		}
		entries = append(entries, e)
	}

	m := runMetrics(t, sid, entries)

	if m.Outcome == nil || *m.Outcome != ingest.OutcomePartial {
		t.Errorf("outcome: expected %q (moderate tail errors), got %v", ingest.OutcomePartial, m.Outcome)
	}
}

func TestCompute_Duration(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	ts1 := int64(1700000000000)            // start
	ts2 := int64(1700000000000 + 5*60000)  // +5 minutes
	ts3 := int64(1700000000000 + 10*60000) // +10 minutes

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, TimestampMs: &ts1},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, TimestampMs: &ts2},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleUser, TimestampMs: &ts3},
	}

	m := runMetrics(t, sid, entries)

	if m.DurationMinutes == nil {
		t.Fatal("duration_minutes: expected non-nil")
	}
	if *m.DurationMinutes != 10.0 {
		t.Errorf("duration_minutes: expected 10.0, got %f", *m.DurationMinutes)
	}
}

func TestCompute_RetryLoops(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	tokIn := 200
	tokOut := 100
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
		// 2 consecutive assistant errors → 1 retry loop.
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, IsError: true, TokensIn: &tokIn, TokensOut: &tokOut},
		{SessionID: sid, EntryIndex: 2, Role: ingest.RoleAssistant, IsError: true, TokensIn: &tokIn, TokensOut: &tokOut},
		{SessionID: sid, EntryIndex: 3, Role: ingest.RoleAssistant}, // non-error → ends streak
	}

	m := runMetrics(t, sid, entries)

	if m.RetryLoops == nil || *m.RetryLoops != 1 {
		t.Errorf("retry_loops: expected 1, got %v", m.RetryLoops)
	}
	// 2 entries × (200+100) tokens = 600 wasted tokens.
	if m.RetryTokensWasted == nil || *m.RetryTokensWasted != 600 {
		t.Errorf("retry_tokens_wasted: expected 600, got %v", m.RetryTokensWasted)
	}
}

func TestCompute_RetryLoops_NoErrors(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant},
	}

	m := runMetrics(t, sid, entries)

	if m.RetryLoops == nil || *m.RetryLoops != 0 {
		t.Errorf("retry_loops: expected 0, got %v", m.RetryLoops)
	}
}

func TestCompute_SpecQuality(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Long message with examples and constraints.
	longMsg := strings.Repeat("word ", 60) + "you must use the example pattern like this"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &longMsg},
	}

	m := runMetrics(t, sid, entries)

	if m.SpecQualityScore == nil {
		t.Fatal("spec_quality_score: expected non-nil")
	}
	// 68 words → 68/150*60 = 27.2, examples: "example"+"like this" → 2/4*20 = 10.0,
	// constraints: "must" → 1/4*20 = 5.0. Total = 42.2.
	if *m.SpecQualityScore != 42.2 {
		t.Errorf("spec_quality_score: expected 42.2, got %f", *m.SpecQualityScore)
	}
}

func TestCompute_SpecQuality_ShortMessage(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	shortMsg := "fix bug"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText, ContentPreview: &shortMsg},
	}

	m := runMetrics(t, sid, entries)

	if m.SpecQualityScore == nil {
		t.Fatal("spec_quality_score: expected non-nil")
	}
	// 2 words → 2/150*60 = 0.8, no examples or constraints.
	if *m.SpecQualityScore != 0.8 {
		t.Errorf("spec_quality_score: expected 0.8 for short message, got %f", *m.SpecQualityScore)
	}
}

func TestCompute_SubagentDetection(t *testing.T) {
	t.Parallel()
	sid := mustSessionID(t, testutil.TestSessionUUID)

	taskCSV := "Task,Read"
	normalCSV := "Read,Grep"
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleAssistant, HasToolUse: true, ToolNamesCSV: &taskCSV},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, HasToolUse: true, ToolNamesCSV: &normalCSV},
	}

	m := runMetrics(t, sid, entries)

	if m.SubagentCount == nil || *m.SubagentCount != 1 {
		t.Errorf("subagent_count: expected 1, got %v", m.SubagentCount)
	}
}
