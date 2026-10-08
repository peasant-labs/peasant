package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The content verification and repair marking (design §5): `harvest
// verify --content` lists the harmonized sessions whose stored objects
// fail self-verification and reports the search index health with its
// size ratio; `--repair` clears the consumed-input proof of each damaged
// session, so the repair predicate selects it and its next activation
// runs in repair mode (§4.6): it bypasses the identical-refresh skip,
// inserts no new generation row, and rewrites the failing entry rows,
// blobs, and descriptors in one repair transaction — explicit DELETE plus
// re-INSERT under the same digest with PRAGMA defer_foreign_keys=ON,
// never INSERT OR REPLACE — setting needs_rebuild, which the rebuild
// clears. Preview and routing reads do not verify (those views are
// non-authoritative); the next full read of a damaged session refuses.

// ContentDamage names one session failing content verification with the
// first refusing object: a body whose canonical text no longer hashes to
// its digest, a blob whose bytes no longer verify, a descriptor with no
// blob, or a capture proof the shim entries no longer match.
type ContentDamage struct {
	SessionID schema.SessionID
	Object    string
	Reason    string
}

// ContentVerifyReport is the `harvest verify --content` report: the
// sessions checked, the damaged ones, the search index health with its
// size ratio, and — with --repair — the sessions marked for the repair
// activation.
type ContentVerifyReport struct {
	SessionsChecked int64
	Damaged         []ContentDamage
	NeedsRebuild    bool
	Size            SearchSizeReport
	Repaired        []schema.SessionID
	IndexRebuilt    bool
}

// VerifyContent checks every harmonized session's mapped objects with
// the same byte proof a full read demands — body digests over the
// canonical text, blob bytes hashed in chunk order against the
// descriptor digest with the summed lengths against the header length,
// and the capture proof over the shim entries — and reports the search
// index health with its size ratio. With repair it clears the
// consumed-input proof of each damaged session (the ordinary marking the
// drain and the migration rollbacks share) and rebuilds the search index
// when the rebuild flag is set. Verify never rewrites content itself:
// healing runs through the repair activation on the next harvest.
func (s *Store) VerifyContent(ctx context.Context, repair bool) (ContentVerifyReport, error) {
	var report ContentVerifyReport
	sessions, err := s.migrateHarmonizedSessions(ctx)
	if err != nil {
		return report, err
	}
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("store: content verification interrupted before session %s: %w; checked sessions keep their report and the next run resumes", sessionID, err)
		}
		report.SessionsChecked++
		damage, err := s.verifyContentSession(ctx, sessionID)
		if err != nil {
			return report, err
		}
		if damage != nil {
			report.Damaged = append(report.Damaged, *damage)
		}
	}
	state, err := s.SearchState(ctx)
	if err != nil {
		return report, err
	}
	report.NeedsRebuild = state.NeedsRebuild
	size, err := s.SearchSizeReport(ctx)
	if err != nil {
		return report, err
	}
	report.Size = size
	if !repair {
		return report, nil
	}
	for _, damage := range report.Damaged {
		if err := s.clearIndexedInputHash(ctx, damage.SessionID); err != nil {
			return report, err
		}
		report.Repaired = append(report.Repaired, damage.SessionID)
	}
	if report.NeedsRebuild {
		rebuilt, err := s.EnsureSearchIndexHealthy(ctx)
		if err != nil {
			return report, err
		}
		report.IndexRebuilt = rebuilt
		if rebuilt {
			report.NeedsRebuild = false
		}
	}
	return report, nil
}

