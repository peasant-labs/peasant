package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// GenerationActivation is one immutable V2 candidate plus the file bytes its
// content records address. The file bytes are staged and fsynced BEFORE the one
// activation transaction; the transaction installs the generation rows, the
// canonical main projection, the count mirrors and the active pointer together.
// An incomplete_new candidate leaves success and indexed-at unset so
// maintenance stays required; only a complete candidate may carry a positive
// producing indexer revision.
type GenerationActivation struct {
	Generation     indexformat.V2
	Blobs          map[schema.SourceEntryRef][]byte
	IndexerVersion int
	IndexedAtMs    int64
	ExpectedState  *ingest.SessionIndexState
	ContentCapture ingest.SessionContentCaptureWrite
	// PriorEvidence is the opaque activation-owned document a reopen reloads to
	// reuse the candidate's identities and retained prefixes. Nil leaves prior
	// evidence absent; the committed generation rows still supply aliases.
	PriorEvidence []byte
	// CaptureRevision binds the index write to its publication metadata capture.
	CaptureRevision int64
	// Capture is the pipeline-certified publication-capture agreement this
	// activation records in the same transaction as the generation install.
	// Nil records no capture and leaves the stored provenance unchanged.
	Capture *ingest.PublicationCaptureWrite
	// IndexedInputHash is the proof of the input this parser consumed. It is
	// set only for complete candidates with a positive producer revision.
	IndexedInputHash *string
	// ArtifactIdentity is the pair identity this parse consumed, established
	// when the captured state records none.
	ArtifactIdentity *string
}

// ActivateGeneration is the production V2 activation entry point. It takes the
// exclusive per-session lock, reconciles any prior intent, stages and fsyncs the
// generation files, records a synced intent carrying the full validated
// envelope, installs the generation in ONE SQLite transaction, repairs the
// exported metadata, and clears the intent. A crash at any seam leaves exactly
// G1 (the prior generation) or G2 (the new generation) visible; recovery
// replays the same guarded transaction and is idempotent.
//
// The outcome carries the lock-derived disposition for the REQUESTED candidate,
// independent of repair state, so per-invocation counts stay truthful: only
// CommittedNow counts once, AlreadyCommitted and NotCommitted count zero. A
// post-commit metadata-repair failure returns CommittedNow or AlreadyCommitted
// with a GenerationRepairPendingError, never a rollback. A pre-commit failure,
// including an unrelated prior-intent repair failure in the preamble, returns
// NotCommitted with no counts for the requested candidate.
func (s *Store) ActivateGeneration(ctx context.Context, activation GenerationActivation) (ingest.ActivationOutcome, error) {
	requestedID := activation.Generation.Generation.ID
	notCommitted := func(err error) (ingest.ActivationOutcome, error) {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: requestedID}, err
	}
	if err := s.requireGenerationSupport(); err != nil {
		return notCommitted(err)
	}
	if err := validateGenerationID(requestedID); err != nil {
		return notCommitted(err)
	}
	sessionID := activation.Generation.Generation.Metadata.SessionID
	// Incomplete candidates never carry success stamps: the guarded writer
	// refuses positive revisions for them, so force the unset state here
	// rather than letting a caller-supplied stamp through.
	if activation.Generation.Generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		activation.IndexerVersion = 0
		activation.IndexedAtMs = 0
		activation.IndexedInputHash = nil
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return notCommitted(err)
	}
	defer func() { _ = release() }()

	activeBefore, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return notCommitted(err)
	}
	// Best-effort preamble read: a nil intent forces the safe NotCommitted
	// path below, so a read failure degrades to refusal, never to a wrong
	// committed disposition.
	pendingBefore, _ := s.generationArtifacts.ReadIntent(ctx, sessionID)

	if err := s.recoverGenerationIntentLocked(ctx, sessionID); err != nil {
		// A stale pending intent for the same generation identifier is a
		// verified-retry case: the new activation carries fresh preconditions
		// that replace the refused envelope. Clear the stale marker and
		// proceed; a stale intent for a different candidate stays refused.
		var stale *ingest.StaleIndexWorkError
		if errors.As(err, &stale) {
			pending, readErr := s.generationArtifacts.ReadIntent(ctx, sessionID)
			if readErr == nil && pending != nil && pending.GenerationID == requestedID {
				if clearErr := s.generationArtifacts.ClearIntent(ctx, sessionID); clearErr == nil {
					goto reconciled
				}
			}
		}
		// A preamble that committed the requested candidate but failed repair
		// reports CommittedNow with pending repair, never rollback. A preamble
		// re-repair failure for an already-committed requested candidate
		// reports AlreadyCommitted. Any other preamble failure, including an
		// unrelated prior-intent repair failure, leaves the requested
		// candidate uncommitted by this call.
		var repairPending *ingest.GenerationRepairPendingError
		if errors.As(err, &repairPending) && pendingBefore != nil && pendingBefore.GenerationID == requestedID && repairPending.CandidateID == requestedID {
			if activeBefore == requestedID {
				return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID, RepairPending: true}, err
			}
			return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID, RepairPending: true}, err
		}
		return notCommitted(err)
	}
