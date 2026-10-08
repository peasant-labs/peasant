package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The harmonized snapshot read (design section 6.2 reader 1): one read
// transaction over the generation catalog, its children, the entry mapping
// joined to the body rows, and the captured stats row. The file-backed path
// in generation_snapshot.go stays untouched for Release N; the dispatch key
// is the active generation row's location.
//
// Authoritative reads verify every mapped body in that read transaction, before
// releasing the connection. Preview and migration-shadow reads do not verify.

// harmonizedActiveOnConn reports whether the session's active generation row
// lives in the harmonized catalog. A session with no active generation is
// legacy; an active row outside the harmonized catalog is file-backed. A
// session with no metadata row at all is not an error here: the mirror
// reads below return empty for unknown sessions, and the dispatch must
// preserve that.
func harmonizedActiveOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (active string, harmonized bool, err error) {
	var activeID *string
	found := false
	if err := sqlitex.Execute(conn, `SELECT active_generation_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if stmt.ColumnType(0) != sqlite.TypeNull {
				value := stmt.ColumnText(0)
				activeID = &value
			}
			return nil
		},
	}); err != nil {
		return "", false, fmt.Errorf("store: read active generation for session %s: %w; no generation read was authorized", sessionID, err)
	}
	if !found || activeID == nil {
		return "", false, nil
	}
	located := false
	if err := sqlitex.Execute(conn, `SELECT 1 FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), *activeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			located = true
			return nil
		},
	}); err != nil {
		return "", false, fmt.Errorf("store: locate active generation %s for session %s: %w; no snapshot was built", *activeID, sessionID, err)
	}
	return *activeID, located, nil
}

// harmonizedReadSnapshotOnConn builds the read snapshot for a harmonized
// session: the generation's flattened columns plus its ordered children,
// main and earlier partitions from the mapping and body rows, the captured
// stats row as the wire stats, and one content record per emitted ref. The
// records carry the stored body digest (not a file path). This entry point is
// the unverified migration-shadow read; production full reads select verified
// mode through the dispatched builder.
func harmonizedReadSnapshotOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (indexformat.ReadSnapshot, error) {
	return harmonizedReadSnapshotModeOnConn(conn, sessionID, generationID, false)
}

func harmonizedReadSnapshotModeOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, authoritative bool) (indexformat.ReadSnapshot, error) {
	fail := func(err error) (indexformat.ReadSnapshot, error) {
		return indexformat.ReadSnapshot{}, fmt.Errorf("store: read harmonized generation %s for session %s: %w; the snapshot cannot be built", generationID, sessionID, err)
	}
	metadata, session, err := harmonizedMetadataOnConn(conn, sessionID, generationID)
	if err != nil {
		return fail(err)
	}
	titleRefs, err := harmonizedTitleRefsOnConn(conn, sessionID, generationID)
	if err != nil {
		return fail(err)
	}
	completeness, err := harmonizedCompletenessOnConn(conn, sessionID, generationID)
	if err != nil {
		return fail(err)
	}
	// A preview-only generation cannot serve a full transcript. Leave its
	// fields unverified so the detail boundary can select the explicit preview
	// exit on incompleteness instead of mistaking preview damage for full-read
	// authority. Complete generations verify before any callback can run.
	authoritative = authoritative && completeness == indexformat.GenerationCompletenessComplete
	partitions, content, err := harmonizedPartitionsModeOnConn(conn, sessionID, generationID, authoritative)
	if err != nil {
		return fail(err)
	}
	return indexformat.ReadSnapshot{
		Session:             session,
		Metadata:            metadata,
		TitleRefs:           titleRefs,
		GenerationID:        generationID,
		Completeness:        completeness,
		IndexVersion:        2,
		Main:                partitions.main,
		Earlier:             partitions.earlier,
		Content:             content,
		FullContentVerified: authoritative,
	}, nil
}

