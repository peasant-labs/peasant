package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// retentionEligible takes a pooled connection and evaluates the eligibility
// predicate. It is the routing hint the activation path uses before it decides
// between repair, skip and the write path.
func (s *Store) retentionEligible(ctx context.Context, write ingest.SessionEntryWrite) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection to decide retention for session %s: %w; nothing was written", write.SessionID, err)
	}
	defer s.pool.Put(conn)
	return retentionEligibleOnConn(conn, write)
}

// errRetentionPairProofUnavailable is the fixed category the retention
// boundary reports when a write qualifies for retention but no verified pair
// identity is available. The store refuses before any mutation rather than
// stashing unbound content or recording a binding that can never settle.
var errRetentionPairProofUnavailable = errors.New("pair-proof-unavailable")

// retainedAttemptSettledSQL builds the settle fragment for one session alias:
// the retained latest attempt has fully settled when the pair, the producer
// revision and the adapter revision all match the current facts. The two
// positional parameters are the producer target (indexer version) and the
// adapter target, in that order. A caller splices the fragment into its own
// predicate (never rewrites the facets), so every selection surface agrees on
// what "settled" means.
//
//   - pair: the recorded attempt identity is non-null and equals the session's
//     current artifact hash, which is also non-null;
//   - producer: the recorded attempt producer revision meets the target;
//   - adapter: the retained generation row's adapter revision is non-null and
//     meets the target, read through the correlated EXISTS over
//     session_generations keyed by latest_attempt_generation_id.
func retainedAttemptSettledSQL(alias string) string {
	return alias + `.latest_attempt_generation_id IS NOT NULL
    AND ` + alias + `.latest_attempt_artifact_hash IS NOT NULL
    AND ` + alias + `.latest_attempt_artifact_hash = ` + alias + `.artifact_hash
    AND ` + alias + `.latest_attempt_indexer_version IS NOT NULL
    AND ` + alias + `.latest_attempt_indexer_version >= ?
    AND EXISTS (
        SELECT 1 FROM session_generations g
        WHERE g.session_id = ` + alias + `.session_id
          AND g.generation_id = ` + alias + `.latest_attempt_generation_id
          AND g.adapter_version IS NOT NULL
          AND g.adapter_version >= ?
    )`
}

// retentionEligibleOnConn is the store-owned eligibility predicate for
// retained latest-preview coexistence. It reports whether this write is the
// assessed unaccounted-SourceOmitted preview over a publishable full
// authority, in which case the write is retained instead of replacing the
// active generation.
//
// The predicate holds only when ALL of these are true:
//
//  1. the result is a managed V2 generation;
//  2. the candidate is complete (GenerationCompletenessComplete);
//  3. the write carries the ASSESSED unaccounted-SourceOmitted preview: the
//     literal capture fields are preview_only with the source_records_omitted
//     code. This is authoritative; RequireFullContent is the caller-supplied
//     filter and never a substitute (an empty or legacy_preview_only format
//     also passes a bare !RequireFullContent gloss, and in the batch path the
//     two fields can disagree). Any other preview-over-full write keeps the
//     existing guard refusal;
//  4. the write is ordinary mode (never a format conversion, an explicit
//     rebuild, or a content backfill);
//  5. the current certificate holds full authority: a stored capture exists and
//     PublishableWithOmissions certifies it;
//  6. a verified pair identity is available: the stored artifact hash is
//     non-null, or the write supplies one.
//
// A write satisfying 1-5 but failing 6 is refused with the actionable
// pair-proof-unavailable error BEFORE any mutation, leaving the active
// authority intact. Legacy rows whose stored hash is NULL bootstrap through the
// existing WHERE-NULL identity path inside the retention transaction; NULL is
// never claimed as equality with NULL.
func retentionEligibleOnConn(conn *sqlite.Conn, write ingest.SessionEntryWrite) (bool, error) {
	v2, ok := asV2Value(write.Result)
	if !ok {
		return false, nil
	}
	if v2.Generation.Completeness != indexformat.GenerationCompletenessComplete {
		return false, nil
	}
	if write.ContentCapture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly ||
		write.ContentCapture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		return false, nil
	}
	switch write.Mode {
	case ingest.SessionEntryWriteReplaceAll, "":
	default:
		return false, nil
	}
	capture, found, err := readCapture(conn, write.SessionID)
	if err != nil {
		return false, err
	}
	if !found || !PublishableWithOmissions(capture) {
		return false, nil
	}
	_, hasStored, err := readStoredArtifactHashOnConn(conn, write.SessionID)
	if err != nil {
		return false, err
	}
	if hasStored {
		return true, nil
	}
	if write.ArtifactIdentity != nil && *write.ArtifactIdentity != "" {
		return true, nil
	}
	return false, retentionPairProofUnavailableError(write.SessionID)
}

