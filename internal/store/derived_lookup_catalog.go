package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The store is the file catalog the full Codex discovery walk maintains. It is
// deliberately not the rebuild: the rebuild replaces both derived tables on its
// own path, while maintenance upserts what this walk enumerated and removes the
// rows whose files are gone. The two paths can disagree, which is what lets the
// rebuild check derivation instead of re-running the same statement.
var _ ingest.CodexRolloutCatalogStore = (*Store)(nil)

const (
	// sqlSelectCodexRolloutCatalog reads the whole catalog. The walk maintains
	// the catalog against a complete enumeration, so it needs the complete
	// set of rows to decide which of them the walk no longer found; the table
	// holds one row per rollout file, so this read is the same order as the
	// walk that is asking.
	sqlSelectCodexRolloutCatalog = `SELECT source_path, session_id FROM codex_rollout_catalog ORDER BY source_path`

	// sqlUpsertCodexRolloutCatalog records one enumerated path. The path is the
	// primary key, so a second copy of an id is a second row and the duplicate
	// count sees both; re-walking a stored path refreshes its id instead of
	// failing.
	sqlUpsertCodexRolloutCatalog = `INSERT OR REPLACE INTO codex_rollout_catalog (source_path, session_id) VALUES (?, ?)`

	// sqlDeleteCodexRolloutCatalog removes the row for a path the walk found
	// gone.
	sqlDeleteCodexRolloutCatalog = `DELETE FROM codex_rollout_catalog WHERE source_path = ?`
)

// LoadCodexRolloutCatalog returns every catalogued rollout path with the session
// id it carries, in path order.
func (s *Store) LoadCodexRolloutCatalog(ctx context.Context) ([]ingest.RolloutFile, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"store.LoadCodexRolloutCatalog: take connection: %w; "+
				"what: failed to obtain a database connection for the rollout catalog read; "+
				"why: the pool is exhausted or the context was cancelled; "+
				"where: store.LoadCodexRolloutCatalog in internal/store/derived_lookup_catalog.go; "+
				"when: before the catalog was read; "+
				"impact: the full walk cannot tell which catalogued paths it still holds, so it leaves the catalog as it is; "+
				"fix: check the pool size and context deadline, then rerun the full discovery walk",
			err)
	}
	defer s.pool.Put(conn)

	var catalog []ingest.RolloutFile
	err = sqlitex.ExecuteTransient(conn, sqlSelectCodexRolloutCatalog, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			file, scanErr := scanCodexRolloutCatalogRow(stmt)
			if scanErr != nil {
				return scanErr
			}
			catalog = append(catalog, file)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf(
			"store.LoadCodexRolloutCatalog: query: %w; "+
				"what: the rollout catalog query failed; "+
				"why: the statement could not run against the catalog (database access or a malformed statement); "+
				"where: store.LoadCodexRolloutCatalog in internal/store/derived_lookup_catalog.go; "+
				"when: reading the catalogued rollout paths; "+
				"impact: the full walk cannot tell which catalogued paths it still holds, so it leaves the catalog as it is; "+
				"fix: restore database access, then rerun the full discovery walk",
			err)
	}
	return catalog, nil
}

// scanCodexRolloutCatalogRow reads one catalog row. A row this build cannot read
// is refused rather than skipped: the caller is the walk about to prune against
// this set, and a row it cannot place would be pruned or kept for the wrong
// reason.
func scanCodexRolloutCatalogRow(stmt *sqlite.Stmt) (ingest.RolloutFile, error) {
	rawPath := stmt.ColumnText(0)
	path, err := ingest.NewResolvedPath(rawPath)
	if err != nil {
		return ingest.RolloutFile{}, fmt.Errorf(
			"store.LoadCodexRolloutCatalog: catalogued rollout carries an invalid path %q: %w; "+
				"a row whose path cannot be resolved cannot be matched to the walk, so the catalog cannot be maintained against it; "+
				"repair or clear the row, then rerun the full discovery walk",
			rawPath, err)
	}
	rawID := stmt.ColumnText(1)
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		return ingest.RolloutFile{}, fmt.Errorf(
			"store.LoadCodexRolloutCatalog: catalogued rollout %q carries an invalid session id %q: %w; "+
				"the catalog exists to answer duplicate and parent questions by id, so a row with an unreadable id cannot be used; "+
				"repair or clear the row, then rerun the full discovery walk",
			rawPath, rawID, err)
	}
	return ingest.RolloutFile{Path: path, SessionID: id}, nil
}

// SaveCodexRolloutCatalog upserts one row per enumerated rollout path and
// removes the rows the walk named as gone, in one transaction, so a failed write
// leaves the catalog as the previous full walk left it. The caller owns both
// halves: it enumerated the paths, and it is the only place that knows which
// catalogued paths the walk re-walked and found absent.
func (s *Store) SaveCodexRolloutCatalog(ctx context.Context, catalog []ingest.RolloutFile, deletes []ingest.ResolvedPath) (err error) {
	if len(catalog) == 0 && len(deletes) == 0 {
		return nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf(
			"store.SaveCodexRolloutCatalog: take connection: %w; "+
				"what: failed to obtain a database connection for the rollout catalog write; "+
				"why: the pool is exhausted or the context was cancelled; "+
				"where: store.SaveCodexRolloutCatalog in internal/store/derived_lookup_catalog.go; "+
				"when: before any catalog row was written; "+
				"impact: the catalog still holds what the previous full walk recorded; "+
				"fix: check the pool size and context deadline, then rerun the full discovery walk",
			err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	for index := range catalog {
		file := &catalog[index]
		path, validationErr := ingest.NewResolvedPath(file.Path.String())
		if validationErr != nil {
			return fmt.Errorf(
				"store.SaveCodexRolloutCatalog: enumerated rollout %d carries an invalid path %q: %w; "+
					"the transaction rolled back and the previous catalog is intact; pass the paths the walk enumerated",
				index, file.Path, validationErr)
		}
		id, validationErr := ingest.NewSessionID(file.SessionID.String())
		if validationErr != nil {
			return fmt.Errorf(
				"store.SaveCodexRolloutCatalog: enumerated rollout %q carries an invalid session id %q: %w; "+
					"the transaction rolled back and the previous catalog is intact; pass the id the rollout file name carries",
				file.Path, file.SessionID, validationErr)
		}
		if err = sqlitex.Execute(conn, sqlUpsertCodexRolloutCatalog, &sqlitex.ExecOptions{
			Args: []any{path.String(), id.String()},
		}); err != nil {
			return fmt.Errorf(
				"store.SaveCodexRolloutCatalog: record enumerated rollout %q: %w; "+
					"the transaction rolled back and the previous catalog is intact; restore database access and rerun the full discovery walk",
				file.Path, err)
		}
	}

	for _, path := range deletes {
		if err = sqlitex.Execute(conn, sqlDeleteCodexRolloutCatalog, &sqlitex.ExecOptions{
			Args: []any{path.String()},
		}); err != nil {
			return fmt.Errorf(
				"store.SaveCodexRolloutCatalog: remove the catalogued rollout %q: %w; "+
					"the transaction rolled back and the previous catalog is intact; restore database access and rerun the full discovery walk",
				path, err)
		}
	}
	return nil
}
