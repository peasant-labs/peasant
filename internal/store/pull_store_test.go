package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

// pullStoreContract mirrors the authoritative PullStore method set that the pull pipeline
// declares consumer-side in internal/pull. It is kept
// here so *store.Store is checked against the exact signatures at compile time
// before internal/pull exists; its interface must match this set.
type pullStoreContract interface {
	CommitPull(ctx context.Context, commit store.PullCommit) error
	UpsertPulledAnnotations(ctx context.Context, annotations []store.PulledAnnotationRow) (created, updated, skipped int, err error)
	ListPulledTranscripts(ctx context.Context) ([]store.PulledTranscriptRow, error)
	GetPulledTranscript(ctx context.Context, villageHost string, id schema.TranscriptID) (*store.PulledTranscriptRow, error)
}

// Compile-time guard: *store.Store implements the PullStore contract.
var _ pullStoreContract = (*store.Store)(nil)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// makeTranscriptRow builds a valid PulledTranscriptRow from shared fixtures.
func makeTranscriptRow() store.PulledTranscriptRow {
	return store.PulledTranscriptRow{
		VillageHost:     testutil.TestVillageHost,
		TranscriptID:    testutil.TestTranscriptID,
		OwnerUserID:     testutil.TestOwnerUserID,
		OwnerUsername:   testutil.TestOwnerUsername,
		LocalSessionID:  testutil.TestSessionUUID,
		Title:           "Refactor the pipeline",
		Harness:         schema.HarnessClaudeCode,
		ProjectName:     testutil.TestProjectName,
		ContentHash:     testutil.TestContentHash,
		Visibility:      schema.VisibilityGroup,
		License:         schema.LicenseCCBY,
		PullDir:         "/pulls/" + testutil.TestVillageHost + "/" + testutil.TestTranscriptUUID,
		FirstPulledAt:   1000,
		LastPulledAt:    1000,
		AnnotationCount: 1,
	}
}

// makeAnnotationRow builds a valid foreign PulledAnnotationRow from fixtures.
func makeAnnotationRow(contentHash string) store.PulledAnnotationRow {
	return store.PulledAnnotationRow{
		VillageHost:    testutil.TestVillageHost,
		ContentHash:    contentHash,
		TranscriptID:   testutil.TestTranscriptID,
		LocalSessionID: testutil.TestSessionUUID,
		AuthorUserID:   testutil.TestAuthorUserID,
		AuthorUsername: testutil.TestAuthorUsername,
		Payload:        `{"authorUserId":"` + testutil.TestAuthorUserID + `"}`,
		PulledAt:       2000,
	}
}

// ---------------------------------------------------------------------------
// CommitPull — happy path + round-trip
// ---------------------------------------------------------------------------

func TestCommitPull_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	commit := store.PullCommit{
		Transcript:  makeTranscriptRow(),
		Annotations: []store.PulledAnnotationRow{makeAnnotationRow(testutil.TestContentHash)},
	}
	if err := s.CommitPull(ctx, commit); err != nil {
		t.Fatalf("CommitPull: %v", err)
	}

	// GetPulledTranscript round-trips all fields.
	got, err := s.GetPulledTranscript(ctx, testutil.TestVillageHost, testutil.TestTranscriptID)
	if err != nil {
		t.Fatalf("GetPulledTranscript: %v", err)
	}
	if got == nil {
		t.Fatal("GetPulledTranscript returned nil for a committed transcript")
	}
	want := makeTranscriptRow()
	if *got != want {
		t.Errorf("transcript round-trip mismatch:\n got  %+v\n want %+v", *got, want)
	}

	// ListPulledTranscripts surfaces the row.
	rows, err := s.ListPulledTranscripts(ctx)
	if err != nil {
		t.Fatalf("ListPulledTranscripts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 pulled transcript, got %d", len(rows))
	}

	// The annotation landed.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if n := queryInt(t, conn, `SELECT COUNT(*) FROM pulled_annotations WHERE content_hash = ?`, testutil.TestContentHash); n != 1 {
		t.Errorf("expected 1 pulled_annotations row, got %d", n)
	}
}