reconciled:

	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return notCommitted(err)
	}
	if active == requestedID {
		if activeBefore != requestedID {
			// Preamble recovery committed the requested candidate during this
			// invocation. Count once; a later ordinary activation of the same
			// candidate reports AlreadyCommitted.
			return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, nil
		}
		// A retry after a crash that already committed. Re-repair the exported
		// metadata from the committed generation row, not caller data, re-persist
		// the prior document when this activation carries it, and clear the
		// intent; nothing else changes. Zero new counts: idempotent repair,
		// not a failed capture. The generation install already committed
		// before its prior document is persisted, so a persist failure here is
		// post-commit repair with committed authority, never a pre-commit
		// refusal: report it as repair-pending like every sibling post-commit
		// repair failure.
		if err := s.persistPriorEvidence(ctx, sessionID, requestedID, activation.PriorEvidence); err != nil {
			return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID, RepairPending: true}, &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: requestedID, Repair: ingest.GenerationRepairPriorPersist}
		}
		if err := s.finishCommittedActivationLocked(ctx, sessionID, requestedID); err != nil {
			var repairPending *ingest.GenerationRepairPendingError
			if errors.As(err, &repairPending) {
				return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID, RepairPending: true}, err
			}
			return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID}, err
		}
		return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID}, nil
	}

	staged, err := s.stageWithIntent(ctx, sessionID, activation.Generation.Generation, activation.Blobs, activation)
	if err != nil {
		return notCommitted(err)
	}
	if err := (generationIndexFormat{}).Validate(indexformat.V2{Generation: staged}); err != nil {
		// The validator text is untrusted: a candidate title ref or another
		// field can carry a private path, so refuse with a fixed category
		// against the already-validated identities and never wrap the raw
		// validator error.
		return notCommitted(fmt.Errorf("store: refuse to activate an invalid managed generation %s for session %s: the staged candidate failed managed generation validation; the staged candidate is retained and the prior generation is unchanged", staged.ID, sessionID))
	}
	metadataJSON, err := json.Marshal(staged.Metadata)
	if err != nil {
		return notCommitted(fmt.Errorf("store: encode exported metadata for generation %s of session %s: %w; activation was refused and the prior generation is unchanged", staged.ID, sessionID, err))
	}

	capture := activation.ContentCapture
	if capture.CaptureFormat == "" {
		capture.CaptureFormat = ingest.ContentCaptureFormatPreviewOnly
	}
	if capture.Status == "" {
		capture.Status = ingest.ContentCaptureIncomplete
	}
	if capture.SourceAuthority == "" {
		capture.SourceAuthority = ingest.ContentSourceNone
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sessionID,
		Result:             indexformat.V2{Generation: staged},
		IndexVersion:       2,
		Mode:               ingest.SessionEntryWriteReplaceAll,
		RequireFullContent: captureRequiresFullContent(capture),
		ContentCapture:     capture,
		IndexerVersion:     activation.IndexerVersion,
		IndexedAtMs:        activation.IndexedAtMs,
		ExpectedState:      activation.ExpectedState,
		CaptureRevision:    activation.CaptureRevision,
		IndexedInputHash:   activation.IndexedInputHash,
		ArtifactIdentity:   activation.ArtifactIdentity,
		PublicationCapture: activation.Capture,
	}})
	for _, result := range results {
		if result.Err != nil {
			return notCommitted(fmt.Errorf("store: activate generation %s for session %s: %w; the database transaction rolled back and the prior generation is visible; the synced candidate is retained for retry", staged.ID, sessionID, result.Err))
		}
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadataJSON); err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID, RepairPending: true}, &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: requestedID, Repair: ingest.GenerationRepairMetadata}
	}
	if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID, RepairPending: true}, &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: requestedID, Repair: ingest.GenerationRepairIntentClear}
	}
	return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, nil
}

