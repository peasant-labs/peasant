package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// seedStatsSession inserts the host, project, and session rows one stats
// test needs. A non-nil active generation makes the session native.
func seedStatsSession(t *testing.T, s *store.Store, id string, active *string) {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	activeArg := any(nil)
	if active != nil {
		activeArg = *active
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('stats-host','stats-host')`, nil},
		{`INSERT OR IGNORE INTO projects(project_hash, canonical_cwd) VALUES('stats-project','/synthetic/stats')`, nil},
		{`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES(?, 'opencode', 'stats-model', 'stats-host', 'stats-project', 1, 2, 3, '/synthetic/stats.jsonl', 'jsonl', 11, ?)`,
			[]any{id, activeArg}},
	} {
		if err := sqlitex.ExecuteTransient(conn, stmt.sql, &sqlitex.ExecOptions{Args: stmt.args}); err != nil {
			t.Fatalf("seed stats session %s: %v", id, err)
		}
	}
}

func statsInt(v int) *int          { return &v }
func statsInt64(v int64) *int64    { return &v }
func statsString(v string) *string { return &v }

func readStatsMirror(t *testing.T, s *store.Store, id string) any {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	var mirror any
	if err := sqlitex.ExecuteTransient(conn, `SELECT input_submission_count FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{id},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnInt64(0)
				mirror = v
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	return mirror
}

// TestCapturedStatsCaptureUpsert pins capture-upsert: a harness insert
// creates the row, and the NULL-to-wire mapping renders unknown measurements
// as zeros and absent pointers.
func TestCapturedStatsCaptureUpsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-capture"
	seedStatsSession(t, s, "ses-stats-capture", &gen)
	seed := `{"turnCount":3,"toolCallCount":7}`
	applied, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID:            schema.SessionID("ses-stats-capture"),
		TurnCount:            statsInt(3),
		InputSubmissionCount: statsInt64(2),
		ToolCallCount:        statsInt(7),
		SeedJSON:             statsString(seed),
		Source:               store.StatsSourceHarness,
		UpdatedAtMs:          100,
	})
	if err != nil || !applied {
		t.Fatalf("upsert = (%v, %v), want (true, nil)", applied, err)
	}
	stats, err := s.ReadCapturedStats(ctx, schema.SessionID("ses-stats-capture"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stats.TurnCount == nil || *stats.TurnCount != 3 {
		t.Fatalf("TurnCount = %v, want 3", stats.TurnCount)
	}
	if stats.TokensOut != nil || stats.ThoughtTokens != nil {
		t.Fatalf("unreported columns must stay NULL: %+v", stats)
	}
	if stats.SeedJSON == nil || *stats.SeedJSON != seed {
		t.Fatalf("SeedJSON = %v, want %s", stats.SeedJSON, seed)
	}
	// Mirror moves in the same transaction.
	if mirror := readStatsMirror(t, s, "ses-stats-capture"); mirror != int64(2) {
		t.Fatalf("input submission mirror = %v, want 2", mirror)
	}
}

// TestCapturedStatsResumeLatestWins pins resume-latest-wins and
// derived-source-overwrite: a newer update wins wholesale on source and
// stamps, whatever its origin.
func TestCapturedStatsResumeLatestWins(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-resume"
	seedStatsSession(t, s, "ses-stats-resume", &gen)
	id := schema.SessionID("ses-stats-resume")
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(3), Source: store.StatsSourceHarness, UpdatedAtMs: 100,
	}); err != nil {
		t.Fatalf("capture upsert: %v", err)
	}
	applied, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(5), ToolCallCount: statsInt(9), Source: store.StatsSourceDerived, UpdatedAtMs: 200,
	})
	if err != nil || !applied {
		t.Fatalf("derived upsert = (%v, %v), want (true, nil)", applied, err)
	}
	stats, err := s.ReadCapturedStats(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stats.Source != store.StatsSourceDerived || stats.UpdatedAtMs != 200 {
		t.Fatalf("row label = (%s, %d), want (derived, 200)", stats.Source, stats.UpdatedAtMs)
	}
	if stats.TurnCount == nil || *stats.TurnCount != 5 {
		t.Fatalf("TurnCount = %v, want 5", stats.TurnCount)
	}
}