// TestCommitPull_NullableSnapshots verifies empty display/correlation fields
// persist as SQL NULL (not the empty string).
func TestCommitPull_NullableSnapshots(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	tr := makeTranscriptRow()
	tr.LocalSessionID = ""
	tr.Title = ""
	tr.Harness = ""
	tr.ProjectName = ""
	tr.License = "" // "" ⇒ NULL: a bound "" would violate the V38 CHECK
	tr.AnnotationCount = 0
	if err := s.CommitPull(ctx, store.PullCommit{Transcript: tr}); err != nil {
		t.Fatalf("CommitPull: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	nullCount := queryInt(t, conn, `SELECT COUNT(*) FROM pulled_transcripts
		WHERE local_session_id IS NULL AND title IS NULL AND harness IS NULL AND project_name IS NULL AND license_id IS NULL`)
	if nullCount != 1 {
		t.Errorf("expected nullable snapshot columns to be SQL NULL, got %d matching rows", nullCount)
	}

	// Read-back decodes NULL to "".
	got, err := s.GetPulledTranscript(ctx, testutil.TestVillageHost, testutil.TestTranscriptID)
	if err != nil {
		t.Fatalf("GetPulledTranscript: %v", err)
	}
	if got.LocalSessionID != "" || got.Title != "" || got.Harness != "" || got.ProjectName != "" || got.License != "" {
		t.Errorf("expected NULL columns to decode to empty strings, got %+v", *got)
	}
}

// TestCommitPull_Idempotent verifies a re-pull upserts: first_pulled_at is
// preserved, last_pulled_at and mutable snapshots are refreshed, and no
// duplicate row is created.
func TestCommitPull_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	first := store.PullCommit{
		Transcript:  makeTranscriptRow(),
		Annotations: []store.PulledAnnotationRow{makeAnnotationRow(testutil.TestContentHash)},
	}
	if err := s.CommitPull(ctx, first); err != nil {
		t.Fatalf("first CommitPull: %v", err)
	}

	// Re-pull: later timestamp, new title, changed license, same PK.
	second := makeTranscriptRow()
	second.LastPulledAt = 5000
	second.Title = "Updated title"
	second.License = schema.LicenseCC0
	if err := s.CommitPull(ctx, store.PullCommit{
		Transcript:  second,
		Annotations: []store.PulledAnnotationRow{makeAnnotationRow(testutil.TestContentHash)},
	}); err != nil {
		t.Fatalf("second CommitPull: %v", err)
	}

	rows, err := s.ListPulledTranscripts(ctx)
	if err != nil {
		t.Fatalf("ListPulledTranscripts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after re-pull (upsert), got %d", len(rows))
	}
	got := rows[0]
	if got.FirstPulledAt != 1000 {
		t.Errorf("first_pulled_at should be preserved (1000), got %d", got.FirstPulledAt)
	}
	if got.LastPulledAt != 5000 {
		t.Errorf("last_pulled_at should be refreshed (5000), got %d", got.LastPulledAt)
	}
	if got.Title != "Updated title" {
		t.Errorf("title should be refreshed, got %q", got.Title)
	}
	if got.License != schema.LicenseCC0 {
		t.Errorf("license should be refreshed on re-pull, got %q", got.License)
	}

	// A third pull where the village sends NO license mirrors the clear
	// (overwrite, not COALESCE): the village's audited ops path can
	// legitimately un-license, and the derived index must reflect server
	// truth rather than resurrect the stale local value.
	third := makeTranscriptRow()
	third.LastPulledAt = 6000
	third.License = ""
	if err := s.CommitPull(ctx, store.PullCommit{Transcript: third}); err != nil {
		t.Fatalf("third CommitPull: %v", err)
	}
	cleared, err := s.GetPulledTranscript(ctx, testutil.TestVillageHost, testutil.TestTranscriptID)
	if err != nil {
		t.Fatalf("GetPulledTranscript after clear: %v", err)
	}
	if cleared.License != "" {
		t.Errorf("license should be cleared to NULL when the village omits it, got %q", cleared.License)
	}

	// Annotation deduped by content hash — still 1 row.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if n := queryInt(t, conn, `SELECT COUNT(*) FROM pulled_annotations`); n != 1 {
		t.Errorf("expected 1 annotation after re-pull (content-hash dedup), got %d", n)
	}
}

// ---------------------------------------------------------------------------
// CommitPull — atomicity (failure injection leaves zero rows)
// ---------------------------------------------------------------------------

// TestCommitPull_AtomicRollback injects a mid-commit failure: the transcript
// upsert succeeds, then the annotation upsert fails (the pulled_annotations
// table is renamed out from under the transaction). The single SQLite
// transaction must roll back, leaving ZERO rows in BOTH tables — the inverted
// fail-open doctrine (a failed pull touches nothing local).
func TestCommitPull_AtomicRollback(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Poison: rename the annotations table so the annotation upsert errors
	// AFTER the transcript upsert has run inside the same transaction.
	poison := takeConn(t, s.PoolForTest())
	if err := sqlitex.ExecuteTransient(poison,
		`ALTER TABLE pulled_annotations RENAME TO pulled_annotations_poison`, nil); err != nil {
		t.Fatalf("poison rename: %v", err)
	}
	s.PoolForTest().Put(poison)

	commit := store.PullCommit{
		Transcript:  makeTranscriptRow(),
		Annotations: []store.PulledAnnotationRow{makeAnnotationRow(testutil.TestContentHash)},
	}
	err := s.CommitPull(ctx, commit)
	if err == nil {
		t.Fatal("expected CommitPull to fail when the annotation insert errors")
	}

	// Restore the table to inspect the result.
	restore := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(restore)
	if err := sqlitex.ExecuteTransient(restore,
		`ALTER TABLE pulled_annotations_poison RENAME TO pulled_annotations`, nil); err != nil {
		t.Fatalf("restore rename: %v", err)
	}

	// The transcript upsert must have been rolled back: zero rows everywhere.
	if n := queryInt(t, restore, `SELECT COUNT(*) FROM pulled_transcripts`); n != 0 {
		t.Errorf("expected 0 pulled_transcripts rows after rollback, got %d", n)
	}
	if n := queryInt(t, restore, `SELECT COUNT(*) FROM pulled_annotations`); n != 0 {
		t.Errorf("expected 0 pulled_annotations rows after rollback, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// UpsertPulledAnnotations — refresh path
// ---------------------------------------------------------------------------

func TestUpsertPulledAnnotations_CreatedUpdatedSkippedCounts(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	a1 := makeAnnotationRow(testutil.TestContentHash)
	a2 := makeAnnotationRow(testutil.TestContentHash2)

	// First batch: both created.
	created, updated, skipped, err := s.UpsertPulledAnnotations(ctx, []store.PulledAnnotationRow{a1, a2})
	if err != nil {
		t.Fatalf("UpsertPulledAnnotations (first): %v", err)
	}
	if created != 2 || updated != 0 || skipped != 0 {
		t.Errorf("first batch: got created=%d updated=%d skipped=%d, want 2/0/0", created, updated, skipped)
	}

	// Second batch covers all three outcomes:
	//   - a1 resubmitted byte-identical        ⇒ skipped (no write, no pulled_at bump)
	//   - a2 resubmitted with changed payload   ⇒ updated
	//   - a3 brand new                          ⇒ created
	a2changed := a2
	a2changed.Payload = `{"authorUserId":"` + testutil.TestAuthorUserID + `","note":"changed"}`
	a3 := makeAnnotationRow(testutil.TestContentHash3)
	created, updated, skipped, err = s.UpsertPulledAnnotations(ctx, []store.PulledAnnotationRow{a1, a2changed, a3})
	if err != nil {
		t.Fatalf("UpsertPulledAnnotations (second): %v", err)
	}
	if created != 1 || updated != 1 || skipped != 1 {
		t.Errorf("second batch: got created=%d updated=%d skipped=%d, want 1/1/1", created, updated, skipped)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if n := queryInt(t, conn, `SELECT COUNT(*) FROM pulled_annotations`); n != 3 {
		t.Errorf("expected 3 distinct annotations, got %d", n)
	}
	// The updated row's payload is the changed one; the skipped row's pulled_at
	// is unchanged from the first batch (no write happened).
	if got := queryText(t, conn,
		`SELECT payload FROM pulled_annotations WHERE content_hash = ?`, testutil.TestContentHash2); got != a2changed.Payload {
		t.Errorf("updated row payload = %q, want %q", got, a2changed.Payload)
	}
	if n := queryInt(t, conn,
		`SELECT pulled_at FROM pulled_annotations WHERE content_hash = ?`, testutil.TestContentHash); int64(n) != a1.PulledAt {
		t.Errorf("skipped row pulled_at = %d, want unchanged %d (no write)", n, a1.PulledAt)
	}
}

// TestUpsertPulledAnnotations_AtomicRollback injects a MID-BATCH failure to
// prove the single-transaction doctrine the annotation-refresh path documents
// ("failure simply leaves zero rows"). A poison TRIGGER fires RAISE(ABORT) on
// the SECOND insert (when the table already has >=1 row), so row 1 has already
// been written inside the txn when row 2 errors. A non-transactional
// implementation would leave row 1 persisted; the txn must roll BOTH back.
//
// (The table-rename poison used by CommitPull cannot exercise this — renaming
// before the call fails the FIRST existence check, so nothing is ever written
// and the zero-rows assertion is vacuous.)
func TestUpsertPulledAnnotations_AtomicRollback(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	poison := takeConn(t, s.PoolForTest())
	if err := sqlitex.ExecuteTransient(poison, `
CREATE TRIGGER poison_second_insert BEFORE INSERT ON pulled_annotations
WHEN (SELECT COUNT(*) FROM pulled_annotations) >= 1
BEGIN SELECT RAISE(ABORT, 'poison'); END`, nil); err != nil {
		t.Fatalf("create poison trigger: %v", err)
	}
	s.PoolForTest().Put(poison)

	// Two distinct rows: row 1 inserts fine; row 2 trips the trigger mid-batch.
	a1 := makeAnnotationRow(testutil.TestContentHash)
	a2 := makeAnnotationRow(testutil.TestContentHash2)
	_, _, _, err := s.UpsertPulledAnnotations(ctx, []store.PulledAnnotationRow{a1, a2})
	if err == nil {
		t.Fatal("expected UpsertPulledAnnotations to fail on the mid-batch poison trigger")
	}

	// Drop the trigger so the inspection insert/count is unobstructed.
	drop := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(drop)
	if err := sqlitex.ExecuteTransient(drop, `DROP TRIGGER poison_second_insert`, nil); err != nil {
		t.Fatalf("drop poison trigger: %v", err)
	}

	// Row 1 must have rolled back with row 2 — the whole batch is one txn.
	if n := queryInt(t, drop, `SELECT COUNT(*) FROM pulled_annotations`); n != 0 {
		t.Errorf("expected 0 pulled_annotations after mid-batch rollback, got %d", n)
	}
}

func TestUpsertPulledAnnotations_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	created, updated, skipped, err := s.UpsertPulledAnnotations(context.Background(), nil)
	if err != nil {
		t.Fatalf("UpsertPulledAnnotations(nil): %v", err)
	}
	if created != 0 || updated != 0 || skipped != 0 {
		t.Errorf("empty batch: got %d/%d/%d, want 0/0/0", created, updated, skipped)
	}
}

// ---------------------------------------------------------------------------
// Read path edge cases + multi-village isolation
// ---------------------------------------------------------------------------

func TestGetPulledTranscript_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	got, err := s.GetPulledTranscript(context.Background(),
		testutil.TestVillageHost, testutil.TestTranscriptID)
	if err != nil {
		t.Fatalf("GetPulledTranscript (missing): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for a missing transcript, got %+v", *got)
	}
}

func TestListPulledTranscripts_OrderingAndMultiVillage(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Same transcript UUID, two villages — must coexist (composite PK).
	older := makeTranscriptRow()
	older.LastPulledAt = 100
	if err := s.CommitPull(ctx, store.PullCommit{Transcript: older}); err != nil {
		t.Fatalf("CommitPull older: %v", err)
	}

	newer := makeTranscriptRow()
	newer.VillageHost = testutil.TestVillageHost2
	newer.LastPulledAt = 900
	if err := s.CommitPull(ctx, store.PullCommit{Transcript: newer}); err != nil {
		t.Fatalf("CommitPull newer: %v", err)
	}

	rows, err := s.ListPulledTranscripts(ctx)
	if err != nil {
		t.Fatalf("ListPulledTranscripts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows across two villages, got %d", len(rows))
	}
	// Newest pull (last_pulled_at DESC) first.
	if rows[0].VillageHost != testutil.TestVillageHost2 {
		t.Errorf("expected newest pull first, got host %q", rows[0].VillageHost)
	}

	// GetPulledTranscript isolates by village.
	got, err := s.GetPulledTranscript(ctx, testutil.TestVillageHost2, testutil.TestTranscriptID)
	if err != nil {
		t.Fatalf("GetPulledTranscript village2: %v", err)
	}
	if got == nil || got.VillageHost != testutil.TestVillageHost2 {
		t.Errorf("expected village2 row, got %+v", got)
	}
}
