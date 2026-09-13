package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// generationIndexFormat persists the immutable V2 managed generation. It runs on
// the caller's connection and savepoint: the activation transaction is owned by
// the common Store writer, not by this handler. It writes every
// session_projection_* row for one generation_id, mirrors the main partition
// into the canonical session_entries table (through the entries the handler
// returns), points the session at the new generation, and derives the one
// session_metrics.turn_count mirror from Metadata.Stats.
type generationIndexFormat struct{}

var _ IndexFormat = generationIndexFormat{}

// Version reports the concrete managed-generation format version.
func (generationIndexFormat) Version() int { return 2 }

// Validate refuses any result that is not a fully validated indexformat.V2, so a
// half-built generation can never reach the database.
func (generationIndexFormat) Validate(result indexformat.Result) error {
	value, ok := result.(indexformat.V2)
	if !ok {
		return fmt.Errorf("index format 2 requires an indexformat.V2 result, got %T; no generation was activated; use the matching concrete indexer result", result)
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("index format 2 rejected the managed generation before activation: %w", err)
	}
	return nil
}

// Write installs one generation in the caller's transaction. The returned main
// entries are the canonical search projection the common writer persists; the
// generation rows are the V2 read authority. It enforces the completeness
// transition transactionally: an incomplete_new candidate is refused when a
// complete generation already exists for the session, so a last-good complete
// generation is never replaced by an incomplete capture. The same guard runs
// for activation, recovery and direct format writes because all three commit
// through this handler on the activation connection.
func (generationIndexFormat) Write(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID, result indexformat.Result) ([]schema.SessionEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, ok := result.(indexformat.V2)
	if !ok {
		return nil, generationIndexFormat{}.Validate(result)
	}
	generation := value.Generation
	if generation.Metadata.SessionID != sessionID {
		return nil, fmt.Errorf("store: managed generation for session %s names session %s; no generation was activated; build the generation for its own captured metadata", sessionID, generation.Metadata.SessionID)
	}
	if generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		if err := refuseIncompleteWhenCompleteExists(conn, sessionID, generation.ID); err != nil {
			return nil, err
		}
	}
	metadataJSON, err := json.Marshal(generation.Metadata)
	if err != nil {
		return nil, fmt.Errorf("store: encode managed generation metadata for session %s: %w; no generation was activated; repair the captured metadata", sessionID, err)
	}
	titleRefsJSON, err := json.Marshal(generation.TitleRefs)
	if err != nil {
		return nil, fmt.Errorf("store: encode managed generation title refs for session %s: %w; no generation was activated", sessionID, err)
	}
	now := time.Now().UnixMilli()
	if err := deleteGenerationRowsOnConn(conn, sessionID, generation.ID); err != nil {
		return nil, err
	}
	if err := insertGenerationRowOnConn(conn, sessionID, generation, metadataJSON, titleRefsJSON, now); err != nil {
		return nil, err
	}
	if err := insertGenerationPartitionsOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationSegmentsOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationContentOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationAliasesOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertRelationshipEvidenceOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := pointSessionAtGenerationOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	return generation.Main.Entries, nil
}

