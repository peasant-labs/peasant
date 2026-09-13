package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Compile-time guards: the production Store is the concrete SnapshotReader and
// ContentResolver consumed by the transcript hydration and export layers.
var (
	_ indexformat.SnapshotReader  = (*Store)(nil)
	_ indexformat.ContentResolver = (*Store)(nil)
)

// WithSessionSnapshot loads ONE immutable read snapshot for a session. It takes
// the shared per-session OS lock BEFORE the SQLite read transaction, loads the
// metadata, active generation, main and earlier partitions, contexts and
// content map in one snapshot, commits the read transaction, returns the pool
// connection, and then keeps the shared lock for the ENTIRE callback through
// hydration and serialization. Returning the connection before the callback
// lets a callback that needs another store lookup proceed without deadlocking
// at pool size one. A concurrent activation or cleanup takes the exclusive
// lock and therefore waits until the callback returns.
//
// A session with no active generation yields a legacy V1 snapshot whose
// LegacySource names the retained transcript; the caller uses the unchanged
// legacy callback and never falls back to mutable native data for a V2 read.
func (s *Store) WithSessionSnapshot(ctx context.Context, sessionID schema.SessionID, fn func(indexformat.ReadSnapshot) error) (retErr error) {
	if s.sessionLocker == nil {
		return fmt.Errorf("store: managed generation support is not configured; a coherent session snapshot cannot be taken; open the store with WithGenerationArtifacts")
	}
	release, err := s.sessionLocker.LockShared(ctx, sessionID)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil && retErr == nil {
			retErr = releaseErr
		}
	}()

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for session snapshot %s: %w", sessionID, err)
	}

	endSnapshot := sqlitex.Save(conn)
	snapshot, readErr := buildReadSnapshotOnConn(conn, sessionID)
	endSnapshot(&readErr)
	// The snapshot is fully materialized in memory; return the pool connection
	// before invoking the callback while retaining the shared OS lock.
	s.pool.Put(conn)
	if readErr != nil {
		return readErr
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("store: the captured snapshot for session %s is not a coherent read: %w; no hydration was authorized", sessionID, err)
	}
	return fn(snapshot)
}

// ReadFullContent resolves one immutable V2 content blob addressed by the
// captured snapshot identity. It takes the shared session lock so cleanup
// cannot remove the generation while the blob is read, and it NEVER reparses a
// mutable native source.
func (s *Store) ReadFullContent(ctx context.Context, sessionID schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	if s.generationArtifacts == nil || s.sessionLocker == nil {
		return nil, fmt.Errorf("store: managed generation support is not configured; captured content cannot be resolved; open the store with WithGenerationArtifacts")
	}
	release, err := s.sessionLocker.LockShared(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = release() }()
	return s.generationArtifacts.ReadBlob(ctx, sessionID, generationID, record)
}

func buildReadSnapshotOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (indexformat.ReadSnapshot, error) {
	row, err := readSnapshotSessionRowOnConn(conn, sessionID)
	if err != nil {
		return indexformat.ReadSnapshot{}, err
	}
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return indexformat.ReadSnapshot{}, err
	}
	if active == nil {
		return legacyReadSnapshot(row), nil
	}
	return generationReadSnapshotOnConn(conn, sessionID, *active, row)
}

type snapshotSessionRow struct {
	sessionID     schema.SessionID
	harness       schema.Harness
	parentID      *schema.SessionID
	rootID        *schema.SessionID
	purpose       schema.SessionPurpose
	sourcePath    string
	startMs       int64
	endMs         int64
	turnCount     int
	toolCallCount int
}

func readSnapshotSessionRowOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (snapshotSessionRow, error) {
	row := snapshotSessionRow{sessionID: sessionID}
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT s.model_harness, s.parent_id, s.root_session_id, s.session_purpose, s.source_path, s.start_ms, s.end_ms,
 COALESCE(m.turn_count, 0), COALESCE(m.tool_calls, 0)
 FROM sessions s LEFT JOIN session_metrics m ON m.session_id = s.session_id
 WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if err := row.harness.UnmarshalText([]byte(stmt.ColumnText(0))); err != nil || !row.harness.IsKnown() {
				return fmt.Errorf("stored harness %q is not recognized; the snapshot cannot be attributed; restore valid session metadata", stmt.ColumnText(0))
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				parent, err := schema.NewSessionID(stmt.ColumnText(1))
				if err != nil {
					return fmt.Errorf("stored parent identity is malformed; the snapshot cannot be built: %w", err)
				}
				row.parentID = &parent
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				root, err := schema.NewSessionID(stmt.ColumnText(2))
				if err != nil {
					return fmt.Errorf("stored root identity is malformed; the snapshot cannot be built: %w", err)
				}
				row.rootID = &root
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				purpose, err := schema.NewSessionPurpose(stmt.ColumnText(3))
				if err != nil {
					return fmt.Errorf("stored session purpose is not recognized; the snapshot cannot be built: %w", err)
				}
				row.purpose = purpose
			}
			row.sourcePath = stmt.ColumnText(4)
			row.startMs = stmt.ColumnInt64(5)
			row.endMs = stmt.ColumnInt64(6)
			row.turnCount = stmt.ColumnInt(7)
			row.toolCallCount = stmt.ColumnInt(8)
			return nil
		},
	}); err != nil {
		return snapshotSessionRow{}, fmt.Errorf("store: read session %s for its snapshot: %w; no read was authorized", sessionID, err)
	}
	if !found {
		return snapshotSessionRow{}, fmt.Errorf("store: session %s has no metadata row; import the session before reading its snapshot", sessionID)
	}
	return row, nil
}

// legacyReadSnapshot builds the unchanged V1 read: no managed generation, the
// retained transcript named by LegacySource, and empty generation partitions so
// the caller uses the legacy overlay callback.
func legacyReadSnapshot(row snapshotSessionRow) indexformat.ReadSnapshot {
	session := schema.SessionDetailPayload{
		ID:               string(row.sessionID),
		Harness:          row.harness,
		StartTime:        time.UnixMilli(row.startMs),
		EndTime:          time.UnixMilli(row.endMs),
		TurnCount:        row.turnCount,
		ToolCallCount:    row.toolCallCount,
		ParentSessionID:  row.parentID,
		RootSessionID:    row.rootID,
		Purpose:          row.purpose,
		WorkingDirectory: "",
	}
	metadata := schema.UnifiedMetadata{
		SchemaVersion: 1,
		SessionID:     row.sessionID,
		ModelHarness:  row.harness,
		Stats:         schema.SessionStats{TurnCount: row.turnCount},
		RootSessionID: row.rootID,
		Purpose:       row.purpose,
	}
	return indexformat.ReadSnapshot{
		Session:      session,
		Metadata:     metadata,
		IndexVersion: 1,
		LegacySource: indexformat.LegacySource{Harness: row.harness, Path: row.sourcePath},
	}
}

func generationReadSnapshotOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, row snapshotSessionRow) (indexformat.ReadSnapshot, error) {
	var metadata schema.UnifiedMetadata
	var completeness string
	var titleRefsJSON string
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT metadata_json, title_refs_json, completeness FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &metadata); err != nil {
				return fmt.Errorf("decode generation metadata: %w", err)
			}
			titleRefsJSON = stmt.ColumnText(1)
			completeness = stmt.ColumnText(2)
			return nil
		},
	}); err != nil {
		return indexformat.ReadSnapshot{}, fmt.Errorf("store: read generation %s for session %s: %w; the snapshot cannot be built", generationID, sessionID, err)
	}
	if !found {
		return indexformat.ReadSnapshot{}, fmt.Errorf("store: session %s points at generation %s but no generation row exists; restore the committed generation before reading", sessionID, generationID)
	}
	completenessValue, err := indexformat.NewGenerationCompleteness(completeness)
	if err != nil {
		return indexformat.ReadSnapshot{}, err
	}
	var titleRefs []schema.SourceEntryRef
	if err := json.Unmarshal([]byte(titleRefsJSON), &titleRefs); err != nil {
		return indexformat.ReadSnapshot{}, fmt.Errorf("store: decode title refs for generation %s of session %s: %w", generationID, sessionID, err)
	}
	partitions, err := readGenerationPartitionsOnConn(conn, sessionID, generationID)
	if err != nil {
		return indexformat.ReadSnapshot{}, err
	}
	content, err := readGenerationContentOnConn(conn, sessionID, generationID)
	if err != nil {
		return indexformat.ReadSnapshot{}, err
	}
	// The snapshot is candidate-derived: timestamps and tool counts come from
	// the committed generation metadata, and the logical parent comes from
	// durable relationship evidence, never from the availability cache. This
	// keeps the snapshot and ordinary store reads in agreement without a prior
	// metadata upsert.
	session := schema.SessionDetailPayload{
		ID:                   string(sessionID),
		Harness:              metadata.ModelHarness,
		StartTime:            time.UnixMilli(metadata.Timestamp.Start),
		EndTime:              time.UnixMilli(metadata.Timestamp.End),
		TurnCount:            metadata.Stats.TurnCount,
		InputSubmissionCount: metadata.Stats.InputSubmissionCount,
		ToolCallCount:        metadata.Stats.ToolCallCount,
		ParentSessionID:      snapshotLogicalParent(metadata),
		RootSessionID:        metadata.RootSessionID,
		Purpose:              metadata.Purpose,
		Relationships:        metadata.Relationships,
	}
	snapshot := indexformat.ReadSnapshot{
		Session:      session,
		Metadata:     metadata,
		TitleRefs:    titleRefs,
		GenerationID: generationID,
		Completeness: completenessValue,
		IndexVersion: 2,
		Main:         partitions.main,
		Earlier:      partitions.earlier,
		Content:      content,
	}
	return snapshot, nil
}

