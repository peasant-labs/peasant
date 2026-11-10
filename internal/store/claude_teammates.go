package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ClaudeTeammateReadBatchSize bounds one teammate evidence query. It aliases
// the store's read batch size rather than opening a second budget; it is
// exported under this name only so callers and the external bounded-page test
// can reason about how many batches a page costs without copying the number.
const ClaudeTeammateReadBatchSize = indexFormatReadBatchSize

// The store reads its own spawn projection by path for the teammate-link delta.
var _ ingest.ClaudeSpawnContributionReader = (*Store)(nil)

// claudeTeammateQuery reads the recorded teammate identity of each requested
// session. The identity is evidence, not inference: the child must be a stored
// Claude Code session with a stored parent, both transcripts' root records must
// be cached, the child's record must name a team and an agent, and the parent's
// recorded spawns must name that exact pair. SQLite decides the edge, so a
// same-team name collision (a parent that spawned a different teammate) never
// becomes an identity, and a session that merely has a parent carries none.
//
// The parent answer is the store's one parent rule (LogicalParentSQL), so a
// reconciled relationship and the availability cache cannot disagree about
// which transcript holds the spawn evidence.
var claudeTeammateQuery = `SELECT c.session_id, ce.identity_team, ce.identity_name
FROM sessions c
JOIN claude_transcript_evidence ce ON ce.source_path = c.source_path
WHERE c.model_harness = ?
  AND ce.scope = 'root'
  AND ce.identity_team IS NOT NULL
  AND ce.identity_name IS NOT NULL
  AND EXISTS (
    SELECT 1
    FROM sessions p
    JOIN claude_transcript_evidence pe ON pe.source_path = p.source_path
    WHERE p.session_id = ` + LogicalParentSQL("c") + `
      AND p.model_harness = ?
      AND pe.scope = 'root'
      AND EXISTS (
        SELECT 1
        FROM json_each(pe.spawns_json) AS spawn
        WHERE json_extract(spawn.value, '$.team') = ce.identity_team
          AND json_extract(spawn.value, '$.name') = ce.identity_name
      )
  )
  AND c.session_id IN (`

// ClaudeTeammates returns the recorded Claude Code teammate identity of each
// named session, keyed by session id, in bounded IN(...) batches. A missing key
// means the session has no teammate: it is not Claude Code, has no stored
// parent, recorded no identity, or its parent's transcript records no spawn of
// that identity. Nothing here re-derives the edge in Go or scans the whole
// evidence table: the caller names the page it already projects.
func (s *Store) ClaudeTeammates(ctx context.Context, sessionIDs []string) (map[string]schema.SessionTeammate, error) {
	if len(sessionIDs) == 0 {
		return map[string]schema.SessionTeammate{}, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for Claude teammate evidence: %w; no teammate identity was read; restore database access and retry", err)
	}
	defer s.pool.Put(conn)

	teammates := make(map[string]schema.SessionTeammate, len(sessionIDs))
	for start := 0; start < len(sessionIDs); start += ClaudeTeammateReadBatchSize {
		end := min(start+ClaudeTeammateReadBatchSize, len(sessionIDs))
		selected := sessionIDs[start:end]
		placeholders := make([]string, len(selected))
		args := make([]any, len(selected))
		for i, id := range selected {
			placeholders[i] = "?"
			args[i] = id
		}
		query := claudeTeammateQuery + strings.Join(placeholders, ", ") + `)`
		// The harness is a typed constant, bound twice: once for the child and
		// once for its parent. The page's identifiers follow in IN(...) order.
		queryArgs := append([]any{defaults.HarnessClaudeCode.String(), defaults.HarnessClaudeCode.String()}, args...)
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: queryArgs,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				sessionID := stmt.ColumnText(0)
				teammate := schema.SessionTeammate{
					Team:     stmt.ColumnText(1),
					Name:     stmt.ColumnText(2),
					Evidence: schema.SessionTeammateEvidenceNativeDynamicWorkflow,
				}
				if validateErr := teammate.Validate(); validateErr != nil {
					return fmt.Errorf("store: stored Claude teammate identity for session %q cannot be emitted on a local read: %w; run `peasant ingest verify`, let discovery re-mine that transcript, then retry", sessionID, validateErr)
				}
				teammates[sessionID] = teammate
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("store: read Claude teammate evidence: %w; no teammate identity was returned for this batch; inspect the stored Claude evidence cache and retry", err)
		}
	}
	return teammates, nil
}