// harmonizedMetadataOnConn rebuilds the captured UnifiedMetadata from the
// generation row and its children, then attaches the wire stats from the
// captured stats row. The session payload mirrors the same values the
// file-backed builder derives, so detail bytes agree field for field.
func harmonizedMetadataOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (schema.UnifiedMetadata, schema.SessionDetailPayload, error) {
	var metadata schema.UnifiedMetadata
	var session schema.SessionDetailPayload
	err := sqlitex.Execute(conn, `SELECT
    g.schema_version, g.harness, g.model, g.version,
    g.ts_start, g.ts_end, g.ts_ingested,
    g.source_file_path, g.source_format,
    g.git_branch, g.git_remote, g.git_worktree, g.git_tracking,
    g.project_hash, g.project_file_path, g.project_name,
    g.host_slug, g.root_session_id, g.purpose, g.cwd, g.derived_at,
    g.content_hash, g.metadata_hash,
    g.redaction_applied, g.redaction_level, g.redaction_rule_set_version,
    g.redaction_at_ms, g.redaction_content_hash_at_redact,
    g.adapter_version, g.diagnostics_partial,
    s.parent_id
FROM session_generations g
JOIN sessions s ON s.session_id = g.session_id
WHERE g.session_id = ? AND g.generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			metadata.SchemaVersion = stmt.ColumnInt(0)
			metadata.SessionID = sessionID
			metadata.ModelHarness = schema.Harness(stmt.ColumnText(1))
			metadata.Model = schema.ModelID(stmt.ColumnText(2))
			metadata.Version = stmt.ColumnText(3)
			metadata.Timestamp = schema.TimestampInfo{
				Start: stmt.ColumnInt64(4),
				End:   stmt.ColumnInt64(5),
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				ingested := stmt.ColumnInt64(6)
				metadata.Timestamp.Ingested = &ingested
			}
			metadata.Source = schema.SourceInfo{
				FilePath: stmt.ColumnText(7),
				Format:   schema.SourceFormat(stmt.ColumnText(8)),
			}
			if stmt.ColumnType(9) != sqlite.TypeNull {
				branch := stmt.ColumnText(9)
				metadata.Git.Branch = &branch
			}
			if stmt.ColumnType(10) != sqlite.TypeNull {
				remote := stmt.ColumnText(10)
				metadata.Git.Remote = &remote
			}
			if stmt.ColumnType(11) != sqlite.TypeNull {
				worktree := stmt.ColumnText(11)
				metadata.Git.Worktree = &worktree
			}
			if stmt.ColumnType(12) != sqlite.TypeNull {
				tracking := stmt.ColumnText(12)
				metadata.Git.Tracking = &tracking
			}
			metadata.Project = schema.ProjectContext{
				Hash:     schema.ProjectHash(stmt.ColumnText(13)),
				FilePath: stmt.ColumnText(14),
				Name:     stmt.ColumnText(15),
			}
			metadata.HostSlug = schema.HostSlug(stmt.ColumnText(16))
			if stmt.ColumnType(17) != sqlite.TypeNull {
				root, err := schema.NewSessionID(stmt.ColumnText(17))
				if err != nil {
					return fmt.Errorf("stored root session identity is malformed: %w", err)
				}
				metadata.RootSessionID = &root
			}
			if stmt.ColumnType(18) != sqlite.TypeNull {
				metadata.Purpose = schema.SessionPurpose(stmt.ColumnText(18))
			}
			metadata.CWD = stmt.ColumnText(19)
			if stmt.ColumnType(20) != sqlite.TypeNull {
				derived := stmt.ColumnInt64(20)
				metadata.DerivedAt = &derived
			}
			metadata.ContentHash = stmt.ColumnText(21)
			metadata.MetadataHash = stmt.ColumnText(22)
			metadata.Redaction = schema.RedactionInfo{
				Applied:             stmt.ColumnInt(23) == 1,
				Level:               stmt.ColumnText(24),
				RuleSetVersion:      stmt.ColumnText(25),
				ContentHashAtRedact: stmt.ColumnText(27),
			}
			if stmt.ColumnType(26) != sqlite.TypeNull {
				at := stmt.ColumnInt64(26)
				metadata.Redaction.RedactedAtMs = &at
			}
			if stmt.ColumnType(28) != sqlite.TypeNull {
				adapter := stmt.ColumnInt(28)
				metadata.AdapterVersion = &adapter
			}
			if stmt.ColumnType(29) != sqlite.TypeNull {
				partial := stmt.ColumnInt(29) == 1
				metadata.Diagnostics.Partial = &partial
			}
			if stmt.ColumnType(30) != sqlite.TypeNull {
				parent, err := schema.NewSessionID(stmt.ColumnText(30))
				if err != nil {
					return fmt.Errorf("stored parent identity is malformed: %w", err)
				}
				metadata.ParentUUID = &parent
			}
			return nil
		},
	})
	if err != nil {
		return metadata, session, err
	}
	if metadata.SchemaVersion == 0 {
		return metadata, session, fmt.Errorf("no harmonized generation row; restore the committed generation before reading")
	}
	children, err := harmonizedChildrenOnConn(conn, sessionID, generationID)
	if err != nil {
		return metadata, session, err
	}
	metadata.Subagents = children.subagents
	metadata.Git.Commits = children.commits
	metadata.Git.Associations = children.associations
	metadata.Diagnostics.Warnings = children.diagnostics
	metadata.Relationships = children.relationships
	legacyPresentCollections(&metadata)
	stats, err := readCapturedStatsOnConn(conn, sessionID)
	if err != nil {
		// A native session always owns a stats row past the v62 backfill;
		// without one its measurements read as unknown, exactly like a
		// missing metric row did.
		stats = CapturedStats{SessionID: sessionID, Source: StatsSourceHarness}
	}
	metadata.Stats = capturedStatsToWire(stats)
	session = schema.SessionDetailPayload{
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
	return metadata, session, nil
}

// harmonizedChildren carries the generation's ordered 1:N collections for
// one snapshot build.
type harmonizedChildren struct {
	subagents     []schema.SubagentRef
	commits       []schema.CommitInfo
	associations  []schema.PublishedAssociation
	diagnostics   []schema.DiagnosticEntry
	relationships []schema.SessionRelationship
}

// harmonizedChildrenOnConn reads the metadata children in ordinal order so
// identity and serialization order stay exact.
func harmonizedChildrenOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (harmonizedChildren, error) {
	var children harmonizedChildren
	queries := []struct {
		sql string
		fn  func(stmt *sqlite.Stmt) error
	}{
		{`SELECT subagent_session_id, parent_uuid FROM session_generation_subagents
WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
			children.subagents = append(children.subagents, schema.SubagentRef{
				SessionID:  schema.SessionID(stmt.ColumnText(0)),
				ParentUUID: schema.SessionID(stmt.ColumnText(1)),
			})
			return nil
		}},
		{`SELECT hash, message, author_name, author_email, commit_time, author_time FROM session_generation_commits
WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
			children.commits = append(children.commits, schema.CommitInfo{
				Hash:        stmt.ColumnText(0),
				Message:     stmt.ColumnText(1),
				AuthorName:  stmt.ColumnText(2),
				AuthorEmail: stmt.ColumnText(3),
				CommitTime:  stmt.ColumnInt64(4),
				AuthorTime:  stmt.ColumnInt64(5),
			})
			return nil
		}},
		{`SELECT association_id, observed_commit_hash FROM session_generation_associations
WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
			children.associations = append(children.associations, schema.PublishedAssociation{
				ID:                 schema.AssociationID(stmt.ColumnText(0)),
				ObservedCommitHash: stmt.ColumnText(1),
			})
			return nil
		}},
		{`SELECT error_type, location, message, remediation FROM session_generation_diagnostics
WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
			children.diagnostics = append(children.diagnostics, schema.DiagnosticEntry{
				ErrorType:   stmt.ColumnText(0),
				Location:    stmt.ColumnText(1),
				Message:     stmt.ColumnText(2),
				Remediation: stmt.ColumnText(3),
			})
			return nil
		}},
	}
	for _, query := range queries {
		if err := sqlitex.Execute(conn, query.sql, &sqlitex.ExecOptions{
			Args:       []any{string(sessionID), generationID},
			ResultFunc: query.fn,
		}); err != nil {
			return harmonizedChildren{}, err
		}
	}
	relationships, err := harmonizedRelationshipsOnConn(conn, sessionID, generationID)
	if err != nil {
		return harmonizedChildren{}, err
	}
	children.relationships = relationships
	return children, nil
}

// harmonizedRelationshipsOnConn rebuilds the generation's relationships from
// the structured evidence rows. An anchor is present exactly when its kind
// column is set; the writer binds NULL for an absent anchor and the stored
// values (possibly empty) for a present one.
func harmonizedRelationshipsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]schema.SessionRelationship, error) {
	var relationships []schema.SessionRelationship
	err := sqlitex.Execute(conn, `SELECT kind, target_state, target_local_id, evidence,
anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref
FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			relationship := schema.SessionRelationship{
				Kind:        schema.SessionRelationshipKind(stmt.ColumnText(0)),
				TargetState: schema.RelationshipTargetState(stmt.ColumnText(1)),
				Evidence:    schema.EvidenceKind(stmt.ColumnText(3)),
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				target := schema.SessionID(stmt.ColumnText(2))
				relationship.TargetLocalID = &target
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				anchor := schema.PublicSourceAnchor{
					Kind: schema.PublicSourceAnchorKind(stmt.ColumnText(4)),
				}
				if stmt.ColumnType(5) != sqlite.TypeNull {
					ref := schema.SourceEntryRef(stmt.ColumnText(5))
					anchor.SourceEntryRef = ref
				}
				if stmt.ColumnType(6) != sqlite.TypeNull {
					anchor.SourceRevisionRef = schema.PublicRevisionRef(stmt.ColumnText(6))
				}
				relationship.Anchor = &anchor
			}
			relationships = append(relationships, relationship)
			return nil
		},
	})
	return relationships, err
}

