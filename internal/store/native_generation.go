package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

var (
	_ ingest.NativeGenerationBatchStager       = (*Store)(nil)
	_ ingest.NativeGenerationBatchActivator    = (*Store)(nil)
	_ ingest.NativeGenerationActivator         = (*Store)(nil)
	_ ingest.NativeGenerationStager            = (*Store)(nil)
	_ ingest.NativeGenerationPreparedActivator = (*Store)(nil)
	_ ingest.NativeGenerationStaged            = (*PreparedGeneration)(nil)
	_ ingest.NativeGenerationPriorReader       = (*Store)(nil)
	_ ingest.NativePriorCaptureAuthorityReader = (*Store)(nil)
	_ ingest.ContentSweeper                    = (*Store)(nil)
)

func (s *Store) StageNativeGenerations(ctx context.Context, native []ingest.NativeGenerationActivation) ([]ingest.NativeGenerationStaged, []error) {
	activations := make([]GenerationActivation, len(native))
	for i, a := range native {
		activations[i] = generationActivationFromNative(a)
	}
	handles, errs := s.StageGenerationBatch(ctx, activations)
	staged := make([]ingest.NativeGenerationStaged, len(handles))
	for i, h := range handles {
		if h != nil {
			staged[i] = h
		}
	}
	return staged, errs
}

func (s *Store) ActivateStagedNativeGenerations(ctx context.Context, native []ingest.NativeGenerationActivation, staged []ingest.NativeGenerationStaged) []ingest.NativeActivationResult {
	activations := make([]GenerationActivation, len(native))
	for i, a := range native {
		activations[i] = generationActivationFromNative(a)
		if i < len(staged) {
			activations[i].Prepared, _ = staged[i].(*PreparedGeneration)
		}
	}
	results := s.ActivateGenerationBatch(ctx, activations)
	adapted := make([]ingest.NativeActivationResult, len(results))
	for i, r := range results {
		adapted[i] = ingest.NativeActivationResult{Outcome: r.Outcome, Err: r.Err}
	}
	return adapted
}

// generationActivationFromNative mirrors the ingest-owned activation envelope
// into the store's own immutable activation. Both entry points (pre-staging and
// activation) share it so their preconditions cannot drift.
func generationActivationFromNative(activation ingest.NativeGenerationActivation) GenerationActivation {
	return GenerationActivation{
		Generation:       activation.Generation,
		Blobs:            activation.Blobs,
		PriorEvidence:    activation.PriorEvidence,
		IndexerVersion:   activation.IndexerVersion,
		IndexedAtMs:      activation.IndexedAtMs,
		ExpectedState:    activation.ExpectedState,
		ContentCapture:   activation.ContentCapture,
		CaptureRevision:  activation.CaptureRevision,
		IndexedInputHash: activation.IndexedInputHash,
		ArtifactIdentity: activation.ArtifactIdentity,
		ExplicitRebuild:  activation.ExplicitRebuild,
		Capture:          activation.Capture,
	}
}

// ActivateNativeGeneration adapts the ingest-owned activation envelope to the
// store's own immutable activation. It stages the content files and fsyncs the
// manifest and the directory,
// persists the opaque prior document, installs the generation, counts and
// pointer in ONE transaction, repairs the exported metadata and clears the
// intent. It reuses the same expected-state compare and success-stamp rules as
// every other managed activation. The outcome carries the lock-derived
// disposition for per-invocation counting; a post-commit repair failure
// returns a GenerationRepairPendingError with committed authority.
func (s *Store) ActivateNativeGeneration(ctx context.Context, activation ingest.NativeGenerationActivation) (ingest.ActivationOutcome, error) {
	outcome, err := s.ActivateGeneration(ctx, generationActivationFromNative(activation))
	return outcome, err
}

