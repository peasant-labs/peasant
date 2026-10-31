package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

var _ ingest.LogicalParentReader = (*Store)(nil)

// The logical-parent answer for independently admitted children.
//
// sessions.parent_id is a nullable FK availability cache. A nil cache never
// proves a root session and never hides a known relationship: the durable
// logical evidence lives in session_relationship_evidence for generations with
// a catalog row and in the managed metadata ParentUUID for V1 pairs. Readers
// that answer "who is the parent" consult the logical evidence; readers that
// answer "which cache row can join" use the cache.
//
// The parent-answer lane has exactly TWO spellings, and no third:
//
//   - LogicalParentSQL, the SQL rendering below, embedded by every read that
//     answers the parent question in any package; and
//   - startedByTarget, the in-memory predicate in parent_cache.go, used where
//     the relationships are already decoded.
//
// indexformat.durableStartedByTarget is the validator layer's own pinned mirror
// of the in-memory predicate (internal/indexformat/generation.go), required by
// validateParentMirrors. It cannot share this spelling: indexformat sits below
// store and cannot import it, and it differs in one edge (it does not require a
// non-empty target local identifier). It is pinned by the validator's own
// tests, carries no behavior change here, and is the one named exception.

// parentGenerationScopeSQL renders the ONE generation scope the parent answer
// consults: the active generation when the session has one, otherwise the
// latest attempt. It is spliced by LogicalParentSQL and by the grouping
// evidence reader and the reverse-reconciliation templates, so every surface
// that resolves a generation for the parent answer agrees on which one.
func parentGenerationScopeSQL(alias string) string {
	return "COALESCE(" + alias + ".active_generation_id, " + alias + ".latest_attempt_generation_id)"
}

// LogicalParentSQL renders the one parent-answer rule as a scalar expression
// that correlates on alias: the started_by evidence target of the session's
// active generation (or its latest attempt when no active generation exists)
// when that target state is known or retained and the target is non-empty,
// otherwise the availability cache.
//
// A NULL answer never proves a root: an evidenced session whose target is
// unavailable still answers its evidence target, and only a session with no
// evidence at all falls back to the cache. Generation scoping is exact: an
// active generation is consulted on its own, and the latest attempt is
// consulted only when no active generation exists.
func LogicalParentSQL(alias string) string {
	return `COALESCE((
  SELECT r.target_local_id FROM session_relationship_evidence r
   WHERE r.session_id = ` + alias + `.session_id
     AND r.generation_id = ` + parentGenerationScopeSQL(alias) + `
     AND r.kind = 'started_by'
     AND r.target_state IN ('target_known','target_known_retained')
     AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
), ` + alias + `.parent_id)`
}

// ParentUnresolvedSQL renders whether a session is a known spawned child whose
// parent cannot be named: its started_by evidence, in the same generation scope
// as LogicalParentSQL, says so. The session list and the local sync list read
// the same two target states.
func ParentUnresolvedSQL(alias string) string {
	return `EXISTS (
  SELECT 1 FROM session_relationship_evidence r
   WHERE r.session_id = ` + alias + `.session_id
     AND r.generation_id = ` + parentGenerationScopeSQL(alias) + `
     AND r.kind = 'started_by'
     AND r.target_state IN ('unknown','conflicting_current_native_evidence')
)`
}

// LogicalChildSQL renders whether a session is a subagent session: it has a
// logical parent, or it is a known child whose parent cannot be named. A parent
// the store does not hold still makes the session a child.
func LogicalChildSQL(alias string) string {
	return `(` + LogicalParentSQL(alias) + ` IS NOT NULL OR ` + ParentUnresolvedSQL(alias) + `)`
}

// SessionLineage is where one session sits under its parent. It carries the
// same meaning as the parent fields of schema.LocalSessionRow: at most one of
// ParentSessionID and ParentUnresolved is set, and ParentSessionID may name a
// session the store does not hold.
type SessionLineage struct {
	ParentSessionID  *string
	ParentUnresolved bool
	RootSessionID    *string
}

// SessionLineages answers the lineage of each named session through the same
// rendered rules the MainOnly filter applies, so a row's fields and the filter
// cannot disagree. A session the store does not hold has no entry.
func (s *Store) SessionLineages(ctx context.Context, sessionIDs []string) (map[string]SessionLineage, error) {
	lineages := make(map[string]SessionLineage, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return lineages, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read session lineage: %w; no lineage was read; retry the command", err)
	}
	defer s.pool.Put(conn)
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		batch := sessionIDs[start:min(start+indexFormatReadBatchSize, len(sessionIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		query := `SELECT s.session_id, ` + LogicalParentSQL("s") + `, ` + ParentUnresolvedSQL("s") + `, s.root_session_id
FROM sessions s WHERE s.session_id IN (?` + strings.Repeat(",?", len(batch)-1) + `)`
		err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				var lineage SessionLineage
				lineage.ParentUnresolved = stmt.ColumnInt(2) != 0
				if !lineage.ParentUnresolved && stmt.ColumnType(1) != sqlite.TypeNull {
					parent := stmt.ColumnText(1)
					lineage.ParentSessionID = &parent
				}
				if stmt.ColumnType(3) != sqlite.TypeNull {
					root := stmt.ColumnText(3)
					lineage.RootSessionID = &root
				}
				lineages[stmt.ColumnText(0)] = lineage
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("store: read session lineage: %w; no lineage was read; run peasant harvest verify and retry", err)
		}
	}
	return lineages, nil
}

// LogicalParentForSession returns the durable logical parent for one session:
// the evidence target of the session's generation when one answers, otherwise
// the availability cache. A nil return means no known logical parent; it never
// proves a root when relationship evidence names an unavailable target. V1
// pairs without catalog evidence fall back to the cache, which was written from
// the same managed metadata document; their full logical ParentUUID remains in
// the managed metadata file.
func (s *Store) LogicalParentForSession(ctx context.Context, sessionID ingest.SessionID) (*string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to answer the logical parent of session %s: %w; the parent answer was not read; restore database access and retry", sessionID, err)
	}
	defer s.pool.Put(conn)
	return logicalParentForSessionOnConn(conn, sessionID)
}

// logicalParentForSessionOnConn is the same answer on a caller's connection, so
// a read that already holds one (the publication bundle composition) answers
// through the same statement instead of opening a second connection.
func logicalParentForSessionOnConn(conn *sqlite.Conn, sessionID ingest.SessionID) (*string, error) {
	var parent *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT `+LogicalParentSQL("s")+` FROM sessions s WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnText(0)
				parent = &v
			}
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read the logical parent of session %s: %w; the parent answer was not read; restore database access and retry", sessionID, err)
	}
	return parent, nil
}
