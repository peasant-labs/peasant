package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The per-session migration conversion (design §7.2 Phase 2): the legacy
// oracle read, the structured row writes with the guarded stats upsert,
// the one catalog transaction with the in-transaction shadow verify before
// any delete, the old-row deletes, the directory removal, the sweep, and
// the flag clear.
//
// The oracle readers below are the Release-N-only legacy readers (§7.4):
// the pre-change snapshot reader over the old tables and blob files plus
// the pre-change full reader over the mirror and chunks. They live in this
// package (used by the migration and the parity tests) and never dispatch
// to the harmonized path, so the shadow verify can never degenerate into
// comparing the new representation with itself. Release N+1 removes them;
// frozen golden corpora keep the parity evidence (§10).

// migrateOracleEntry is one legacy projection entry: the parsed struct the
// conversion encodes plus the exact old entry_json bytes the serialization
// dimension compares against.
type migrateOracleEntry struct {
	entry schema.SessionEntry
	raw   string
}

// migrateOracle is the Phase 2a legacy read of one file-backed native
// session: every old catalog/entry/content row, every verified blob file,
// the mirror, the native full-content rows (through the mirror pager),
// the metadata document with its captured stats, the stored hashes, the
// durable parent, and the prior evidence. It is read under the session's
// exclusive lock before the catalog transaction, so no other writer can
// interleave between the oracle read and the commit.
type migrateOracle struct {
	sessionID            schema.SessionID
	generationID         string
	metadata             schema.UnifiedMetadata
	metadataRaw          string
	titleRefs            []schema.SourceEntryRef
	completeness         indexformat.GenerationCompleteness
	sourceEvidenceDigest string
	installedAtMs        int64
	activatedAtMs        *int64
	entries              map[int][]migrateOracleEntry
	sections             []migrateOracleSection
	content              []indexformat.ContentRecord
	blobs                map[schema.SourceEntryRef][]byte
	aliases              []indexformat.NativeAlias
	nativeMetadata       map[int][]schema.NativeMetadataRecord
	segments             []indexformat.ContextSegment
	mirror               []schema.SessionEntry
	full                 []schema.SessionEntry
	fullAvailable        bool
	capture              ingest.SessionContentCapture
	captureFound         bool
	sessionEntriesHash   *string
	parentID             *string
	priorEvidence        []byte
	// statsSeedDoc is the captured stats document: the harness's own JSON
	// over the capture's measurements, which the Phase 2c upsert writes
	// to the seed home and the metadata dimension consumes as its operand.
	statsSeedDoc string
	// statsCaptureTime stamps the Phase 2c upsert: the generation's
	// activated_at_ms, falling back to installed_at_ms (the v62 backfill
	// rule). A pre-existing newer row survives the age gate, is never
	// clobbered, and the conversion proceeds.
	statsCaptureTime int64
}

type migrateOracleSection struct {
	partitionID int
	state       string
}

// MigrateDataRollbackError reports one Phase 2d data rollback: the
// session, the shadow-verify dimension (or the conversion step) that
// refused, and why. It is a disposition, not a failure: the catalog
// transaction rolled back, the input proof is cleared so the next harvest
// re-indexes the session, and the driver records it and continues. Any
// other error from MigrateSession halts the run.
type MigrateDataRollbackError struct {
	SessionID schema.SessionID
	Dimension string
	Reason    string
}

func (e *MigrateDataRollbackError) Error() string {
	return fmt.Sprintf("store: migration rolled back session %s at %s: %s; the old representation is intact, the session is marked for re-index, and the run continues", e.SessionID, e.Dimension, e.Reason)
}

// MigrateSession converts one file-backed native session under its
// exclusive lock (design §7.2 Phase 2). Sessions with no conversion work
// — already converted, non-native, or a settled refusal — report skipped
// without touching anything. A data mismatch reports rolled_back with a
// *MigrateDataRollbackError carrying the dimension; a defect mismatch (or
// any other failure) returns an error that halts the run.
func (s *Store) MigrateSession(ctx context.Context, sessionID schema.SessionID) (MigrateOutcome, error) {
	if err := s.requireGenerationSupport(); err != nil {
		return "", err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("store: lock session %s for migration: %w; nothing was converted", sessionID, err)
	}
	defer func() { _ = release() }()
	work, err := s.migrateSessionWork(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if !work.convert {
		return MigrateOutcomeSkipped, nil
	}
	if err := s.setSweepFlag(ctx, sessionID); err != nil {
		return "", err
	}
	oracle, err := s.readMigrateOracle(ctx, sessionID, work.generationID)
	if err != nil {
		return s.migrateDataRollback(ctx, sessionID, "oracle-read", err.Error())
	}
	if err := s.checkMigrateOracleConverges(ctx, oracle); err != nil {
		return s.migrateDataRollback(ctx, sessionID, "relationships-diverged", err.Error())
	}
	if err := checkMigrateEmittedIntegrity(oracle); err != nil {
		return s.migrateDataRollback(ctx, sessionID, "field-blob-integrity", err.Error())
	}
	prepared, err := prepareHarmonizedCandidate(sessionID, oracleGeneration(oracle), oracle.blobs)
	if err != nil {
		return s.migrateDataRollback(ctx, sessionID, "prepare", err.Error())
	}
	if err := s.stageMigrateObjects(ctx, prepared); err != nil {
		return "", fmt.Errorf("store: migration staging of session %s failed: %w; staged sub-transactions stand committed under the set flag, the old representation is intact, and the re-run resumes", sessionID, err)
	}
	if err := s.upsertMigrateStats(ctx, oracle); err != nil {
		return "", fmt.Errorf("store: migration stats upsert of session %s failed: %w; staged rows stand committed under the set flag, the old representation is intact, and the re-run resumes", sessionID, err)
	}
	converted, rollbackErr, err := s.commitMigrateSession(ctx, oracle, prepared)
	if err != nil {
		return "", err
	}
	if rollbackErr != nil {
		return s.migrateDataRollback(ctx, sessionID, rollbackErr.Dimension, rollbackErr.Reason)
	}
	if !converted {
		return "", fmt.Errorf("store: migration of session %s committed nothing and verified nothing; the session keeps its state; re-run to retry", sessionID)
	}
	if err := s.deleteConvertedMirrorRows(ctx, sessionID); err != nil {
		return "", fmt.Errorf("store: migration of session %s converted but the post-commit mirror delete failed: %w; the harmonized generation is active and the next pass deletes the leftovers", sessionID, err)
	}
	if s.generationArtifacts != nil {
		if _, err := s.generationArtifacts.RemoveConvertedSessionFiles(ctx, sessionID); err != nil {
			return "", fmt.Errorf("store: migration of session %s converted but the owned files remain: %w; the database proves conversion and the next pass retries the removal", sessionID, err)
		}
	}
	if _, err := s.sweepSessionLocked(ctx, sessionID); err != nil {
		return "", fmt.Errorf("store: migration of session %s converted but the closing sweep failed: %w; the flag stays set and the next pass resumes the sweep", sessionID, err)
	}
	return MigrateOutcomeConverted, nil
}

// migrateSessionWork reports whether a session has conversion work and
// which file-backed generation to convert. Converted sessions (harmonized
// active), non-native sessions (no generation-backed rows), and settled
// refusals (a capture failure code this build cannot lift) carry no work:
// conversion never touches their capture rows, their failure code, or
// their input proof.
func (s *Store) migrateSessionWork(ctx context.Context, sessionID schema.SessionID) (struct {
	convert      bool
	generationID string
}, error) {
	var work struct {
		convert      bool
		generationID string
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return work, fmt.Errorf("store: take connection to inspect session %s for migration: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return work, err
	}
	if active == nil || *active == "" {
		return work, nil
	}
	harmonizedActive, harmonized, err := harmonizedActiveOnConn(conn, sessionID)
	if err != nil {
		return work, err
	}
	_ = harmonizedActive
	if harmonized {
		return work, nil
	}
	fileBacked := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_projection_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), *active},
		ResultFunc: func(*sqlite.Stmt) error {
			fileBacked = true
			return nil
		},
	}); err != nil {
		return work, fmt.Errorf("store: locate the file-backed generation for session %s: %w", sessionID, err)
	}
	if !fileBacked {
		return work, nil
	}
	settled := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_content_captures WHERE session_id = ? AND failure_code IS NOT NULL LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(*sqlite.Stmt) error {
			settled = true
			return nil
		},
	}); err != nil {
		return work, fmt.Errorf("store: check the capture state of session %s: %w", sessionID, err)
	}
	if settled {
		return work, nil
	}
	work.convert = true
	work.generationID = *active
	return work, nil
}

