package ingest

import (
	"context"
	"fmt"

	"zombiezen.com/go/sqlite"
)

const (
	openCodeSessionCursorRecordStatement   = "SELECT id, parent_id, time_updated, directory, title, time_created, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert FROM session WHERE id = ?1 LIMIT 2"
	openCodeSessionV2CursorRecordStatement = "SELECT id, parent_id, time_updated, directory, title, time_created, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert FROM session_v2 WHERE id = ?1 LIMIT 2"
	openCodeCursorDirectoryStatement       = "SELECT project_id, directory, type FROM project_directory WHERE directory = ?1 ORDER BY project_id LIMIT 2"
	openCodeCursorProjectStatement         = "SELECT id, worktree, vcs, name FROM project WHERE id = ?1 LIMIT 2"
	openCodeCursorSequenceStatement        = "SELECT seq FROM event_sequence WHERE aggregate_id = ?1 LIMIT 2"
)

type openCodeCursorSnapshot interface {
	acquiredMaterializationCursor(context.Context, DiscoveredSession) (*int64, []DiagnosticEntry, error)
}

var _ CursorTranscriptMaterializer = (*OpenCodeAdapter)(nil)

func acquireOpenCodeMaterializationCursor(ctx context.Context, source OpenCodeSQLiteSource, session DiscoveredSession) (*int64, []DiagnosticEntry, error) {
	reader, ok := source.(openCodeCursorSnapshot)
	if !ok {
		return unavailableOpenCodeCursor(session, "the configured source cannot prove a shared read-only materialization snapshot")
	}
	return reader.acquiredMaterializationCursor(ctx, session)
}

func unavailableOpenCodeCursor(session DiscoveredSession, reason string) (*int64, []DiagnosticEntry, error) {
	return nil, []DiagnosticEntry{{
		ErrorType: "native_cursor_unavailable", Location: session.SourcePath.String(),
		Message:     fmt.Sprintf("materialized session %s without advancing native event progress: %s; earlier progress was preserved instead of copying discovery's cursor", session.SessionID, reason),
		Remediation: "Retry harvest when the native cursor is readable; unsupported or absent optional evidence remains unknown and may require conservative re-extraction.",
	}}, nil
}

func (s *zombiezenOpenCodeSQLiteSource) acquiredMaterializationCursor(ctx context.Context, session DiscoveredSession) (*int64, []DiagnosticEntry, error) {
	if !s.options.readSnapshot {
		return unavailableOpenCodeCursor(session, "no read transaction covered the materialized input")
	}
	lease, err := s.beginSourceRead(ctx, "qualify acquired materialization cursor")
	if err != nil {
		return nil, nil, err
	}
	defer lease.release()
	support, err := s.sessionColumnSupportLocked(lease.ctx)
	if err != nil {
		return nil, nil, err
	}
	if !support.extendedAttribution() {
		return unavailableOpenCodeCursor(session, "this older partial session layout has no supported targeted attribution proof")
	}
	var record *OpenCodeSessionRecord
	rows := 0
	decode := func(stmt *sqlite.Stmt) error {
		rows++
		value, _, err := decodeOpenCodeSessionRecord(stmt, support)
		if err != nil {
			return err
		}
		record = &value
		return nil
	}
	if support.table == OpenCodeSessionTableV2 {
		err = s.executeRowsLocked(lease.ctx, openCodeSessionV2CursorRecordStatement, []any{string(session.SessionID)}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeSessionCursorRecordStatement, []any{string(session.SessionID)}, decode)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("validate consumed attribution for session %s: %w; prior artifact was preserved; rediscover the native session before retrying", session.SessionID, err)
	}
	if rows != 1 || record == nil || record.SessionID.String() != string(session.SessionID) {
		return nil, nil, fmt.Errorf("validate consumed attribution for session %s: authoritative session row disappeared or is ambiguous; prior artifact was preserved; rediscover the native session before retrying", session.SessionID)
	}
	// Discovery may deliberately keep an orphan as a root. An unselected raw
	// parent is not consumed by metadata, so do not invent a new parent decision.
	parentChanged := session.ParentUUID != nil && record.ParentID.String() != string(*session.ParentUUID)
	storedIn, storedOut := normalizedOpenCodeCursorTokens(int64(session.TokensIn), int64(session.TokensOut))
	currentIn, currentOut := normalizedOpenCodeCursorTokens(int64(int(record.TokensInput)), int64(int(record.TokensOutput)))
	if parentChanged || session.CWD != record.Directory || session.Version != record.Version || storedIn != currentIn || storedOut != currentOut {
		return nil, nil, fmt.Errorf("native attribution changed after discovery for session %s; materialization did not publish mixed input or consume the newer cursor; prior artifact was preserved; retry harvest to rediscover the session", session.SessionID)
	}
	attribution, ambiguous, err := s.cursorProjectAttributionLocked(lease.ctx, session.CWD)
	if err != nil {
		return nil, nil, fmt.Errorf("validate consumed project attribution for session %s: %w; prior artifact was preserved; restore source read access and rediscover before retrying", session.SessionID, err)
	}
	if ambiguous {
		return unavailableOpenCodeCursor(session, "multiple native project mappings prevent bounded attribution proof")
	}
	candidates := []openCodeSessionCandidate{{session: DiscoveredSession{CWD: session.CWD}}}
	attributeOpenCodeProjects(candidates, attribution)
	current := candidates[0].session
	projectName := session.ProjectName
	if session.ProjectWorktree == "" {
		projectName = ""
	}
	if session.ProjectWorktree != current.ProjectWorktree || projectName != current.ProjectName {
		return nil, nil, fmt.Errorf("native project attribution changed after discovery for session %s; prior artifact and cursor were preserved; retry harvest to rediscover its project", session.SessionID)
	}
	sequence, err := s.cursorEventSequenceLocked(lease.ctx, session.SessionID)
	if err != nil {
		return unavailableOpenCodeCursor(session, err.Error())
	}
	if sequence == nil {
		return unavailableOpenCodeCursor(session, "the native source has no event sequence for this session")
	}
	return sequence, nil, nil
}