// TestCapturedStatsPartialUpdateMerges pins partial-update-merges and
// older-update-ignored: omitted fields keep stored knowledge, and an older
// update loses the age gate without error.
func TestCapturedStatsPartialUpdateMerges(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-partial"
	seedStatsSession(t, s, "ses-stats-partial", &gen)
	id := schema.SessionID("ses-stats-partial")
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(3), ToolCallCount: statsInt(7),
		Source: store.StatsSourceHarness, UpdatedAtMs: 100,
	}); err != nil {
		t.Fatalf("capture upsert: %v", err)
	}
	overflow := `{"m9_score":0.5}`
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TokensOut: statsInt(400), Overflow: statsString(overflow),
		Source: store.StatsSourceHarness, UpdatedAtMs: 150,
	}); err != nil {
		t.Fatalf("partial upsert: %v", err)
	}
	applied, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(99), Source: store.StatsSourceHarness, UpdatedAtMs: 120,
	})
	if err != nil {
		t.Fatalf("older upsert: %v", err)
	}
	if applied {
		t.Fatal("older update applied, want ignored")
	}
	stats, err := s.ReadCapturedStats(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stats.TurnCount == nil || *stats.TurnCount != 3 {
		t.Fatalf("TurnCount = %v, want stored 3", stats.TurnCount)
	}
	if stats.TokensOut == nil || *stats.TokensOut != 400 {
		t.Fatalf("TokensOut = %v, want merged 400", stats.TokensOut)
	}
	if stats.Overflow == nil || *stats.Overflow != overflow {
		t.Fatalf("Overflow = %v, want %s", stats.Overflow, overflow)
	}
}

// TestSeedUnknownForComputeOnlyRow pins seed-unknown-for-compute-only-row: a
// row only COMPUTE ever wrote carries no seed, so the seed path reports
// unknown instead of mistaking derived values for adapter evidence.
func TestSeedUnknownForComputeOnlyRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-compute-only"
	seedStatsSession(t, s, "ses-stats-compute-only", &gen)
	id := schema.SessionID("ses-stats-compute-only")
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(4), Source: store.StatsSourceDerived, UpdatedAtMs: 50,
	}); err != nil {
		t.Fatalf("derived upsert: %v", err)
	}
	seed, err := s.ReadMetricSeed(ctx, id)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if seed != nil {
		t.Fatalf("seed = %+v, want unknown (nil)", seed)
	}
}

// TestSeedJSONSurvivesDerivedUpdate pins seed-json-survives-derived-update:
// a derived update relabels the row without touching the captured seed.
func TestSeedJSONSurvivesDerivedUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-seed-survives"
	seedStatsSession(t, s, "ses-stats-seed-survives", &gen)
	id := schema.SessionID("ses-stats-seed-survives")
	seed := `{"turnCount":3,"toolCallCount":7}`
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(3), SeedJSON: statsString(seed),
		Source: store.StatsSourceHarness, UpdatedAtMs: 100,
	}); err != nil {
		t.Fatalf("capture upsert: %v", err)
	}
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(9), Source: store.StatsSourceDerived, UpdatedAtMs: 200,
	}); err != nil {
		t.Fatalf("derived upsert: %v", err)
	}
	got, err := s.ReadMetricSeed(ctx, id)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if got == nil || got.TurnCount != 3 || got.ToolCallCount != 7 {
		t.Fatalf("seed = %+v, want captured {3 7}", got)
	}
}

// TestSeedUnknownAfterPartialHarnessUpdate pins
// seed-unknown-after-partial-harness-update: a harness update carrying no seed
// document clears the home, so stale evidence never outlives the knowledge
// that replaced it.
func TestSeedUnknownAfterPartialHarnessUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	gen := "gen-seed-partial"
	seedStatsSession(t, s, "ses-stats-seed-partial", &gen)
	id := schema.SessionID("ses-stats-seed-partial")
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(3), SeedJSON: statsString(`{"turnCount":3}`),
		Source: store.StatsSourceHarness, UpdatedAtMs: 100,
	}); err != nil {
		t.Fatalf("capture upsert: %v", err)
	}
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: id, TurnCount: statsInt(4), Source: store.StatsSourceHarness, UpdatedAtMs: 150,
	}); err != nil {
		t.Fatalf("partial harness upsert: %v", err)
	}
	seed, err := s.ReadMetricSeed(ctx, id)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if seed != nil {
		t.Fatalf("seed = %+v, want unknown (nil) after seedless harness update", seed)
	}
	stats, err := s.ReadCapturedStats(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stats.TurnCount == nil || *stats.TurnCount != 4 {
		t.Fatalf("TurnCount = %v, want merged 4", stats.TurnCount)
	}
}

// TestSeedWriteAllowed pins the seed rule at the gate: harness origins may
// write the seed home, derived origins may not, and anything outside the
// closed set is refused outright.
func TestSeedWriteAllowed(t *testing.T) {
	if ok, err := store.SeedWriteAllowed(store.StatsSourceHarness); !ok || err != nil {
		t.Fatalf("harness = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := store.SeedWriteAllowed(store.StatsSourceDerived); ok || err != nil {
		t.Fatalf("derived = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.SeedWriteAllowed(store.StatsSource("archived")); ok || err == nil {
		t.Fatalf("unknown origin = (%v, %v), want (false, error)", ok, err)
	}
}
