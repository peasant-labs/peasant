package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// GenerationActivation is one immutable V2 candidate plus the content
// bytes its non-emitted records address. No file is written anywhere: the
// candidate stages digest-addressed objects without a lock, and the commit
// installs the generation rows, the count mirrors and the active pointer
// together in one transaction. An incomplete_new candidate leaves success
// and indexed-at unset so maintenance stays required; only a complete
// candidate may carry a positive producing indexer revision.
type GenerationActivation struct {
	Generation     indexformat.V2
	Blobs          map[schema.SourceEntryRef][]byte
	IndexerVersion int
	IndexedAtMs    int64
	ExpectedState  *ingest.SessionIndexState
	ContentCapture ingest.SessionContentCaptureWrite
	// ExplicitRebuild marks an operator-initiated rebuild (harvest index
	// --force, Reindex) that deliberately replaces full read authority with a
	// preview. It exempts the last-good preview-over-full refusal on the same
	// principle as a format conversion; accidental previews stay refused.
	ExplicitRebuild bool
	// PriorEvidence is the opaque activation-owned document the commit
	// persists beside the generation row. Nil leaves prior evidence absent;
	// the committed generation rows still supply aliases.
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
	// Prepared is the lock-free object staging StageGeneration produced for
	// this candidate. When set, activation installs those staged objects
	// instead of writing them again; nil stages inline.
	Prepared *PreparedGeneration
	// ArtifactIdentity is the pair identity this parse consumed, established
	// when the captured state records none.
	ArtifactIdentity *string
}

// ActivateGeneration is the production V2 activation entry point (design
// §4.1): prepare without a lock (P1–P4), compare with the active
// generation (P5) with repair bypass, stage objects without a lock
// (S0–S2), commit one generation over them (C0–C6), and run the
// post-commit delete pass for a converted predecessor. A crash at any seam
// leaves exactly the old or the new generation visible: before S1 nothing
// changes; after S1 or S2 orphan objects remain with the flag set and the
// old generation stays active and readable; after the commit the new
// generation is active and the old one awaits the sweep. Every case
// recovers on the next harvest, which re-selects and sweeps.
//
// The outcome carries the lock-derived disposition for the REQUESTED
// candidate, independent of repair state, so per-invocation counts stay
// truthful: only CommittedNow counts once; Skipped, AlreadyCommitted and
// NotCommitted count zero.
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
	if activation.Generation.Generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		activation.IndexerVersion = 0
		activation.IndexedAtMs = 0
		activation.IndexedInputHash = nil
	}
	prepared, err := prepareHarmonizedCandidate(sessionID, activation.Generation.Generation, activation.Blobs)
	if err != nil {
		return notCommitted(err)
	}
	if err := reportHarmonizedWriterSeam(harmonizedSeamAfterPrepare); err != nil {
		return notCommitted(err)
	}
	if err := s.checkImmutableIdentity(ctx, sessionID, requestedID, prepared.binding); err != nil {
		var already *alreadyCommittedError
		if errors.As(err, &already) {
			return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID}, nil
		}
		return notCommitted(err)
	}
	if repair, err := s.needsRepair(ctx, sessionID); err != nil {
		return notCommitted(err)
	} else if repair {
		stamps := activationStamps(activation, prepared)
		if err := s.lockedRepair(ctx, sessionID, prepared, stamps); err != nil {
			return notCommitted(err)
		}
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, nil
	}
	if active, err := s.readActiveView(ctx, sessionID); err != nil {
		return notCommitted(err)
	} else if active.found && refreshEqualsActive(prepared, active.view) {
		stamps := activationStamps(activation, prepared)
		if err := s.lockedSkip(ctx, prepared, active.generationID, stamps); err != nil {
			if errors.Is(err, errSkipPointerMoved) {
				goto writePath
			}
			return notCommitted(err)
		}
		return ingest.ActivationOutcome{Disposition: ingest.ActivationSkipped, CandidateID: requestedID}, nil
	}