// RecoverGenerationActivation reconciles one session's pending intent under the
// exclusive lock. It is safe to call at any time; a committed generation is
// re-repaired from its committed row and its intent cleared, while an
// uncommitted synced candidate is activated by replaying the persisted
// envelope with the original guarded preconditions. A stale or unverified
// candidate stays inactive for a verified retry.
//
// The outcome carries the lock-derived disposition for the recovered intent:
// CommittedNow when recovery newly committed it, AlreadyCommitted when it was
// already active before, NotCommitted when nothing was committed by this call.
// A post-commit repair failure returns a GenerationRepairPendingError with
// committed authority, never a rollback.
func (s *Store) RecoverGenerationActivation(ctx context.Context, sessionID schema.SessionID) (ingest.ActivationOutcome, error) {
	if err := s.requireGenerationSupport(); err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted}, err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted}, err
	}
	defer func() { _ = release() }()
	intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
	if err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted}, err
	}
	if intent == nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted}, nil
	}
	recoveredID := intent.GenerationID
	activeBefore, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: recoveredID}, err
	}
	if err := s.recoverGenerationIntentLocked(ctx, sessionID); err != nil {
		var repairPending *ingest.GenerationRepairPendingError
		if errors.As(err, &repairPending) && repairPending.CandidateID == recoveredID {
			activeAfter, activeErr := s.activeGenerationID(ctx, sessionID)
			if activeErr == nil && activeAfter == recoveredID {
				if activeBefore == recoveredID {
					return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: recoveredID, RepairPending: true}, err
				}
				return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: recoveredID, RepairPending: true}, err
			}
		}
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: recoveredID}, err
	}
	activeAfter, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: recoveredID}, err
	}
	if activeAfter == recoveredID {
		if activeBefore == recoveredID {
			return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: recoveredID}, nil
		}
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: recoveredID}, nil
	}
	return ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: recoveredID}, nil
}

