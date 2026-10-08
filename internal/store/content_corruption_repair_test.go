package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestContentCorruptionRepair runs the repair-owned content_corruption
// cases under their section-10 names: verify lists the session, repair
// marks it, the repair activation rewrites the failing objects in place
// with no new generation row, and the session verifies clean with the
// search index rebuilt.
func TestContentCorruptionRepair(t *testing.T) {
	t.Parallel()
	for _, c := range loadContentCorruptionCases(t) {
		if c.Damage == "" {
			continue
		}
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			runCorruptionRepairCase(t, c)
		})
	}
}

// runCorruptionRepairCase seeds one harmonized session through the
// production activation, applies the case damage, and walks the
// verify-mark-repair-rebuild path.
func runCorruptionRepairCase(t *testing.T, c contentCorruptionCase) {
	t.Helper()
	ctx := context.Background()
	sid, err := schema.NewSessionID("d3d3d3d3-d3d3-43d3-83d3-d3d3d3d3d3d3")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_repair_case", "repair durable text", "repair durable input", "repair durable output")
	if c.Damage == "blob-bytes" || c.Damage == "descriptor-digest" {
		inherited := schema.SourceEntryRef("e_repair_inherited")
		v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: inherited})
		blobs[inherited] = []byte(strings.Repeat("inherited repair bytes ", 200))
		v2.Generation.Segments = []indexformat.ContextSegment{{
			Ordinal: 0, PhysicalSourceID: "source-repair-inherited",
			Coordinates:  indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
			Inclusion:    indexformat.SegmentInclusionInherited,
			CapturedRefs: []schema.SourceEntryRef{inherited},
		}}
	}
	filled := filledCandidateForValidation(t, v2, blobs)
	identity := strings.Repeat("c", 64)
	proof := strings.Repeat("d", 64)
	state, err := s.ReadIndexState(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation:       filled,
		Blobs:            blobs,
		IndexerVersion:   1,
		IndexedAtMs:      100,
		ExpectedState:    state,
		ArtifactIdentity: &identity,
		IndexedInputHash: &proof,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := verifyAllBodies(t, s, sid); err != nil {
		t.Fatal("healthy bodies fail verification")
	}
	digest := applyCorruptionDamage(t, s, sid, c.Damage)
	if report, err := s.VerifyContent(ctx, false); err != nil {
		t.Fatalf("verify before repair: %v", err)
	} else if len(report.Damaged) != 1 || report.Damaged[0].SessionID != sid {
		t.Fatalf("verify damage = %+v; want exactly this session", report.Damaged)
	}
	if report, err := s.VerifyContent(ctx, true); err != nil {
		t.Fatalf("verify with repair: %v", err)
	} else if len(report.Repaired) != 1 {
		t.Fatalf("repaired = %d; want 1", len(report.Repaired))
	}
	if hash := corruptionInputProof(t, s, sid); hash != nil {
		t.Fatalf("repair left input proof %q; want it cleared for the repair activation", *hash)
	}
	repairState, err := s.ReadIndexState(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	// The repair candidate carries a fresh identifier over identical
	// content: the immutable-identity check treats a same-identifier
	// retry as already committed, while a fresh identifier over
	// unchanged bytes reaches the repair predicate, which bypasses the
	// skip and rewrites the failing objects with no new generation row.
	repairV2 := v2
	repairV2.Generation.ID += "-retry"
	repairFilled := filledCandidateForValidation(t, repairV2, blobs)
	outcome, err := s.ActivateGeneration(ctx, GenerationActivation{
		Generation:       repairFilled,
		Blobs:            blobs,
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
	// No new generation row: the generation, its mapping rows, and the
	// active pointer are unchanged.
	if got := visibleGeneration(t, s, sid); got != "gen_repair_case" {
		t.Fatalf("visible = %q after repair, want the unchanged generation", got)
	}
	if got := countSessionGenerations(t, s, sid); got != 1 {
		t.Fatalf("repair wrote %d generation rows, want exactly the one", got)
	}
	if err := verifyAllBodies(t, s, sid); err != nil {
		t.Fatal("repaired bodies fail verification")
	}
	if c.Damage == "blob-bytes" {
		if got := readBlobBytes(t, s, sid, digest); string(got) != string(blobs[schema.SourceEntryRef("e_repair_inherited")]) {
			t.Fatal("repaired blob bytes differ from the candidate bytes")
		}
	}
	if report, err := s.VerifyContent(ctx, false); err != nil {
		t.Fatalf("verify after repair: %v", err)
	} else if len(report.Damaged) != 0 {
		t.Fatalf("damage after repair = %+v; want none", report.Damaged)
	}
	if rebuilt, err := s.EnsureSearchIndexHealthy(ctx); err != nil {
		t.Fatalf("rebuild after repair: %v", err)
	} else if !rebuilt {
		t.Fatal("repair did not flag the search index for rebuild")
	}
	assertRepairMatch(t, s, "durable")
}

// corruptionInheritedDigest reads the inherited descriptor's digest.
func corruptionInheritedDigest(t *testing.T, s *Store, sid schema.SessionID) string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var digest string
	if err := sqlitex.ExecuteTransient(conn, `SELECT digest FROM session_generation_content WHERE session_id = ? AND source_entry_ref = 'e_repair_inherited'`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digest = stmt.ColumnText(0)
			return nil
		},
	}); err != nil || digest == "" {
		t.Fatalf("find inherited descriptor: %v", err)
	}
	return digest
}

// applyCorruptionDamage corrupts one stored object the way a torn write
// or a bit flip below SQLite would, returning the affected digest for
// the blob cases.
func applyCorruptionDamage(t *testing.T, s *Store, sid schema.SessionID, damage string) string {
	t.Helper()
	switch damage {
	case "body-column":
		damageBodyPreview(t, s, sid, 0)
		return ""
	case "blob-bytes":
		digest := corruptionInheritedDigest(t, s, sid)
		flipBlobChunkByte(t, s, sid, digest)
		return digest
	case "descriptor-digest":
		// Leaving a dangling descriptor needs enforcement off: deferral
		// would only move the check to the corrupting commit. The
		// pragma restores on the way out, even on failure.
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer s.pool.Put(conn)
		exec := func(script string) {
			t.Helper()
			if err := sqlitex.ExecuteTransient(conn, script, nil); err != nil {
				t.Fatalf("corrupt descriptor: %v", err)
			}
		}
		exec(`PRAGMA foreign_keys=OFF`)
		defer exec(`PRAGMA foreign_keys=ON`)
		if err := sqlitex.ExecuteTransient(conn, `UPDATE session_generation_content SET digest = '`+strings.Repeat("b", 64)+`' WHERE session_id = ? AND source_entry_ref = 'e_repair_inherited'`, &sqlitex.ExecOptions{
			Args: []any{string(sid)},
		}); err != nil {
			t.Fatalf("corrupt descriptor: %v", err)
		}
		return ""
	default:
		t.Fatalf("unknown corruption damage %q", damage)
		return ""
	}
}

// corruptionInputProof reads the session's consumed-input proof.
func corruptionInputProof(t *testing.T, s *Store, sid schema.SessionID) *string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var proof *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT indexed_input_hash FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				hash := stmt.ColumnText(0)
				proof = &hash
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return proof
}

// assertRepairMatch proves the rebuilt index serves the repaired term
// through the production search path.
func assertRepairMatch(t *testing.T, s *Store, term string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_search_fts WHERE session_search_fts MATCH ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{term},
		ResultFunc: func(*sqlite.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("match after repair rebuild: %v", err)
	}
	if !found {
		t.Fatalf("rebuilt index misses term %q", term)
	}
}