writePath:
	if activation.Prepared != nil && activation.Prepared.claim(sessionID, requestedID) {
		activation.Prepared.markInstalled()
	} else {
		if activation.Prepared != nil {
			activation.Prepared.discardUnlessInstalled()
			activation.Prepared = nil
		}
		if err := s.stageHarmonizedSession(ctx, prepared); err != nil {
			return notCommitted(err)
		}
	}
	if activation.Prepared != nil {
		defer activation.Prepared.discardUnlessInstalled()
	}
	if err := reportHarmonizedWriterSeam(harmonizedSeamBeforeCommit); err != nil {
		return notCommitted(err)
	}
	committed, err := s.commitHarmonizedWrite(ctx, sessionID, activation, prepared)
	if err != nil {
		return notCommitted(err)
	}
	if !committed {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationAlreadyCommitted, CandidateID: requestedID}, nil
	}
	if err := reportHarmonizedWriterSeam(harmonizedSeamAfterCommit); err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, err
	}
	if err := s.deleteConvertedMirrorRows(ctx, sessionID); err != nil {
		return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, err
	}
	return ingest.ActivationOutcome{Disposition: ingest.ActivationCommittedNow, CandidateID: requestedID}, nil
}

// alreadyCommittedError reports an idempotent retry: the requested
// identifier is installed with the identical candidate binding, so there
// is nothing to commit.
type alreadyCommittedError struct {
	sessionID    schema.SessionID
	generationID string
}

func (e *alreadyCommittedError) Error() string {
	return fmt.Sprintf("store: generation %s for session %s is already installed with the identical candidate binding; the retry committed nothing", e.generationID, e.sessionID)
}

// checkImmutableIdentity refuses an identifier collision before anything is
// staged: when no candidate is installed the activation proceeds; when one
// is installed it must bind to the identical candidate (the same catalog
// values and every digest), and the retry reports AlreadyCommitted. An
// identifier present in the file-backed table is always refused, because
// only the migration moves it.
func (s *Store) checkImmutableIdentity(ctx context.Context, sessionID schema.SessionID, generationID, binding string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to check generation identity for session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	var stored string
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			stored = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: check generation identity for session %s: %w", sessionID, err)
	}
	if found {
		if stored != binding {
			return fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is already installed with a different candidate binding; immutable identifiers cannot be reused; the installed generation is unchanged", generationID, sessionID)
		}
		return &alreadyCommittedError{sessionID: sessionID, generationID: generationID}
	}
	fileBacked := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_projection_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { fileBacked = true; return nil },
	}); err != nil {
		return fmt.Errorf("store: check the file-backed catalog for session %s: %w", sessionID, err)
	}
	if fileBacked {
		return fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is installed in the file-backed catalog, which only the migration moves; the installed generation is unchanged", generationID, sessionID)
	}
	return nil
}

// needsRepair reports whether the session needs repair-mode activation: it
// matches the repair predicate while its active generation is harmonized.
func (s *Store) needsRepair(ctx context.Context, sessionID schema.SessionID) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection to check the repair predicate for session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	return repairNeedsHarmonized(conn, sessionID)
}

// activeView is one snapshot read of the active harmonized generation for
// the skip comparison.
type activeView struct {
	found        bool
	generationID string
	view         activeHarmonizedView
}

