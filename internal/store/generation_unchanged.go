package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ManagedGenerationRefreshResult reports the
// generation that is the active read authority afterwards: the settled source
// generation.
type ManagedGenerationRefreshResult struct {
	GenerationID string
}

// ManagedGenerationRefreshError is the actionable refusal of one unchanged
// settlement. It names what was refused, why, the impact, and the caller's
// next step; nothing changed.
type ManagedGenerationRefreshError struct {
	SessionID          schema.SessionID
	SourceGenerationID string
	Detail             string
}

func (e *ManagedGenerationRefreshError) Error() string {
	return fmt.Sprintf(
		"store: refuse the unchanged settlement of session %s generation %s: source-unsupported; where: store.SettleManagedGenerationUnchanged, before any row changed; impact: the active authority is unchanged and the session stays refresh work; fix: %s",
		e.SessionID, e.SourceGenerationID, e.Detail)
}

// ManagedGenerationUnchangedProof describes the actual captured input and SQL
// state to compare. Expected is captured after the metadata pair was installed.
type ManagedGenerationUnchangedProof struct {
	SessionID     ingest.SessionID
	GenerationID  string
	Metadata      ingest.UnifiedMetadata
	InputDigest   string
	IndexerTarget int
	FormatTarget  int
	Expected      *ingest.SessionIndexState
	UpdatedAtMs   int64
}

// ProveManagedGenerationUnchanged reads the catalog and index in one snapshot.
// It never reads generation mappings, content descriptors, or title references.
func (s *Store) ProveManagedGenerationUnchanged(ctx context.Context, p ManagedGenerationUnchangedProof) (result ingest.UnchangedProof, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: read unchanged proof for %s: %w; restore database access and retry", p.SessionID, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	return proveManagedGenerationOnConn(conn, p)
}

func nonzeroDigest(s string) bool { return validIndexInputHash(s) && strings.Trim(s, "0") != "" }