// Delete removes every generation-scoped row for a session when the session's
// representation is replaced by a different format. It deliberately does not
// touch content blobs: the owned-artifact store removes inactive generation
// directories under its exclusive lock, after this activation commits.
func (generationIndexFormat) Delete(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM session_relationship_evidence WHERE session_id = ?`,
		`DELETE FROM session_projection_entries WHERE session_id = ?`,
		`DELETE FROM session_projection_sections WHERE session_id = ?`,
		`DELETE FROM session_context_segments WHERE session_id = ?`,
		`DELETE FROM session_projection_content WHERE session_id = ?`,
		`DELETE FROM session_projection_aliases WHERE session_id = ?`,
		`DELETE FROM session_projection_generations WHERE session_id = ?`,
		`UPDATE sessions SET active_generation_id = NULL WHERE session_id = ?`,
	} {
		if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
			return fmt.Errorf("store: clear managed generations for session %s before representation replacement: %w; the transaction was refused and the prior representation is preserved", sessionID, err)
		}
	}
	return nil
}

func deleteGenerationRowsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) error {
	for _, statement := range []string{
		`DELETE FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_projection_entries WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_projection_sections WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_context_segments WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_projection_content WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_projection_aliases WHERE session_id = ? AND generation_id = ?`,
		`DELETE FROM session_projection_generations WHERE session_id = ? AND generation_id = ?`,
	} {
		if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{Args: []any{string(sessionID), generationID}}); err != nil {
			return fmt.Errorf("store: clear prior rows for generation %s of session %s before activation: %w; the transaction was refused", generationID, sessionID, err)
		}
	}
	return nil
}

func insertGenerationRowOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation, metadataJSON, titleRefsJSON []byte, now int64) error {
	var inputCount any
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputCount = *generation.Metadata.Stats.InputSubmissionCount
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_generations
 (session_id, generation_id, metadata_json, title_refs_json, input_submission_count, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms)
 VALUES (?, ?, ?, ?, ?, ?, ?, 2, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
		string(sessionID), generation.ID, string(metadataJSON), string(titleRefsJSON), inputCount, generation.SourceEvidenceDigest,
		string(generation.Completeness), now, now,
	}}); err != nil {
		return fmt.Errorf("store: install generation %s for session %s: %w; no generation was activated", generation.ID, sessionID, err)
	}
	return nil
}

func insertGenerationPartitionsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	// partition_id 0 is the main stream; earlier sections are 1..N in order.
	mainMetadata, err := json.Marshal(generation.Main.NativeMetadata)
	if err != nil {
		return fmt.Errorf("store: encode main native metadata for generation %s: %w; no generation was activated", generation.ID, err)
	}
	if err := insertSectionOnConn(conn, sessionID, generation.ID, 0, "", mainMetadata); err != nil {
		return err
	}
	if err := insertEntriesOnConn(conn, sessionID, generation.ID, 0, generation.Main.Entries); err != nil {
		return err
	}
	for i := range generation.Earlier {
		partitionID := i + 1
		section := generation.Earlier[i]
		nativeMetadata, err := json.Marshal(section.Content.NativeMetadata)
		if err != nil {
			return fmt.Errorf("store: encode earlier[%d] native metadata for generation %s: %w; no generation was activated", i, generation.ID, err)
		}
		if err := insertSectionOnConn(conn, sessionID, generation.ID, partitionID, string(section.State), nativeMetadata); err != nil {
			return err
		}
		if err := insertEntriesOnConn(conn, sessionID, generation.ID, partitionID, section.Content.Entries); err != nil {
			return err
		}
	}
	return nil
}

func insertSectionOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, partitionID int, earlierState string, nativeMetadata []byte) error {
	var state any
	if earlierState != "" {
		state = earlierState
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_sections (session_id, generation_id, partition_id, earlier_state, native_metadata) VALUES (?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
		string(sessionID), generationID, partitionID, state, string(nativeMetadata),
	}}); err != nil {
		return fmt.Errorf("store: install partition %d for generation %s of session %s: %w; no generation was activated", partitionID, generationID, sessionID, err)
	}
	return nil
}

func insertEntriesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, partitionID int, entries []schema.SessionEntry) error {
	for i := range entries {
		entry := entries[i]
		if entry.SessionID != sessionID {
			return fmt.Errorf("store: generation %s partition %d entry %d names session %s; no generation was activated; build entries for the owning session", generationID, partitionID, entry.EntryIndex, entry.SessionID)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("store: encode entry %d of partition %d for generation %s: %w; no generation was activated", entry.EntryIndex, partitionID, generationID, err)
		}
		var ref any
		if entry.SourceEntryRef != "" {
			ref = string(entry.SourceEntryRef)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_entries (session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, partitionID, entry.EntryIndex, ref, string(encoded),
		}}); err != nil {
			return fmt.Errorf("store: install entry %d of partition %d for generation %s: %w; no generation was activated", entry.EntryIndex, partitionID, generationID, err)
		}
	}
	return nil
}

func insertGenerationSegmentsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	for i := range generation.Segments {
		segment := generation.Segments[i]
		refs, err := json.Marshal(segment.CapturedRefs)
		if err != nil {
			return fmt.Errorf("store: encode captured refs for segment %d of generation %s: %w; no generation was activated", segment.Ordinal, generation.ID, err)
		}
		var logical any
		if segment.LogicalSessionID != nil {
			logical = string(*segment.LogicalSessionID)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_context_segments
 (session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id, coordinate_kind,
  start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion, captured_refs_json)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generation.ID, segment.Ordinal, logical, segment.PhysicalSourceID,
			string(segment.Coordinates.Kind), nullableInt64Value(segment.Coordinates.Start), nullableInt64Value(segment.Coordinates.EndExclusive),
			nullableInt64Value(segment.Coordinates.DecodedByteStart), nullableInt64Value(segment.Coordinates.DecodedByteEndExclusive),
			string(segment.Inclusion), string(refs),
		}}); err != nil {
			return fmt.Errorf("store: install context segment %d for generation %s: %w; no generation was activated", segment.Ordinal, generation.ID, err)
		}
	}
	return nil
}

func insertGenerationContentOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	for i := range generation.Content {
		record := generation.Content[i]
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_content (session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generation.ID, string(record.Ref), record.RelativeBlob, record.ByteLength, record.Digest,
		}}); err != nil {
			return fmt.Errorf("store: install content record %s for generation %s: %w; no generation was activated", record.Ref, generation.ID, err)
		}
	}
	return nil
}

func insertGenerationAliasesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	for i := range generation.Aliases {
		alias := generation.Aliases[i]
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_aliases (session_id, generation_id, native_key, source_entry_ref) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generation.ID, alias.NativeKey, string(alias.Ref),
		}}); err != nil {
			return fmt.Errorf("store: install native alias %q for generation %s: %w; no generation was activated", alias.NativeKey, generation.ID, err)
		}
	}
	return nil
}

func insertRelationshipEvidenceOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	for i := range generation.Metadata.Relationships {
		relationship := generation.Metadata.Relationships[i]
		var target, evidence, anchor any
		if relationship.TargetLocalID != nil && *relationship.TargetLocalID != "" {
			target = string(*relationship.TargetLocalID)
		}
		if relationship.Evidence != "" {
			evidence = string(relationship.Evidence)
		}
		if relationship.Anchor != nil {
			encoded, err := json.Marshal(relationship.Anchor)
			if err != nil {
				return fmt.Errorf("store: encode anchor for %s relationship of generation %s: %w; no generation was activated", relationship.Kind, generation.ID, err)
			}
			anchor = string(encoded)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state, target_local_id, evidence, anchor) VALUES (?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generation.ID, string(relationship.Kind), string(relationship.TargetState), target, evidence, anchor,
		}}); err != nil {
			return fmt.Errorf("store: install %s relationship evidence for generation %s: %w; no generation was activated", relationship.Kind, generation.ID, err)
		}
	}
	return nil
}

func pointSessionAtGenerationOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	var root any
	if generation.Metadata.RootSessionID != nil {
		root = string(*generation.Metadata.RootSessionID)
	}
	var purpose any
	if generation.Metadata.Purpose != "" {
		purpose = string(generation.Metadata.Purpose)
	}
	var inputCount any
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputCount = *generation.Metadata.Stats.InputSubmissionCount
	}
	var adapter any
	if generation.Metadata.AdapterVersion != nil {
		if *generation.Metadata.AdapterVersion <= 0 {
			return fmt.Errorf("store: managed generation %s for session %s carries non-positive adapter revision; no generation was activated; omit unknown provenance instead", generation.ID, sessionID)
		}
		adapter = *generation.Metadata.AdapterVersion
	}
	// The logical parent is durable relationship evidence, not the
	// availability cache. sessions.parent_id is the FK-safe cache: the durable
	// started_by target when it names a stored session, else NULL so an
	// admitted child survives an absent, unselected or cyclic parent.
	logicalParent := durableParentTarget(generation.Metadata.Relationships)
	var parent any
	if logicalParent != nil {
		exists := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
			Args:       []any{string(*logicalParent)},
			ResultFunc: func(*sqlite.Stmt) error { exists = true; return nil },
		}); err != nil {
			return fmt.Errorf("store: resolve logical parent for session %s generation %s: %w; no generation was activated", sessionID, generation.ID, err)
		}
		if exists {
			parent = string(*logicalParent)
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = ?, root_session_id = ?, session_purpose = ?, input_submission_count = ?, adapter_version = ?, start_ms = ?, end_ms = ?, model_harness = ?, parent_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{
		generation.ID, root, purpose, inputCount, adapter, generation.Metadata.Timestamp.Start, generation.Metadata.Timestamp.End, string(generation.Metadata.ModelHarness), parent, string(sessionID),
	}}); err != nil {
		return fmt.Errorf("store: point session %s at generation %s: %w; no generation was activated", sessionID, generation.ID, err)
	}
	// session_metrics.turn_count and tool_calls are derived mirrors of
	// Metadata.Stats, never a second authority. The title is derived from the
	// generation's TitleRefs so ordinary list reads agree with the snapshot
	// without a separate metadata upsert. INSERT ... ON CONFLICT keeps a
	// session without a metrics row valid.
	title := deriveGenerationTitle(generation)
	var titleValue any
	if title != "" {
		titleValue = title
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_metrics (session_id, turn_count, tool_calls, title) VALUES (?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET turn_count = excluded.turn_count, tool_calls = excluded.tool_calls, title = excluded.title`, &sqlitex.ExecOptions{Args: []any{
		string(sessionID), generation.Metadata.Stats.TurnCount, generation.Metadata.Stats.ToolCallCount, titleValue,
	}}); err != nil {
		return fmt.Errorf("store: derive turn_count mirror for session %s generation %s: %w; no generation was activated", sessionID, generation.ID, err)
	}
	return nil
}