// StageNativeGeneration prepares one managed generation's content files in an
// owned temporary directory without recording an activation intent or
// installing anything, so independent sessions can write in
// parallel while only the install and commit stay on the serialized writer.
// The returned handle is consumed by ActivateStagedNativeGeneration; a handle
// that is dropped (for example after the caller refuses stale input) can
// never be activated by recovery.
func (s *Store) StageNativeGeneration(ctx context.Context, activation ingest.NativeGenerationActivation) (ingest.NativeGenerationStaged, error) {
	prepared, err := s.StageGeneration(ctx, generationActivationFromNative(activation))
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

// ActivateStagedNativeGeneration activates one candidate, installing the
// files a prior StageNativeGeneration prepared when the handle belongs to this
// store and candidate. Any other handle is ignored and the candidate is
// staged inline, exactly as ActivateNativeGeneration does.
func (s *Store) ActivateStagedNativeGeneration(ctx context.Context, activation ingest.NativeGenerationActivation, staged ingest.NativeGenerationStaged) (ingest.ActivationOutcome, error) {
	generationActivation := generationActivationFromNative(activation)
	if prepared, ok := staged.(*PreparedGeneration); ok {
		generationActivation.Prepared = prepared
	}
	return s.ActivateGeneration(ctx, generationActivation)
}

// SweepSessionForHarvest sweeps one committed session through the
// harvest-side entry point: the same per-session pass the harvest start
// runs, scoped to the session the pipeline just committed. The pipeline
// calls it after every committed activation; the sweep error never fails
// the commit. A store without managed-generation support skips silently:
// it holds no staged state this build could have created, and a flag set
// by a capable opener on the same database stays set for that opener.
func (s *Store) SweepSessionForHarvest(ctx context.Context, sessionID schema.SessionID) error {
	if err := s.requireGenerationSupport(); err != nil {
		return nil
	}
	_, err := s.SweepSession(ctx, sessionID)
	return err
}

// SweepFlaggedSessionsForHarvest sweeps every flagged session through the
// harvest-side entry point: the harvest-start recovery pass with
// pipeline-neutral types (a swept count, per-session warnings, and a fatal
// error). The pipeline calls it before discovery. Like the per-session
// entry point, it skips silently without managed-generation support.
func (s *Store) SweepFlaggedSessionsForHarvest(ctx context.Context) (int, []error, error) {
	if err := s.requireGenerationSupport(); err != nil {
		return 0, nil, nil
	}
	report, err := s.SweepFlaggedSessions(ctx)
	if err != nil {
		return 0, nil, err
	}
	return len(report.Sessions), report.Warnings, nil
}

// ReadNativeGenerationPrior loads the active generation's reusable evidence for
// one session: its committed metadata, the native-key aliases the projection
// must reuse, whether a complete generation is already the last-good authority,
// and the harness-owned prior document when one was persisted. A session with
// no active generation returns (nil, nil), so a first discovery builds an empty
// prior state instead of guessing. A harmonized active generation reads its
// structured rows (the metadata rebuilds from the catalog row, the children,
// and the stats row, with the durable parent from the session row); a
// file-backed one reads the projection tables and the prior document file.
func (s *Store) ReadNativeGenerationPrior(ctx context.Context, sessionID schema.SessionID) (*ingest.NativeGenerationPrior, error) {
	if s.generationArtifacts == nil {
		return nil, fmt.Errorf("store: managed generation support is not configured; the active generation's prior evidence cannot be loaded for session %s; open the store with WithGenerationArtifacts before refreshing", sessionID)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read the native generation prior for session %s: %w; no candidate was produced; restore database access and retry", sessionID, err)
	}
	defer s.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return nil, err
	}
	if active == nil || *active == "" {
		return nil, nil
	}
	if generationIsHarmonized(conn, sessionID, *active) {
		return readHarmonizedPriorOnConn(conn, sessionID, *active)
	}
	prior, err := readGenerationPriorOnConn(conn, sessionID, *active)
	if err != nil {
		return nil, err
	}
	evidence, err := s.generationArtifacts.ReadPriorEvidence(ctx, sessionID, *active)
	if err != nil {
		return nil, err
	}
	prior.PriorEvidence = evidence
	return prior, nil
}