// harmonizedTitleRefsOnConn reads the ordered title refs for one generation.
func harmonizedTitleRefsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]schema.SourceEntryRef, error) {
	var refs []schema.SourceEntryRef
	err := sqlitex.Execute(conn, `SELECT source_entry_ref FROM session_generation_title_refs
WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("decode title ref: %w", err)
			}
			refs = append(refs, ref)
			return nil
		},
	})
	return refs, err
}

// harmonizedCompletenessOnConn reads the generation's closed completeness
// value through its constructor, so an unknown stored value refuses here.
func harmonizedCompletenessOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (indexformat.GenerationCompleteness, error) {
	raw := ""
	found := false
	if err := sqlitex.Execute(conn, `SELECT completeness FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			raw = stmt.ColumnText(0)
			found = true
			return nil
		},
	}); err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no harmonized generation row; restore the committed generation before reading")
	}
	return indexformat.NewGenerationCompleteness(raw)
}

// harmonizedPartitionsOnConn reads every section with its mapped bodies in
// (partition, entry_index) order, returning the display partitions and one
// content record per emitted ref. Partition 0 is the main partition; any
// other partition becomes an earlier-history section under its stored state.
// Each record carries its mapped body's stored digest so the resolver can
// re-verify the row before serving a byte.
func harmonizedPartitionsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (generationPartitions, []indexformat.ContentRecord, error) {
	return harmonizedPartitionsModeOnConn(conn, sessionID, generationID, false)
}

func harmonizedPartitionsModeOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, authoritative bool) (generationPartitions, []indexformat.ContentRecord, error) {
	partitions := generationPartitions{}
	type section struct {
		partitionID int
		state       string
	}
	var sections []section
	if err := sqlitex.Execute(conn, `SELECT partition_id, COALESCE(earlier_state, '') FROM session_projection_sections
WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sections = append(sections, section{stmt.ColumnInt(0), stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		return partitions, nil, err
	}
	mappedByPartition := map[int][]harmonizedMappedEntry{}
	if err := sqlitex.Execute(conn, `SELECT
    m.partition_id,
    b.body_id, b.session_id, b.body_digest, b.entry_index, b.harness, b.entry_type, b.role,
    b.timestamp_ms, b.content_preview, b.tokens_in, b.tokens_out,
    b.has_tool_use, b.tool_kind, b.tool_names_csv,
    b.has_thinking, b.is_error, b.stop_reason, b.raw_byte_length,
    b.tool_call_id, b.entry_id, b.parent_entry_id, b.depth, b.parent_index,
    b.tool_input, b.tool_output,
    b.model_id, b.tokens_reasoning, b.cache_read, b.cache_write,
    b.extra, b.extra_verbatim, b.part_type, b.source_entry_ref,
    b.prov_origin, b.prov_actor, b.prov_delivery, b.prov_ownership,
    b.prov_evidence, b.prov_input_modality, b.prov_submission_ref
FROM session_generation_entries m
LEFT JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
WHERE m.session_id = ? AND m.generation_id = ?
ORDER BY m.partition_id, m.entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			partitionID := stmt.ColumnInt(0)
			if stmt.ColumnType(1) == sqlite.TypeNull {
				return fmt.Errorf("store: a mapped body for session %s generation %s partition %d is missing during snapshot read; no partial transcript was emitted; run harvest verify --content and re-index the session to repair", sessionID, generationID, partitionID)
			}
			row, err := scanBodyRow(stmt, 1)
			if err != nil {
				return err
			}
			if authoritative {
				if err := verifySnapshotBody(row); err != nil {
					return err
				}
			}
			// A mapped body without a source ref is retained evidence the
			// producer never addressed (a carrier row): it hydrates as an
			// entry but emits no content record, since no ref addresses it.
			// The mapping table leaves source_entry_ref NULL for such rows
			// by schema, so the reader must not refuse them.
			mappedByPartition[partitionID] = append(mappedByPartition[partitionID], harmonizedMappedEntry{
				entry:  entryFromRow(row),
				digest: row.BodyDigest,
			})
			return nil
		},
	}); err != nil {
		return partitions, nil, err
	}
	var content []indexformat.ContentRecord
	for _, s := range sections {
		native, err := harmonizedNativeMetadataOnConn(conn, sessionID, generationID, s.partitionID)
		if err != nil {
			return partitions, nil, err
		}
		mappedEntries := mappedByPartition[s.partitionID]
		entries := make([]schema.SessionEntry, 0, len(mappedEntries))
		for _, mappedEntry := range mappedEntries {
			entries = append(entries, mappedEntry.entry)
			if mappedEntry.entry.SourceEntryRef == "" {
				continue
			}
			field := harmonizedContentField(mappedEntry.entry)
			content = append(content, indexformat.ContentRecord{
				Ref:          mappedEntry.entry.SourceEntryRef,
				RelativeBlob: "content/" + string(mappedEntry.entry.SourceEntryRef),
				ByteLength:   int64(len(field)),
				Digest:       mappedEntry.digest,
			})
		}
		partition := indexformat.Partition{Entries: entries, NativeMetadata: native}
		if s.partitionID == 0 {
			partitions.main = partition
			continue
		}
		state, err := schema.NewEarlierHistoryState(s.state)
		if err != nil {
			return partitions, nil, fmt.Errorf("earlier partition %d carries an unknown state: %w", s.partitionID, err)
		}
		partitions.earlier = append(partitions.earlier, indexformat.EarlierPartition{State: state, Content: partition})
	}
	return partitions, content, nil
}