// readActiveView loads the active harmonized generation without a lock.
func (s *Store) readActiveView(ctx context.Context, sessionID schema.SessionID) (activeView, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return activeView{}, fmt.Errorf("store: take connection to read the active generation for session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	view, err := readActiveHarmonizedView(conn, sessionID)
	if err != nil {
		return activeView{}, err
	}
	return activeView{found: view.found, generationID: view.generationID, view: view}, nil
}

// lockedSkip runs the skip bookkeeping under the exclusive session lock.
func (s *Store) lockedSkip(ctx context.Context, prepared *preparedHarmonized, comparedID string, stamps skipStamps) error {
	release, err := s.sessionLocker.LockExclusive(ctx, prepared.sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	return stampSkippedHarmonized(ctx, s, prepared, comparedID, stamps)
}

// lockedRepair runs the repair transaction under the exclusive session
// lock: it re-reads the active pointer inside, bypasses the skip
// comparison, inserts no new generation row, and rewrites the failing
// objects in place with the ordinary bookkeeping.
func (s *Store) lockedRepair(ctx context.Context, sessionID schema.SessionID, prepared *preparedHarmonized, stamps skipStamps) error {
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for the repair guard of session %s: %w", sessionID, err)
	}
	active, err := readActiveGenerationOnConn(conn, sessionID)
	s.pool.Put(conn)
	if err != nil {
		return err
	}
	if active == nil || *active == "" {
		return fmt.Errorf("store: session %s needs repair but names no active generation; convert it through the ordinary write path instead", sessionID)
	}
	return repairHarmonizedObjects(ctx, s, prepared, *active, stamps)
}

// activationStamps maps one activation to the bookkeeping stamps the
// commit, the skip, and the repair share.
func activationStamps(activation GenerationActivation, prepared *preparedHarmonized) skipStamps {
	return skipStamps{
		publicationCapture: activation.Capture,
		captureRevision:    activation.CaptureRevision,
		indexerVersion:     activation.IndexerVersion,
		indexedAtMs:        activation.IndexedAtMs,
		indexedInputHash:   activation.IndexedInputHash,
		artifactIdentity:   activation.ArtifactIdentity,
		adapterVersion:     prepared.record.AdapterVersion,
		stats:              prepared.generation.Metadata.Stats,
		seedJSON:           seedJSONForStats(prepared.generation.Metadata.Stats),
		updatedAtMs:        activation.IndexedAtMs,
	}
}

// commitHarmonizedWrite commits one staged candidate through the batched
// writer under the exclusive session lock: one outer transaction with a
// per-session savepoint, so the compare-and-swap refusal and every C-step
// share the commit. It reports whether the write committed (a concurrent
// identical commit reports AlreadyCommitted instead).
func (s *Store) commitHarmonizedWrite(ctx context.Context, sessionID schema.SessionID, activation GenerationActivation, prepared *preparedHarmonized) (bool, error) {
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return false, err
	}
	defer func() { _ = release() }()
	// Staging complete: hand the candidate to the batched writer. The V2
	// result carries filled records (completed from the staged bytes) and
	// the prior evidence beside the generation, so validation sees the
	// staged shape and the commit persists the evidence in the same
	// transaction as the catalog row.
	v2 := activation.Generation
	v2.Generation.Content = fillContentRecords(v2.Generation.Content, activation.Blobs)
	v2.PriorEvidence = activation.PriorEvidence
	mode := ingest.SessionEntryWriteReplaceAll
	if activation.ExplicitRebuild {
		mode = ingest.SessionEntryWriteExplicitRebuild
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sessionID,
		Result:             v2,
		IndexVersion:       2,
		Mode:               mode,
		RequireFullContent: captureRequiresFullContent(activation.ContentCapture),
		ContentCapture:     activation.ContentCapture,
		IndexerVersion:     activation.IndexerVersion,
		IndexedAtMs:        activation.IndexedAtMs,
		ExpectedState:      activation.ExpectedState,
		CaptureRevision:    activation.CaptureRevision,
		IndexedInputHash:   activation.IndexedInputHash,
		ArtifactIdentity:   activation.ArtifactIdentity,
		PublicationCapture: activation.Capture,
	}})
	if len(results) != 1 {
		return false, fmt.Errorf("store: activate generation %s for session %s: the batched writer returned %d results; the prior generation is unchanged", prepared.generation.ID, sessionID, len(results))
	}
	if results[0].Err != nil {
		var stale *ingest.StaleIndexWorkError
		if errors.As(results[0].Err, &stale) {
			return false, results[0].Err
		}
		return false, fmt.Errorf("store: activate generation %s for session %s: %w; the database transaction rolled back and the prior generation is visible", prepared.generation.ID, sessionID, results[0].Err)
	}
	if results[0].Skipped {
		return false, nil
	}
	return true, nil
}