func (s *Store) recoverGenerationIntentLocked(ctx context.Context, sessionID schema.SessionID) error {
	intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
	if err != nil || intent == nil {
		return err
	}
	if err := validateGenerationID(intent.GenerationID); err != nil {
		// A malformed intent can never name an owned generation; refuse it
		// without touching the active read authority.
		return fmt.Errorf("store: pending activation for session %s names an invalid generation: %w; the active generation is unchanged", sessionID, err)
	}
	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == intent.GenerationID {
		// The generation install already committed before its prior document
		// is persisted, so a persist failure here is post-commit repair with
		// committed authority, never a pre-commit refusal.
		if err := s.persistPriorEvidence(ctx, sessionID, intent.GenerationID, intent.PriorEvidence); err != nil {
			return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairPriorPersist}
		}
		metadata, err := s.exportedMetadataForGeneration(ctx, sessionID, intent.GenerationID)
		if err != nil {
			return err
		}
		if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadata); err != nil {
			return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairMetadata}
		}
		if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
			return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairIntentClear}
		}
		return nil
	}
	generation, err := s.loadManifestGeneration(ctx, sessionID, intent.GenerationID)
	if err != nil {
		if errors.Is(err, ErrStagedGenerationAbsent) {
			// The candidate was never renamed into place; the intent is stale
			// and no new generation exists. Clearing it is safe: the active
			// generation was never touched.
			return s.generationArtifacts.ClearIntent(ctx, sessionID)
		}
		return fmt.Errorf("store: recover pending activation for session %s: %w; the synced candidate was preserved and the prior generation is unchanged", sessionID, err)
	}
	// Verify the envelope binding before replaying anything: the staged bytes
	// must still produce the digest the intent recorded. A rejected candidate
	// whose envelope was replaced, or a manifest that changed under the intent,
	// stays inactive for a verified retry instead of being activated under the
	// wrong stamps.
	stagedDigest, err := computeActivationBinding(generation, bindingFromStaged)
	if err != nil {
		// The staged manifest is untrusted: a content reference can carry a
		// private path. The wrapped error is the fixed, reference-free binding
		// category, so the refusal names only the validated requested
		// identities and never the manifest detail.
		return fmt.Errorf("store: recover pending activation for session %s generation %s: %w; the synced candidate was preserved and the prior generation is unchanged; retry a verified activation", sessionID, intent.GenerationID, err)
	}
	if intent.CandidateDigest == "" || stagedDigest != intent.CandidateDigest {
		return fmt.Errorf("store: refuse pending activation for session %s generation %s in recoverGenerationIntentLocked: the durable intent does not bind to the staged candidate bytes; the candidate was preserved and the prior generation is unchanged; retry a verified activation", sessionID, intent.GenerationID)
	}
	// Replay the persisted envelope with the original guarded preconditions:
	// the producing revision and time, the captured compare-and-swap state,
	// the input proof and pair identity, and the content-capture evidence. A
	// candidate whose original compare-and-swap failed is refused again here
	// instead of bypassing the stale rejection, and an incomplete candidate
	// replays without success stamps.
	capture := intent.ContentCapture
	if capture.CaptureFormat == "" {
		capture.CaptureFormat = ingest.ContentCaptureFormatPreviewOnly
	}
	if capture.Status == "" {
		capture.Status = ingest.ContentCaptureIncomplete
	}
	if capture.SourceAuthority == "" {
		capture.SourceAuthority = ingest.ContentSourceNone
	}
	indexerVersion := intent.IndexerVersion
	indexedAtMs := intent.IndexedAtMs
	indexedInputHash := intent.IndexedInputHash
	if generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		indexerVersion = 0
		indexedAtMs = 0
		indexedInputHash = nil
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sessionID,
		Result:             indexformat.V2{Generation: generation},
		IndexVersion:       2,
		Mode:               ingest.SessionEntryWriteReplaceAll,
		RequireFullContent: captureRequiresFullContent(capture),
		ContentCapture:     capture,
		IndexerVersion:     indexerVersion,
		IndexedAtMs:        indexedAtMs,
		ExpectedState:      intent.ExpectedState,
		CaptureRevision:    intent.CaptureRevision,
		IndexedInputHash:   indexedInputHash,
		ArtifactIdentity:   intent.ArtifactIdentity,
		PublicationCapture: intent.PublicationCapture,
	}})
	for _, result := range results {
		if result.Err != nil {
			// Preserve the typed compare-and-swap refusal so the caller's
			// verified-retry path keeps working; its message names only the
			// validated session identifier.
			var stale *ingest.StaleIndexWorkError
			if errors.As(result.Err, &stale) {
				return fmt.Errorf("store: recover pending activation for session %s generation %s: %w; the prior generation is preserved and the candidate is retained", sessionID, intent.GenerationID, result.Err)
			}
			// Every other replay failure can carry untrusted candidate detail:
			// the managed validator echoes title and content references. The
			// refusal names only the validated requested identities.
			return fmt.Errorf("store: recover pending activation for session %s generation %s: the managed generation transaction refused the staged candidate; the prior generation is preserved and the candidate is retained; retry a verified activation", sessionID, intent.GenerationID)
		}
	}
	if err := s.persistPriorEvidence(ctx, sessionID, intent.GenerationID, intent.PriorEvidence); err != nil {
		return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairPriorPersist}
	}
	metadataJSON, err := json.Marshal(generation.Metadata)
	if err != nil {
		return fmt.Errorf("store: recover pending activation for session %s: encode metadata: %w", sessionID, err)
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadataJSON); err != nil {
		return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairMetadata}
	}
	if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
		return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: intent.GenerationID, Repair: ingest.GenerationRepairIntentClear}
	}
	return nil
}

