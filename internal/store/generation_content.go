package store

import (
	"fmt"
	"slices"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// A generation reads its parsed rows either from itself or from the one
// generation its mapping_set_id names (schema 82). The named generation is
// the owner: a generation in the same session whose mapping_set_id is NULL.
// References are one level deep: a writer that references a generation which
// itself references another names that generation's owner instead, so an
// owner never carries a mapping_set_id and a reader resolves in one step.
//
// Parsed rows come from the parse of the transcript: the entry mapping, the
// content descriptors, the title refs, the aliases, the sections and their
// native metadata, and the context segments and their refs. Every other
// generation-scoped row (the catalog row itself, subagents, commits,
// associations, diagnostics and relationship evidence) belongs to the
// generation that wrote it and is read under its own id.

// parseTables are the generation-scoped tables whose rows a generation may
// read from its owner. It is an exact subset of reclaimTableNames.
var parseTables = []string{
	"session_generation_entries",
	"session_generation_content",
	"session_generation_title_refs",
	"session_projection_aliases",
	"session_projection_sections",
	"session_section_native_metadata",
	"session_context_segments",
	"session_context_segment_refs",
}

// rowsGenerationIDOnConn returns the id under which the parsed rows of one
// generation are stored: its mapping_set_id when it references an owner, its
// own id otherwise. A generation with no harmonized catalog row (a
// file-backed generation, or none) resolves to its own id, as the SQL
// fragment does. It reads on the caller's connection, inside the caller's
// read transaction.
func rowsGenerationIDOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (string, error) {
	resolved := generationID
	err := sqlitex.Execute(conn, `SELECT `+sqlRowsGeneration("?1", "?2"), &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			resolved = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: resolve the parsed rows of generation %s for session %s: %w; no row was read", generationID, sessionID, err)
	}
	return resolved, nil
}

// sqlRowsGeneration is the SQL expression a query uses in place of a
// generation id when it reads a parse table: the generation's mapping_set_id
// when it has one, the generation id otherwise. sessionExpr and generationExpr
// are SQL expressions (a column, a parameter); each is evaluated where the
// fragment is placed.
func sqlRowsGeneration(sessionExpr, generationExpr string) string {
	return `COALESCE((SELECT g.mapping_set_id FROM session_generations g WHERE g.session_id = ` + sessionExpr +
		` AND g.generation_id = ` + generationExpr + `), ` + generationExpr + `)`
}

// generationKeepSet is what a sweep or reclaim must keep for one session: the
// two roots (the active read authority and the latest attempt) and the
// generations whose parsed rows they read. An empty field keeps nothing.
type generationKeepSet struct {
	active      string
	latest      string
	activeOwner string
	latestOwner string
}

// contains reports whether a generation is kept.
func (k generationKeepSet) contains(generationID string) bool {
	return generationID != "" && slices.Contains([]string{k.active, k.latest, k.activeOwner, k.latestOwner}, generationID)
}

// supersededGenerationArgs binds the four slots of
// supersededGenerationPredicate. An empty member binds NULL, which IS NOT
// never matches, so it keeps nothing.
func supersededGenerationArgs(k generationKeepSet) []any {
	args := make([]any, 0, 4)
	for _, id := range []string{k.active, k.latest, k.activeOwner, k.latestOwner} {
		if id == "" {
			args = append(args, nil)
			continue
		}
		args = append(args, id)
	}
	return args
}

// generationKeepSetColumns selects one session's keep set from sessions s:
// the two roots and the owner each resolves to, in scanGenerationKeepSet's
// order.
var generationKeepSetColumns = `s.active_generation_id, s.latest_attempt_generation_id, ` +
	sqlRowsGeneration("s.session_id", "s.active_generation_id") + `, ` +
	sqlRowsGeneration("s.session_id", "s.latest_attempt_generation_id")

// scanGenerationKeepSet reads the four generationKeepSetColumns from a row,
// starting at column first.
func scanGenerationKeepSet(stmt *sqlite.Stmt, first int) generationKeepSet {
	text := func(col int) string {
		if stmt.ColumnType(col) == sqlite.TypeNull {
			return ""
		}
		return stmt.ColumnText(col)
	}
	return generationKeepSet{active: text(first), latest: text(first + 1), activeOwner: text(first + 2), latestOwner: text(first + 3)}
}

// generationKeepSetOnConn reads one session's keep set on the caller's
// connection. A caller that deletes holds the exclusive session lock, which
// every writer of a root or a reference also takes, so the set cannot change
// under it.
func generationKeepSetOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (generationKeepSet, error) {
	var keep generationKeepSet
	found := false
	if err := sqlitex.Execute(conn, `SELECT `+generationKeepSetColumns+` FROM sessions s WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			keep = scanGenerationKeepSet(stmt, 0)
			return nil
		},
	}); err != nil {
		return generationKeepSet{}, fmt.Errorf("store: read the generation keep set for session %s: %w", sessionID, err)
	}
	if !found {
		return generationKeepSet{}, fmt.Errorf("store: session %s has no metadata row; import the session before sweeping or reclaiming its generations", sessionID)
	}
	return keep, nil
}
