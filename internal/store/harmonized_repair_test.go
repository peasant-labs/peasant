package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// errBodyDigestMismatch is the test-only verification signal: a stored
// body whose canonical text no longer hashes to its digest.
var errBodyDigestMismatch = errors.New("body digest mismatch")

// damageBodyPreview corrupts one stored body column directly, the way a
// torn write or a bit flip below SQLite would: the digest column still
// names the old bytes, so the next full-read verification fails. SQL-level
// updates are refused by the immutability trigger, so the helper drops it
// for the corrupt write and recreates it byte-identical right after.
func damageBodyPreview(t *testing.T, s *Store, sid schema.SessionID, entryIndex int) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER session_entry_bodies_immutable`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_bodies SET content_preview = 'corrupt bytes' WHERE session_id = ? AND entry_index = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), entryIndex},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `CREATE TRIGGER session_entry_bodies_immutable BEFORE UPDATE ON session_entry_bodies
BEGIN
  SELECT RAISE(ABORT, 'session_entry_bodies rows are immutable; insert a new entry instead');
END`, nil); err != nil {
		t.Fatal(err)
	}
}

// verifyAllBodies recomputes every body digest of the session: a mismatch
// refuses, so the repair test proves damage before and health after.
func verifyAllBodies(t *testing.T, s *Store, sid schema.SessionID) error {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var retErr error
	if err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record := scanEntryRecord(stmt)
			if string(bodyDigestForRecord(record)) != record.BodyDigest {
				retErr = errBodyDigestMismatch
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return retErr
}

// clearInputProof marks the session for repair the way harvest verify
// --content --repair does: the artifact identity stays while the consumed
// input proof is cleared, so the repair predicate selects it.
func clearInputProof(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET artifact_hash = ?, indexed_input_hash = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{strings.Repeat("c", 64), string(sid)},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHarmonizedRepair proves repair mode bypasses the skip, inserts no new
// generation row, and rewrites the failing objects in place: a damaged body
// fails verification, the repair re-index rewrites it under the same
// digest, the search index is flagged for rebuild, and the input proof is
// restored so the predicate clears.
func TestHarmonizedRepair(t *testing.T) {
	sid, err := schema.NewSessionID("e5e5e5e5-e5e5-45e5-85e5-e5e5e5e5e5e5")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	v2, blobs := buildTestGeneration(t, sid, "gen_repair_g1", "repair text", "repair input", "repair output")
	identity := strings.Repeat("c", 64)
	proof := strings.Repeat("d", 64)
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:       filledCandidateForValidation(t, v2, blobs),
		Blobs:            blobs,
		IndexerVersion:   1,
		IndexedAtMs:      100,
		ExpectedState:    state,
		ArtifactIdentity: &identity,
		IndexedInputHash: &proof,
	}); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	if err := verifyAllBodies(t, s, sid); err != nil {
		t.Fatal("healthy bodies fail verification")
	}

	damageBodyPreview(t, s, sid, 0)
	if verifyAllBodies(t, s, sid) == nil {
		t.Fatal("damaged body verifies; the corruption is invisible")
	}
	clearInputProof(t, s, sid)

	// A repair re-index of unchanged input would compare equal and skip,
	// leaving the corrupt object in place. Repair bypasses the comparison.
	repair, repairBlobs := buildTestGeneration(t, sid, "gen_repair_g2", "repair text", "repair input", "repair output")
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
	// No new generation row: the generation, its mapping rows, and the
	// active pointer are unchanged.
	if got := visibleGeneration(t, s, sid); got != "gen_repair_g1" {
		t.Fatalf("visible = %q after repair, want the unchanged generation", got)
	}
	if got := countSessionGenerations(t, s, sid); got != 1 {
		t.Fatalf("repair wrote %d generation rows, want exactly the one", got)
	}
	if err := verifyAllBodies(t, s, sid); err != nil {
		t.Fatal("repaired bodies fail verification")
	}
	// The deleted row failed verification, so the repair flagged the
	// search index for its whole-index rebuild.
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	needsRebuild := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT needs_rebuild FROM session_search_state WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			needsRebuild = stmt.ColumnInt64(0) == 1
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !needsRebuild {
		t.Fatal("repair did not flag the search index for rebuild")
	}
	// The bookkeeping restored the input proof, clearing the predicate.
	restored, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if restored.IndexedInputHash == nil || *restored.IndexedInputHash != proof {
		t.Fatalf("repair input proof = %v, want the re-indexed input", restored.IndexedInputHash)
	}
}

// blobStoredDigest hashes one blob's bytes the way staging addresses them:
// the descriptor digest the repair gate verifies chunk bytes against.
func blobStoredDigest(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readBlobBytes concatenates one stored blob's chunks in chunk order: the
// byte string the repair gate hashes.
func readBlobBytes(t *testing.T, s *Store, sid schema.SessionID, digest string) []byte {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var out []byte
	if err := sqlitex.ExecuteTransient(conn, `SELECT data FROM session_content_chunks WHERE session_id = ? AND digest = ? ORDER BY chunk_index`, &sqlitex.ExecOptions{
		Args: []any{string(sid), digest},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnLen(0)
			chunk := make([]byte, n)
			stmt.ColumnBytes(0, chunk)
			out = append(out, chunk...)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// flipBlobChunkByte corrupts one stored chunk byte directly, the way a torn
// write or a bit flip below SQLite would: the header digest still names the
// old bytes, so only a byte-verifying reader sees the damage. The chunk
// tables carry no immutability trigger, so the corrupt write runs directly.
func flipBlobChunkByte(t *testing.T, s *Store, sid schema.SessionID, digest string) {
	t.Helper()
	data := readBlobBytes(t, s, sid, digest)
	if len(data) == 0 {
		t.Fatal("no stored chunk bytes to corrupt")
	}
	data[0] ^= 0xff
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_content_chunks SET data = ? WHERE session_id = ? AND digest = ? AND chunk_index = 0`, &sqlitex.ExecOptions{
		Args: []any{data, string(sid), digest},
	}); err != nil {
		t.Fatal(err)
	}
}