// captureRequiresFullContent reports whether an activation's content capture
// certifies the full stored content. A native managed generation carries every
// content record it names, so its activation writes through the full-content
// path; a caller that supplies no capture keeps the bounded preview default.
func captureRequiresFullContent(capture ingest.SessionContentCaptureWrite) bool {
	return capture.CaptureFormat == ingest.ContentCaptureFormatFull
}

// persistPriorEvidence writes the opaque harness prior document beside one
// staged generation. An empty document leaves any existing prior evidence
// untouched; the committed generation rows remain the alias authority.
func (s *Store) persistPriorEvidence(ctx context.Context, sessionID schema.SessionID, generationID string, evidence []byte) error {
	if len(evidence) == 0 {
		return nil
	}
	if err := s.generationArtifacts.WritePriorEvidence(ctx, sessionID, generationID, evidence); err != nil {
		return err
	}
	return nil
}

// finishCommittedActivationLocked re-repairs an already-committed generation
// from its committed database row, never from caller-supplied metadata. A
// repair failure returns a GenerationRepairPendingError with committed
// authority, never a rollback.
func (s *Store) finishCommittedActivationLocked(ctx context.Context, sessionID schema.SessionID, generationID string) error {
	metadata, err := s.exportedMetadataForGeneration(ctx, sessionID, generationID)
	if err != nil {
		return err
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadata); err != nil {
		return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: generationID, Repair: ingest.GenerationRepairMetadata}
	}
	if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
		return &ingest.GenerationRepairPendingError{SessionID: string(sessionID), CandidateID: generationID, Repair: ingest.GenerationRepairIntentClear}
	}
	return nil
}

func (s *Store) finishActivationLocked(ctx context.Context, sessionID schema.SessionID, generation indexformat.Generation) error {
	return s.finishCommittedActivationLocked(ctx, sessionID, generation.ID)
}

// loadManifestGeneration reads the self-contained manifest staged for the
// pending candidate. The manifest is the immutable projection, so recovery
// never re-derives entries from a mutable native source.
func (s *Store) loadManifestGeneration(ctx context.Context, sessionID schema.SessionID, generationID string) (indexformat.Generation, error) {
	return s.generationArtifacts.ReadManifest(ctx, sessionID, generationID)
}

func (s *Store) exportedMetadataForGeneration(ctx context.Context, sessionID schema.SessionID, generationID string) ([]byte, error) {
	metadata, err := s.generationMetadata(ctx, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(metadata)
}

// activeGenerationID reads the session's committed active pointer.
func (s *Store) activeGenerationID(ctx context.Context, sessionID schema.SessionID) (string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: take connection to read the active generation for %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return "", err
	}
	if active == nil {
		return "", nil
	}
	return *active, nil
}

func readActiveGenerationOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (*string, error) {
	var active *string
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT active_generation_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if stmt.ColumnType(0) != sqlite.TypeNull {
				value := stmt.ColumnText(0)
				active = &value
			}
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read active generation for session %s: %w; no generation read was authorized", sessionID, err)
	}
	if !found {
		return nil, fmt.Errorf("store: session %s has no metadata row; import the session before reading its generation", sessionID)
	}
	return active, nil
}