// verifyContentSession verifies one harmonized session's active
// generation: every mapped body recomputes its digest, every descriptor
// resolves to bytes that verify, and the capture proof matches the shim
// entries. It names the first refusing object; a healthy session
// reports no damage.
func (s *Store) verifyContentSession(ctx context.Context, sessionID schema.SessionID) (*ContentDamage, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to verify session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	active, harmonized, err := harmonizedActiveOnConn(conn, sessionID)
	if err != nil {
		return nil, err
	}
	if !harmonized {
		return nil, nil
	}
	if damage := verifyContentBodiesOnConn(conn, sessionID, active); damage != nil {
		return damage, nil
	}
	if damage := verifyContentBlobsOnConn(conn, sessionID, active); damage != nil {
		return damage, nil
	}
	capture, found, err := readCapture(conn, ingest.SessionID(sessionID))
	if err != nil {
		return nil, fmt.Errorf("store: read the capture row while verifying session %s: %w", sessionID, err)
	}
	if found && capture.FullCaptureSHA256 != "" && string(capture.SessionID) == string(sessionID) {
		if _, err := verifyShimCaptureProjection(ctx, conn, ingest.SessionID(sessionID), capture); err != nil {
			return &ContentDamage{SessionID: sessionID, Object: "capture-proof", Reason: fmt.Sprintf("the shim entries no longer match the capture proof: %v", err)}, nil
		}
	}
	return nil, nil
}

// verifyContentBodiesOnConn recomputes every mapped body's canonical
// digest against its stored anchor: a column altered under its digest
// refuses here, the way a full read refuses.
func verifyContentBodiesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, active string) *ContentDamage {
	var digests []string
	_ = sqlitex.ExecuteTransient(conn, `SELECT body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), active},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	})
	for _, digest := range digests {
		found := false
		failing := false
		_ = sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				record := scanEntryRecord(stmt)
				if string(bodyDigestForRecord(record)) != record.BodyDigest {
					failing = true
				}
				return nil
			},
		})
		if !found {
			return &ContentDamage{SessionID: sessionID, Object: "body:" + digest, Reason: "the mapped body row is missing; the mapping references a body the store does not hold"}
		}
		if failing {
			return &ContentDamage{SessionID: sessionID, Object: "body:" + digest, Reason: "the body row's canonical text no longer hashes to its stored digest; a column changed under the digest"}
		}
	}
	return nil
}

// verifyContentBlobsOnConn verifies every descriptor's blob the way the
// repair gate does: the header exists, the chunk bytes hashed in order
// match the descriptor digest, and the summed lengths match the header
// length. A flipped chunk byte, a wrong header length, or a torn chunk
// row all refuse here.
func verifyContentBlobsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, active string) *ContentDamage {
	var refs []struct {
		ref    string
		digest string
	}
	_ = sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref, digest FROM session_generation_content WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), active},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			refs = append(refs, struct {
				ref    string
				digest string
			}{stmt.ColumnText(0), stmt.ColumnText(1)})
			return nil
		},
	})
	for _, descriptor := range refs {
		var byteLength int64
		header := false
		_ = sqlitex.ExecuteTransient(conn, `SELECT byte_length FROM session_content WHERE session_id = ? AND digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), descriptor.digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				header = true
				byteLength = stmt.ColumnInt64(0)
				return nil
			},
		})
		if !header {
			return &ContentDamage{SessionID: sessionID, Object: "blob:" + descriptor.digest, Reason: fmt.Sprintf("descriptor %q names a blob header the store does not hold", descriptor.ref)}
		}
		hasher := sha256.New()
		var total, chunks int64
		contiguous := true
		_ = sqlitex.ExecuteTransient(conn, `SELECT chunk_index, data FROM session_content_chunks WHERE session_id = ? AND digest = ? ORDER BY chunk_index`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), descriptor.digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnInt64(0) != chunks {
					contiguous = false
				}
				n := stmt.ColumnLen(1)
				data := make([]byte, n)
				stmt.ColumnBytes(1, data)
				hasher.Write(data)
				total += int64(n)
				chunks++
				return nil
			},
		})
		wantChunks := (byteLength + 65535) / 65536
		if !contiguous || chunks != wantChunks || total != byteLength {
			return &ContentDamage{SessionID: sessionID, Object: "blob:" + descriptor.digest, Reason: fmt.Sprintf("descriptor %q has torn chunks (%d of %d, %d of %d bytes)", descriptor.ref, chunks, wantChunks, total, byteLength)}
		}
		if hex.EncodeToString(hasher.Sum(nil)) != descriptor.digest {
			return &ContentDamage{SessionID: sessionID, Object: "blob:" + descriptor.digest, Reason: fmt.Sprintf("descriptor %q has bytes that no longer hash to its digest", descriptor.ref)}
		}
	}
	return nil
}
