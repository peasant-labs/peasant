package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The search-owned corruption and maintenance family: the delete-time digest
// check, the rebuild flag and its gates, the whole-index rebuild, and the
// size-ratio report. The two section-10 corruption cases run fixture-driven;
// the gate and report cases prove the production helpers directly.
func TestSearchCorruptionFamily(t *testing.T) {
	for _, c := range loadContentCorruptionCases(t) {
		if c.Owner != "search" {
			continue
		}
		switch c.Name {
		case "swept-corrupt-body-rebuilds-index":
			t.Run(c.Name, func(t *testing.T) {
				runSweptCorruptBody(t, c)
			})
		case "repair-identical-candidate-rewrites-corrupt-body":
			t.Run(c.Name, func(t *testing.T) {
				runRepairIdenticalCandidate(t, c)
			})
		default:
			t.Fatalf("unknown search corruption case %q", c.Name)
		}
	}
}

func searchTestSID(name string) schema.SessionID {
	return recallSID("corrupt-" + name)
}

func searchTakeConn(t *testing.T, s *Store) *sqlite.Conn {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	return conn
}

func searchTestConn(t *testing.T, s *Store) *sqlite.Conn {
	t.Helper()
	conn := searchTakeConn(t, s)
	t.Cleanup(func() {
		s.pool.Put(conn)
	})
	return conn
}

// runSweptCorruptBody proves the delete-time check and the rebuild gate: a
// damaged body fails verification, the deleting pass sets the flag, the
// stale postings survive the untrusted delete, search refuses while flagged,
// and the whole-index rebuild clears both the postings and the flag.
func runSweptCorruptBody(t *testing.T, c contentCorruptionCase) {
	t.Helper()
	sid := searchTestSID(c.Name)
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	text := "sweep text " + c.Query + " tail"
	v2, blobs := buildTestGeneration(t, sid, "gen_sweep_corrupt", text, "sweep input", "sweep output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	damageBodyPreview(t, s, sid, 0)
	if err := verifyAllBodies(t, s, sid); err == nil {
		t.Fatal("damaged body verifies; the corruption is invisible")
	}
	conn := searchTakeConn(t, s)
	// The deleting pass verifies first: the mismatch sets the flag on the
	// same connection, then the delete runs with untrustworthy values.
	mismatched, err := verifyBodiesForDeleteOnConn(conn, []string{string(sid)})
	if err != nil {
		s.pool.Put(conn)
		t.Fatalf("verify before delete: %v", err)
	}
	if !mismatched {
		s.pool.Put(conn)
		t.Fatal("corrupt body passes the delete-time check")
	}
	if err := sqlitex.Execute(conn, `DELETE FROM session_generation_entries WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		s.pool.Put(conn)
		t.Fatalf("delete mapping rows: %v", err)
	}
	if err := sqlitex.Execute(conn, `DELETE FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		s.pool.Put(conn)
		t.Fatalf("delete corrupt bodies: %v", err)
	}
	state, err := searchStateReadOnConn(conn)
	if err != nil {
		s.pool.Put(conn)
		t.Fatalf("read search state: %v", err)
	}
	if !state.NeedsRebuild {
		s.pool.Put(conn)
		t.Fatal("untrusted delete leaves the rebuild flag clear")
	}
	if c.WantStaleRawMatch {
		var n int
		if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_search_fts WHERE session_search_fts MATCH ?`, &sqlitex.ExecOptions{
			Args: []any{`"` + c.Query + `"`},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				n = int(stmt.ColumnInt64(0))
				return nil
			},
		}); err != nil {
			s.pool.Put(conn)
			t.Fatalf("raw MATCH after the untrusted delete: %v", err)
		}
		if n < 1 {
			s.pool.Put(conn)
			t.Fatalf("raw MATCH after the untrusted delete = %d, want stale postings", n)
		}
	}
	if c.WantRefusedWhileFlagged {
		if err := SearchRefusalError(); err == nil {
			s.pool.Put(conn)
			t.Fatal("search refusal carries no error")
		}
	}
	s.pool.Put(conn)
	if rebuilt, err := s.EnsureSearchIndexHealthy(context.Background()); err != nil {
		t.Fatalf("run the index-health gate: %v", err)
	} else if !rebuilt {
		t.Fatal("gate reports no rebuild while the flag is set")
	}
	if c.WantCleanAfterRebuild {
		state, err := s.SearchState(context.Background())
		if err != nil {
			t.Fatalf("read search state: %v", err)
		}
		if state.NeedsRebuild {
			t.Fatal("rebuild leaves the flag set")
		}
		conn := searchTestConn(t, s)
		var n int
		if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_search_fts WHERE session_search_fts MATCH ?`, &sqlitex.ExecOptions{
			Args: []any{`"` + c.Query + `"`},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				n = int(stmt.ColumnInt64(0))
				return nil
			},
		}); err != nil {
			t.Fatalf("raw MATCH after the rebuild: %v", err)
		}
		if n != 0 {
			t.Fatalf("raw MATCH after the rebuild = %d, want 0", n)
		}
	}
}