// serializeEntryChecked keeps corrupt structured content on the error path,
// never on serializeEntry's writer-side panic path.
func serializeEntryChecked(row EntryRecord) ([]byte, error) {
	encoded, err := json.Marshal(entryFromRow(row))
	if err != nil {
		return nil, fmt.Errorf("store: serialize entry %d for session %s during full read: %w; no partial transcript was emitted; re-index the session to repair", row.EntryIndex, row.SessionID, err)
	}
	return encoded, nil
}

func verifySnapshotBody(row EntryRecord) error {
	encoded, err := serializeEntryChecked(row)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != row.BodyDigest {
		return fmt.Errorf("store: entry at index %d for session %s fails digest verification during full snapshot read; stored columns do not match the captured digest; no partial transcript was emitted; run harvest verify --content and re-index the session to repair", row.EntryIndex, row.SessionID)
	}
	return nil
}

// harmonizedMappedEntry is one emitted entry with its stored body digest.
// The digest travels with the entry into the content record so the resolver
// can re-verify the row before serving a single byte.
type harmonizedMappedEntry struct {
	entry  schema.SessionEntry
	digest string
}

// scanBodyRow reads one session_entry_bodies row starting at column offset.
// The selected columns must follow the table's definition order from the
// offset on; closed sets reuse the schema types with direct casts (the
// scanSessionEntry precedent), and the assembled snapshot's Validate fails
// closed on anything unrecognized.
func scanBodyRow(stmt *sqlite.Stmt, off int) (EntryRecord, error) {
	row := EntryRecord{
		BodyID:         stmt.ColumnInt64(off),
		SessionID:      schema.SessionID(stmt.ColumnText(off + 1)),
		BodyDigest:     stmt.ColumnText(off + 2),
		EntryIndex:     stmt.ColumnInt(off + 3),
		Harness:        schema.Harness(stmt.ColumnText(off + 4)),
		EntryType:      schema.EntryType(stmt.ColumnText(off + 5)),
		Role:           schema.Role(stmt.ColumnText(off + 6)),
		HasToolUse:     stmt.ColumnInt(off+11) == 1,
		ToolNamesCSV:   nullableText(stmt, off+13),
		HasThinking:    stmt.ColumnInt(off+14) == 1,
		IsError:        stmt.ColumnInt(off+15) == 1,
		Depth:          stmt.ColumnInt(off + 21),
		SourceEntryRef: schema.SourceEntryRef(stmt.ColumnText(off + 32)),
	}
	row.TimestampMs = nullableInt64Col(stmt, off+7)
	row.ContentPreview = nullableText(stmt, off+8)
	row.TokensIn = nullableIntCol(stmt, off+9)
	row.TokensOut = nullableIntCol(stmt, off+10)
	if stmt.ColumnType(off+12) != sqlite.TypeNull {
		kind := schema.ToolCallKind(stmt.ColumnText(off + 12))
		row.ToolKind = &kind
	}
	if stmt.ColumnType(off+16) != sqlite.TypeNull {
		reason := schema.StopReason(stmt.ColumnText(off + 16))
		row.StopReason = &reason
	}
	row.RawByteLength = nullableIntCol(stmt, off+17)
	row.ToolCallID = nullableText(stmt, off+18)
	row.EntryID = nullableText(stmt, off+19)
	row.ParentEntryID = nullableText(stmt, off+20)
	row.ParentIndex = nullableIntCol(stmt, off+22)
	row.ToolInput = nullableText(stmt, off+23)
	row.ToolOutput = nullableText(stmt, off+24)
	row.ModelID = nullableText(stmt, off+25)
	row.TokensReasoning = nullableIntCol(stmt, off+26)
	row.CacheRead = nullableIntCol(stmt, off+27)
	row.CacheWrite = nullableIntCol(stmt, off+28)
	row.Extra = nullableText(stmt, off+29)
	row.ExtraVerbatim = nullableText(stmt, off+30)
	row.PartType = nullableText(stmt, off+31)
	row.Provenance = scanProvenance(stmt, off+33)
	return row, nil
}

