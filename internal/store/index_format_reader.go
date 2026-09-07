package store

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const indexFormatReadBatchSize = 256

const sqlIndexFormatState = `SELECT s.session_id, s.index_format_version,
EXISTS (SELECT 1 FROM session_entries e WHERE e.session_id = s.session_id)
FROM sessions s`

// ValidateIndexFormatsOnConn checks only the requested session IDs in the
// caller's active snapshot. Nil and empty IDs mean no sessions, never all.
// Read the dependent projection using this same connection before ending the
// transaction. This method neither obtains a connection nor starts a transaction.
func (s *Store) ValidateIndexFormatsOnConn(conn *sqlite.Conn, ids []schema.SessionID) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += indexFormatReadBatchSize {
		end := min(start+indexFormatReadBatchSize, len(ids))
		raw := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			raw = append(raw, string(id))
		}
		if err := s.validateIndexFormatIDsOnConn(conn, raw); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAllIndexFormatsOnConn explicitly checks the global index scope.
// Global search must call this before entry-dependent filters or LIMIT, because
// an unsupported representation may not have a complete compatible projection.
func (s *Store) ValidateAllIndexFormatsOnConn(conn *sqlite.Conn) error {
	if err := requireIndexReadSnapshot(conn); err != nil {
		return err
	}
	return s.validateIndexFormatQueryOnConn(conn, sqlIndexFormatState+" ORDER BY s.session_id", nil)
}

func requireIndexReadSnapshot(conn *sqlite.Conn) error {
	if conn == nil || conn.AutocommitEnabled() {
		return fmt.Errorf("store: index compatibility requires an active SQLite snapshot before projection access; no read was authorized; begin a read transaction and use the same connection for version and projection queries")
	}
	return nil
}

// String IDs preserve the established Store methods that accept database keys
// without adding a new public ID-validation policy at an unrelated boundary.
func (s *Store) validateIndexFormatIDsOnConn(conn *sqlite.Conn, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := requireIndexReadSnapshot(conn); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += indexFormatReadBatchSize {
		end := min(start+indexFormatReadBatchSize, len(ids))
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		query := sqlIndexFormatState + " WHERE s.session_id IN (" + marks + ") ORDER BY s.session_id"
		if err := s.validateIndexFormatQueryOnConn(conn, query, args); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) validateIndexFormatQueryOnConn(conn *sqlite.Conn, query string, args []any) error {
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		version := stmt.ColumnInt(1)
		if stmt.ColumnType(1) == sqlite.TypeNull && stmt.ColumnInt(2) == 0 {
			return nil // A never-indexed session has no representation to decode.
		}
		if stmt.ColumnType(1) != sqlite.TypeNull && s.SupportsIndexFormat(version) {
			return nil
		}
		id, err := schema.NewSessionID(stmt.ColumnText(0))
		if err != nil {
			return fmt.Errorf("store: unsupported index has invalid session identity %q: %w; projection access was refused; repair the stored identity and format evidence before retrying", stmt.ColumnText(0), err)
		}
		return &UnsupportedIndexFormatError{SessionID: id, Version: version}
	}}); err != nil {
		return fmt.Errorf("store: verify actual index format before projection access: %w", err)
	}
	return nil
}
