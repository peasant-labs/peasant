package store

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_reclaim.yaml
var generationReclaimYAML []byte

type generationReclaimFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		SupersededID string `yaml:"superseded_id"`
		ActiveID     string `yaml:"active_id"`
		PendingID    string `yaml:"pending_id"`
	} `yaml:"generation"`
}

func loadGenerationReclaimFixture(t *testing.T) generationReclaimFixture {
	t.Helper()
	var fixture generationReclaimFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationReclaimYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_reclaim.yaml: %v", err)
	}
	return fixture
}

// seedSupersededReclaimSession seeds one session, activates the superseded
// generation and then the active one, and returns both generation identifiers.
// Activation installs the superseded generation's rows and directory first, so
// the later activation leaves it a real inactive generation to reclaim.
func seedSupersededReclaimSession(t *testing.T, s *Store, fixture generationReclaimFixture) schema.SessionID {
	t.Helper()
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, fixture.Session.ID)
	superseded, supersededBlobs := buildTestGeneration(t, id, fixture.Generation.SupersededID, "superseded user text", "superseded tool input", "superseded tool output")
	if err := activateTestGeneration(t, s, superseded, supersededBlobs); err != nil {
		t.Fatalf("activate superseded generation: %v", err)
	}
	active, activeBlobs := buildTestGeneration(t, id, fixture.Generation.ActiveID, "active user text", "active tool input", "active tool output")
	if err := activateTestGeneration(t, s, active, activeBlobs); err != nil {
		t.Fatalf("activate active generation: %v", err)
	}
	if visible := visibleGeneration(t, s, id); visible != fixture.Generation.ActiveID {
		t.Fatalf("visible generation = %q, want active %q", visible, fixture.Generation.ActiveID)
	}
	return id
}

// nonActiveReclaimCounts counts, per table, every row for a session whose
// generation is not the active one. It is the independent oracle the dry-run
// forecast must equal: total minus active per table.
func nonActiveReclaimCounts(t *testing.T, s *Store, id schema.SessionID, active string) ReclaimTableCounts {
	t.Helper()
	var counts ReclaimTableCounts
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	for _, table := range reclaimTableNames {
		var total int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args:       []any{string(id)},
			ResultFunc: func(stmt *sqlite.Stmt) error { total = stmt.ColumnInt64(0); return nil },
		}); err != nil {
			t.Fatalf("count %s total: %v", table, err)
		}
		var activeCount int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
			Args:       []any{string(id), active},
			ResultFunc: func(stmt *sqlite.Stmt) error { activeCount = stmt.ColumnInt64(0); return nil },
		}); err != nil {
			t.Fatalf("count %s active: %v", table, err)
		}
		field := reclaimCountField(&counts, table)
		if field == nil {
			t.Fatalf("table %s has no count field", table)
		}
		*field = total - activeCount
	}
	return counts
}

// countGenerationRows returns the row count in one table for one generation.
func countGenerationRows(t *testing.T, s *Store, table string, id schema.SessionID, generationID string) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(id), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error { count = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatalf("count %s for generation: %v", table, err)
	}
	return count
}

// generationRowCounts returns the per-table row counts for one generation.
func generationRowCounts(t *testing.T, s *Store, id schema.SessionID, generationID string) ReclaimTableCounts {
	t.Helper()
	var counts ReclaimTableCounts
	for _, table := range reclaimTableNames {
		field := reclaimCountField(&counts, table)
		if field == nil {
			t.Fatalf("table %s has no count field", table)
		}
		*field = countGenerationRows(t, s, table, id, generationID)
	}
	return counts
}

// generationDirectoryExists reports whether an owned generation directory is
// present under the artifact root.
func generationDirectoryExists(root string, id schema.SessionID, generationID string) bool {
	_, err := os.Stat(filepath.Join(root, string(id), "generations", generationID))
	return err == nil
}