func proveManagedGenerationOnConn(conn *sqlite.Conn, p ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	mismatch := func(reason ingest.UnchangedProofMismatch) (ingest.UnchangedProof, error) {
		return ingest.UnchangedProof{Mismatch: reason}, nil
	}
	var active, attempt, content, metadataHash, harness, format, completeness, evidence, seal, level, redactionHash string
	var indexFormat, applied int
	var adapter, derived *int64
	var activated bool
	found := false
	err := sqlitex.Execute(conn, `SELECT COALESCE(s.active_generation_id,''), COALESCE(s.latest_attempt_generation_id,''),
 g.content_hash,g.metadata_hash,g.harness,g.source_format,g.completeness,g.source_evidence_digest,
 g.candidate_digest,g.index_format_version,g.redaction_applied,COALESCE(g.redaction_level,''),
 COALESCE(g.redaction_content_hash_at_redact,''),g.adapter_version,g.derived_at,g.activated_at_ms
 FROM sessions s LEFT JOIN session_generations g ON g.session_id=s.session_id AND g.generation_id=s.active_generation_id
 WHERE s.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}, ResultFunc: func(st *sqlite.Stmt) error {
		found = true
		active = st.ColumnText(0)
		attempt = st.ColumnText(1)
		content = st.ColumnText(2)
		metadataHash = st.ColumnText(3)
		harness = st.ColumnText(4)
		format = st.ColumnText(5)
		completeness = st.ColumnText(6)
		evidence = st.ColumnText(7)
		seal = st.ColumnText(8)
		indexFormat = st.ColumnInt(9)
		applied = st.ColumnInt(10)
		level = st.ColumnText(11)
		redactionHash = st.ColumnText(12)
		adapter = nullableInt64Col(st, 13)
		derived = nullableInt64Col(st, 14)
		activated = st.ColumnType(15) != sqlite.TypeNull
		return nil
	}})
	if err != nil {
		return ingest.UnchangedProof{}, fmt.Errorf("store: read generation for unchanged proof of %s: %w; retry with database access restored", p.SessionID, err)
	}
	if !found || active == "" || active != p.GenerationID || p.Metadata.SessionID != p.SessionID {
		return mismatch(ingest.UnchangedNotProvenActive)
	}
	if indexFormat != 2 || !activated {
		return mismatch(ingest.UnchangedNotProvenUnsupported)
	}
	if attempt != "" {
		return mismatch(ingest.UnchangedNotProvenLatestAttempt)
	}
	if !nonzeroDigest(content) || content != p.Metadata.ContentHash {
		return mismatch(ingest.UnchangedNotProvenContent)
	}
	if format != string(p.Metadata.Source.Format) {
		return mismatch(ingest.UnchangedNotProvenSourceFormat)
	}
	if harness != string(p.Metadata.ModelHarness) {
		return mismatch(ingest.UnchangedNotProvenHarness)
	}
	if completeness != string(indexformat.GenerationCompletenessComplete) {
		return mismatch(ingest.UnchangedNotProvenCompleteness)
	}
	if !nonzeroDigest(evidence) {
		return mismatch(ingest.UnchangedNotProvenSourceEvidence)
	}
	meta := p.Metadata
	meta.AdapterVersion = nil
	if adapter != nil {
		v := int(*adapter)
		meta.AdapterVersion = &v
	}
	if !nonzeroDigest(metadataHash) || statsExcludedMetadataHash(meta) != metadataHash || (meta.DerivedAt != nil && !sameIndexValue(meta.DerivedAt, derived)) {
		return mismatch(ingest.UnchangedNotProvenMetadata)
	}
	if (applied == 1) != meta.Redaction.Applied || level != meta.Redaction.Level || redactionHash != meta.Redaction.ContentHashAtRedact {
		return mismatch(ingest.UnchangedNotProvenRedaction)
	}
	state, err := readIndexStateOnConn(conn, p.SessionID)
	if err != nil {
		return ingest.UnchangedProof{}, err
	}
	if state == nil || p.IndexerTarget <= 0 || p.FormatTarget != 2 || state.IndexerVersion != p.IndexerTarget || state.IndexVersion == nil || *state.IndexVersion != p.FormatTarget || state.IndexedAt == nil {
		return mismatch(ingest.UnchangedNotProvenIndexer)
	}
	if !nonzeroDigest(p.InputDigest) || state.IndexedInputHash == nil || *state.IndexedInputHash != p.InputDigest {
		return mismatch(ingest.UnchangedNotProvenInputProof)
	}
	if !nonzeroDigest(seal) {
		return mismatch(ingest.UnchangedNotProvenSeal)
	}
	if p.Expected == nil || !sameIndexState(p.Expected, state) || !sameIndexValue(p.Expected.LatestAttemptArtifactHash, state.LatestAttemptArtifactHash) || !sameIndexValue(p.Expected.LatestAttemptIndexerVersion, state.LatestAttemptIndexerVersion) || !sameIndexValue(p.Expected.LatestAttemptAdapterVersion, state.LatestAttemptAdapterVersion) || p.Expected.ContentFailureCode != state.ContentFailureCode {
		return mismatch(ingest.UnchangedNotProvenInputProof)
	}
	return ingest.UnchangedProof{Proven: true}, nil
}

// SettleManagedGenerationUnchanged repeats the proof under the session lock
// and an immediate transaction, so changed authority cannot be settled.
func (s *Store) SettleManagedGenerationUnchanged(ctx context.Context, p ManagedGenerationUnchangedProof) (result ManagedGenerationRefreshResult, err error) {
	if s.sessionLocker == nil || p.UpdatedAtMs <= 0 {
		return result, fmt.Errorf("store: settle unchanged generation for %s: session locking or capture time is missing; nothing was written; configure generation support and supply the capture time", p.SessionID)
	}
	release, err := s.sessionLocker.LockExclusive(ctx, p.SessionID)
	if err != nil {
		return result, fmt.Errorf("store: lock session %s for unchanged settlement: %w; nothing was written; retry", p.SessionID, err)
	}
	defer release()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: take connection for unchanged settlement of %s: %w; nothing was written; retry", p.SessionID, err)
	}
	defer s.pool.Put(conn)
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return result, err
	}
	defer end(&err)
	proof, err := proveManagedGenerationOnConn(conn, p)
	if err != nil {
		return result, err
	}
	if !proof.Proven {
		return result, &ingest.StaleIndexWorkError{SessionID: p.SessionID}
	}
	stored, readErr := readCapturedStatsOnConn(conn, p.SessionID)
	if readErr != nil && !errors.Is(readErr, ErrNoCapturedStats) {
		return result, fmt.Errorf("store: read parse counts for unchanged settlement of %s: %w; nothing was written; restore database access and retry", p.SessionID, readErr)
	}
	if readErr != nil || stored.TurnCount == nil || stored.ConversationTurnCount == nil || stored.InputSubmissionCount == nil {
		return result, &ManagedGenerationRefreshError{
			SessionID: p.SessionID, SourceGenerationID: p.GenerationID,
			Detail: "the stored parse counts are incomplete; re-parse the source to restore authoritative counts",
		}
	}
	p.Metadata.Stats.TurnCount = *stored.TurnCount
	p.Metadata.Stats.InputSubmissionCount = stored.InputSubmissionCount
	conversation := int64(*stored.ConversationTurnCount)
	p.Metadata.Stats.ConversationTurnCount = &conversation
	stats := capturedStatsForHarnessWrite(p.SessionID, p.Metadata.Stats, seedJSONForStats(p.Metadata.Stats), p.UpdatedAtMs)
	var warnings []schema.DiagnosticEntry
	stats.Overflow, warnings, err = retainPriorStatsOverflowOnConn(conn, stats)
	if err != nil {
		return result, err
	}
	if len(warnings) != 0 {
		return result, fmt.Errorf("store: settle unchanged generation for %s: stats overflow needs diagnostic bookkeeping outside this small transaction; nothing was written; use the ordinary parse path", p.SessionID)
	}
	err = sqlitex.Execute(conn, sqlUpsertCapturedStats, &sqlitex.ExecOptions{Args: []any{string(stats.SessionID), nullableArg(stats.TurnCount), nullableArg64(stats.InputSubmissionCount), nullableArg(stats.ToolCallCount), nullableArg(stats.SubagentCount), nullableArg64(stats.DurationMs), nullableArg(stats.TokensIn), nullableArg(stats.TokensOut), nullableArg(stats.ThoughtTokens), nullableArg(stats.CachedReadTokens), nullableArg(stats.CachedWriteTokens), nullableStr(stats.SeedJSON), string(stats.Source), stats.UpdatedAtMs, nullableStr(stats.Overflow), nullableArg(stats.ConversationTurnCount), nullableArg(stats.ConversationTurnCount)}})
	if err != nil {
		return result, fmt.Errorf("store: merge unchanged stats for %s: %w; transaction rolled back; retry", p.SessionID, err)
	}
	if err = syncInputSubmissionMirrorOnConn(conn, p.SessionID); err != nil {
		return result, err
	}
	revision := p.Expected.PublicationCaptureRevision
	if revision > 0 {
		if err = checkPublicationIndexRevision(conn, p.SessionID, revision); err != nil {
			return result, err
		}
		if err = sqlitex.Execute(conn, `UPDATE session_content_captures SET publication_capture_revision=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{revision, string(p.SessionID)}}); err != nil {
			return result, err
		}
		if err = stampPublicationIndex(conn, p.SessionID, revision); err != nil {
			return result, err
		}
	}
	return ManagedGenerationRefreshResult{GenerationID: p.GenerationID}, nil
}