// stageWithIntent records the durable activation envelope before staging the
// generation files, so a crash after the atomic rename is recoverable by
// replaying the same guarded transaction and a crash before the rename leaves
// only a stale intent that recovery clears. The envelope is bound to the
// complete candidate digest, and any identifier collision is rejected BEFORE
// the intent is replaced, so a refused candidate cannot leave an activatable
// envelope bound to older staged bytes.
func (s *Store) stageWithIntent(ctx context.Context, sessionID schema.SessionID, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte, activation GenerationActivation) (indexformat.Generation, error) {
	if err := validateGenerationID(generation.ID); err != nil {
		return indexformat.Generation{}, err
	}
	candidateDigest, err := computeActivationBinding(generation, bindingFromBlobs(blobs))
	if err != nil {
		// The wrapped error is the fixed, reference-free binding category; a
		// missing captured blob is untrusted candidate detail and is never
		// echoed.
		return indexformat.Generation{}, fmt.Errorf("store: bind activation for generation %s of session %s: %w; the candidate was not staged and any installed generation is unchanged", generation.ID, sessionID, err)
	}
	if err := s.verifyImmutableCandidateIdentity(ctx, sessionID, generation, candidateDigest); err != nil {
		return indexformat.Generation{}, err
	}
	capture := activation.ContentCapture
	if capture.CaptureFormat == "" {
		capture.CaptureFormat = ingest.ContentCaptureFormatPreviewOnly
	}
	if capture.Status == "" {
		capture.Status = ingest.ContentCaptureIncomplete
	}
	if capture.SourceAuthority == "" {
		capture.SourceAuthority = ingest.ContentSourceNone
	}
	indexerVersion := activation.IndexerVersion
	indexedAtMs := activation.IndexedAtMs
	indexedInputHash := activation.IndexedInputHash
	if generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		indexerVersion = 0
		indexedAtMs = 0
		indexedInputHash = nil
	}
	if err := s.generationArtifacts.WriteIntent(ctx, GenerationIntent{
		SessionID:          sessionID,
		GenerationID:       generation.ID,
		ManifestPath:       "generations/" + generation.ID + "/manifest.json",
		Completeness:       string(generation.Completeness),
		StagedAtMs:         time.Now().UnixMilli(),
		IndexerVersion:     indexerVersion,
		IndexedAtMs:        indexedAtMs,
		CaptureRevision:    activation.CaptureRevision,
		ExpectedState:      activation.ExpectedState,
		ContentCapture:     capture,
		IndexedInputHash:   indexedInputHash,
		ArtifactIdentity:   activation.ArtifactIdentity,
		PublicationCapture: activation.Capture,
		PriorEvidence:      activation.PriorEvidence,
		CandidateDigest:    candidateDigest,
	}); err != nil {
		return indexformat.Generation{}, err
	}
	staged, err := s.generationArtifacts.Stage(ctx, generation, blobs)
	if err != nil {
		return indexformat.Generation{}, err
	}
	if err := s.persistPriorEvidence(ctx, sessionID, staged.ID, activation.PriorEvidence); err != nil {
		return indexformat.Generation{}, err
	}
	return staged, nil
}

// verifyImmutableCandidateIdentity refuses an identifier collision before the
// activation intent is replaced. When no candidate is installed the activation
// proceeds; when one is installed it must bind to the identical complete
// candidate (the same whole manifest and every blob digest), and an
// unverifiable installed manifest is refused rather than overwritten.
func (s *Store) verifyImmutableCandidateIdentity(ctx context.Context, sessionID schema.SessionID, generation indexformat.Generation, candidateDigest string) error {
	installed, err := s.generationArtifacts.ReadManifest(ctx, sessionID, generation.ID)
	if err != nil {
		if errors.Is(err, ErrStagedGenerationAbsent) {
			return nil
		}
		return fmt.Errorf("store: verify installed candidate for generation %s of session %s before staging: %w; the candidate was not staged and any installed bytes are unchanged", generation.ID, sessionID, err)
	}
	installedDigest, err := computeActivationBinding(installed, bindingFromStaged)
	if err != nil {
		// The installed manifest is untrusted; its content references can carry
		// private paths. The wrapped error is the fixed, reference-free binding
		// category for the validated requested identities.
		return fmt.Errorf("store: verify installed candidate for generation %s of session %s before staging: %w; the candidate was not staged and any installed bytes are unchanged", generation.ID, sessionID, err)
	}
	if installed.ID != generation.ID || installed.Metadata.SessionID != sessionID || installedDigest != candidateDigest {
		return fmt.Errorf("store: refuse to stage generation %s for session %s in verifyImmutableCandidateIdentity: the identifier is already installed with different candidate evidence; immutable identifiers cannot be reused; the installed generation is unchanged", generation.ID, sessionID)
	}
	return nil
}