// refuseIncompleteWhenCompleteExists implements the completeness transition:
// incomplete_new is the one first-discovery exception and is allowed only when
// no complete generation exists for the session. Retrying the same incomplete
// identifier is idempotent and allowed; replacing any other generation while a
// complete last-good exists is refused transactionally.
func refuseIncompleteWhenCompleteExists(conn *sqlite.Conn, sessionID schema.SessionID, candidateID string) error {
	completeID := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT generation_id FROM session_projection_generations WHERE session_id = ? AND completeness = 'complete' LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			completeID = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: check completeness transition for session %s: %w; no generation was activated", sessionID, err)
	}
	if completeID != "" && completeID != candidateID {
		return fmt.Errorf("store: refuse incomplete generation %s for session %s: a complete generation %s is the last-good read authority; incomplete_new is the first-discovery exception only and never replaces it", candidateID, sessionID, completeID)
	}
	return nil
}

// durableParentTarget returns the durable started_by target when the
// relationship evidence names a known retained session. It mirrors the
// snapshot's logical-parent derivation so activation and reads agree.
func durableParentTarget(relationships []schema.SessionRelationship) *schema.SessionID {
	for i := range relationships {
		relationship := relationships[i]
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
	return nil
}

// deriveGenerationTitle derives the display title from the generation's
// TitleRefs: the first ref whose main entry carries usable prose, reduced to
// its first non-empty line. It never invents a title when no ref yields one.
func deriveGenerationTitle(generation indexformat.Generation) string {
	if len(generation.TitleRefs) == 0 {
		return ""
	}
	byRef := make(map[schema.SourceEntryRef]schema.SessionEntry, len(generation.Main.Entries))
	for _, entry := range generation.Main.Entries {
		if entry.SourceEntryRef != "" {
			byRef[entry.SourceEntryRef] = entry
		}
	}
	for _, ref := range generation.TitleRefs {
		entry, ok := byRef[ref]
		if !ok {
			continue
		}
		var text string
		if entry.ContentPreview != nil && *entry.ContentPreview != "" {
			text = *entry.ContentPreview
		} else if entry.ToolInput != nil && *entry.ToolInput != "" {
			text = *entry.ToolInput
		} else if entry.ToolOutput != nil && *entry.ToolOutput != "" {
			text = *entry.ToolOutput
		}
		if title := firstTitleLine(text); title != "" {
			return title
		}
	}
	return ""
}

func firstTitleLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if runes := []rune(trimmed); len(runes) > 80 {
				return string(runes[:77]) + "..."
			}
			return trimmed
		}
	}
	return ""
}

func nullableInt64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