// setSweepFlag marks the session before its body sub-transactions commit
// orphan bodies under it (design §7.2 Phase 2 step 0). The flag is the
// crash marker the sweep and the re-run resume from.
func (s *Store) setSweepFlag(ctx context.Context, sessionID schema.SessionID) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to flag session %s for migration: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: flag session %s for migration: %w", sessionID, err)
	}
	return nil
}

// migrateDataRollback clears the consumed-input proof in a new transaction
// (so the repair predicate re-selects the session and the next harvest
// re-indexes it) and reports the rolled_back disposition with the
// refusing dimension. Its staged rows stay flagged and are swept.
func (s *Store) migrateDataRollback(ctx context.Context, sessionID schema.SessionID, dimension, reason string) (MigrateOutcome, error) {
	if err := s.clearIndexedInputHash(ctx, sessionID); err != nil {
		return "", fmt.Errorf("store: roll back session %s at %s (%s) but the re-index mark failed: %w; the old representation is intact", sessionID, dimension, reason, err)
	}
	return MigrateOutcomeRolledBack, &MigrateDataRollbackError{SessionID: sessionID, Dimension: dimension, Reason: reason}
}

// readMigrateOracle reads one session through the legacy oracle readers
// (design §7.2 Phase 2a): the old catalog/entry/content rows, every blob
// file with its length and digest verified, the mirror, the native
// full-content rows through the mirror pager, the metadata document with
// its captured stats, the stored hashes, the durable parent, and the
// prior evidence. A session the legacy readers themselves cannot read is
// a data rollback: the old representation is damaged, and re-indexing
// from the retained transcripts is the fix.
func (s *Store) readMigrateOracle(ctx context.Context, sessionID schema.SessionID, generationID string) (*migrateOracle, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take connection for the oracle read: %w", err)
	}
	defer s.pool.Put(conn)
	oracle := &migrateOracle{
		sessionID:      sessionID,
		generationID:   generationID,
		entries:        map[int][]migrateOracleEntry{},
		blobs:          map[schema.SourceEntryRef][]byte{},
		nativeMetadata: map[int][]schema.NativeMetadataRecord{},
	}
	if err := readMigrateOracleCatalogOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleEntriesOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleSectionsOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleContentOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleAliasesOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleSegmentsOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleSessionOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleMirrorOnConn(ctx, conn, oracle); err != nil {
		return nil, err
	}
	if err := readMigrateOracleNativeMetadataOnConn(conn, oracle); err != nil {
		return nil, err
	}
	if err := s.readMigrateOracleBlobsOnConn(ctx, conn, oracle); err != nil {
		return nil, err
	}
	return oracle, nil
}

// readMigrateOracleCatalogOnConn reads the old generation row: the
// metadata and title documents, the completeness, the evidence digest,
// and the install stamps. The stats seed document is the harness's own
// JSON over the capture's measurements; the capture time follows the v62
// backfill rule (activated, else installed).
func readMigrateOracleCatalogOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_json, title_refs_json, completeness, source_evidence_digest, installed_at_ms, activated_at_ms FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			oracle.metadataRaw = stmt.ColumnText(0)
			if err := json.Unmarshal([]byte(oracle.metadataRaw), &oracle.metadata); err != nil {
				return fmt.Errorf("decode generation metadata: %w", err)
			}
			titleRaw := stmt.ColumnText(1)
			if err := json.Unmarshal([]byte(titleRaw), &oracle.titleRefs); err != nil {
				return fmt.Errorf("decode title refs: %w", err)
			}
			completeness, err := indexformat.NewGenerationCompleteness(stmt.ColumnText(2))
			if err != nil {
				return err
			}
			oracle.completeness = completeness
			oracle.sourceEvidenceDigest = stmt.ColumnText(3)
			oracle.installedAtMs = stmt.ColumnInt64(4)
			if stmt.ColumnType(5) != sqlite.TypeNull {
				at := stmt.ColumnInt64(5)
				oracle.activatedAtMs = &at
			}
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old generation row: %w", err)
	}
	if !found {
		return fmt.Errorf("no old generation row exists; the session was converted or drained by a concurrent pass")
	}
	seed, err := json.Marshal(oracle.metadata.Stats)
	if err != nil {
		return fmt.Errorf("encode the captured stats document: %w", err)
	}
	oracle.statsSeedDoc = string(seed)
	oracle.statsCaptureTime = oracle.installedAtMs
	if oracle.activatedAtMs != nil {
		oracle.statsCaptureTime = *oracle.activatedAtMs
	}
	return nil
}

// readMigrateOracleEntriesOnConn reads every old entry row in
// (partition, index) order, keeping the exact entry_json bytes beside
// the parsed struct.
func readMigrateOracleEntriesOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			partitionID := stmt.ColumnInt(0)
			raw := stmt.ColumnText(1)
			var entry schema.SessionEntry
			if err := json.Unmarshal([]byte(raw), &entry); err != nil {
				return fmt.Errorf("decode entry of partition %d: %w", partitionID, err)
			}
			oracle.entries[partitionID] = append(oracle.entries[partitionID], migrateOracleEntry{entry: entry, raw: raw})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old entry rows: %w", err)
	}
	if len(oracle.entries[0]) == 0 {
		return fmt.Errorf("the old generation holds no main entry; the session cannot be converted")
	}
	return nil
}

// readMigrateOracleSectionsOnConn reads the old partition sections in
// partition order.
func readMigrateOracleSectionsOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			oracle.sections = append(oracle.sections, migrateOracleSection{partitionID: stmt.ColumnInt(0), state: stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old partition sections: %w", err)
	}
	return nil
}

// readMigrateOracleContentOnConn reads the old content records in ref
// order.
func readMigrateOracleContentOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref, relative_blob, byte_length, integrity_digest FROM session_projection_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("decode content ref: %w", err)
			}
			oracle.content = append(oracle.content, indexformat.ContentRecord{
				Ref:          ref,
				RelativeBlob: stmt.ColumnText(1),
				ByteLength:   stmt.ColumnInt64(2),
				Digest:       stmt.ColumnText(3),
			})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old content records: %w", err)
	}
	return nil
}

// readMigrateOracleAliasesOnConn reads the old native aliases in key
// order.
func readMigrateOracleAliasesOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(1))
			if err != nil {
				return fmt.Errorf("decode alias ref: %w", err)
			}
			oracle.aliases = append(oracle.aliases, indexformat.NativeAlias{NativeKey: stmt.ColumnText(0), Ref: ref})
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old native aliases: %w", err)
	}
	return nil
}