// retentionPairProofUnavailableError names the missing proof, why retention
// needs it, and what the caller must do, so a direct caller that omits the
// identity sees an actionable refusal instead of a silent unbound stash.
func retentionPairProofUnavailableError(sessionID ingest.SessionID) error {
	return fmt.Errorf("store: refuse to retain the latest preview for session %s: pair-proof-unavailable; the write is an unaccounted source-records omission preview over a publishable full authority, but neither the stored artifact hash nor the supplied pair identity proves which pair the attempt consumed; nothing was written and the active generation is unchanged; supply the verified pair identity (SessionEntryWrite.ArtifactIdentity) or re-index the source so the stored identity exists, then retry: %w", sessionID, errRetentionPairProofUnavailable)
}

// readStoredArtifactHashOnConn reads sessions.artifact_hash, distinguishing a
// NULL (no stored identity) from an empty value. The column CHECK admits only a
// 64-hex value or NULL, so a non-null read is a real identity.
func readStoredArtifactHashOnConn(conn *sqlite.Conn, sessionID ingest.SessionID) (string, bool, error) {
	stored := ""
	found := false
	if err := sqlitex.Execute(conn, `SELECT artifact_hash FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				found = true
				stored = stmt.ColumnText(0)
			}
			return nil
		},
	}); err != nil {
		return "", false, fmt.Errorf("store: read the stored pair identity for session %s: %w; nothing was written", sessionID, err)
	}
	return stored, found, nil
}

// retainedAttemptPointer is the stored retained-latest-attempt pointer plus the
// retained generation's own binding and adapter revision, read in one snapshot
// so the settle comparison cannot mix facts from two states.
type retainedAttemptPointer struct {
	generationID   string
	artifactHash   string
	indexerVersion int
	failureCode    string
	failureMessage string
	capturedAtMs   int64
	statsJSON      string
	// candidateDigest and adapterVersion come from the retained generation row.
	candidateDigest string
	adapterVersion  *int
}

// found reports whether a retained attempt is recorded.
func (p retainedAttemptPointer) found() bool { return p.generationID != "" }

// readRetainedAttemptPointerOnConn reads the session's retained pointer and the
// retained generation's binding and adapter revision.
func readRetainedAttemptPointerOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (retainedAttemptPointer, error) {
	var pointer retainedAttemptPointer
	if err := sqlitex.Execute(conn, `SELECT latest_attempt_generation_id, latest_attempt_artifact_hash, latest_attempt_indexer_version, latest_attempt_failure_code, latest_attempt_failure_message, latest_attempt_captured_at_ms, latest_attempt_stats_json FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				pointer.generationID = stmt.ColumnText(0)
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				pointer.artifactHash = stmt.ColumnText(1)
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				pointer.indexerVersion = stmt.ColumnInt(2)
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				pointer.failureCode = stmt.ColumnText(3)
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				pointer.failureMessage = stmt.ColumnText(4)
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				pointer.capturedAtMs = stmt.ColumnInt64(5)
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				pointer.statsJSON = stmt.ColumnText(6)
			}
			return nil
		},
	}); err != nil {
		return pointer, fmt.Errorf("store: read the retained latest-attempt pointer for session %s: %w; nothing was written", sessionID, err)
	}
	if !pointer.found() {
		return pointer, nil
	}
	if err := sqlitex.Execute(conn, `SELECT candidate_digest, adapter_version FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), pointer.generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			pointer.candidateDigest = stmt.ColumnText(0)
			if stmt.ColumnType(1) != sqlite.TypeNull {
				version := stmt.ColumnInt(1)
				pointer.adapterVersion = &version
			}
			return nil
		},
	}); err != nil {
		return pointer, fmt.Errorf("store: read the retained generation binding for session %s generation %s: %w; nothing was written", sessionID, pointer.generationID, err)
	}
	return pointer, nil
}

// deleteGenerationRowsOnConn deletes every generation-scoped row of one exact
// (session, generation) pair through the shared reclaimTableNames inventory, so
// the retention replace branch and the reclaim/sweep paths cannot drift apart.
// It runs inside the caller's transaction.
func deleteGenerationRowsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) error {
	for _, table := range reclaimTableNames {
		if err := sqlitex.Execute(conn, `DELETE FROM `+table+` WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), generationID},
		}); err != nil {
			return fmt.Errorf("store: delete retained generation %s rows from %s for session %s: %w; the retention transaction rolled back and the prior state is unchanged", generationID, table, sessionID, err)
		}
	}
	return nil
}

