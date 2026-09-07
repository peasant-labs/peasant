package main

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const indexScopeBatchSize = 256

// validateIndexQueryScope checks a metadata-only candidate query before callers
// apply entry-derived predicates or LIMIT. The query and subsequent projection
// access share the caller's read snapshot; candidate IDs are checked in batches.
func validateIndexQueryScope(db *store.Store, conn *sqlite.Conn, query string, args []any) error {
	ids := make([]schema.SessionID, 0, indexScopeBatchSize)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		id, err := schema.NewSessionID(stmt.ColumnText(0))
		if err != nil {
			return fmt.Errorf("read index scope: invalid stored session ID: %w; repair the stored identity before retrying", err)
		}
		ids = append(ids, id)
		if len(ids) == indexScopeBatchSize {
			if err := db.ValidateIndexFormatsOnConn(conn, ids); err != nil {
				return err
			}
			ids = ids[:0]
		}
		return nil
	}}); err != nil {
		return err
	}
	return db.ValidateIndexFormatsOnConn(conn, ids)
}

func validateIndexStringIDsOnConn(db *store.Store, conn *sqlite.Conn, raw []string) error {
	for start := 0; start < len(raw); start += indexScopeBatchSize {
		end := min(start+indexScopeBatchSize, len(raw))
		ids := make([]schema.SessionID, 0, end-start)
		for _, value := range raw[start:end] {
			id, err := schema.NewSessionID(value)
			if err != nil {
				return fmt.Errorf("read index scope: invalid session identity: %w; repair the stored identity before retrying", err)
			}
			ids = append(ids, id)
		}
		if err := db.ValidateIndexFormatsOnConn(conn, ids); err != nil {
			return err
		}
	}
	return nil
}