// assertReclaimIntegrity runs the two structural checks the reclaim must leave
// clean.
func assertReclaimIntegrity(t *testing.T, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var integrity string
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA integrity_check`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { integrity = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want ok", integrity)
	}
	rows := 0
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { rows++; return nil },
	}); err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	if rows != 0 {
		t.Fatalf("PRAGMA foreign_key_check reported %d violation(s)", rows)
	}
}

// TestSupersededGenerationReclaimForecastAndApply proves the dry-run forecast
// equals total minus active per table, the apply removes exactly that, and the
// active generation's rows and directory are untouched.
func TestSupersededGenerationReclaimForecastAndApply(t *testing.T) {
	fixture := loadGenerationReclaimFixture(t)
	s, root := openGenerationStore(t)
	id := seedSupersededReclaimSession(t, s, fixture)

	wantCounts := nonActiveReclaimCounts(t, s, id, fixture.Generation.ActiveID)
	if wantCounts.Total() == 0 {
		t.Fatal("fixture left no superseded rows to reclaim")
	}
	activeBefore := generationRowCounts(t, s, id, fixture.Generation.ActiveID)

	plan, err := s.PlanSupersededGenerationReclaim(context.Background(), 0)
	if err != nil {
		t.Fatalf("plan reclaim: %v", err)
	}
	if plan.SessionCount() != 1 || plan.GenerationCount() != 1 {
		t.Fatalf("plan = %d sessions, %d generations, want 1 and 1", plan.SessionCount(), plan.GenerationCount())
	}
	plannedRows, plannedFootprint := plan.Totals()
	if plannedRows != wantCounts {
		t.Fatalf("plan rows = %+v, want total-minus-active %+v", plannedRows, wantCounts)
	}
	if plannedFootprint.Bytes == 0 {
		t.Fatal("plan footprint reported no bytes for a staged generation")
	}

	result, err := s.ReclaimSupersededGenerations(context.Background(), 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 1 || result.Generations != 1 {
		t.Fatalf("result = %d sessions, %d generations, want 1 and 1", result.Sessions, result.Generations)
	}
	if result.Rows != wantCounts {
		t.Fatalf("reclaimed rows = %+v, want %+v", result.Rows, wantCounts)
	}
	if result.DirectoriesRemoved != 1 {
		t.Fatalf("directories removed = %d, want 1", result.DirectoriesRemoved)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("reclaim warnings = %v, want none", result.Warnings)
	}

	// Zero superseded rows remain; every active-generation row is intact.
	for _, table := range reclaimTableNames {
		if remaining := countGenerationRows(t, s, table, id, fixture.Generation.SupersededID); remaining != 0 {
			t.Fatalf("%s still holds %d superseded row(s)", table, remaining)
		}
	}
	if activeAfter := generationRowCounts(t, s, id, fixture.Generation.ActiveID); activeAfter != activeBefore {
		t.Fatalf("active generation rows changed across the reclaim: before=%+v after=%+v", activeBefore, activeAfter)
	}

	// The active complete row remains the last-good read authority, so the
	// completeness guard still refuses an incomplete candidate.
	incomplete, incompleteBlobs := buildTestGeneration(t, id, "gen_reclaim_incomplete", "incomplete text", "incomplete input", "incomplete output")
	incomplete.Generation.Completeness = indexformat.GenerationCompletenessIncompleteNew
	if err := activateTestGeneration(t, s, incomplete, incompleteBlobs); err == nil {
		t.Fatal("completeness guard no longer refuses an incomplete candidate after the reclaim")
	}
	if visible := visibleGeneration(t, s, id); visible != fixture.Generation.ActiveID {
		t.Fatalf("visible generation = %q after reclaim, want active %q", visible, fixture.Generation.ActiveID)
	}
	if !generationDirectoryExists(root, id, fixture.Generation.ActiveID) {
		t.Fatal("active generation directory was removed")
	}
	if generationDirectoryExists(root, id, fixture.Generation.SupersededID) {
		t.Fatal("superseded generation directory survived the reclaim")
	}
	assertReclaimIntegrity(t, s)
}

// TestSupersededGenerationReclaimLimitResumes proves the batch limit advances:
// the first pass reclaims one session, the next pass skips it and reclaims the
// next, and a final pass finds no work.
func TestSupersededGenerationReclaimLimitResumes(t *testing.T) {
	fixture := loadGenerationReclaimFixture(t)
	s, _ := openGenerationStore(t)
	seedSupersededReclaimSession(t, s, fixture)

	secondID, err := schema.NewSessionID("bbbb4444-4444-4444-8444-444444444444")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(secondID))
	old, oldBlobs := buildTestGeneration(t, secondID, fixture.Generation.SupersededID, "second old text", "second old input", "second old output")
	if err := activateTestGeneration(t, s, old, oldBlobs); err != nil {
		t.Fatalf("activate second session superseded generation: %v", err)
	}
	active, activeBlobs := buildTestGeneration(t, secondID, fixture.Generation.ActiveID, "second active text", "second active input", "second active output")
	if err := activateTestGeneration(t, s, active, activeBlobs); err != nil {
		t.Fatalf("activate second session active generation: %v", err)
	}

	first, err := s.ReclaimSupersededGenerations(context.Background(), 1)
	if err != nil {
		t.Fatalf("first limited pass: %v", err)
	}
	if first.Sessions != 1 {
		t.Fatalf("first pass reclaimed %d session(s), want 1", first.Sessions)
	}
	second, err := s.ReclaimSupersededGenerations(context.Background(), 1)
	if err != nil {
		t.Fatalf("second limited pass: %v", err)
	}
	if second.Sessions != 1 {
		t.Fatalf("second pass reclaimed %d session(s), want 1", second.Sessions)
	}
	third, err := s.ReclaimSupersededGenerations(context.Background(), 1)
	if err != nil {
		t.Fatalf("third limited pass: %v", err)
	}
	if third.Sessions != 0 {
		t.Fatalf("third pass reclaimed %d session(s), want 0", third.Sessions)
	}
	assertReclaimIntegrity(t, s)
}

// TestSupersededGenerationReclaimSkipsPendingIntent proves a session with a
// durable activation intent is never touched: no row is deleted and no
// directory is removed.
func TestSupersededGenerationReclaimSkipsPendingIntent(t *testing.T) {
	fixture := loadGenerationReclaimFixture(t)
	s, root := openGenerationStore(t)
	id := seedSupersededReclaimSession(t, s, fixture)

	if err := s.generationArtifacts.WriteIntent(context.Background(), GenerationIntent{
		SessionID:    id,
		GenerationID: fixture.Generation.PendingID,
		ManifestPath: "generations/" + fixture.Generation.PendingID + "/manifest.json",
		Completeness: "complete",
		StagedAtMs:   1,
	}); err != nil {
		t.Fatalf("write pending intent: %v", err)
	}

	plan, err := s.PlanSupersededGenerationReclaim(context.Background(), 0)
	if err != nil {
		t.Fatalf("plan reclaim: %v", err)
	}
	if plan.SessionCount() != 0 {
		t.Fatalf("plan included %d session(s) with a pending intent, want 0", plan.SessionCount())
	}
	if len(plan.PendingIntentSessions) != 1 || plan.PendingIntentSessions[0] != id {
		t.Fatalf("plan pending-intent sessions = %v, want [%s]", plan.PendingIntentSessions, id)
	}

	result, err := s.ReclaimSupersededGenerations(context.Background(), 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 0 || result.Rows.Total() != 0 {
		t.Fatalf("reclaim touched a pending-intent session: %+v", result)
	}
	if len(result.PendingIntentSessions) != 1 || result.PendingIntentSessions[0] != id {
		t.Fatalf("result pending-intent sessions = %v, want [%s]", result.PendingIntentSessions, id)
	}
	if got := countGenerationRows(t, s, "session_projection_entries", id, fixture.Generation.SupersededID); got == 0 {
		t.Fatal("superseded rows were deleted for a session with a pending intent")
	}
	if !generationDirectoryExists(root, id, fixture.Generation.SupersededID) {
		t.Fatal("superseded directory was removed for a session with a pending intent")
	}
}

// TestSupersededGenerationReclaimCrashLeavesRetryableOrphan proves the
// row-first order is crash-safe: a fault between the row transaction and
// directory removal leaves a readable store whose superseded rows are gone,
// and the next pass removes the orphan directory.
func TestSupersededGenerationReclaimCrashLeavesRetryableOrphan(t *testing.T) {
	fixture := loadGenerationReclaimFixture(t)
	s, root := openGenerationStore(t)
	id := seedSupersededReclaimSession(t, s, fixture)

	s.reclaimSeam = func(stage string) error {
		if stage == reclaimSeamAfterRows {
			return errors.New("injected crash between row deletion and directory removal")
		}
		return nil
	}
	if _, err := s.ReclaimSupersededGenerations(context.Background(), 0); err == nil {
		t.Fatal("reclaim with an injected crash returned nil error")
	}
	s.reclaimSeam = nil

	// The store stays readable and the active generation is still served.
	if visible := visibleGeneration(t, s, id); visible != fixture.Generation.ActiveID {
		t.Fatalf("visible generation = %q after the crash, want active %q", visible, fixture.Generation.ActiveID)
	}
	if got := countGenerationRows(t, s, "session_projection_entries", id, fixture.Generation.SupersededID); got != 0 {
		t.Fatalf("superseded rows survived the committed crash: %d", got)
	}
	if !generationDirectoryExists(root, id, fixture.Generation.SupersededID) {
		t.Fatal("the crash test expected an orphan directory to remain")
	}
	assertReclaimIntegrity(t, s)

	retry, err := s.ReclaimSupersededGenerations(context.Background(), 0)
	if err != nil {
		t.Fatalf("retry reclaim: %v", err)
	}
	if retry.DirectoriesRemoved != 1 {
		t.Fatalf("retry removed %d directories, want the 1 orphan", retry.DirectoriesRemoved)
	}
	if generationDirectoryExists(root, id, fixture.Generation.SupersededID) {
		t.Fatal("retry left the orphan directory in place")
	}
	if visible := visibleGeneration(t, s, id); visible != fixture.Generation.ActiveID {
		t.Fatalf("visible generation = %q after retry, want active %q", visible, fixture.Generation.ActiveID)
	}
	assertReclaimIntegrity(t, s)
}