// scanProvenance rebuilds the content provenance from the eight prov_*
// columns. All NULL means the entry carries none; any other combination
// builds the struct as stored.
func scanProvenance(stmt *sqlite.Stmt, off int) *schema.ContentProvenance {
	cols := make([]*string, 7)
	empty := true
	for i := range cols {
		if stmt.ColumnType(off+i) != sqlite.TypeNull {
			v := stmt.ColumnText(off + i)
			cols[i] = &v
			empty = false
		}
	}
	if empty {
		return nil
	}
	provenance := &schema.ContentProvenance{}
	if cols[0] != nil {
		provenance.Origin = schema.ContentOrigin(*cols[0])
	}
	if cols[1] != nil {
		provenance.Actor = schema.ActorOrigin(*cols[1])
	}
	if cols[2] != nil {
		provenance.Delivery = schema.DeliveryOrigin(*cols[2])
	}
	if cols[3] != nil {
		provenance.Ownership = schema.ContentOwnership(*cols[3])
	}
	if cols[4] != nil {
		provenance.Evidence = schema.EvidenceKind(*cols[4])
	}
	if cols[5] != nil {
		provenance.InputModality = schema.InputModality(*cols[5])
	}
	if cols[6] != nil {
		provenance.SubmissionRef = schema.SubmissionRef(*cols[6])
	}
	return provenance
}