// ReadStoredCaptureAuthority reports the stored content-capture certificate
// that holds last-good full authority for a session with no active managed
// generation. It reads the same session_content_captures row and applies the
// same PublishableWithOmissions predicate the content writer uses to guard a
// preview replacement, so the bridge cannot drift from the transactional
// guard. The certificate's own source authority and capture format are
// preserved; a session with no publishable certificate returns (nil, nil), so
// a genuine first discovery keeps its empty prior. A publication revision is
// never consulted.
func (s *Store) ReadStoredCaptureAuthority(ctx context.Context, sessionID schema.SessionID) (*ingest.StoredCaptureAuthority, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read the stored capture authority for session %s: %w; the stored certificate was not consulted; restore database access and retry", sessionID, err)
	}
	defer s.pool.Put(conn)
	capture, found, err := readCapture(conn, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: read the stored capture certificate for session %s: %w; the authority bridge was not applied; repair the capture row or use a build that knows its codes", sessionID, err)
	}
	if !found || !PublishableWithOmissions(capture) {
		return nil, nil
	}
	return &ingest.StoredCaptureAuthority{
		Status:           capture.Status,
		SourceAuthority:  capture.SourceAuthority,
		TranscriptOrigin: capture.TranscriptOrigin,
		CaptureFormat:    capture.CaptureFormat,
		FailureCode:      capture.FailureCode,
	}, nil
}

// readHarmonizedPriorOnConn loads the reusable evidence for a harmonized
// active generation: the catalog row plus children, segments, sections, and
// evidence rebuild the captured metadata beside the stats row (the durable
// parent comes from the session row, since the catalog stores no ParentUUID),
// the main entries hydrate from their body rows, and the alias table stays
// the reusable-identity authority exactly as for file-backed generations.
func readHarmonizedPriorOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (*ingest.NativeGenerationPrior, error) {
	prior := &ingest.NativeGenerationPrior{
		GenerationID: generationID,
		Aliases:      ingest.NewProjectionPriorState(),
	}
	var view activeHarmonizedView
	view.descriptors = map[schema.SourceEntryRef]string{}
	view.aliases = map[string]schema.SourceEntryRef{}
	if err := loadGenerationRecordOnConn(conn, sessionID, generationID, &view); err != nil {
		return nil, err
	}
	if !view.found {
		return nil, fmt.Errorf("store: session %s points at harmonized generation %s with no catalog row; the prior evidence cannot be trusted; re-index the session for a fresh candidate", sessionID, generationID)
	}
	if err := loadGenerationChildrenOnConn(conn, sessionID, generationID, &view); err != nil {
		return nil, err
	}
	stats, err := readCapturedStatsOnConn(conn, sessionID)
	if err != nil {
		// A native session always owns a stats row past the v62 backfill;
		// without one its measurements read as unknown, exactly like a
		// missing metric row did.
		if !errors.Is(err, ErrNoCapturedStats) {
			return nil, err
		}
		stats = CapturedStats{SessionID: sessionID, Source: StatsSourceHarness}
	}
	metadata, err := serializedMetadataToUnified(view.record, view.children, stats)
	if err != nil {
		return nil, err
	}
	if parent := readSessionParentOnConn(conn, sessionID); parent != nil {
		metadata.ParentUUID = parent
	}
	prior.Metadata = &metadata
	prior.HasCompleteGeneration = view.record.Completeness == indexformat.GenerationCompletenessComplete
	prior.PriorEvidence = view.record.PriorEvidence
	aliases, err := readGenerationAliasesOnConn(conn, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	entries, err := readHarmonizedMainEntriesOnConn(conn, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	stub := indexformat.Generation{
		ID:       generationID,
		Aliases:  aliases,
		Metadata: metadata,
		Main:     indexformat.Partition{Entries: entries},
	}
	state, err := ingest.PriorStateFromGeneration(stub)
	if err != nil {
		return nil, fmt.Errorf("store: rebuild reusable identities for session %s generation %s: %w; no candidate was produced; the committed generation stays active", sessionID, generationID, err)
	}
	prior.Aliases = state
	return prior, nil
}

// serializedMetadataToUnified rebuilds the captured document from the
// structured rows: serializeMetadata carries the exact inverse mapping,
// and the JSON round-trips through the schema type so the prior loader
// hands the pipeline the same shape the file-backed path decoded.
func serializedMetadataToUnified(gen GenerationRecord, children GenerationChildren, stats CapturedStats) (schema.UnifiedMetadata, error) {
	var metadata schema.UnifiedMetadata
	encoded := serializeMetadata(gen, children, stats)
	if encoded == nil {
		return metadata, fmt.Errorf("store: rebuild captured metadata: the structured rows did not serialize; no candidate was produced")
	}
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return metadata, fmt.Errorf("store: rebuild captured metadata: the rebuilt document did not decode: %w; no candidate was produced", err)
	}
	return metadata, nil
}

// readSessionParentOnConn reads the durable parent target cache: the
// started_by target when it names a stored session, else NULL.
func readSessionParentOnConn(conn *sqlite.Conn, sessionID schema.SessionID) *schema.SessionID {
	var parent *schema.SessionID
	_ = sqlitex.Execute(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				id := schema.SessionID(stmt.ColumnText(0))
				parent = &id
			}
			return nil
		},
	})
	return parent
}