func normalizedOpenCodeCursorTokens(input, output int64) (int64, int64) {
	if input > 0 || output > 0 {
		return input, output
	}
	return 0, 0 // Both selectors fall back to the same transcript-derived totals.
}

func (s *zombiezenOpenCodeSQLiteSource) cursorProjectAttributionLocked(ctx context.Context, directory string) (OpenCodeProjectAttribution, bool, error) {
	var attribution OpenCodeProjectAttribution
	if directory == "" {
		return attribution, false, nil
	}
	projects, err := s.columnsLocked(ctx, "project")
	if err != nil {
		return attribution, false, err
	}
	directories, err := s.columnsLocked(ctx, "project_directory")
	if err != nil {
		return attribution, false, err
	}
	if !projectColumnsPresent(projects, "id", "worktree", "vcs", "name") || !projectColumnsPresent(directories, "project_id", "directory", "type") {
		return attribution, false, nil
	}
	attribution.ProjectsPresent, attribution.DirectoriesPresent = true, true
	rows := 0
	err = s.executeRowsLocked(ctx, openCodeCursorDirectoryStatement, []any{directory}, func(stmt *sqlite.Stmt) error {
		rows++
		id, err := NewOpenCodeProjectID(stmt.ColumnText(0))
		if err != nil {
			return nil
		}
		attribution.Directories = append(attribution.Directories, OpenCodeProjectDirectoryRecord{ProjectID: id, Directory: stmt.ColumnText(1), Type: stmt.ColumnText(2)})
		return nil
	})
	if err != nil || rows > 1 || len(attribution.Directories) == 0 {
		return attribution, rows > 1, err
	}
	rows = 0
	err = s.executeRowsLocked(ctx, openCodeCursorProjectStatement, []any{attribution.Directories[0].ProjectID.String()}, func(stmt *sqlite.Stmt) error {
		rows++
		id, err := NewOpenCodeProjectID(stmt.ColumnText(0))
		if err == nil {
			attribution.Projects = append(attribution.Projects, OpenCodeProjectRecord{ID: id, Worktree: stmt.ColumnText(1), VCS: stmt.ColumnText(2), Name: stmt.ColumnText(3)})
		}
		return nil
	})
	return attribution, rows > 1, err
}

func (s *zombiezenOpenCodeSQLiteSource) cursorEventSequenceLocked(ctx context.Context, sid SessionID) (*int64, error) {
	columns, err := s.columnsLocked(ctx, "event_sequence")
	if err != nil {
		return nil, err
	}
	var sequence *int64
	rows := 0
	decode := func(stmt *sqlite.Stmt) error {
		rows++
		if stmt.ColumnType(0) == sqlite.TypeNull {
			return nil
		}
		if stmt.ColumnType(0) != sqlite.TypeInteger || stmt.ColumnInt64(0) < 0 {
			return fmt.Errorf("native event cursor is not a nonnegative integer")
		}
		value := stmt.ColumnInt64(0)
		sequence = &value
		return nil
	}
	if projectColumnsPresent(columns, "aggregate_id", "seq") {
		err = s.executeRowsLocked(ctx, openCodeCursorSequenceStatement, []any{string(sid)}, decode)
	} else {
		id, err := NewOpenCodeSessionLinkID(string(sid))
		if err != nil {
			return nil, err
		}
		value, err := s.maxEventSeqLocked(ctx, id)
		if err != nil || !value.Present {
			return nil, err
		}
		if value.Seq < 0 {
			return nil, fmt.Errorf("native event cursor is not a nonnegative integer")
		}
		return &value.Seq, nil
	}
	if rows > 1 {
		return nil, fmt.Errorf("native event cursor is ambiguous")
	}
	if err != nil {
		return nil, err
	}
	return sequence, nil
}