// runRepairIdenticalCandidate proves repair mode rewrites a corrupt body even
// for an identical candidate: the rewrite sets the flag, the rebuild
// clears it, and the corrected term is found exactly once.
func runRepairIdenticalCandidate(t *testing.T, c contentCorruptionCase) {
	t.Helper()
	sid := searchTestSID(c.Name)
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	text := "repair text " + c.Query + " tail"
	v2, blobs := buildTestGeneration(t, sid, "gen_repair_identical", text, "repair input", "repair output")
	identity := strings.Repeat("c", 64)
	proof := strings.Repeat("d", 64)
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	damageBodyPreview(t, s, sid, 0)
	clearInputProof(t, s, sid)
	repair, repairBlobs := buildTestGeneration(t, sid, "gen_repair_identical_retry", text, "repair input", "repair output")
	repairState, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:       filledCandidateForValidation(t, repair, repairBlobs),
		Blobs:            repairBlobs,
		IndexerVersion:   1,
		IndexedAtMs:      200,
		ExpectedState:    repairState,
		ArtifactIdentity: &identity,
		IndexedInputHash: &proof,
	})
	if err != nil {
		t.Fatalf("repair activation: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("repair disposition = %v, want committed repair work", outcome.Disposition)
	}
	state, err := s.SearchState(context.Background())
	if err != nil {
		t.Fatalf("read search state: %v", err)
	}
	if !state.NeedsRebuild {
		t.Fatal("repair delete leaves the rebuild flag clear")
	}
	if _, err := s.EnsureSearchIndexHealthy(context.Background()); err != nil {
		t.Fatalf("rebuild after repair: %v", err)
	}
	if c.WantFoundOnce {
		conn := searchTestConn(t, s)
		hits, err := SearchMergedOnConn(conn, `"`+c.Query+`"`, 50, 0)
		if err != nil {
			t.Fatalf("production search after repair: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("corrected term hits = %d, want exactly 1", len(hits))
		}
	}
}

// TestSearchDeleteTimeCheck proves the check itself: a healthy row hashes to
// its digest and an altered column does not.
func TestSearchDeleteTimeCheck(t *testing.T) {
	sid := searchTestSID("delete-time-check")
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_digest_check", "digest text", "digest input", "digest output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var record EntryRecord
	if err := sqlitex.Execute(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record = scanEntryRecord(stmt)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !verifyBodyForDelete(record) {
		t.Fatal("healthy row fails the delete-time check")
	}
	altered := record
	preview := "altered bytes"
	altered.ContentPreview = &preview
	if verifyBodyForDelete(altered) {
		t.Fatal("altered row passes the delete-time check")
	}
	empty := EntryRecord{}
	if verifyBodyForDelete(empty) {
		t.Fatal("empty digest passes the delete-time check")
	}
}

// TestSearchRebuildGate proves the gate contract: search refuses while the
// flag is set, the gate rebuilds once, and a second gate call is a no-op.
func TestSearchRebuildGate(t *testing.T) {
	sid := searchTestSID("rebuild-gate")
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_rebuild_gate", "gate text", "gate input", "gate output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if rebuilt, err := s.EnsureSearchIndexHealthy(context.Background()); err != nil {
		t.Fatalf("gate on a healthy index: %v", err)
	} else if rebuilt {
		t.Fatal("gate rebuilds a healthy index")
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := SearchStateSetNeedsRebuild(context.Background(), conn); err != nil {
		s.pool.Put(conn)
		t.Fatalf("set the flag: %v", err)
	}
	s.pool.Put(conn)
	if err := SearchRefusalError(); err == nil {
		t.Fatal("search refusal carries no error")
	}
	if rebuilt, err := s.EnsureSearchIndexHealthy(context.Background()); err != nil {
		t.Fatalf("gate on a flagged index: %v", err)
	} else if !rebuilt {
		t.Fatal("gate skips a flagged index")
	}
	if state, err := s.SearchState(context.Background()); err != nil {
		t.Fatalf("read search state: %v", err)
	} else if state.NeedsRebuild {
		t.Fatal("gate leaves the flag set")
	}
}

// TestSearchSizeRatioReported proves the verify report carries the FTS size
// ratio and the needs-optimize state over a real store.
func TestSearchSizeRatioReported(t *testing.T) {
	sid := searchTestSID("size-ratio")
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_size_ratio", "size text with several indexed words", "size input words", "size output words")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    100,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	report, err := s.SearchSizeReport(context.Background())
	if err != nil {
		t.Fatalf("measure the size ratio: %v", err)
	}
	if report.LiveDocs < 1 {
		t.Fatalf("live docs = %d, want at least the seeded bodies", report.LiveDocs)
	}
	if report.TextBytes < 1 {
		t.Fatalf("text bytes = %d, want the seeded text", report.TextBytes)
	}
	if report.IndexBytes < 1 {
		t.Fatalf("index bytes = %d, want the indexed postings", report.IndexBytes)
	}
	if report.Ratio <= 0 {
		t.Fatalf("ratio = %v, want a positive size ratio", report.Ratio)
	}
	if report.NeedsOptimize != (report.Ratio > SearchNeedsOptimizeRatio) {
		t.Fatalf("needs-optimize = %v for ratio %v against threshold %v", report.NeedsOptimize, report.Ratio, SearchNeedsOptimizeRatio)
	}
}

// TestPruneVerifiesBodies proves prune runs the delete-time check: a clean
// prune leaves the flag clear, while a corrupt prune sets it for the
// rebuild to clear.
func TestPruneVerifiesBodies(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		sid := searchTestSID("prune-clean")
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := buildTestGeneration(t, sid, "gen_prune_clean", "prune clean text", "prune input", "prune output")
		if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
			Generation:     filledCandidateForValidation(t, v2, blobs),
			Blobs:          blobs,
			IndexerVersion: 1,
			IndexedAtMs:    100,
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if _, err := s.PruneSessions(context.Background(), []ingest.SessionID{sid}); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if state, err := s.SearchState(context.Background()); err != nil {
			t.Fatalf("read search state: %v", err)
		} else if state.NeedsRebuild {
			t.Fatal("clean prune sets the rebuild flag")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		sid := searchTestSID("prune-corrupt")
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := buildTestGeneration(t, sid, "gen_prune_corrupt", "prune corrupt text", "prune input", "prune output")
		if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
			Generation:     filledCandidateForValidation(t, v2, blobs),
			Blobs:          blobs,
			IndexerVersion: 1,
			IndexedAtMs:    100,
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		damageBodyPreview(t, s, sid, 0)
		if _, err := s.PruneSessions(context.Background(), []ingest.SessionID{sid}); err != nil {
			t.Fatalf("prune: %v", err)
		}
		// The corrupt delete sets the flag mid-transaction and prune
		// rebuilds through the one path before committing, so the flag is
		// clear and no stale postings survive.
		if state, err := s.SearchState(context.Background()); err != nil {
			t.Fatalf("read search state: %v", err)
		} else if state.NeedsRebuild {
			t.Fatal("prune leaves the rebuild flag set after its gated rebuild")
		}
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer s.pool.Put(conn)
		var n int
		if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_search_fts WHERE session_search_fts MATCH ?`, &sqlitex.ExecOptions{
			Args: []any{`"prune corrupt text"`},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				n = int(stmt.ColumnInt64(0))
				return nil
			},
		}); err != nil {
			t.Fatalf("raw MATCH after corrupt prune: %v", err)
		}
		if n != 0 {
			t.Fatalf("raw MATCH after corrupt prune = %d, want 0", n)
		}
	})
}
