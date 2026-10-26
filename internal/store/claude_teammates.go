package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ClaudeTeammateReadBatchSize bounds one teammate evidence query. It aliases
// the store's read batch size rather than opening a second budget; it is
// exported under this name only so callers and the external bounded-page test
// can reason about how many batches a page costs without copying the number.
const ClaudeTeammateReadBatchSize = indexFormatReadBatchSize

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