// sqlClaudeTeammateSpawnsForPathsTemplate reads the stored spawn projection for
// one bounded path batch through the spawns primary key (spawner_path, team,
// name). It is the authoritative spawn side of the evidence delta: the row is
// readable even when the transcript's spawns_json cannot be parsed, and the
// caller only asks about paths it already holds.
const sqlClaudeTeammateSpawnsForPathsTemplate = `SELECT spawner_path, team, name FROM claude_teammate_spawns
WHERE spawner_path IN (%s)
ORDER BY spawner_path, team, name`

// sqlClaudeTeammateSpawnsForPaths renders the by-path spawn read for the given
// path count. Production and its query-plan guard call it, so the guarded
// statement is the statement production runs.
func sqlClaudeTeammateSpawnsForPaths(pathCount int) string {
	return fmt.Sprintf(sqlClaudeTeammateSpawnsForPathsTemplate, sqlPlaceholders(pathCount))
}

// ClaudeTeammateSpawnsForPaths returns the stored spawn projection of each
// named transcript path, keyed by path, in bounded primary-key batches. A path
// with no row maps to no entry. It reads no JSON column and never scans the
// evidence table.
func (s *Store) ClaudeTeammateSpawnsForPaths(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath][]ingest.ClaudeTeammateIdentity, error) {
	if len(paths) == 0 {
		return map[ingest.ResolvedPath][]ingest.ClaudeTeammateIdentity{}, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"store.ClaudeTeammateSpawnsForPaths: take connection: %w; "+
				"what: failed to obtain a database connection for the stored spawn read; "+
				"why: the pool is exhausted or the context was cancelled; "+
				"where: store.ClaudeTeammateSpawnsForPaths in internal/store/claude_teammates.go; "+
				"when: before the first spawn batch ran; "+
				"impact: the teammate-link count delta cannot be made exact, so no link was decided; "+
				"fix: check the pool size and context deadline, then retry the run",
			err)
	}
	defer s.pool.Put(conn)
	return s.claudeTeammateSpawnsForPathsOnConn(ctx, conn, paths)
}

// claudeTeammateSpawnsForPathsOnConn is the connection-scoped form, so a caller
// already holding the pool connection (the by-path evidence read) does not take
// a second one.
func (s *Store) claudeTeammateSpawnsForPathsOnConn(ctx context.Context, conn *sqlite.Conn, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath][]ingest.ClaudeTeammateIdentity, error) {
	spawns := make(map[ingest.ResolvedPath][]ingest.ClaudeTeammateIdentity)
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		value := path.String()
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	if len(unique) == 0 {
		return spawns, nil
	}
	for start := 0; start < len(unique); start += indexFormatReadBatchSize {
		end := min(start+indexFormatReadBatchSize, len(unique))
		batch := unique[start:end]
		args := make([]any, len(batch))
		for index, value := range batch {
			args[index] = value
		}
		query := sqlClaudeTeammateSpawnsForPaths(len(batch))
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				path := ingest.ResolvedPath(stmt.ColumnText(0))
				spawns[path] = append(spawns[path], ingest.ClaudeTeammateIdentity{
					Team: stmt.ColumnText(1),
					Name: stmt.ColumnText(2),
				})
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf(
				"store.ClaudeTeammateSpawnsForPaths: query batch %d-%d of %d: %w; "+
					"what: the stored spawn projection query failed; "+
					"why: the statement could not run against claude_teammate_spawns (database access or a malformed statement); "+
					"where: store.ClaudeTeammateSpawnsForPaths in internal/store/claude_teammates.go; "+
					"when: reading the spawn projection for paths %d-%d of %d; "+
					"impact: the teammate-link count delta cannot be made exact, so no link was decided; "+
					"fix: restore database access, or rebuild the derived lookups with `peasant harvest index --all`, then retry",
				start, end, len(unique), err, start, end, len(unique))
		}
	}
	return spawns, nil
}