// readMigrateOracleSegmentsOnConn carries the old context segments with
// their captured refs into the candidate. Sessions without segments
// convert without them.
func readMigrateOracleSegmentsOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	type rawSegment struct {
		ordinal   int
		logical   *schema.SessionID
		physical  string
		kind      indexformat.CoordinateKind
		start     *int64
		end       *int64
		decStart  *int64
		decEnd    *int64
		inclusion indexformat.SegmentInclusion
	}
	var raws []rawSegment
	if err := sqlitex.ExecuteTransient(conn, `SELECT segment_ordinal, logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion FROM session_context_segments WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			raw := rawSegment{ordinal: stmt.ColumnInt(0), physical: stmt.ColumnText(2)}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				logical, err := schema.NewSessionID(stmt.ColumnText(1))
				if err != nil {
					return fmt.Errorf("decode segment logical session: %w", err)
				}
				raw.logical = &logical
			}
			raw.kind = indexformat.CoordinateKind(stmt.ColumnText(3))
			if stmt.ColumnType(4) != sqlite.TypeNull {
				start := stmt.ColumnInt64(4)
				raw.start = &start
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				end := stmt.ColumnInt64(5)
				raw.end = &end
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				decStart := stmt.ColumnInt64(6)
				raw.decStart = &decStart
			}
			if stmt.ColumnType(7) != sqlite.TypeNull {
				decEnd := stmt.ColumnInt64(7)
				raw.decEnd = &decEnd
			}
			raw.inclusion = indexformat.SegmentInclusion(stmt.ColumnText(8))
			raws = append(raws, raw)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the old context segments: %w", err)
	}
	refsBySegment := map[int][]schema.SourceEntryRef{}
	if len(raws) > 0 {
		if err := sqlitex.ExecuteTransient(conn, `SELECT segment_ordinal, source_entry_ref FROM session_context_segment_refs WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal, ordinal`, &sqlitex.ExecOptions{
			Args: []any{string(oracle.sessionID), oracle.generationID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ref, err := schema.NewSourceEntryRef(stmt.ColumnText(1))
				if err != nil {
					return fmt.Errorf("decode segment ref: %w", err)
				}
				ordinal := stmt.ColumnInt(0)
				refsBySegment[ordinal] = append(refsBySegment[ordinal], ref)
				return nil
			},
		}); err != nil {
			return fmt.Errorf("read the old segment refs: %w", err)
		}
	}
	for _, raw := range raws {
		oracle.segments = append(oracle.segments, indexformat.ContextSegment{
			Ordinal:          raw.ordinal,
			LogicalSessionID: raw.logical,
			PhysicalSourceID: raw.physical,
			Coordinates: indexformat.SegmentCoordinates{
				Kind:                    raw.kind,
				Start:                   raw.start,
				EndExclusive:            raw.end,
				DecodedByteStart:        raw.decStart,
				DecodedByteEndExclusive: raw.decEnd,
			},
			Inclusion:    raw.inclusion,
			CapturedRefs: refsBySegment[raw.ordinal],
		})
	}
	return nil
}

// readMigrateOracleSessionOnConn reads the session-level oracle inputs:
// the stored entries hash (NULL skips its dimension), the durable
// parent the metadata compare restores beside the catalog mapping, and
// the capture row with its full-capture proof.
func readMigrateOracleSessionOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_entries_hash, parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				hash := stmt.ColumnText(0)
				oracle.sessionEntriesHash = &hash
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				parent := stmt.ColumnText(1)
				oracle.parentID = &parent
			}
			return nil
		},
	}); err != nil {
		return fmt.Errorf("read the session row: %w", err)
	}
	capture, found, err := readCapture(conn, ingest.SessionID(oracle.sessionID))
	if err != nil {
		return fmt.Errorf("read the capture row: %w", err)
	}
	oracle.capture = capture
	oracle.captureFound = found
	return nil
}

// readMigrateOracleMirrorOnConn reads the mirror through the pre-change
// mirror branch (the legacy rows with the ext merge) and, for a full
// capture, the full entries through the mirror pager. Both readers query
// the legacy tables directly and never dispatch to the shim.
func readMigrateOracleMirrorOnConn(ctx context.Context, conn *sqlite.Conn, oracle *migrateOracle) error {
	entries, err := readOracleMirrorEntries(conn, oracle.sessionID)
	if err != nil {
		return err
	}
	oracle.mirror = entries
	if oracle.captureFound && oracle.capture.FullCaptureSHA256 != "" && string(oracle.capture.SessionID) == string(oracle.sessionID) {
		full, err := readOracleFullEntries(ctx, conn, oracle.sessionID)
		if err != nil {
			return err
		}
		oracle.full = full
		oracle.fullAvailable = true
	}
	return nil
}

// readMigrateOracleNativeMetadataOnConn reads the structured native
// metadata per partition: the reshape backfilled every file-backed
// generation, so the legacy and harmonized readers share these rows.
func readMigrateOracleNativeMetadataOnConn(conn *sqlite.Conn, oracle *migrateOracle) error {
	partitions := map[int]bool{0: true}
	for _, section := range oracle.sections {
		partitions[section.partitionID] = true
	}
	for partition := range partitions {
		records, err := harmonizedNativeMetadataOnConn(conn, oracle.sessionID, oracle.generationID, partition)
		if err != nil {
			return fmt.Errorf("read native metadata of partition %d: %w", partition, err)
		}
		oracle.nativeMetadata[partition] = records
	}
	return nil
}

// readMigrateOracleBlobsOnConn reads every content blob through the
// artifact store, which verifies each file's length and integrity
// digest. A missing file or a damaged file is a data rollback, never a
// halt: the old representation itself is damaged.
func (s *Store) readMigrateOracleBlobsOnConn(ctx context.Context, _ *sqlite.Conn, oracle *migrateOracle) error {
	if s.generationArtifacts == nil {
		return fmt.Errorf("managed generation support is not configured; blob files cannot be read")
	}
	for _, record := range oracle.content {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("interrupted while reading blob files: %w", err)
		}
		payload, err := s.generationArtifacts.ReadBlob(ctx, oracle.sessionID, oracle.generationID, record)
		if err != nil {
			return fmt.Errorf("read blob file for ref %q: %w", record.Ref, err)
		}
		oracle.blobs[record.Ref] = payload
	}
	prior, err := s.readMigrateOraclePrior(ctx, oracle)
	if err != nil {
		return err
	}
	oracle.priorEvidence = prior
	return nil
}

// readMigrateOraclePrior reads the persisted prior document, or nothing
// when none was written.
func (s *Store) readMigrateOraclePrior(ctx context.Context, oracle *migrateOracle) ([]byte, error) {
	if s.generationArtifacts == nil {
		return nil, fmt.Errorf("managed generation support is not configured; prior evidence cannot be read")
	}
	prior, err := s.generationArtifacts.ReadPriorEvidence(ctx, oracle.sessionID, oracle.generationID)
	if err != nil {
		return nil, fmt.Errorf("read prior evidence: %w", err)
	}
	return prior, nil
}

// oracleGeneration rebuilds the managed candidate from the oracle read:
// the parsed entries per partition with their native metadata, the
// content records with their verified bytes, the aliases, the segments,
// and the captured metadata. The prepare phase validates the whole
// candidate before anything is staged.
func oracleGeneration(oracle *migrateOracle) indexformat.Generation {
	mainEntries := make([]schema.SessionEntry, 0, len(oracle.entries[0]))
	for _, item := range oracle.entries[0] {
		mainEntries = append(mainEntries, item.entry)
	}
	generation := indexformat.Generation{
		ID:                   oracle.generationID,
		Completeness:         oracle.completeness,
		Metadata:             oracle.metadata,
		Main:                 indexformat.Partition{Entries: mainEntries, NativeMetadata: oracle.nativeMetadata[0]},
		Content:              append([]indexformat.ContentRecord(nil), oracle.content...),
		Aliases:              append([]indexformat.NativeAlias(nil), oracle.aliases...),
		Segments:             append([]indexformat.ContextSegment(nil), oracle.segments...),
		SourceEvidenceDigest: oracle.sourceEvidenceDigest,
		TitleRefs:            append([]schema.SourceEntryRef(nil), oracle.titleRefs...),
	}
	for _, section := range oracle.sections {
		if section.partitionID == 0 {
			continue
		}
		state, err := schema.NewEarlierHistoryState(section.state)
		if err != nil {
			state = ""
		}
		partitionEntries := make([]schema.SessionEntry, 0, len(oracle.entries[section.partitionID]))
		for _, item := range oracle.entries[section.partitionID] {
			partitionEntries = append(partitionEntries, item.entry)
		}
		generation.Earlier = append(generation.Earlier, indexformat.EarlierPartition{
			State:   state,
			Content: indexformat.Partition{Entries: partitionEntries, NativeMetadata: oracle.nativeMetadata[section.partitionID]},
		})
	}
	return generation
}

// checkMigrateEmittedIntegrity enforces the Phase 2b content rule: the
// sha256 of an emitted ref's entry field must equal the record's
// integrity digest (the field is the projectionEntryContent selection; a
// nil field hashes the empty string). Otherwise the entry and its blob
// disagree and no descriptor is ever written for an emitted ref: a data
// rollback.
func checkMigrateEmittedIntegrity(oracle *migrateOracle) error {
	emitted := map[schema.SourceEntryRef]schema.SessionEntry{}
	for _, items := range oracle.entries {
		for _, item := range items {
			if item.entry.SourceEntryRef != "" {
				emitted[item.entry.SourceEntryRef] = item.entry
			}
		}
	}
	for _, record := range oracle.content {
		entry, ok := emitted[record.Ref]
		if !ok {
			continue
		}
		field := harmonizedContentField(entry)
		sum := sha256.Sum256([]byte(field))
		if !equalDigests(hex.EncodeToString(sum[:]), record.Digest) {
			return fmt.Errorf("content record %q names an emitted entry whose field hashes differently than the integrity digest; the entry and its blob disagree", record.Ref)
		}
	}
	return nil
}

// readOracleMirrorEntries reads the mirror the way the pre-change
// ListEntries did: the mirror rows with the ext merge, without any shim
// dispatch.
func readOracleMirrorEntries(conn *sqlite.Conn, sessionID schema.SessionID) ([]schema.SessionEntry, error) {
	var entries []schema.SessionEntry
	if err := sqlitex.ExecuteTransient(conn, sqlListEntries, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entry := scanSessionEntry(stmt)
			if _, _, err := ingest.DecodePiEntryExtra(entry); err != nil {
				return err
			}
			entries = append(entries, entry)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("read the oracle mirror rows: %w", err)
	}
	extMap := make(map[int]map[string]any)
	if err := sqlitex.ExecuteTransient(conn, sqlListEntriesExt, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			idx := stmt.ColumnInt(0)
			key := stmt.ColumnText(1)
			if extMap[idx] == nil {
				extMap[idx] = make(map[string]any)
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnText(2)
			} else if stmt.ColumnType(3) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnInt(3)
			} else if stmt.ColumnType(4) != sqlite.TypeNull {
				extMap[idx][key] = stmt.ColumnFloat(4)
			}
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("read the oracle mirror ext rows: %w", err)
	}
	for i := range entries {
		extKVs, ok := extMap[entries[i].EntryIndex]
		if !ok || len(extKVs) == 0 {
			continue
		}
		if err := mergeExtIntoExtra(&entries[i], extKVs); err != nil {
			return nil, fmt.Errorf("merge the oracle mirror ext rows: %w", err)
		}
	}
	return entries, nil
}

// readOracleFullEntries pages the whole mirror with its chunks through
// the pre-change mirror pager, which reads the legacy tables directly
// and never dispatches to the shim.
func readOracleFullEntries(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID) ([]schema.SessionEntry, error) {
	var entries []schema.SessionEntry
	for from := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := readContentPageOnConn(ctx, conn, ingest.SessionID(sessionID), ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent, FromIndex: from, Limit: 100})
		if err != nil {
			return nil, fmt.Errorf("read the oracle full entries: %w", err)
		}
		entries = append(entries, page.Entries...)
		if page.NextIndex == nil {
			return entries, nil
		}
		if len(page.Entries) == 0 || *page.NextIndex <= from {
			return nil, fmt.Errorf("the oracle full content cursor did not advance")
		}
		from = *page.NextIndex
	}
}

// stageMigrateObjects writes the prepared bodies and blobs in bounded
// sub-transactions under the configured write budget (design §0.5): a
// crash inside one sub-transaction rolls only it back, earlier ones stand
// committed as orphans under the set flag, and the re-run resumes
// idempotently through ON CONFLICT DO NOTHING. The sweep removes the
// orphans if the session later rolls back.
func (s *Store) stageMigrateObjects(ctx context.Context, prepared *preparedHarmonized) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take connection to stage blobs: %w", err)
	}
	// No defer here: the blob transaction commits and the connection
	// returns before the body sub-transactions take their own, so the
	// blob write lock is never held across the body writes. Every exit
	// below ends the transaction and returns the connection explicitly.
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	for _, blob := range prepared.blobs {
		if err := ctx.Err(); err != nil {
			txnErr = err
			endFn(&txnErr)
			s.pool.Put(conn)
			return txnErr
		}
		blobCopy := blob
		if err := insertStagedBlobOnConn(conn, prepared.sessionID, &blobCopy); err != nil {
			txnErr = err
			endFn(&txnErr)
			s.pool.Put(conn)
			return txnErr
		}
	}
	endFn(&txnErr)
	s.pool.Put(conn)
	if txnErr != nil {
		return txnErr
	}
	cfg := s.writeConfigForLane()
	flush := func(batch []int) error {
		conn, err := s.pool.Take(ctx)
		if err != nil {
			return fmt.Errorf("take connection to stage bodies: %w", err)
		}
		defer s.pool.Put(conn)
		txnErr := error(nil)
		endFn := sqlitex.Transaction(conn)
		defer endFn(&txnErr)
		for _, index := range batch {
			if err := ctx.Err(); err != nil {
				txnErr = err
				return txnErr
			}
			record := prepared.bodies[index]
			if err := insertStagedBodyOnConn(conn, prepared.sessionID, &record); err != nil {
				txnErr = err
				return txnErr
			}
		}
		return nil
	}
	var pending []int
	var pendingBytes int64
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := flush(pending); err != nil {
			return err
		}
		pending = nil
		pendingBytes = 0
		return reportMigrateStageSeam("between-body-subtxns")
	}
	for index := range prepared.bodies {
		size := int64(len(serializeEntry(prepared.bodies[index])))
		if len(pending) > 0 && ingest.ExceedsWriteBudget(cfg, len(pending), pendingBytes, size) {
			if err := flushPending(); err != nil {
				return fmt.Errorf("store: migration staging of session %s interrupted: %w; staged sub-transactions stand committed under the set flag and the re-run resumes", prepared.sessionID, err)
			}
		}
		pending = append(pending, index)
		pendingBytes += size
	}
	if err := flushPending(); err != nil {
		return fmt.Errorf("store: migration staging of session %s interrupted: %w; staged sub-transactions stand committed under the set flag and the re-run resumes", prepared.sessionID, err)
	}
	return nil
}

// upsertMigrateStats runs the Phase 2c guarded stats write (source
// harness, the captured stats, seed_json as the captured stats document,
// capture time as the stamp) inside its own transaction. A pre-existing
// newer row survives the age gate, is never clobbered, and the
// conversion proceeds; the shadow verify consumes the captured document,
// never the live row.
func (s *Store) upsertMigrateStats(ctx context.Context, oracle *migrateOracle) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take connection for the stats upsert: %w", err)
	}
	defer s.pool.Put(conn)
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	stats := capturedStatsForHarnessWrite(oracle.sessionID, oracle.metadata.Stats, oracle.statsSeedDoc, oracle.statsCaptureTime)
	if _, err := upsertCapturedStatsOnConn(conn, stats); err != nil {
		txnErr = fmt.Errorf("upsert the captured stats: %w", err)
		return txnErr
	}
	return nil
}

// migrateShadowMismatch is one refusing shadow-verify dimension: its
// name, whether it is a reader defect (halts) or a data mismatch (rolls
// back), and why.
type migrateShadowMismatch struct {
	Dimension string
	Defect    bool
	Reason    string
}

// commitMigrateSession runs the one catalog transaction (design §7.2
// Phase 2c–2e): delete the old shared-table rows the new catalog
// reuses, insert the harmonized generation, shadow-verify the new rows
// against the oracle before any old row is deleted, then delete the old
// projection rows and commit. A data mismatch rolls the transaction back
// and reports the dimension; a defect mismatch rolls back and halts the
// run naming the session and the dimension.
func (s *Store) commitMigrateSession(ctx context.Context, oracle *migrateOracle, prepared *preparedHarmonized) (bool, *migrateShadowMismatch, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("take connection for the catalog transaction: %w", err)
	}
	defer s.pool.Put(conn)
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	if err := deleteMigrateSharedOldRows(conn, oracle); err != nil {
		txnErr = err
		return false, nil, txnErr
	}
	if err := insertHarmonizedGenerationOnConn(conn, prepared, oracle.priorEvidence, oracle.installedAtMs); err != nil {
		txnErr = fmt.Errorf("install the harmonized generation: %w", err)
		return false, nil, txnErr
	}
	if err := reportMigrateShadowSeam(conn, "before-shadow-verify"); err != nil {
		txnErr = fmt.Errorf("interrupted at the shadow-verify seam: %w; the transaction rolled back and the old representation is intact", err)
		return false, nil, txnErr
	}
	if mismatch := verifyMigrateShadow(conn, oracle, prepared); mismatch != nil {
		txnErr = fmt.Errorf("shadow verify refused: %s", mismatch.Reason)
		if mismatch.Defect {
			return false, nil, fmt.Errorf("session %s: defect mismatch at %s: %s; the catalog transaction rolled back and the run halts", oracle.sessionID, mismatch.Dimension, mismatch.Reason)
		}
		return false, mismatch, nil
	}
	if err := deleteMigrateProjectionOldRows(conn, oracle); err != nil {
		txnErr = err
		return false, nil, txnErr
	}
	if err := ctx.Err(); err != nil {
		txnErr = fmt.Errorf("interrupted before the catalog commit: %w; the transaction rolled back and the old representation is intact", err)
		return false, nil, txnErr
	}
	return true, nil, nil
}

// migrateSharedOldTables are the tables the harmonized catalog shares
// with the old representation: the old child rows are deleted before the
// insert (same keys), while the old entry/content documents stay until
// after the shadow verify.
var migrateSharedOldTables = []string{
	"session_projection_aliases",
	"session_projection_sections",
	"session_section_native_metadata",
	"session_context_segments",
	"session_context_segment_refs",
	"session_relationship_evidence",
	"session_generation_subagents",
	"session_generation_commits",
	"session_generation_associations",
	"session_generation_diagnostics",
	"session_generation_title_refs",
	"session_generation_entries",
	"session_generation_content",
}

// deleteMigrateSharedOldRows deletes the old child rows the new catalog
// reuses under the same keys. The old entry/content documents and the
// mirror stay: the shadow verify reads them.
func deleteMigrateSharedOldRows(conn *sqlite.Conn, oracle *migrateOracle) error {
	for _, table := range migrateSharedOldTables {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(oracle.sessionID), oracle.generationID},
		}); err != nil {
			return fmt.Errorf("delete old %s rows: %w", table, err)
		}
	}
	return nil
}

// deleteMigrateProjectionOldRows deletes the old generation row with its
// entry and content rows after the shadow verify passed.
func deleteMigrateProjectionOldRows(conn *sqlite.Conn, oracle *migrateOracle) error {
	for _, table := range []string{"session_projection_entries", "session_projection_content", "session_projection_generations"} {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(oracle.sessionID), oracle.generationID},
		}); err != nil {
			return fmt.Errorf("delete old %s rows: %w", table, err)
		}
	}
	return nil
}

// verifyMigrateShadow runs the seven shadow-verify dimensions inside the
// catalog transaction, before any delete (design §7.2 Phase 2d): the new
// readers forced to the harmonized path against the legacy oracle
// readers. A data mismatch rolls back; a defect mismatch halts.
func verifyMigrateShadow(conn *sqlite.Conn, oracle *migrateOracle, prepared *preparedHarmonized) *migrateShadowMismatch {
	checks := []func(*sqlite.Conn, *migrateOracle, *preparedHarmonized) *migrateShadowMismatch{
		verifyMigrateEntrySerialization,
		verifyMigrateBoundedShape,
		verifyMigrateFullShape,
		verifyMigrateMetadataDocument,
		verifyMigrateSessionHash,
		verifyMigrateCaptureHash,
		verifyMigrateCarriedChildren,
		verifyMigrateDetailChildren,
		verifyMigrateDetailBytes,
	}
	for _, check := range checks {
		if mismatch := check(conn, oracle, prepared); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

// readMigrateStagedBodies reads the staged body rows back through the
// inserted mapping rows in (partition, index) order, aligned with the
// oracle's entry order. The mapping — not the oracle digest — locates
// each row, so a serialization drift still reports the differing bytes
// instead of a missing row.
func readMigrateStagedBodies(conn *sqlite.Conn, oracle *migrateOracle) ([]EntryRecord, []int, error) {
	records := make([]EntryRecord, 0, len(oracle.entries[0]))
	partitions := make([]int, 0, len(oracle.entries[0]))
	type mapped struct {
		partition int
		index     int
		digest    string
	}
	var mapping []mapped
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, entry_index, body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			mapping = append(mapping, mapped{partition: stmt.ColumnInt(0), index: stmt.ColumnInt(1), digest: stmt.ColumnText(2)})
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("read the staged mapping rows: %w", err)
	}
	want := 0
	for _, partition := range orderedOraclePartitions(oracle) {
		want += len(oracle.entries[partition])
	}
	if len(mapping) != want {
		return nil, nil, fmt.Errorf("the staged mapping holds %d rows but the oracle carried %d entries", len(mapping), want)
	}
	position := 0
	for _, partition := range orderedOraclePartitions(oracle) {
		for _, item := range oracle.entries[partition] {
			mapped := mapping[position]
			if mapped.partition != partition || mapped.index != item.entry.EntryIndex {
				return nil, nil, fmt.Errorf("the staged mapping names partition %d entry %d at position %d, want partition %d entry %d", mapped.partition, mapped.index, position, partition, item.entry.EntryIndex)
			}
			found := false
			err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
				Args: []any{string(oracle.sessionID), mapped.digest},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					records = append(records, scanEntryRecord(stmt))
					partitions = append(partitions, partition)
					found = true
					return nil
				},
			})
			if err != nil {
				return nil, nil, fmt.Errorf("read the staged body: %w", err)
			}
			if !found {
				return nil, nil, fmt.Errorf("the staged body for partition %d entry %d is missing", partition, item.entry.EntryIndex)
			}
			position++
		}
	}
	return records, partitions, nil
}

// verifyMigrateEntrySerialization checks serializeEntry(row) against the
// old entry_json, byte for byte, for every row.
func verifyMigrateEntrySerialization(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	records, _, err := readMigrateStagedBodies(conn, oracle)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "entry-serialization", Reason: err.Error()}
	}
	index := 0
	for _, partition := range orderedOraclePartitions(oracle) {
		for _, item := range oracle.entries[partition] {
			if got := string(serializeEntry(records[index])); got != item.raw {
				return &migrateShadowMismatch{Dimension: "entry-serialization", Reason: fmt.Sprintf("partition %d entry %d serializes differently than its old entry_json", partition, item.entry.EntryIndex)}
			}
			index++
		}
	}
	return nil
}

func orderedOraclePartitions(oracle *migrateOracle) []int {
	ordered := []int{0}
	for partition := range oracle.entries {
		if partition == 0 {
			continue
		}
		ordered = append(ordered, partition)
	}
	return ordered
}

// migrateBoundedShapes shapes the staged main-partition rows the way the
// routed readers shape them, with the mirror's own bound: full captures
// bound previews, preview-only captures read unbounded, exactly as the
// mirror stored them.
func migrateBoundedShapes(records []EntryRecord, full bool) ([]schema.SessionEntry, []schema.SessionEntry) {
	bounded := make([]schema.SessionEntry, 0, len(records))
	fullShape := make([]schema.SessionEntry, 0, len(records))
	for _, record := range records {
		bounded = append(bounded, legacyShape(record, full))
		fullShape = append(fullShape, legacyShape(record, false))
	}
	return bounded, fullShape
}

// verifyMigrateBoundedShape checks legacyShape(bounded) against the mirror
// rows (ListEntries with the ext merge), entry for entry.
func verifyMigrateBoundedShape(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	records, partitions, err := readMigrateStagedBodies(conn, oracle)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "bounded-shape", Reason: err.Error()}
	}
	var main []EntryRecord
	for i, partition := range partitions {
		if partition == 0 {
			main = append(main, records[i])
		}
	}
	full := oracle.captureFound && oracle.capture.CaptureFormat == ingest.ContentCaptureFormatFull
	bounded, _ := migrateBoundedShapes(main, full)
	if len(bounded) != len(oracle.mirror) {
		return &migrateShadowMismatch{Dimension: "bounded-shape", Reason: fmt.Sprintf("the shim shapes %d main entries but the mirror holds %d", len(bounded), len(oracle.mirror))}
	}
	for i := range bounded {
		want, err := json.Marshal(oracle.mirror[i])
		if err != nil {
			return &migrateShadowMismatch{Dimension: "bounded-shape", Reason: fmt.Sprintf("encode oracle mirror entry %d: %v", i, err)}
		}
		got, err := json.Marshal(bounded[i])
		if err != nil {
			return &migrateShadowMismatch{Dimension: "bounded-shape", Reason: fmt.Sprintf("encode shim entry %d: %v", i, err)}
		}
		if string(got) != string(want) {
			return &migrateShadowMismatch{Dimension: "bounded-shape", Reason: fmt.Sprintf("shim entry %d shapes differently than its mirror row", oracle.mirror[i].EntryIndex)}
		}
	}
	return nil
}

// verifyMigrateFullShape checks legacyShape(full) against the legacy full
// reader over the mirror and chunks. Preview-only captures have no full
// proof, so the dimension is skipped for them. The source ref and
// provenance have no mirror home, so they are zeroed on the shim side
// before the compare: their parity is proven by the serialization
// dimension, which covers every column.
func verifyMigrateFullShape(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	if !oracle.fullAvailable {
		return nil
	}
	records, partitions, err := readMigrateStagedBodies(conn, oracle)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "full-shape", Reason: err.Error()}
	}
	var main []EntryRecord
	for i, partition := range partitions {
		if partition == 0 {
			main = append(main, records[i])
		}
	}
	_, fullShape := migrateBoundedShapes(main, true)
	for i := range fullShape {
		fullShape[i].SourceEntryRef = ""
		fullShape[i].Provenance = nil
	}
	if len(fullShape) != len(oracle.full) {
		return &migrateShadowMismatch{Dimension: "full-shape", Reason: fmt.Sprintf("the shim shapes %d full entries but the legacy full reader holds %d", len(fullShape), len(oracle.full))}
	}
	for i := range fullShape {
		want, err := json.Marshal(oracle.full[i])
		if err != nil {
			return &migrateShadowMismatch{Dimension: "full-shape", Reason: fmt.Sprintf("encode oracle full entry %d: %v", i, err)}
		}
		got, err := json.Marshal(fullShape[i])
		if err != nil {
			return &migrateShadowMismatch{Dimension: "full-shape", Reason: fmt.Sprintf("encode shim full entry %d: %v", i, err)}
		}
		if string(got) != string(want) {
			return &migrateShadowMismatch{Dimension: "full-shape", Reason: fmt.Sprintf("shim full entry %d shapes differently than the legacy full reader", oracle.full[i].EntryIndex)}
		}
	}
	return nil
}

// verifyMigrateMetadataDocument checks serializeMetadata(rows plus the
// captured stats) against the old metadata_json. The stats operand is
// the captured document from the oracle read, never the live stats row
// (a newer row, e.g. a COMPUTE derived update, is expected and does not
// fail this dimension). The rebuilt document's metadataHash is recomputed
// stats-inclusive; the durable parent restores beside the catalog
// mapping, the way the snapshot reader restores it.
func verifyMigrateMetadataDocument(conn *sqlite.Conn, oracle *migrateOracle, prepared *preparedHarmonized) *migrateShadowMismatch {
	_ = conn
	stats := capturedStatsForHarnessWrite(oracle.sessionID, oracle.metadata.Stats, oracle.statsSeedDoc, oracle.statsCaptureTime)
	rebuilt := serializeMetadata(prepared.record, prepared.children, stats)
	var document schema.UnifiedMetadata
	if err := json.Unmarshal(rebuilt, &document); err != nil {
		return &migrateShadowMismatch{Dimension: "metadata-document", Reason: fmt.Sprintf("decode the rebuilt metadata document: %v", err)}
	}
	if oracle.parentID != nil {
		parent, err := schema.NewSessionID(*oracle.parentID)
		if err != nil {
			return &migrateShadowMismatch{Dimension: "metadata-document", Reason: fmt.Sprintf("the durable parent identity is malformed: %v", err)}
		}
		document.ParentUUID = &parent
	}
	document.MetadataHash = schema.ComputeMetadataHash(&document)
	encoded, err := json.Marshal(document)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "metadata-document", Reason: fmt.Sprintf("encode the rebuilt metadata document: %v", err)}
	}
	if string(encoded) != oracle.metadataRaw {
		return &migrateShadowMismatch{Dimension: "metadata-document", Reason: "the rebuilt metadata document differs from the old metadata_json"}
	}
	return nil
}

// verifyMigrateSessionHash checks computeSessionEntriesHash over the shim
// main entries against the stored hash. A NULL stored hash skips only
// this dimension; every other dimension still runs.
func verifyMigrateSessionHash(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	if oracle.sessionEntriesHash == nil {
		return nil
	}
	records, partitions, err := readMigrateStagedBodies(conn, oracle)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "session-hash", Reason: err.Error()}
	}
	var main []EntryRecord
	for i, partition := range partitions {
		if partition == 0 {
			main = append(main, records[i])
		}
	}
	full := oracle.captureFound && oracle.capture.CaptureFormat == ingest.ContentCaptureFormatFull
	bounded, _ := migrateBoundedShapes(main, full)
	hash, err := computeSessionEntriesHash(bounded)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "session-hash", Reason: fmt.Sprintf("hash the shim entries: %v", err)}
	}
	if hash != *oracle.sessionEntriesHash {
		return &migrateShadowMismatch{Dimension: "session-hash", Reason: "the shim entries hash differently than the stored session entries hash"}
	}
	return nil
}

// verifyMigrateCaptureHash checks fullCaptureHash over the shim full main
// entries against the capture proof, for full captures only.
func verifyMigrateCaptureHash(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	if !oracle.captureFound || oracle.capture.FullCaptureSHA256 == "" {
		return nil
	}
	records, partitions, err := readMigrateStagedBodies(conn, oracle)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "capture-hash", Reason: err.Error()}
	}
	var main []EntryRecord
	for i, partition := range partitions {
		if partition == 0 {
			main = append(main, records[i])
		}
	}
	_, fullShape := migrateBoundedShapes(main, true)
	hash, err := fullCaptureHash(fullShape)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "capture-hash", Reason: fmt.Sprintf("hash the shim full entries: %v", err)}
	}
	if hash != oracle.capture.FullCaptureSHA256 {
		return &migrateShadowMismatch{Dimension: "capture-hash", Reason: "the shim full entries hash differently than the capture proof"}
	}
	return nil
}

// verifyMigrateDetailChildren is the data half of the detail
// dimension: the snapshot's metadata children (title refs, subagents,
// commits, associations, diagnostics, relationships) through the
// forced-harmonized readers against the legacy oracle snapshot. The
// children prove the derived catalog rows, so a difference is a data
// mismatch that rolls back.
func verifyMigrateDetailChildren(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	sessionRow, err := readSnapshotSessionRowOnConn(conn, oracle.sessionID)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: fmt.Sprintf("read the legacy session row: %v", err)}
	}
	legacy, err := generationReadSnapshotOnConn(conn, oracle.sessionID, oracle.generationID, sessionRow)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: fmt.Sprintf("build the legacy oracle snapshot: %v", err)}
	}
	forced, err := harmonizedReadSnapshotOnConn(conn, oracle.sessionID, oracle.generationID)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: fmt.Sprintf("build the forced-harmonized snapshot: %v", err)}
	}
	shape := func(snapshot indexformat.ReadSnapshot) (string, error) {
		children := struct {
			TitleRefs     []schema.SourceEntryRef       `json:"titleRefs"`
			Subagents     []schema.SubagentRef          `json:"subagents"`
			Commits       []schema.CommitInfo           `json:"commits"`
			Associations  []schema.PublishedAssociation `json:"associations"`
			Diagnostics   []schema.DiagnosticEntry      `json:"diagnostics"`
			Relationships []schema.SessionRelationship  `json:"relationships"`
		}{
			TitleRefs:     snapshot.TitleRefs,
			Subagents:     snapshot.Metadata.Subagents,
			Commits:       snapshot.Metadata.Git.Commits,
			Associations:  snapshot.Metadata.Git.Associations,
			Diagnostics:   snapshot.Metadata.Diagnostics.Warnings,
			Relationships: snapshot.Metadata.Relationships,
		}
		raw, err := json.Marshal(children)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	want, err := shape(legacy)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: fmt.Sprintf("encode the legacy children: %v", err)}
	}
	got, err := shape(forced)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: fmt.Sprintf("encode the forced-harmonized children: %v", err)}
	}
	if got != want {
		return &migrateShadowMismatch{Dimension: "detail-children", Reason: "the forced-harmonized snapshot children differ from the legacy oracle children"}
	}
	return nil
}

// verifyMigrateDetailBytes is the defect dimension: the snapshot detail
// payload through the forced-harmonized readers against the legacy
// oracle snapshot with its blob bytes, checked only after every data
// dimension passed, so a difference here is a reader defect and halts
// the run. The representation-derived metadata anchor is normalized on
// both sides: the stored catalog anchor is stats-excluded by design,
// while the captured document's is stats-inclusive.
func verifyMigrateDetailBytes(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	sessionRow, err := readSnapshotSessionRowOnConn(conn, oracle.sessionID)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("read the legacy session row: %v", err)}
	}
	legacy, err := generationReadSnapshotOnConn(conn, oracle.sessionID, oracle.generationID, sessionRow)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("build the legacy oracle snapshot: %v", err)}
	}
	forced, err := harmonizedReadSnapshotOnConn(conn, oracle.sessionID, oracle.generationID)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("build the forced-harmonized snapshot: %v", err)}
	}
	legacy.Metadata.MetadataHash = ""
	forced.Metadata.MetadataHash = ""
	// Content records are representation-shaped on both sides (owned blob
	// paths and integrity digests on the legacy side; synthetic body
	// digests on the forced side), so the snapshot compare normalizes
	// them to their display contract — the emitted ref set with its byte
	// lengths — while the loop below proves the bytes per ref.
	if mismatch := compareMigrateSnapshotContent(legacy.Content, forced.Content); mismatch != nil {
		return mismatch
	}
	legacy.Content = nil
	forced.Content = nil
	want, err := json.Marshal(legacy)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("encode the legacy oracle snapshot: %v", err)}
	}
	got, err := json.Marshal(forced)
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("encode the forced-harmonized snapshot: %v", err)}
	}
	if string(got) != string(want) {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: "the forced-harmonized snapshot differs from the legacy oracle snapshot"}
	}
	emitted := map[schema.SourceEntryRef]string{}
	for _, items := range oracle.entries {
		for _, item := range items {
			if item.entry.SourceEntryRef != "" {
				sum := sha256.Sum256([]byte(item.raw))
				emitted[item.entry.SourceEntryRef] = hex.EncodeToString(sum[:])
			}
		}
	}
	for _, record := range oracle.content {
		wantBytes, ok := oracle.blobs[record.Ref]
		if !ok {
			return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("content record %q carries no oracle bytes", record.Ref)}
		}
		var gotBytes []byte
		if digest, ok := emitted[record.Ref]; ok {
			row, err := readHarmonizedBodyOnConn(conn, oracle.sessionID, oracle.generationID, record.Ref, digest)
			if err != nil {
				return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("resolve harmonized entry for ref %q: %v", record.Ref, err)}
			}
			gotBytes = []byte(harmonizedContentField(entryFromRow(row)))
		} else {
			content, err := readMigrateBlobBytes(conn, oracle.sessionID, record.Digest)
			if err != nil {
				return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("resolve harmonized blob for ref %q: %v", record.Ref, err)}
			}
			gotBytes = content
		}
		if string(gotBytes) != string(wantBytes) {
			return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("harmonized bytes for ref %q differ from the oracle blob", record.Ref)}
		}
	}
	return nil
}

// compareMigrateSnapshotContent compares the display contract of two
// snapshot content maps: the same emitted refs with the same byte
// lengths, in ref order. Paths and digests are representation-shaped
// and stay out of the compare; the byte loop proves the bytes per ref.
func compareMigrateSnapshotContent(legacy, forced []indexformat.ContentRecord) *migrateShadowMismatch {
	shape := func(records []indexformat.ContentRecord) [][2]string {
		out := make([][2]string, 0, len(records))
		for _, record := range records {
			out = append(out, [2]string{string(record.Ref), fmt.Sprintf("%d", record.ByteLength)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		return out
	}
	want, err := json.Marshal(shape(legacy))
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("encode the legacy content shape: %v", err)}
	}
	got, err := json.Marshal(shape(forced))
	if err != nil {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: fmt.Sprintf("encode the forced-harmonized content shape: %v", err)}
	}
	if string(got) != string(want) {
		return &migrateShadowMismatch{Dimension: "detail-bytes", Defect: true, Reason: "the forced-harmonized snapshot emits different content refs than the legacy oracle snapshot"}
	}
	return nil
}

// readMigrateBlobBytes concatenates one blob's chunks in order: the same
// byte proof the repair gate demands of blob bytes.
func readMigrateBlobBytes(conn *sqlite.Conn, sessionID schema.SessionID, digest string) ([]byte, error) {
	var payload []byte
	if err := sqlitex.ExecuteTransient(conn, `SELECT data FROM session_content_chunks WHERE session_id = ? AND digest = ? ORDER BY chunk_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), digest},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnLen(0)
			chunk := make([]byte, n)
			stmt.ColumnBytes(0, chunk)
			payload = append(payload, chunk...)
			return nil
		},
	}); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("no chunks stored")
	}
	return payload, nil
}