// readHarmonizedMainEntriesOnConn hydrates the active generation's main
// entries from their body rows in index order.
func readHarmonizedMainEntriesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]schema.SessionEntry, error) {
	var entries []schema.SessionEntry
	err := sqlitex.Execute(conn, `SELECT `+sqlSelectBodyColumnsJoined+` FROM session_entry_bodies b JOIN session_generation_entries m ON m.session_id = b.session_id AND m.body_digest = b.body_digest WHERE m.session_id = ? AND m.generation_id = ? AND m.partition_id = 0 ORDER BY m.entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, entryFromRow(scanEntryRecord(stmt)))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read harmonized main entries for session %s generation %s: %w; no candidate was produced; restore database access and retry", sessionID, generationID, err)
	}
	return entries, nil
}

func readGenerationPriorOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (*ingest.NativeGenerationPrior, error) {
	prior := &ingest.NativeGenerationPrior{
		GenerationID: generationID,
		Aliases:      ingest.NewProjectionPriorState(),
	}
	var metadataJSON string
	var completeness string
	found := false
	if err := sqlitex.Execute(conn, `SELECT metadata_json, completeness FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			metadataJSON = stmt.ColumnText(0)
			completeness = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read prior generation row for session %s generation %s: %w; no candidate was produced; restore database access and retry", sessionID, generationID, err)
	}
	if !found {
		// The pointer names a generation with no row; the caller must not reuse
		// another generation's identities under this authority.
		return nil, fmt.Errorf("store: session %s points at generation %s with no committed row; the prior evidence cannot be trusted; run managed recovery before refreshing the session", sessionID, generationID)
	}
	var metadata schema.UnifiedMetadata
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return nil, fmt.Errorf("store: decode committed metadata for session %s generation %s: %w; no candidate was produced; run managed recovery before refreshing the session", sessionID, generationID, err)
	}
	prior.Metadata = &metadata
	prior.HasCompleteGeneration = indexformat.GenerationCompleteness(completeness) == indexformat.GenerationCompletenessComplete

	aliases, err := readGenerationAliasesOnConn(conn, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	entries, err := readGenerationAliasEntriesOnConn(conn, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	stub := indexformat.Generation{
		ID:       generationID,
		Aliases:  aliases,
		Metadata: metadata,
		Main:     indexformat.Partition{Entries: entries},
	}
	state, err := ingest.PriorStateFromGeneration(stub)
	if err != nil {
		return nil, fmt.Errorf("store: rebuild reusable identities for session %s generation %s: %w; no candidate was produced; the committed generation stays active", sessionID, generationID, err)
	}
	prior.Aliases = state
	return prior, nil
}

func readGenerationAliasesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]indexformat.NativeAlias, error) {
	var aliases []indexformat.NativeAlias
	if err := sqlitex.Execute(conn, `SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			aliases = append(aliases, indexformat.NativeAlias{NativeKey: stmt.ColumnText(0), Ref: schema.SourceEntryRef(stmt.ColumnText(1))})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read reusable aliases for session %s generation %s: %w; no candidate was produced; restore database access and retry", sessionID, generationID, err)
	}
	return aliases, nil
}

func readGenerationAliasEntriesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]schema.SessionEntry, error) {
	entries := make([]schema.SessionEntry, 0)
	if err := sqlitex.Execute(conn, `SELECT entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var entry schema.SessionEntry
			if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &entry); err != nil {
				return err
			}
			entries = append(entries, entry)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read committed entries for session %s generation %s: %w; no candidate was produced; restore database access and retry", sessionID, generationID, err)
	}
	return entries, nil
}