// CleanupInactiveGeneration removes one owned inactive generation directory
// under the exclusive session lock. It centrally validates the generation
// identifier, reconciles the pending intent, verifies the target is an owned
// inactive generation (committed or staged, never active and never intent
// owned), and only then removes its files. Because it takes the same lock
// readers share, it waits for any reader that is still hydrating or
// serializing a snapshot of it.
func (s *Store) CleanupInactiveGeneration(ctx context.Context, sessionID schema.SessionID, generationID string) error {
	if err := s.requireGenerationSupport(); err != nil {
		return err
	}
	if err := validateGenerationID(generationID); err != nil {
		return err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == generationID {
		return fmt.Errorf("store: refuse to remove generation %s for session %s: it is the active read authority; select an inactive generation or leave managed recovery to replace it", generationID, sessionID)
	}
	intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
	if err != nil {
		return err
	}
	if intent != nil && intent.GenerationID == generationID {
		return fmt.Errorf("store: refuse to remove generation %s for session %s: it owns the pending activation intent; recover or retry the activation before cleaning it", generationID, sessionID)
	}
	if err := s.verifyOwnedInactiveGeneration(ctx, sessionID, generationID); err != nil {
		return err
	}
	return s.generationArtifacts.RemoveGeneration(ctx, sessionID, generationID)
}

// verifyOwnedInactiveGeneration proves the requested target is an owned
// inactive generation before any recursive deletion: it must be committed in
// the catalog or staged on disk, and it must not be the active pointer.
func (s *Store) verifyOwnedInactiveGeneration(ctx context.Context, sessionID schema.SessionID, generationID string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to verify inactive generation %s for session %s: %w", generationID, sessionID, err)
	}
	defer s.pool.Put(conn)
	committed := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_projection_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { committed = true; return nil },
	}); err != nil {
		return fmt.Errorf("store: verify inactive generation %s for session %s: %w", generationID, sessionID, err)
	}
	if committed {
		return nil
	}
	// No committed row: the target is owned only when its staged manifest is a
	// valid generation that names this exact session and generation. A
	// decodable manifest is not ownership: an empty {}, malformed, mismatched
	// or unknown manifest is refused, so an unrelated or foreign-owner
	// directory is never removed.
	staged, err := s.generationArtifacts.ReadManifest(ctx, sessionID, generationID)
	if err != nil {
		return fmt.Errorf("store: refuse to remove generation %s for session %s: it is neither a committed nor a staged owned generation; the directory was left in place", generationID, sessionID)
	}
	if staged.ID != generationID || staged.Metadata.SessionID != sessionID {
		return fmt.Errorf("store: refuse to remove generation %s for session %s: the staged manifest identity does not match the requested owner; ownership could not be proven; the directory was left in place", generationID, sessionID)
	}
	if err := staged.Validate(); err != nil {
		return fmt.Errorf("store: refuse to remove generation %s for session %s: the staged manifest identity matches but the generation is not valid and self-contained; the directory was left in place", generationID, sessionID)
	}
	return nil
}

func (s *Store) requireGenerationSupport() error {
	if s.generationArtifacts == nil || s.sessionLocker == nil {
		return fmt.Errorf("store: managed generation support is not configured; V2 activation and generation snapshots cannot run; open the store with WithGenerationArtifacts and an owned-artifact root")
	}
	if !s.SupportsIndexFormat(2) {
		return fmt.Errorf("store: index format 2 is not registered; the managed generation cannot be installed; open the store with WithIndexFormats(generationIndexFormat{})")
	}
	return nil
}

// generationMetadata reads the durable UnifiedMetadata of one committed
// generation. Metadata.Stats is the single count authority.
func (s *Store) generationMetadata(ctx context.Context, sessionID schema.SessionID, generationID string) (schema.UnifiedMetadata, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return schema.UnifiedMetadata{}, fmt.Errorf("store: take connection to read generation metadata for %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	return readGenerationMetadataOnConn(conn, sessionID, generationID)
}

func readGenerationMetadataOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (schema.UnifiedMetadata, error) {
	var metadata schema.UnifiedMetadata
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_json FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			return json.Unmarshal([]byte(stmt.ColumnText(0)), &metadata)
		},
	}); err != nil {
		return schema.UnifiedMetadata{}, fmt.Errorf("store: read generation metadata for session %s generation %s: %w; no generation read was authorized", sessionID, generationID, err)
	}
	if !found {
		return schema.UnifiedMetadata{}, fmt.Errorf("store: session %s has no committed generation %s; the snapshot cannot be loaded", sessionID, generationID)
	}
	return metadata, nil
}