// migrateStageSeam is a nil production hook a test sets to fail between
// body sub-transactions, proving an interrupted staging resumes: the
// re-run stages idempotently through ON CONFLICT DO NOTHING and
// converts. Production never sets it.
var migrateStageSeam func(stage string) error

// migrateShadowSeam is a nil production hook a test sets to corrupt one
// derived row between the catalog insert and the shadow verify, proving
// the verify refuses a conversion whose derived output disagrees with
// its oracle. It runs on the catalog transaction's own connection, so
// the corruption shares the transaction's fate. Production never sets it.
var migrateShadowSeam func(conn *sqlite.Conn, stage string) error

// reportMigrateStageSeam runs the staging seam hook when set.
func reportMigrateStageSeam(stage string) error {
	if migrateStageSeam == nil {
		return nil
	}
	return migrateStageSeam(stage)
}

// reportMigrateShadowSeam runs the seam hook when set. A set hook that
// fails stops the conversion with the injected error, like the
// reclaimSeam crash tests stop the reclaim.
func reportMigrateShadowSeam(conn *sqlite.Conn, stage string) error {
	if migrateShadowSeam == nil {
		return nil
	}
	return migrateShadowSeam(conn, stage)
}

// checkMigrateOracleConverges proves the reshaped relationship rows still
// agree with the captured metadata document before the conversion
// replaces them: the v62 reshape moved the anchor JSON into structured
// columns, and the conversion must not choose between two disagreeing
// sources. A divergence is a data rollback.
func (s *Store) checkMigrateOracleConverges(ctx context.Context, oracle *migrateOracle) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take connection for the convergence check: %w", err)
	}
	defer s.pool.Put(conn)
	structured, err := harmonizedRelationshipsOnConn(conn, oracle.sessionID, oracle.generationID)
	if err != nil {
		return fmt.Errorf("read the reshaped relationship rows: %w", err)
	}
	shape := func(relationships []schema.SessionRelationship) string {
		encoded := make([]string, 0, len(relationships))
		for _, relationship := range relationships {
			raw, err := json.Marshal(relationship)
			if err != nil {
				continue
			}
			encoded = append(encoded, string(raw))
		}
		sort.Strings(encoded)
		raw, _ := json.Marshal(encoded)
		return string(raw)
	}
	if shape(structured) != shape(oracle.metadata.Relationships) {
		return fmt.Errorf("the reshaped relationship rows disagree with the captured metadata document; the conversion cannot choose between them: reshaped %.200q against captured %.200q", shape(structured), shape(oracle.metadata.Relationships))
	}
	return nil
}

