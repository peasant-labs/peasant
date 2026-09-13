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
// canonical main projection, the count mirror and the active pointer together.
type GenerationActivation struct {
	Generation     indexformat.V2
	Blobs          map[schema.SourceEntryRef][]byte
	IndexerVersion int
	IndexedAtMs    int64
	ExpectedState  *ingest.SessionIndexState
	ContentCapture ingest.SessionContentCaptureWrite
}

// ActivateGeneration is the production V2 activation entry point. It takes the
// exclusive per-session lock, reconciles any prior intent, stages and fsyncs the
// generation files, records a synced intent, installs the generation in ONE
// SQLite transaction, repairs the exported metadata, and clears the intent.
// A crash at any seam leaves exactly G1 (the prior generation) or G2 (the new
// generation) visible; recovery is idempotent.
func (s *Store) ActivateGeneration(ctx context.Context, activation GenerationActivation) error {
	if err := s.requireGenerationSupport(); err != nil {
		return err
	}
	sessionID := activation.Generation.Generation.Metadata.SessionID
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	if err := s.recoverGenerationIntentLocked(ctx, sessionID); err != nil {
		return err
	}

	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == activation.Generation.Generation.ID {
		// A retry after a crash that already committed. Re-repair the exported
		// metadata and clear the intent; nothing else changes.
		return s.finishActivationLocked(ctx, sessionID, activation.Generation.Generation)
	}

	staged, err := s.stageWithIntent(ctx, sessionID, activation.Generation.Generation, activation.Blobs)
	if err != nil {
		return err
	}
	if err := (generationIndexFormat{}).Validate(indexformat.V2{Generation: staged}); err != nil {
		return fmt.Errorf("store: refuse to activate an invalid managed generation for session %s: %w; the staged candidate is retained and the prior generation is unchanged", sessionID, err)
	}
	metadataJSON, err := json.Marshal(staged.Metadata)
	if err != nil {
		return fmt.Errorf("store: encode exported metadata for generation %s of session %s: %w; activation was refused and the prior generation is unchanged", staged.ID, sessionID, err)
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
		RequireFullContent: false,
		ContentCapture:     capture,
		IndexerVersion:     activation.IndexerVersion,
		IndexedAtMs:        activation.IndexedAtMs,
		ExpectedState:      activation.ExpectedState,
	}})
	for _, result := range results {
		if result.Err != nil {
			return fmt.Errorf("store: activate generation %s for session %s: %w; the database transaction rolled back and the prior generation is visible; the synced candidate is retained for retry", staged.ID, sessionID, result.Err)
		}
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadataJSON); err != nil {
		return fmt.Errorf("store: generation %s for session %s is active but metadata repair failed: %w; the active generation is valid and the repair is retried on the next open or activation", staged.ID, sessionID, err)
	}
	if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
		return fmt.Errorf("store: generation %s for session %s is active but the intent was not cleared: %w; recovery clears it idempotently", staged.ID, sessionID, err)
	}
	return nil
}

// RecoverGenerationActivation reconciles one session's pending intent under the
// exclusive lock. It is safe to call at any time; a committed generation is
// re-repaired and its intent cleared, while an uncommitted synced candidate is
// activated from its self-contained manifest.
func (s *Store) RecoverGenerationActivation(ctx context.Context, sessionID schema.SessionID) error {
	if err := s.requireGenerationSupport(); err != nil {
		return err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	return s.recoverGenerationIntentLocked(ctx, sessionID)
}

func (s *Store) recoverGenerationIntentLocked(ctx context.Context, sessionID schema.SessionID) error {
	intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
	if err != nil || intent == nil {
		return err
	}
	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return err
	}
	if active == intent.GenerationID {
		metadata, err := s.exportedMetadataForGeneration(ctx, sessionID, intent.GenerationID)
		if err != nil {
			return err
		}
		if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadata); err != nil {
			return err
		}
		return s.generationArtifacts.ClearIntent(ctx, sessionID)
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
	metadataJSON, err := json.Marshal(generation.Metadata)
	if err != nil {
		return fmt.Errorf("store: recover pending activation for session %s: encode metadata: %w", sessionID, err)
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          sessionID,
		Result:             indexformat.V2{Generation: generation},
		IndexVersion:       2,
		Mode:               ingest.SessionEntryWriteReplaceAll,
		RequireFullContent: false,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:          ingest.ContentCaptureIncomplete,
			SourceAuthority: ingest.ContentSourceNone,
			CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
		},
	}})
	for _, result := range results {
		if result.Err != nil {
			return fmt.Errorf("store: recover pending activation for session %s: %w; the prior generation is preserved and the candidate is retained", sessionID, result.Err)
		}
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadataJSON); err != nil {
		return err
	}
	return s.generationArtifacts.ClearIntent(ctx, sessionID)
}

func (s *Store) finishActivationLocked(ctx context.Context, sessionID schema.SessionID, generation indexformat.Generation) error {
	metadata, err := json.Marshal(generation.Metadata)
	if err != nil {
		return fmt.Errorf("store: encode exported metadata for session %s: %w", sessionID, err)
	}
	if err := s.generationArtifacts.RepairMetadata(ctx, sessionID, metadata); err != nil {
		return err
	}
	return s.generationArtifacts.ClearIntent(ctx, sessionID)
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

// stageWithIntent records the durable activation intent before staging the
// generation files, so a crash after the atomic rename is recoverable from the
// self-contained manifest and a crash before the rename leaves only a stale
// intent that recovery clears.
func (s *Store) stageWithIntent(ctx context.Context, sessionID schema.SessionID, generation indexformat.Generation, blobs map[schema.SourceEntryRef][]byte) (indexformat.Generation, error) {
	if err := s.generationArtifacts.WriteIntent(ctx, GenerationIntent{
		SessionID:    sessionID,
		GenerationID: generation.ID,
		ManifestPath: "generations/" + generation.ID + "/manifest.json",
		Completeness: string(generation.Completeness),
		StagedAtMs:   time.Now().UnixMilli(),
	}); err != nil {
		return indexformat.Generation{}, err
	}
	return s.generationArtifacts.Stage(ctx, generation, blobs)
}

// CleanupInactiveGeneration removes one owned generation directory under the
// exclusive session lock. It refuses to remove the session's active generation,
// and because it takes the same lock readers share, it waits for any reader that
// is still hydrating or serializing a snapshot of it.
func (s *Store) CleanupInactiveGeneration(ctx context.Context, sessionID schema.SessionID, generationID string) error {
	if err := s.requireGenerationSupport(); err != nil {
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
	return s.generationArtifacts.RemoveGeneration(ctx, sessionID, generationID)
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