// harmonizedNativeMetadataOnConn reads one partition's ordered native
// metadata from the structured child rows. The attachment is present exactly
// when a column carries it: the writer binds NULL for an absent attachment
// and the stored text (possibly empty) for a present one, so an empty tool
// call ID reassembles instead of collapsing to absent.
func harmonizedNativeMetadataOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, partitionID int) ([]schema.NativeMetadataRecord, error) {
	var records []schema.NativeMetadataRecord
	err := sqlitex.Execute(conn, `SELECT native_id, kind, source_entry_ref, source_type, source_message_role,
    attachment_turn_index, attachment_tool_call_id, custom_type, data
FROM session_section_native_metadata
WHERE session_id = ? AND generation_id = ? AND partition_id = ? ORDER BY ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID, partitionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record := schema.NativeMetadataRecord{
				ID:   stmt.ColumnText(0),
				Kind: schema.NativeMetadataKind(stmt.ColumnText(1)),
				Source: schema.NativeSourceRef{
					EntryRef:   schema.SourceEntryRef(stmt.ColumnText(2)),
					SourceType: schema.NativeMetadataSourceType(stmt.ColumnText(3)),
				},
				CustomType: stmt.ColumnText(7),
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				record.Source.MessageRole = schema.NativePiMessageRole(stmt.ColumnText(4))
			}
			if stmt.ColumnType(5) != sqlite.TypeNull || stmt.ColumnType(6) != sqlite.TypeNull {
				attachment := &schema.NativeAttachmentRef{}
				if stmt.ColumnType(5) != sqlite.TypeNull {
					turn := stmt.ColumnInt(5)
					attachment.TurnIndex = &turn
				}
				if stmt.ColumnType(6) != sqlite.TypeNull {
					attachment.ToolCallID = stmt.ColumnText(6)
				}
				record.Attachment = attachment
			}
			if stmt.ColumnType(8) != sqlite.TypeNull {
				record.Data = append([]byte(nil), stmt.ColumnText(8)...)
			}
			records = append(records, record)
			return nil
		},
	})
	return records, err
}

// harmonizedContentField selects the content bytes one entry owns, matching
// the projection builder's field selection: tool input for tool_use,
// tool output for tool_result, display text otherwise.
func harmonizedContentField(entry schema.SessionEntry) string {
	switch entry.EntryType {
	case schema.EntryTypeToolUse:
		if entry.ToolInput != nil {
			return *entry.ToolInput
		}
	case schema.EntryTypeToolResult:
		if entry.ToolOutput != nil {
			return *entry.ToolOutput
		}
	default:
		if entry.ContentPreview != nil {
			return *entry.ContentPreview
		}
	}
	return ""
}

// ReadHarmonizedEntry resolves one mapped body for the resolver path below.
func readHarmonizedBodyOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, ref schema.SourceEntryRef, digest string) (EntryRecord, error) {
	// The mapping binds the read to the generation; the digest addresses the
	// exact body, so a superseded generation's identical entry can never leak
	// into this generation's hydration.
	var row EntryRecord
	found := false
	err := sqlitex.Execute(conn, `SELECT
    b.body_id, b.session_id, b.body_digest, b.entry_index, b.harness, b.entry_type, b.role,
    b.timestamp_ms, b.content_preview, b.tokens_in, b.tokens_out,
    b.has_tool_use, b.tool_kind, b.tool_names_csv,
    b.has_thinking, b.is_error, b.stop_reason, b.raw_byte_length,
    b.tool_call_id, b.entry_id, b.parent_entry_id, b.depth, b.parent_index,
    b.tool_input, b.tool_output,
    b.model_id, b.tokens_reasoning, b.cache_read, b.cache_write,
    b.extra, b.extra_verbatim, b.part_type, b.source_entry_ref,
    b.prov_origin, b.prov_actor, b.prov_delivery, b.prov_ownership,
    b.prov_evidence, b.prov_input_modality, b.prov_submission_ref
FROM session_generation_entries m
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
WHERE m.session_id = ? AND m.generation_id = ? AND m.source_entry_ref = ? AND m.body_digest = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID, string(ref), digest},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			scanned, err := scanBodyRow(stmt, 0)
			if err != nil {
				return err
			}
			row = scanned
			found = true
			return nil
		},
	})
	if err != nil {
		return EntryRecord{}, err
	}
	if !found {
		return EntryRecord{}, fmt.Errorf("no mapped entry for ref %q under digest %.12s; the snapshot is not self-contained; no partial transcript was emitted", ref, digest)
	}
	return row, nil
}

