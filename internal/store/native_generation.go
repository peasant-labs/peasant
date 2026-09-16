package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var (
	_ ingest.NativeGenerationActivator   = (*Store)(nil)
	_ ingest.NativeGenerationPriorReader = (*Store)(nil)
)

// ActivateNativeGeneration adapts the ingest-owned activation envelope to the
// store's own immutable activation. It stages and fsyncs the content files,
// persists the opaque prior document, installs the generation, counts and
// pointer in ONE transaction, repairs the exported metadata and clears the
// intent. It reuses the same expected-state compare and success-stamp rules as
// every other managed activation.
func (s *Store) ActivateNativeGeneration(ctx context.Context, activation ingest.NativeGenerationActivation) error {
	return s.ActivateGeneration(ctx, GenerationActivation{
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
		Capture:          activation.Capture,
	})
}

// ReadNativeGenerationPrior loads the active generation's reusable evidence for
// one session: its committed metadata, the native-key aliases the projection
// must reuse, whether a complete generation is already the last-good authority,
// and the harness-owned prior document when one was persisted. A session with
// no active generation returns (nil, nil), so a first discovery builds an empty
// prior state instead of guessing.
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

func readGenerationPriorOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (*ingest.NativeGenerationPrior, error) {
	prior := &ingest.NativeGenerationPrior{
		GenerationID: generationID,
		Aliases:      ingest.NewProjectionPriorState(),
	}
	var metadataJSON string
	var completeness string
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_json, completeness FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
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
	if err := sqlitex.ExecuteTransient(conn, `SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? ORDER BY native_key`, &sqlitex.ExecOptions{
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
	if err := sqlitex.ExecuteTransient(conn, `SELECT entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
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
