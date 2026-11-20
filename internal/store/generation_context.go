package store

import (
	"fmt"
	"reflect"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// candidateContextMatchesOnConn reports whether a metadata candidate maps to
// exactly the stored context of one generation: its catalog row and its
// context rows (subagents, commits, associations, diagnostics and
// relationship evidence). It is the context half of the parse's skip
// comparison, applied to a candidate built before any entry parse.
//
// The comparison ignores what the skip comparison ignores (the generation
// identity, the stats-excluded metadata hash, the adapter revision and the
// install stamps), and also the activation seal, which only a parse can
// recompute; the seal's presence is a separate term. Diagnostics drop the
// stats-overflow row the stats upsert writes, and relationship ordinals are
// cleared on both sides, since the stored read orders by ordinal without
// returning it. Order counts everywhere else.
//
// It reads the generation row and the context rows only: no mapping row, no
// body, no blob and no stat. The caller owns the transaction.
func candidateContextMatchesOnConn(conn *sqlite.Conn, sid schema.SessionID, generationID string, c ingest.GenerationMetadataCandidate) (bool, error) {
	if c.Metadata.SessionID != sid {
		return false, nil
	}
	var view activeHarmonizedView
	if err := loadGenerationRecordOnConn(conn, sid, generationID, &view); err != nil {
		return false, err
	}
	if !view.found {
		return false, nil
	}
	stored := view.record
	if c.Metadata.ModelHarness != stored.Harness || c.Metadata.ContentHash != stored.ContentHash || c.Metadata.Source.Format != stored.SourceFormat {
		return false, nil
	}
	record, children, err := mapGenerationRecord(sid, indexformat.Generation{
		ID:                   generationID,
		Metadata:             c.Metadata,
		Completeness:         c.Completeness,
		SourceEvidenceDigest: c.SourceEvidenceDigest,
	})
	if err != nil {
		return false, fmt.Errorf("store: map the metadata candidate of session %s for context validation: %w; the candidate is not comparable, so the session parses", sid, err)
	}
	record.CandidateDigest = ""
	stored.CandidateDigest = ""
	if !generationRecordsEqual(record, stored) {
		return false, nil
	}
	var storedChildren GenerationChildren
	if err := loadGenerationContextChildrenOnConn(conn, sid, generationID, &storedChildren); err != nil {
		return false, err
	}
	return reflect.DeepEqual(comparableContextChildren(children), comparableContextChildren(storedChildren)), nil
}

// comparableContextChildren keeps only the context rows of a generation's
// children, in their stored order, with the stats-overflow diagnostic dropped
// and the relationship ordinals cleared.
func comparableContextChildren(children GenerationChildren) GenerationChildren {
	captured := capturedGenerationChildren(children)
	out := GenerationChildren{
		Subagents:    captured.Subagents,
		Commits:      captured.Commits,
		Associations: captured.Associations,
		Diagnostics:  captured.Diagnostics,
	}
	for _, relationship := range captured.Relationships {
		relationship.Ordinal = 0
		out.Relationships = append(out.Relationships, relationship)
	}
	return out
}

// loadGenerationContextChildrenOnConn reads the context rows of one
// generation, each in ordinal order: subagents, commits, associations,
// diagnostics and relationship evidence. These are the rows the metadata
// mapper writes; the parse rows (title refs, sections, native metadata,
// segments) are not read.
func loadGenerationContextChildrenOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, children *GenerationChildren) error {
	query := func(sql string, fn func(*sqlite.Stmt) error) error {
		return sqlitex.Execute(conn, sql, &sqlitex.ExecOptions{
			Args:       []any{string(sessionID), generationID},
			ResultFunc: fn,
		})
	}
	if err := query(`SELECT ordinal, subagent_session_id, parent_uuid FROM session_generation_subagents WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		children.Subagents = append(children.Subagents, GenerationSubagent{
			Ordinal: int(stmt.ColumnInt64(0)), SubagentSessionID: schema.SessionID(stmt.ColumnText(1)), ParentUUID: schema.SessionID(stmt.ColumnText(2)),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation subagents for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, hash, message, author_name, author_email, commit_time, author_time FROM session_generation_commits WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		children.Commits = append(children.Commits, GenerationCommit{
			Ordinal: int(stmt.ColumnInt64(0)), Hash: stmt.ColumnText(1), Message: stmt.ColumnText(2),
			AuthorName: stmt.ColumnText(3), AuthorEmail: stmt.ColumnText(4),
			CommitTime: stmt.ColumnInt64(5), AuthorTime: stmt.ColumnInt64(6),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation commits for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, association_id, observed_commit_hash FROM session_generation_associations WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		children.Associations = append(children.Associations, GenerationAssociation{
			Ordinal: int(stmt.ColumnInt64(0)), AssociationID: stmt.ColumnText(1), ObservedCommitHash: stmt.ColumnText(2),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation associations for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, error_type, location, message, remediation FROM session_generation_diagnostics WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		children.Diagnostics = append(children.Diagnostics, GenerationDiagnostic{
			Ordinal: int(stmt.ColumnInt64(0)), ErrorType: stmt.ColumnText(1), Location: stmt.ColumnText(2),
			Message: stmt.ColumnText(3), Remediation: stmt.ColumnText(4),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation diagnostics for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		relationship := GenerationRelationship{
			Kind:        schema.SessionRelationshipKind(stmt.ColumnText(0)),
			TargetState: schema.RelationshipTargetState(stmt.ColumnText(1)),
		}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			target := schema.SessionID(stmt.ColumnText(2))
			relationship.TargetLocalID = &target
		}
		if stmt.ColumnType(3) != sqlite.TypeNull {
			evidence := stmt.ColumnText(3)
			relationship.Evidence = &evidence
		}
		if stmt.ColumnType(4) != sqlite.TypeNull {
			kind := schema.PublicSourceAnchorKind(stmt.ColumnText(4))
			relationship.AnchorKind = &kind
		}
		if stmt.ColumnType(5) != sqlite.TypeNull {
			ref := schema.SourceEntryRef(stmt.ColumnText(5))
			relationship.AnchorSourceEntryRef = &ref
		}
		if stmt.ColumnType(6) != sqlite.TypeNull {
			revision := stmt.ColumnText(6)
			relationship.AnchorSourceRevisionRef = &revision
		}
		children.Relationships = append(children.Relationships, relationship)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read relationship evidence for session %s: %w", sessionID, err)
	}
	return nil
}