// captureRequiresFullContent reports whether an activation's content capture
// certifies the full stored content. A native managed generation carries every
// content record it names, so its activation writes through the full-content
// path; a caller that supplies no capture keeps the bounded preview default.
func captureRequiresFullContent(capture ingest.SessionContentCaptureWrite) bool {
	return capture.CaptureFormat == ingest.ContentCaptureFormatFull
}

// PreparedGeneration is one candidate's lock-free object staging: every
// body and blob is written idempotently without holding any lock, but no
// generation row exists and nothing is installed. It is opaque to callers
// and only ActivateGeneration (through GenerationActivation.Prepared)
// commits it. A handle that is never activated leaves only staged objects,
// which the flagged sweep clears; recovery can never activate it because no
// generation row records it.
type PreparedGeneration struct {
	sessionID    schema.SessionID
	generationID string
	prepared     *preparedHarmonized
	installed    atomic.Bool
}

// NativeGenerationCandidateID names the candidate the handle prepared. It
// marks the handle as a staged harmonized generation.
func (p *PreparedGeneration) NativeGenerationCandidateID() string {
	if p == nil {
		return ""
	}
	return p.generationID
}

// claim reports whether the handle can install the requested candidate. A
// handle is single-use: once claimed it never installs again.
func (p *PreparedGeneration) claim(sessionID schema.SessionID, generationID string) bool {
	if p == nil || p.prepared == nil || p.sessionID != sessionID || p.generationID != generationID {
		return false
	}
	return p.installed.CompareAndSwap(false, true)
}

// markInstalled records that the claimed handle's objects were committed.
// It only flips the single-use guard; the objects were already staged.
func (p *PreparedGeneration) markInstalled() {
	if p == nil {
		return
	}
	p.installed.Store(true)
}

// discardUnlessInstalled drops a prepared handle that no activation claimed.
// Staged objects are digest-addressed and idempotent: a later activation
// reuses them, and the flagged sweep clears whatever stays unreferenced.
func (p *PreparedGeneration) discardUnlessInstalled() {
	if p == nil {
		return
	}
	p.installed.CompareAndSwap(false, true)
}

// StageGeneration prepares one managed generation's objects WITHOUT making
// it activatable: with no lock it validates, encodes, digests, classifies,
// and idempotently stages every body and blob, and returns a handle. It
// installs no generation row and records no intent, so a candidate that a
// caller later refuses can never become authority. Several sessions may
// prepare concurrently; each writes only digest-addressed rows. A store
// whose generation support is not configured refuses here, and activation
// stages inline instead.
func (s *Store) StageGeneration(ctx context.Context, activation GenerationActivation) (*PreparedGeneration, error) {
	requestedID := activation.Generation.Generation.ID
	if err := s.requireGenerationSupport(); err != nil {
		return nil, err
	}
	if err := validateGenerationID(requestedID); err != nil {
		return nil, err
	}
	sessionID := activation.Generation.Generation.Metadata.SessionID
	handle := &PreparedGeneration{sessionID: sessionID, generationID: requestedID}
	prepared, err := prepareHarmonizedCandidate(sessionID, activation.Generation.Generation, activation.Blobs)
	if err != nil {
		return nil, err
	}
	if err := reportHarmonizedWriterSeam(harmonizedSeamAfterPrepare); err != nil {
		return nil, err
	}
	if err := s.stageHarmonizedSession(ctx, prepared); err != nil {
		return nil, err
	}
	handle.prepared = prepared
	return handle, nil
}

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
