package store

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// trimLeadingAnd turns a rendered " AND x" predicate into "x", for statements
// whose only WHERE terms come from the scope. The all-store scope renders no
// predicate and such a statement is never built for it.
func trimLeadingAnd(predicate string) string {
	return strings.TrimPrefix(predicate, " AND ")
}

// Scope narrowing for the store-wide reads and writes.
//
// Every pass that gains a scope validates it before this file renders
// anything, so a zero value never reaches the SQL. The all-store scope renders
// no predicate at all, which keeps a full run's statement and result
// unchanged. A narrowed scope renders an IN predicate over the pass's own
// alias, so the same helper is used by every pass and cannot drift.

// narrowedSessionPredicate renders " AND <alias>.session_id IN (?,...)" for a
// narrowed scope and the ids to bind after the caller's own arguments. An
// all-store scope renders nothing.
//
// A narrowed scope with no ids renders " AND 0". Every pass already returns
// before calling this for that case, so the branch is a fail-closed net rather
// than a live path: a future pass that forgets the early return selects nothing
// instead of producing an invalid empty IN list or, worse, widening.
func narrowedSessionPredicate(alias string, scope ingest.StoreScope) (string, []any) {
	ids, narrowed := scope.Narrowed()
	if !narrowed {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 0", nil
	}
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = string(id)
	}
	return " AND " + alias + ".session_id IN (" + sqlPlaceholders(len(ids)) + ")", args
}

// narrowedProjectPredicate renders " AND <alias>.project_hash IN (?,...)" for
// a narrowed project scope and the hashes to bind. An all-store scope renders
// nothing. A narrowed scope with no hashes keeps the same " AND 0" fail-closed
// net as the session predicate, for the same reason: the live path returns
// before this, and a pass that forgets must select nothing rather than widen.
func narrowedProjectPredicate(alias string, scope ingest.ProjectScope) (string, []any) {
	hashes, narrowed := scope.Narrowed()
	if !narrowed {
		return "", nil
	}
	if len(hashes) == 0 {
		return " AND 0", nil
	}
	args := make([]any, len(hashes))
	for index, hash := range hashes {
		args[index] = string(hash)
	}
	return " AND " + alias + ".project_hash IN (" + sqlPlaceholders(len(hashes)) + ")", args
}

// narrowedSessionsEmpty reports whether a narrowed scope names no session. A
// pass that reads or writes must return without touching the database in that
// case: the empty selection is a real answer, not a reason to widen.
func narrowedSessionsEmpty(scope ingest.StoreScope) bool {
	ids, narrowed := scope.Narrowed()
	return narrowed && len(ids) == 0
}

// narrowedProjectsEmpty reports whether a narrowed project scope names no
// project.
func narrowedProjectsEmpty(scope ingest.ProjectScope) bool {
	hashes, narrowed := scope.Narrowed()
	return narrowed && len(hashes) == 0
}