// retainHarmonizedAttemptOnConn performs the retention commit inside the
// caller's transaction (the batch savepoint or the single-route commit). It
// records the candidate as the retained latest attempt without touching the
// active authority: no active pointer move, no content rewrite, no index state,
// no publication stamp.
//
// A TRUE no-op is taken only when a retained attempt already exists AND the
// candidate binding equals the retained generation's binding AND every settle
// facet equals the current facts (the verified pair identity, the producer
// revision, and the candidate's actual adapter revision). Otherwise the REPLACE
// branch deletes the previous attempt's rows through the shared inventory and
// inserts the candidate under its own generation identifier with its own
// metadata, then records the pointer in one sessions UPDATE. The generation row
// is INSERTED, never updated in place; the previous attempt's rows go in the
// same transaction, so one retained attempt at a time is kept.
//
// The candidate's producer stats are recorded (never the active authority's).
// A failure rolls the caller's transaction back: the pointer and the active
// generation are unchanged.
func retainHarmonizedAttemptOnConn(conn *sqlite.Conn, prepared *preparedHarmonized, write ingest.SessionEntryWrite) error {
	v2, ok := asV2Value(write.Result)
	if !ok {
		return fmt.Errorf("store: retain the latest preview for session %s: the write carries no managed generation; nothing was written", write.SessionID)
	}
	sessionID := write.SessionID
	// Establish a missing stored pair identity from the supplied verified
	// identity BEFORE reading the pointer, so the recorded pointer identity is
	// non-null and the settle facet is a real current-pair proof. The WHERE
	// clause keeps a stored identity untouched.
	verifiedIdentity, hasStored, err := readStoredArtifactHashOnConn(conn, sessionID)
	if err != nil {
		return err
	}
	if !hasStored && write.ArtifactIdentity != nil && *write.ArtifactIdentity != "" {
		if err := sqlitex.Execute(conn, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ? AND artifact_hash IS NULL`, &sqlitex.ExecOptions{
			Args: []any{*write.ArtifactIdentity, string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: establish the pair identity for session %s while retaining the latest preview: %w; the retention transaction rolled back and the prior state is unchanged", sessionID, err)
		}
		verifiedIdentity = *write.ArtifactIdentity
		hasStored = true
	}
	if !hasStored || verifiedIdentity == "" {
		// Defensive: eligibility already refused this, so a caller that bypasses
		// the pre-check still cannot record a NULL binding.
		return retentionPairProofUnavailableError(sessionID)
	}
	pointer, err := readRetainedAttemptPointerOnConn(conn, sessionID)
	if err != nil {
		return err
	}
	candidateAdapter := prepared.record.AdapterVersion
	if pointer.found() &&
		pointer.candidateDigest == prepared.binding &&
		pointer.artifactHash == verifiedIdentity &&
		pointer.indexerVersion == write.IndexerVersion &&
		nullableIntEqual(pointer.adapterVersion, candidateAdapter) {
		// TRUE no-op: the retained attempt already matches the current facts.
		// Nothing is written: no row, no pointer, no object.
		return nil
	}
	if pointer.found() {
		if err := deleteGenerationRowsOnConn(conn, sessionID, pointer.generationID); err != nil {
			return err
		}
	}
	now := time.Now().UnixMilli()
	if err := insertHarmonizedGenerationOnConn(conn, prepared, v2.PriorEvidence, now, nil); err != nil {
		return err
	}
	capturedAt := write.ContentCapture.CapturedAtMs
	if capturedAt == 0 {
		capturedAt = now
	}
	if err := sqlitex.Execute(conn, `UPDATE sessions SET latest_attempt_generation_id = ?, latest_attempt_artifact_hash = ?, latest_attempt_indexer_version = ?, latest_attempt_failure_code = ?, latest_attempt_failure_message = ?, latest_attempt_captured_at_ms = ?, latest_attempt_stats_json = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{
			prepared.generation.ID, verifiedIdentity, write.IndexerVersion,
			string(write.ContentCapture.FailureCode), write.ContentCapture.FailureMessage,
			capturedAt, seedJSONForStats(v2.Generation.Metadata.Stats), string(sessionID),
		},
	}); err != nil {
		return fmt.Errorf("store: record the retained latest-attempt pointer for session %s: %w; the retention transaction rolled back and the prior state is unchanged", sessionID, err)
	}
	return nil
}

