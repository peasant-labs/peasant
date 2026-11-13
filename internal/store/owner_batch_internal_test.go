package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	sqlite "github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestClaimArtifactsBatchesAndResumes proves a claim-pointer update runs in
// bounded transactions: an interruption after the first batch keeps the claim
// entry and that batch, and the next automatic claim reuses the entry and
// finishes the remaining rows.
func TestClaimArtifactsBatchesAndResumes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "claim-batch.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()

	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('batch-host', 'batch-host');
INSERT INTO projects(project_hash) VALUES('batch-project');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('batch-a', 'claude-code', 'fixture-model', 'batch-host', 'batch-project', 1, 2, 3, '/synthetic/a.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('batch-b', 'claude-code', 'fixture-model', 'batch-host', 'batch-project', 1, 2, 3, '/synthetic/b.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('batch-c', 'claude-code', 'fixture-model', 'batch-host', 'batch-project', 1, 2, 3, '/synthetic/c.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('batch-d', 'claude-code', 'fixture-model', 'batch-host', 'batch-project', 1, 2, 3, '/synthetic/d.jsonl', 'jsonl');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('batch-e', 'claude-code', 'fixture-model', 'batch-host', 'batch-project', 1, 2, 3, '/synthetic/e.jsonl', 'jsonl');
`, nil); err != nil {
		s.pool.Put(conn)
		t.Fatal(err)
	}
	s.pool.Put(conn)

	home := t.TempDir()
	creds := auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     "11111111-1111-4111-8111-111111111111",
		Username:   "alice",
		VillageURL: "https://village.example/team",
	}
	if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	snapshot, err := auth.CaptureCredentials(ctx, home)
	if err != nil {
		t.Fatal(err)
	}

	s.claimBatchSize = 2
	interrupted := false
	s.claimBatchSeam = func(table string, batch, changed int) error {
		if table == "sessions" && !interrupted && changed > 0 {
			interrupted = true
			return errors.New("simulated interruption")
		}
		return nil
	}
	result, err := s.ClaimArtifacts(ctx, snapshot, false, ClaimSourceAutomatic, 2000)
	if err == nil {
		t.Fatal("the interrupted claim succeeded, want the seam to stop it")
	}
	if !interrupted || result.ClaimID != "" {
		t.Fatalf("interrupted claim = %+v, noticed=%v; want a failed partial claim", result, interrupted)
	}
	claims, err := s.OwnerClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims after interruption = %+v, want the one appended entry", claims)
	}
	claimedAfterInterruption := claimCountForTest(t, s)
	if claimedAfterInterruption == 0 || claimedAfterInterruption >= 5 {
		t.Fatalf("claimed rows after interruption = %d, want a partial first batch", claimedAfterInterruption)
	}

	s.claimBatchSeam = nil
	resumed, err := s.ClaimArtifacts(ctx, snapshot, false, ClaimSourceAutomatic, 3000)
	if err != nil {
		t.Fatalf("resume claim: %v", err)
	}
	if resumed.ClaimID != claims[0].ID {
		t.Fatalf("resumed claim id = %q, want the reused entry %q", resumed.ClaimID, claims[0].ID)
	}
	claims, err = s.OwnerClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims after resume = %+v, want the automatic claim to reuse its entry", claims)
	}
	if got := claimCountForTest(t, s); got != 5 {
		t.Fatalf("claimed rows after resume = %d, want 5", got)
	}
	if got := claimPointerCountForTest(t, s, claims[0].ID); got != 5 {
		t.Fatalf("rows pointing at the reused entry = %d, want 5", got)
	}
}

func claimCountForTest(t *testing.T, s *Store) int64 {
	t.Helper()
	return claimScalarForTest(t, s, `SELECT COUNT(owner_claim_id) FROM sessions`)
}

func claimPointerCountForTest(t *testing.T, s *Store, claimID string) int64 {
	t.Helper()
	return claimScalarForTest(t, s, `SELECT COUNT(*) FROM sessions WHERE owner_claim_id = '`+claimID+`'`)
}

func claimScalarForTest(t *testing.T, s *Store, query string) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { value = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

// TestClaimArtifactsLargeStoreBatchesWithinWriterHold measures a default claim
// on the review's synthetic row counts. Each pointer transaction must fit the
// writer hold; the claim as a whole may span many batches by design.
//
// Set PEASANT_LARGE_CLAIM_TEST=1 to run it (the seed takes a few seconds).
func TestClaimArtifactsLargeStoreBatchesWithinWriterHold(t *testing.T) {
	if testing.Short() || os.Getenv("PEASANT_LARGE_CLAIM_TEST") != "1" {
		t.Skip("set PEASANT_LARGE_CLAIM_TEST=1 to measure the large-store claim")
	}
	const sessions = 20616
	const annotations = 467365
	dbPath := filepath.Join(t.TempDir(), "claim-large.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('large-host', 'large-host');
INSERT INTO projects(project_hash) VALUES('large-project');
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < `+claimItoa(sessions)+`)
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
SELECT 'large-session-' || n, 'claude-code', 'fixture-model', 'large-host', 'large-project', n, n, n, '/synthetic/large-' || n || '.jsonl', 'jsonl' FROM seq;
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < `+claimItoa(annotations)+`)
INSERT INTO annotations(id, target_kind_id, annotation_type_id, annotator_id, value, created_at)
SELECT 'large-ann-' || n,
       (SELECT id FROM target_kinds WHERE name = 'session'),
       (SELECT id FROM annotation_types LIMIT 1),
       (SELECT id FROM annotators WHERE name = 'human-web'),
       'large', n
FROM seq;
`, nil); err != nil {
		s.pool.Put(conn)
		t.Fatal(err)
	}
	s.pool.Put(conn)

	home := t.TempDir()
	creds := auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     "11111111-1111-4111-8111-111111111111",
		Username:   "alice",
		VillageURL: "https://village.example/team",
	}
	if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	snapshot, err := auth.CaptureCredentials(ctx, home)
	if err != nil {
		t.Fatal(err)
	}

	var maxBatch time.Duration
	last := time.Now()
	s.claimBatchSeam = func(_ string, _ int, _ int) error {
		if elapsed := time.Since(last); elapsed > maxBatch {
			maxBatch = elapsed
		}
		last = time.Now()
		return nil
	}
	start := time.Now()
	result, err := s.ClaimArtifacts(ctx, snapshot, false, ClaimSourceAutomatic, 2000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != sessions+annotations {
		t.Fatalf("claimed = %d, want %d", result.Claimed, sessions+annotations)
	}
	hold := s.writeConfig.WithDefaults(1).HoldTarget
	t.Logf("large claim: sessions=%d annotations=%d claimed=%d elapsed=%s maxBatch=%s hold=%s margin=%s",
		sessions, annotations, result.Claimed, elapsed, maxBatch, hold, hold-maxBatch)
	if maxBatch > hold {
		t.Fatalf("slowest pointer batch %s exceeds the writer hold %s", maxBatch, hold)
	}
}

func claimItoa(n int) string { return strconv.Itoa(n) }