// activateRepairCandidate activates one candidate generation over the
// session's current index state, returning its disposition. Every repair
// round re-reads the state first: each activation advances the stamps the
// compare-and-swap guards.
func activateRepairCandidate(t *testing.T, s *Store, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte, identity, proof string) ingest.ActivationOutcome {
	t.Helper()
	state, err := s.ReadIndexState(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:       filledCandidateForValidation(t, v2, blobs),
		Blobs:            blobs,
		IndexerVersion:   1,
		IndexedAtMs:      200,
		ExpectedState:    state,
		ArtifactIdentity: &identity,
		IndexedInputHash: &proof,
	})
	if err != nil {
		t.Fatalf("repair-round activation: %v", err)
	}
	return outcome
}

// TestHarmonizedRepairRewritesByteFlippedBlob proves the repair gate
// verifies blob bytes, not just blob structure. A verify-marked session
// whose descriptor blob has one flipped chunk byte takes the repair path
// on an unchanged-input re-index — rewriting the blob in place with no new
// generation row — while the healthy control, predicate set and bytes
// intact, still skips.
func TestHarmonizedRepairRewritesByteFlippedBlob(t *testing.T) {
	sid, err := schema.NewSessionID("f5f5f5f5-f5f5-45f5-85f5-f5f5f5f5f5f5")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	retained := schema.SourceEntryRef("e_retained_repair")
	blobBytes := []byte("retained repair blob bytes")
	v2, blobs := buildTestGeneration(t, sid, "gen_repair_blob_g1", "repair blob text", "repair blob input", "repair blob output")
	v2.Generation.Segments = []indexformat.ContextSegment{{
		Ordinal:          0,
		PhysicalSourceID: "repair-source",
		Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
		Inclusion:        indexformat.SegmentInclusionInherited,
		CapturedRefs:     []schema.SourceEntryRef{retained},
	}}
	v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: retained})
	blobs[retained] = blobBytes
	digest := blobStoredDigest(t, blobBytes)
	identity := strings.Repeat("c", 64)
	proof := strings.Repeat("d", 64)
	if outcome := activateRepairCandidate(t, s, sid, v2, blobs, identity, proof); outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("base activation disposition = %v, want the committed generation", outcome.Disposition)
	}
	if got := blobStoredDigest(t, readBlobBytes(t, s, sid, digest)); got != digest {
		t.Fatal("stored blob bytes do not verify right after the commit")
	}
	// Healthy control: the predicate is set but every object verifies, so
	// the unchanged-input re-index skips instead of repairing.
	clearInputProof(t, s, sid)
	refreshed, _ := buildTestGeneration(t, sid, "gen_repair_blob_g2", "repair blob text", "repair blob input", "repair blob output")
	refreshed.Generation.Segments = v2.Generation.Segments
	refreshed.Generation.Content = v2.Generation.Content
	if outcome := activateRepairCandidate(t, s, sid, refreshed, blobs, identity, proof); outcome.Disposition != ingest.ActivationSkipped {
		t.Fatalf("healthy control disposition = %v, want the unchanged-input skip", outcome.Disposition)
	}
	if got := countSessionGenerations(t, s, sid); got != 1 {
		t.Fatalf("control wrote %d generation rows, want exactly the one", got)
	}
	// Damage: one flipped chunk byte under an intact header and chunk
	// count. Structure still verifies; only bytes fail.
	flipBlobChunkByte(t, s, sid, digest)
	if got := blobStoredDigest(t, readBlobBytes(t, s, sid, digest)); got == digest {
		t.Fatal("flipped blob still verifies; the corruption is invisible")
	}
	// Verify-marked re-index of the same content: the gate sees failing
	// bytes and takes the repair path, rewriting the blob in place.
	clearInputProof(t, s, sid)
	repaired, _ := buildTestGeneration(t, sid, "gen_repair_blob_g3", "repair blob text", "repair blob input", "repair blob output")
	repaired.Generation.Segments = v2.Generation.Segments
	repaired.Generation.Content = v2.Generation.Content
	if outcome := activateRepairCandidate(t, s, sid, repaired, blobs, identity, proof); outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("byte-flip repair disposition = %v, want committed repair work", outcome.Disposition)
	}
	if got := visibleGeneration(t, s, sid); got != "gen_repair_blob_g1" {
		t.Fatalf("visible = %q after blob repair, want the unchanged generation", got)
	}
	if got := countSessionGenerations(t, s, sid); got != 1 {
		t.Fatalf("blob repair wrote %d generation rows, want exactly the one", got)
	}
	if got := blobStoredDigest(t, readBlobBytes(t, s, sid, digest)); got != digest {
		t.Fatal("repaired blob bytes still fail verification")
	}
}
