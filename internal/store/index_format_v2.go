package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The per-row generation install inserts. They are prepared once per install in
// generationInstallStatements and re-bound per row, so a native session with
// thousands of projection entries does not parse and plan the same SQL for
// every row. The one-off statements (the generation row, the pointing UPDATE
// and the metrics upsert) stay on sqlitex.ExecuteTransient.
const (
	sqlInsertGenerationSection = `INSERT INTO session_projection_sections (session_id, generation_id, partition_id, earlier_state, native_metadata) VALUES (?, ?, ?, ?, ?)`

	sqlInsertGenerationEntry = `INSERT INTO session_projection_entries (session_id, generation_id, partition_id, entry_index, source_entry_ref, entry_json) VALUES (?, ?, ?, ?, ?, ?)`

	sqlInsertGenerationSegment = `INSERT INTO session_context_segments
 (session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id, coordinate_kind,
  start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion, captured_refs_json)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlInsertGenerationContent = `INSERT INTO session_projection_content (session_id, generation_id, source_entry_ref, relative_blob, byte_length, integrity_digest) VALUES (?, ?, ?, ?, ?, ?)`

	sqlInsertGenerationAlias = `INSERT INTO session_projection_aliases (session_id, generation_id, native_key, source_entry_ref) VALUES (?, ?, ?, ?)`

	sqlInsertRelationshipEvidence = `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state, target_local_id, evidence, anchor) VALUES (?, ?, ?, ?, ?, ?, ?)`
)

// generationInstallStatements holds the reusable per-row inserts for one
// managed-generation install. Each accessor prepares its statement on first
// use and returns the same statement for every later row; stepAndReset returns
// it to the reusable state. Close finalizes every prepared statement, so a
// failed install leaves nothing behind on the caller's connection.
type generationInstallStatements struct {
	conn         *sqlite.Conn
	sectionStmt  *sqlite.Stmt
	entryStmt    *sqlite.Stmt
	segmentStmt  *sqlite.Stmt
	contentStmt  *sqlite.Stmt
	aliasStmt    *sqlite.Stmt
	evidenceStmt *sqlite.Stmt
}

func newGenerationInstallStatements(conn *sqlite.Conn) *generationInstallStatements {
	return &generationInstallStatements{conn: conn}
}

// Close finalizes every statement this install prepared. It is safe to call on
// an install that prepared none.
func (stmts *generationInstallStatements) Close() error {
	if stmts == nil {
		return nil
	}
	var err error
	for _, entry := range []struct {
		stmt  **sqlite.Stmt
		label string
	}{
		{&stmts.sectionStmt, "session_projection_sections"},
		{&stmts.entryStmt, "session_projection_entries"},
		{&stmts.segmentStmt, "session_context_segments"},
		{&stmts.contentStmt, "session_projection_content"},
		{&stmts.aliasStmt, "session_projection_aliases"},
		{&stmts.evidenceStmt, "session_relationship_evidence"},
	} {
		stmt := *entry.stmt
		if stmt == nil {
			continue
		}
		if closeErr := stmt.Finalize(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("finalize %s insert statement: %w", entry.label, closeErr))
		}
		*entry.stmt = nil
	}
	return err
}

func (stmts *generationInstallStatements) Section() (*sqlite.Stmt, error) {
	if stmts.sectionStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertGenerationSection)
		if err != nil {
			return nil, err
		}
		stmts.sectionStmt = stmt
	}
	return stmts.sectionStmt, nil
}

func (stmts *generationInstallStatements) Entry() (*sqlite.Stmt, error) {
	if stmts.entryStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertGenerationEntry)
		if err != nil {
			return nil, err
		}
		stmts.entryStmt = stmt
	}
	return stmts.entryStmt, nil
}

func (stmts *generationInstallStatements) Segment() (*sqlite.Stmt, error) {
	if stmts.segmentStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertGenerationSegment)
		if err != nil {
			return nil, err
		}
		stmts.segmentStmt = stmt
	}
	return stmts.segmentStmt, nil
}

func (stmts *generationInstallStatements) Content() (*sqlite.Stmt, error) {
	if stmts.contentStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertGenerationContent)
		if err != nil {
			return nil, err
		}
		stmts.contentStmt = stmt
	}
	return stmts.contentStmt, nil
}

func (stmts *generationInstallStatements) Alias() (*sqlite.Stmt, error) {
	if stmts.aliasStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertGenerationAlias)
		if err != nil {
			return nil, err
		}
		stmts.aliasStmt = stmt
	}
	return stmts.aliasStmt, nil
}

func (stmts *generationInstallStatements) Evidence() (*sqlite.Stmt, error) {
	if stmts.evidenceStmt == nil {
		stmt, _, err := stmts.conn.PrepareTransient(sqlInsertRelationshipEvidence)
		if err != nil {
			return nil, err
		}
		stmts.evidenceStmt = stmt
	}
	return stmts.evidenceStmt, nil
}

// generationIndexFormat persists the immutable V2 managed generation. It runs on
// the caller's connection and savepoint: the activation transaction is owned by
// the common Store writer, not by this handler. It writes every
// session_projection_* row for one generation_id, mirrors the main partition
// into the canonical session_entries table (through the entries the handler
// returns), points the session at the new generation, and derives the one
// session_metrics.turn_count mirror from Metadata.Stats.
type generationIndexFormat struct{}

var _ IndexFormat = generationIndexFormat{}

// V2IndexFormat returns the concrete managed-generation index-format handler. A
// store that installs or reads managed generations must register it with
// WithIndexFormats; without it, activation and generation snapshots fail closed
// instead of pretending an unknown representation is an empty transcript.
func V2IndexFormat() IndexFormat { return generationIndexFormat{} }

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
func (generationIndexFormat) Write(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID, result indexformat.Result) (entries []schema.SessionEntry, retErr error) {
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
	// Prepare the per-row inserts once and re-bind them for every row. The
	// projection entry insert dominates: a native session writes one row per
	// entry, so re-preparing it for every row parses and plans the same SQL
	// thousands of times.
	stmts := newGenerationInstallStatements(conn)
	defer func() { retErr = errors.Join(retErr, stmts.Close()) }()
	now := time.Now().UnixMilli()
	if err := deleteGenerationRowsOnConn(conn, sessionID, generation.ID); err != nil {
		return nil, err
	}
	if err := insertGenerationRowOnConn(conn, sessionID, generation, metadataJSON, titleRefsJSON, now); err != nil {
		return nil, err
	}
	if err := insertGenerationPartitionsOnConn(stmts, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationSegmentsOnConn(stmts, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationContentOnConn(stmts, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertGenerationAliasesOnConn(stmts, sessionID, generation); err != nil {
		return nil, err
	}
	if err := insertRelationshipEvidenceOnConn(stmts, sessionID, generation); err != nil {
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

func insertGenerationPartitionsOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generation indexformat.Generation) error {
	// partition_id 0 is the main stream; earlier sections are 1..N in order.
	mainMetadata, err := json.Marshal(generation.Main.NativeMetadata)
	if err != nil {
		return fmt.Errorf("store: encode main native metadata for generation %s: %w; no generation was activated", generation.ID, err)
	}
	if err := insertSectionOnConn(stmts, sessionID, generation.ID, 0, "", mainMetadata); err != nil {
		return err
	}
	if err := insertEntriesOnConn(stmts, sessionID, generation.ID, 0, generation.Main.Entries); err != nil {
		return err
	}
	for i := range generation.Earlier {
		partitionID := i + 1
		section := generation.Earlier[i]
		nativeMetadata, err := json.Marshal(section.Content.NativeMetadata)
		if err != nil {
			return fmt.Errorf("store: encode earlier[%d] native metadata for generation %s: %w; no generation was activated", i, generation.ID, err)
		}
		if err := insertSectionOnConn(stmts, sessionID, generation.ID, partitionID, string(section.State), nativeMetadata); err != nil {
			return err
		}
		if err := insertEntriesOnConn(stmts, sessionID, generation.ID, partitionID, section.Content.Entries); err != nil {
			return err
		}
	}
	return nil
}

func insertSectionOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generationID string, partitionID int, earlierState string, nativeMetadata []byte) error {
	stmt, err := stmts.Section()
	if err != nil {
		return fmt.Errorf("store: prepare partition insert for generation %s of session %s: %w; no generation was activated", generationID, sessionID, err)
	}
	stmt.BindText(1, string(sessionID))
	stmt.BindText(2, generationID)
	stmt.BindInt64(3, int64(partitionID))
	if earlierState != "" {
		stmt.BindText(4, earlierState)
	} else {
		stmt.BindNull(4)
	}
	stmt.BindText(5, string(nativeMetadata))
	if err := stepAndReset(stmt); err != nil {
		return fmt.Errorf("store: install partition %d for generation %s of session %s: %w; no generation was activated", partitionID, generationID, sessionID, err)
	}
	return nil
}

func insertEntriesOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generationID string, partitionID int, entries []schema.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	stmt, err := stmts.Entry()
	if err != nil {
		return fmt.Errorf("store: prepare projection entry insert for generation %s: %w; no generation was activated", generationID, err)
	}
	for i := range entries {
		entry := entries[i]
		if entry.SessionID != sessionID {
			return fmt.Errorf("store: generation %s partition %d entry %d names session %s; no generation was activated; build entries for the owning session", generationID, partitionID, entry.EntryIndex, entry.SessionID)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("store: encode entry %d of partition %d for generation %s: %w; no generation was activated", entry.EntryIndex, partitionID, generationID, err)
		}
		stmt.BindText(1, string(sessionID))
		stmt.BindText(2, generationID)
		stmt.BindInt64(3, int64(partitionID))
		stmt.BindInt64(4, int64(entry.EntryIndex))
		if entry.SourceEntryRef != "" {
			stmt.BindText(5, string(entry.SourceEntryRef))
		} else {
			stmt.BindNull(5)
		}
		stmt.BindText(6, string(encoded))
		if err := stepAndReset(stmt); err != nil {
			return fmt.Errorf("store: install entry %d of partition %d for generation %s: %w; no generation was activated", entry.EntryIndex, partitionID, generationID, err)
		}
	}
	return nil
}

func insertGenerationSegmentsOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generation indexformat.Generation) error {
	if len(generation.Segments) == 0 {
		return nil
	}
	stmt, err := stmts.Segment()
	if err != nil {
		return fmt.Errorf("store: prepare context segment insert for generation %s: %w; no generation was activated", generation.ID, err)
	}
	for i := range generation.Segments {
		segment := generation.Segments[i]
		refs, err := json.Marshal(segment.CapturedRefs)
		if err != nil {
			return fmt.Errorf("store: encode captured refs for segment %d of generation %s: %w; no generation was activated", segment.Ordinal, generation.ID, err)
		}
		stmt.BindText(1, string(sessionID))
		stmt.BindText(2, generation.ID)
		stmt.BindInt64(3, int64(segment.Ordinal))
		if segment.LogicalSessionID != nil {
			stmt.BindText(4, string(*segment.LogicalSessionID))
		} else {
			stmt.BindNull(4)
		}
		stmt.BindText(5, segment.PhysicalSourceID)
		stmt.BindText(6, string(segment.Coordinates.Kind))
		bindNullableInt64Value(stmt, 7, segment.Coordinates.Start)
		bindNullableInt64Value(stmt, 8, segment.Coordinates.EndExclusive)
		bindNullableInt64Value(stmt, 9, segment.Coordinates.DecodedByteStart)
		bindNullableInt64Value(stmt, 10, segment.Coordinates.DecodedByteEndExclusive)
		stmt.BindText(11, string(segment.Inclusion))
		stmt.BindText(12, string(refs))
		if err := stepAndReset(stmt); err != nil {
			return fmt.Errorf("store: install context segment %d for generation %s: %w; no generation was activated", segment.Ordinal, generation.ID, err)
		}
	}
	return nil
}

func insertGenerationContentOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generation indexformat.Generation) error {
	if len(generation.Content) == 0 {
		return nil
	}
	stmt, err := stmts.Content()
	if err != nil {
		return fmt.Errorf("store: prepare content record insert for generation %s: %w; no generation was activated", generation.ID, err)
	}
	for i := range generation.Content {
		record := generation.Content[i]
		stmt.BindText(1, string(sessionID))
		stmt.BindText(2, generation.ID)
		stmt.BindText(3, string(record.Ref))
		stmt.BindText(4, record.RelativeBlob)
		stmt.BindInt64(5, record.ByteLength)
		stmt.BindText(6, record.Digest)
		if err := stepAndReset(stmt); err != nil {
			return fmt.Errorf("store: install content record %s for generation %s: %w; no generation was activated", record.Ref, generation.ID, err)
		}
	}
	return nil
}

func insertGenerationAliasesOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generation indexformat.Generation) error {
	if len(generation.Aliases) == 0 {
		return nil
	}
	stmt, err := stmts.Alias()
	if err != nil {
		return fmt.Errorf("store: prepare native alias insert for generation %s: %w; no generation was activated", generation.ID, err)
	}
	for i := range generation.Aliases {
		alias := generation.Aliases[i]
		stmt.BindText(1, string(sessionID))
		stmt.BindText(2, generation.ID)
		stmt.BindText(3, alias.NativeKey)
		stmt.BindText(4, string(alias.Ref))
		if err := stepAndReset(stmt); err != nil {
			return fmt.Errorf("store: install native alias %q for generation %s: %w; no generation was activated", alias.NativeKey, generation.ID, err)
		}
	}
	return nil
}

func insertRelationshipEvidenceOnConn(stmts *generationInstallStatements, sessionID schema.SessionID, generation indexformat.Generation) error {
	if len(generation.Metadata.Relationships) == 0 {
		return nil
	}
	stmt, err := stmts.Evidence()
	if err != nil {
		return fmt.Errorf("store: prepare relationship evidence insert for generation %s: %w; no generation was activated", generation.ID, err)
	}
	for i := range generation.Metadata.Relationships {
		relationship := generation.Metadata.Relationships[i]
		var target string
		if relationship.TargetLocalID != nil && *relationship.TargetLocalID != "" {
			target = string(*relationship.TargetLocalID)
		}
		var anchor string
		if relationship.Anchor != nil {
			encoded, err := json.Marshal(relationship.Anchor)
			if err != nil {
				return fmt.Errorf("store: encode anchor for %s relationship of generation %s: %w; no generation was activated", relationship.Kind, generation.ID, err)
			}
			anchor = string(encoded)
		}
		stmt.BindText(1, string(sessionID))
		stmt.BindText(2, generation.ID)
		stmt.BindText(3, string(relationship.Kind))
		stmt.BindText(4, string(relationship.TargetState))
		if target != "" {
			stmt.BindText(5, target)
		} else {
			stmt.BindNull(5)
		}
		if relationship.Evidence != "" {
			stmt.BindText(6, string(relationship.Evidence))
		} else {
			stmt.BindNull(6)
		}
		if anchor != "" {
			stmt.BindText(7, anchor)
		} else {
			stmt.BindNull(7)
		}
		if err := stepAndReset(stmt); err != nil {
			return fmt.Errorf("store: install %s relationship evidence for generation %s: %w; no generation was activated", relationship.Kind, generation.ID, err)
		}
	}
	return nil
}

// bindNullableInt64Value binds a nullable *int64 parameter. A nil bound value
// stays NULL, matching the value sqlitex.ExecuteTransient wrote before the
// statements were reused.
func bindNullableInt64Value(stmt *sqlite.Stmt, param int, value *int64) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindInt64(param, *value)
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
	// The pointing UPDATE names session facts the row-version trigger watches
	// (parent_id, model_harness, start_ms, end_ms), so the trigger clears the
	// recorded publication provenance. Read it first and re-state it right
	// after the UPDATE: pointing a session at a generation is not a new
	// capture, so it must not itself invalidate the session's recorded capture
	// agreement. The capture revision is deliberately NOT re-stated here; the
	// index writer's success stamp remains its only evidence.
	var provenance string
	if err := sqlitex.ExecuteTransient(conn, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			provenance = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read recorded publication provenance before pointing session %s at generation %s: %w; no generation was activated; restore database access and retry activation", sessionID, generation.ID, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = ?, root_session_id = ?, session_purpose = ?, input_submission_count = ?, adapter_version = ?, start_ms = ?, end_ms = ?, model_harness = ?, parent_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{
		generation.ID, root, purpose, inputCount, adapter, generation.Metadata.Timestamp.Start, generation.Metadata.Timestamp.End, string(generation.Metadata.ModelHarness), parent, string(sessionID),
	}}); err != nil {
		return fmt.Errorf("store: point session %s at generation %s: %w; no generation was activated", sessionID, generation.ID, err)
	}
	if provenance != "" {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET cwd_provenance_kind = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{provenance, string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: retain recorded publication provenance after pointing session %s at generation %s: %w; the activation transaction rolled back and the prior generation is visible; repair database access and retry activation", sessionID, generation.ID, err)
		}
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