type generationPartitions struct {
	main    indexformat.Partition
	earlier []indexformat.EarlierPartition
}

func readGenerationPartitionsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (generationPartitions, error) {
	sections := []struct {
		partitionID int
		state       string
		native      string
	}{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, COALESCE(earlier_state, ''), native_metadata FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sections = append(sections, struct {
				partitionID int
				state       string
				native      string
			}{stmt.ColumnInt(0), stmt.ColumnText(1), stmt.ColumnText(2)})
			return nil
		},
	}); err != nil {
		return generationPartitions{}, fmt.Errorf("store: read generation partitions for session %s generation %s: %w", sessionID, generationID, err)
	}
	entries, err := readGenerationEntriesOnConn(conn, sessionID, generationID)
	if err != nil {
		return generationPartitions{}, err
	}
	partitions := generationPartitions{}
	for _, section := range sections {
		partition, err := partitionFromRows(section.native, entries[section.partitionID], section.partitionID)
		if err != nil {
			return generationPartitions{}, err
		}
		if section.partitionID == 0 {
			partitions.main = partition
			continue
		}
		state, err := schema.NewEarlierHistoryState(section.state)
		if err != nil {
			return generationPartitions{}, fmt.Errorf("store: earlier partition %d of generation %s carries an unknown state: %w", section.partitionID, generationID, err)
		}
		partitions.earlier = append(partitions.earlier, indexformat.EarlierPartition{State: state, Content: partition})
	}
	return partitions, nil
}

func partitionFromRows(nativeJSON string, entries []schema.SessionEntry, partitionID int) (indexformat.Partition, error) {
	partition := indexformat.Partition{Entries: entries}
	if nativeJSON != "" && nativeJSON != "null" {
		if err := json.Unmarshal([]byte(nativeJSON), &partition.NativeMetadata); err != nil {
			return indexformat.Partition{}, fmt.Errorf("store: decode native metadata for partition %d: %w", partitionID, err)
		}
	}
	return partition, nil
}

func readGenerationEntriesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (map[int][]schema.SessionEntry, error) {
	entries := map[int][]schema.SessionEntry{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			partitionID := stmt.ColumnInt(0)
			var entry schema.SessionEntry
			if err := json.Unmarshal([]byte(stmt.ColumnText(1)), &entry); err != nil {
				return fmt.Errorf("decode entry of partition %d: %w", partitionID, err)
			}
			entries[partitionID] = append(entries[partitionID], entry)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read generation entries for session %s generation %s: %w", sessionID, generationID, err)
	}
	return entries, nil
}

func readGenerationContentOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]indexformat.ContentRecord, error) {
	var records []indexformat.ContentRecord
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref, relative_blob, byte_length, integrity_digest FROM session_projection_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("decode content ref: %w", err)
			}
			records = append(records, indexformat.ContentRecord{
				Ref:          ref,
				RelativeBlob: stmt.ColumnText(1),
				ByteLength:   stmt.ColumnInt64(2),
				Digest:       stmt.ColumnText(3),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read generation content for session %s generation %s: %w", sessionID, generationID, err)
	}
	return records, nil
}

// snapshotLogicalParent derives the snapshot's logical parent from durable
// relationship evidence, never from the availability cache. A known or
// retained started_by target is the logical parent; otherwise the legacy
// ParentUUID carries the parent when the generation records one.
func snapshotLogicalParent(metadata schema.UnifiedMetadata) *schema.SessionID {
	for i := range metadata.Relationships {
		relationship := metadata.Relationships[i]
		if relationship.Kind != schema.SessionRelationshipStartedBy {
			continue
		}
		if relationship.TargetState == schema.RelationshipTargetKnown || relationship.TargetState == schema.RelationshipTargetKnownRetained {
			if relationship.TargetLocalID != nil && *relationship.TargetLocalID != "" {
				target := *relationship.TargetLocalID
				return &target
			}
		}
	}
	return metadata.ParentUUID
}