// verifyMigrateCarriedChildren proves the carried child rows survived
// the delete-plus-insert move verbatim: the aliases, the partition
// states, the native-metadata records, and the segments with their
// captured refs read back exactly as the oracle carried them. The
// snapshot dimensions already cover the subagents, commits,
// associations, diagnostics, relationships, and title refs through both
// snapshots; the mapping rows through the body reads; and the
// descriptors through the blob bytes. A difference is a data rollback.
func verifyMigrateCarriedChildren(conn *sqlite.Conn, oracle *migrateOracle, _ *preparedHarmonized) *migrateShadowMismatch {
	if mismatch := verifyMigrateCarriedAliases(conn, oracle); mismatch != nil {
		return mismatch
	}
	if mismatch := verifyMigrateCarriedSections(conn, oracle); mismatch != nil {
		return mismatch
	}
	if mismatch := verifyMigrateCarriedNativeMetadata(conn, oracle); mismatch != nil {
		return mismatch
	}
	return verifyMigrateCarriedSegments(conn, oracle)
}

// verifyMigrateCarriedAliases compares the alias map back: same keys to
// the same refs, in key order.
func verifyMigrateCarriedAliases(conn *sqlite.Conn, oracle *migrateOracle) *migrateShadowMismatch {
	type alias struct {
		key string
		ref string
	}
	var stored []alias
	if err := sqlitex.ExecuteTransient(conn, `SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stored = append(stored, alias{stmt.ColumnText(0), stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		return &migrateShadowMismatch{Dimension: "carried-aliases", Reason: fmt.Sprintf("read back the alias rows: %v", err)}
	}
	if len(stored) != len(oracle.aliases) {
		return &migrateShadowMismatch{Dimension: "carried-aliases", Reason: fmt.Sprintf("the insert holds %d aliases but the oracle carried %d", len(stored), len(oracle.aliases))}
	}
	for i, want := range oracle.aliases {
		if stored[i].key != want.NativeKey || stored[i].ref != string(want.Ref) {
			return &migrateShadowMismatch{Dimension: "carried-aliases", Reason: fmt.Sprintf("alias %q points at %q, want %q", stored[i].key, stored[i].ref, want.Ref)}
		}
	}
	return nil
}

// verifyMigrateCarriedSections compares the partition states back: the
// same partitions with the same earlier states.
func verifyMigrateCarriedSections(conn *sqlite.Conn, oracle *migrateOracle) *migrateShadowMismatch {
	type section struct {
		partition int
		state     string
	}
	var stored []section
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, COALESCE(earlier_state, '') FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stored = append(stored, section{stmt.ColumnInt(0), stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		return &migrateShadowMismatch{Dimension: "carried-sections", Reason: fmt.Sprintf("read back the section rows: %v", err)}
	}
	if len(stored) != len(oracle.sections) {
		return &migrateShadowMismatch{Dimension: "carried-sections", Reason: fmt.Sprintf("the insert holds %d sections but the oracle carried %d", len(stored), len(oracle.sections))}
	}
	for i, want := range oracle.sections {
		if stored[i].partition != want.partitionID || stored[i].state != want.state {
			return &migrateShadowMismatch{Dimension: "carried-sections", Reason: fmt.Sprintf("partition %d carries state %q, want %q", stored[i].partition, stored[i].state, want.state)}
		}
	}
	return nil
}

// verifyMigrateCarriedNativeMetadata compares the native-metadata
// records back per partition: the same records with byte-identical
// opaque payloads, in ordinal order.
func verifyMigrateCarriedNativeMetadata(conn *sqlite.Conn, oracle *migrateOracle) *migrateShadowMismatch {
	for partition, want := range oracle.nativeMetadata {
		stored, err := harmonizedNativeMetadataOnConn(conn, oracle.sessionID, oracle.generationID, partition)
		if err != nil {
			return &migrateShadowMismatch{Dimension: "carried-native-metadata", Reason: fmt.Sprintf("read back partition %d: %v", partition, err)}
		}
		if len(stored) != len(want) {
			return &migrateShadowMismatch{Dimension: "carried-native-metadata", Reason: fmt.Sprintf("partition %d holds %d records but the oracle carried %d", partition, len(stored), len(want))}
		}
		for i := range want {
			wantRaw, err := json.Marshal(want[i])
			if err != nil {
				return &migrateShadowMismatch{Dimension: "carried-native-metadata", Reason: fmt.Sprintf("encode oracle record %d of partition %d: %v", i, partition, err)}
			}
			gotRaw, err := json.Marshal(stored[i])
			if err != nil {
				return &migrateShadowMismatch{Dimension: "carried-native-metadata", Reason: fmt.Sprintf("encode stored record %d of partition %d: %v", i, partition, err)}
			}
			if string(gotRaw) != string(wantRaw) {
				return &migrateShadowMismatch{Dimension: "carried-native-metadata", Reason: fmt.Sprintf("record %d of partition %d differs from the oracle record", i, partition)}
			}
		}
	}
	return nil
}

// verifyMigrateCarriedSegments compares the context segments with their
// captured refs back: the same segments with the same coordinates and
// the same ordered refs.
func verifyMigrateCarriedSegments(conn *sqlite.Conn, oracle *migrateOracle) *migrateShadowMismatch {
	var stored []indexformat.ContextSegment
	refsBySegment := map[int][]schema.SourceEntryRef{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT segment_ordinal, logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion FROM session_context_segments WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(oracle.sessionID), oracle.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			segment := indexformat.ContextSegment{
				Ordinal:          stmt.ColumnInt(0),
				PhysicalSourceID: stmt.ColumnText(2),
				Coordinates: indexformat.SegmentCoordinates{
					Kind: indexformat.CoordinateKind(stmt.ColumnText(3)),
				},
				Inclusion: indexformat.SegmentInclusion(stmt.ColumnText(8)),
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				logical, err := schema.NewSessionID(stmt.ColumnText(1))
				if err != nil {
					return err
				}
				segment.LogicalSessionID = &logical
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				start := stmt.ColumnInt64(4)
				segment.Coordinates.Start = &start
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				end := stmt.ColumnInt64(5)
				segment.Coordinates.EndExclusive = &end
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				decStart := stmt.ColumnInt64(6)
				segment.Coordinates.DecodedByteStart = &decStart
			}
			if stmt.ColumnType(7) != sqlite.TypeNull {
				decEnd := stmt.ColumnInt64(7)
				segment.Coordinates.DecodedByteEndExclusive = &decEnd
			}
			stored = append(stored, segment)
			return nil
		},
	}); err != nil {
		return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("read back the segment rows: %v", err)}
	}
	if len(stored) > 0 {
		if err := sqlitex.ExecuteTransient(conn, `SELECT segment_ordinal, source_entry_ref FROM session_context_segment_refs WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal, ordinal`, &sqlitex.ExecOptions{
			Args: []any{string(oracle.sessionID), oracle.generationID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				ref, err := schema.NewSourceEntryRef(stmt.ColumnText(1))
				if err != nil {
					return err
				}
				ordinal := stmt.ColumnInt(0)
				refsBySegment[ordinal] = append(refsBySegment[ordinal], ref)
				return nil
			},
		}); err != nil {
			return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("read back the segment refs: %v", err)}
		}
		for i := range stored {
			stored[i].CapturedRefs = refsBySegment[stored[i].Ordinal]
		}
	}
	if len(stored) != len(oracle.segments) {
		return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("the insert holds %d segments but the oracle carried %d", len(stored), len(oracle.segments))}
	}
	for i, want := range oracle.segments {
		wantRaw, err := json.Marshal(want)
		if err != nil {
			return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("encode oracle segment %d: %v", i, err)}
		}
		gotRaw, err := json.Marshal(stored[i])
		if err != nil {
			return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("encode stored segment %d: %v", i, err)}
		}
		if string(gotRaw) != string(wantRaw) {
			return &migrateShadowMismatch{Dimension: "carried-segments", Reason: fmt.Sprintf("segment %d differs from the oracle segment", want.Ordinal)}
		}
	}
	return nil
}