// nullableIntEqual compares two optional adapter revisions for facet equality.
func nullableIntEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// retainSessionEntryWriteOnConn runs the retention branch of one batch savepoint
// on the caller's connection. It re-runs the immutable-identity check through
// harmonizedBatchPrecommit with the retain marker, re-runs the unknown-evidence
// preflight the content writer runs, and commits the retained attempt. It skips
// every active-authority statement and releases the savepoint itself.
//
// When the identity check finds the candidate already installed with the
// identical binding, it reports the idempotent retry without running any
// active-authority statement, so the active authority and the retained pointer
// stay identical.
func (s *Store) retainSessionEntryWriteOnConn(conn *sqlite.Conn, write ingest.SessionEntryWrite, savepointName string) (sessionEntryWriteOutcome, error, bool) {
	var out sessionEntryWriteOutcome
	_, prepared, skip, err := s.harmonizedBatchPrecommit(conn, write, true)
	if err != nil {
		rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, err, write.SessionID)
		return out, rollbackErr, fatal
	}
	if !skip.retain {
		out = skip.outcome
		relErr, fatal := s.releaseRetentionSavepoint(conn, write, savepointName)
		return out, relErr, fatal
	}
	v2, ok := asV2Value(write.Result)
	if !ok {
		rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, fmt.Errorf("store: retain the latest preview for session %s: the write carries no managed generation; nothing was written", write.SessionID), write.SessionID)
		return out, rollbackErr, fatal
	}
	evidence := append([]schema.SessionEntry(nil), prepared.mainEntries...)
	for _, section := range v2.Generation.Earlier {
		evidence = append(evidence, section.Content.Entries...)
	}
	if err := preflightUnknownEvidence(evidence, write.RequireFullContent); err != nil {
		rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, err, write.SessionID)
		return out, rollbackErr, fatal
	}
	if err := retainHarmonizedAttemptOnConn(conn, prepared, write); err != nil {
		rollbackErr, fatal := rollbackSessionEntrySavepoint(conn, savepointName, err, write.SessionID)
		return out, rollbackErr, fatal
	}
	out.retainedPreview = true
	relErr, fatal := s.releaseRetentionSavepoint(conn, write, savepointName)
	return out, relErr, fatal
}

// releaseRetentionSavepoint reports the mid-activation seam and releases the
// retention savepoint. A seam failure aborts the outer transaction (fatal), so
// a simulated process loss rolls the retention back.
func (s *Store) releaseRetentionSavepoint(conn *sqlite.Conn, write ingest.SessionEntryWrite, savepointName string) (error, bool) {
	if err := reportHarmonizedWriterSeam(harmonizedSeamMidActivationBatch); err != nil {
		return err, true
	}
	if err := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepointName, nil); err != nil {
		return fmt.Errorf("store: release session entry savepoint for %s: %w", write.SessionID, err), true
	}
	return nil, false
}