// readHarmonizedContentOnConn serves one entry's content-field bytes after
// re-verifying the stored body digest against the recomputed canonical text.
// A column altered under the stored digest refuses here, so a full read never
// emits bytes the store cannot prove it captured.
func readHarmonizedContentOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, record indexformat.ContentRecord) ([]byte, error) {
	if record.Digest == "" {
		return nil, fmt.Errorf("store: harmonized content record for ref %q carries no integrity digest; no bytes were served; re-index the session to repair its refs", record.Ref)
	}
	row, err := readHarmonizedBodyOnConn(conn, sessionID, generationID, record.Ref, record.Digest)
	if err != nil {
		return nil, err
	}
	if err := verifySnapshotBody(row); err != nil {
		return nil, err
	}
	field := harmonizedContentField(entryFromRow(row))
	if int64(len(field)) != record.ByteLength {
		return nil, fmt.Errorf("store: entry at index %d for session %s fails length verification (%d against %d); no partial transcript was emitted; run harvest verify --content and re-index the session to repair", row.EntryIndex, sessionID, len(field), record.ByteLength)
	}
	return []byte(field), nil
}

// WithHarmonizedSessionSnapshot is the harmonized side of the one read
// router, exposed for the migration's forced-harmonized shadow reads: it
// builds the snapshot from the body rows even when the caller already holds
// the session. Production reads enter through WithSessionSnapshot, which
// selects this path by the active generation row's location.
func (s *Store) WithHarmonizedSessionSnapshot(ctx context.Context, sessionID schema.SessionID, fn func(indexformat.ReadSnapshot) error) (retErr error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for harmonized snapshot %s: %w", sessionID, err)
	}
	active, harmonized, err := harmonizedActiveOnConn(conn, sessionID)
	if err != nil {
		s.pool.Put(conn)
		return err
	}
	if !harmonized {
		s.pool.Put(conn)
		return fmt.Errorf("%w: session %s has no harmonized generation; read it through the dispatched snapshot instead", ErrLegacySnapshot, sessionID)
	}
	endSnapshot := sqlitex.Save(conn)
	snapshot, readErr := harmonizedReadSnapshotOnConn(conn, sessionID, active)
	endSnapshot(&readErr)
	s.pool.Put(conn)
	if readErr != nil {
		return readErr
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("store: the harmonized snapshot for session %s is not a coherent read: %w; no hydration was authorized", sessionID, err)
	}
	return fn(snapshot)
}
