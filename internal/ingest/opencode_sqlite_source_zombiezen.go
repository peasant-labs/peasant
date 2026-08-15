package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type zombiezenOpenCodeSQLiteSource struct {
	path    OpenCodeSQLiteSourcePath
	options OpenCodeSQLiteSourceOptions
	conn    *sqlite.Conn
	permit  chan struct{}
	closed  bool
	denied  string
}

var _ OpenCodeSQLiteSource = (*zombiezenOpenCodeSQLiteSource)(nil)

// OpenOpenCodeSQLiteSource opens one private zombiezen SQLite connection in
// read-only URI mode, enables query_only, installs a deny-by-default statement
// authorizer, and returns only the restrictive read interface.
func OpenOpenCodeSQLiteSource(
	ctx context.Context,
	path OpenCodeSQLiteSourcePath,
	options OpenCodeSQLiteSourceOptions,
) (OpenCodeSQLiteSource, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open OpenCode SQLite source %q failed before connection setup: context is nil, so cancellation and deadlines cannot be enforced; no database was accessed; pass a non-nil context", path.String())
	}
	if path.path == "" {
		return nil, fmt.Errorf("open OpenCode SQLite source failed before connection setup: source path was not created by NewOpenCodeSQLiteSourcePath; no database was accessed; validate the absolute path first")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open OpenCode SQLite source %q stopped before connection setup because the caller context ended: %w; no database was accessed; retry with a live bounded context", path.String(), err)
	}

	uri := url.URL{Scheme: "file", Path: path.String()}
	query := uri.Query()
	query.Set("mode", "ro")
	uri.RawQuery = query.Encode()

	conn, err := sqlite.OpenConn(uri.String(), sqlite.OpenReadOnly|sqlite.OpenURI|sqlite.OpenPrivateCache)
	if err != nil {
		return nil, fmt.Errorf("open OpenCode SQLite source %q failed while creating the single mode=ro connection: %w; no schema or data was read and no source write was requested; verify that the file exists, is a readable SQLite database, and is not blocked by filesystem permissions", path.String(), err)
	}

	source := &zombiezenOpenCodeSQLiteSource{
		path:    path,
		options: options,
		conn:    conn,
		permit:  make(chan struct{}, 1),
	}
	source.permit <- struct{}{}
	conn.SetBusyTimeout(options.busyTimeout)

	if err := source.initialize(ctx); err != nil {
		closeErr := conn.Close()
		return nil, errors.Join(err, closeErr)
	}
	return source, nil
}

func (s *zombiezenOpenCodeSQLiteSource) initialize(parent context.Context) error {
	ctx, cancel := s.options.clock.WithTimeout(parent, s.options.queryTimeout)
	defer cancel()

	oldInterrupt := s.conn.SetInterrupt(ctx.Done())
	defer s.conn.SetInterrupt(oldInterrupt)

	if err := sqlitex.ExecuteTransient(s.conn, "PRAGMA query_only=ON", nil); err != nil {
		return fmt.Errorf("open OpenCode SQLite source %q failed while enabling PRAGMA query_only=ON before the first schema or data read: %w; the connection will be closed and the source will not be queried; verify that the database is readable and retry without changing the OpenCode-owned files", s.path.String(), err)
	}
	if err := s.conn.SetAuthorizer(sqlite.AuthorizeFunc(s.authorizeRead)); err != nil {
		return fmt.Errorf("open OpenCode SQLite source %q failed after query_only setup while installing the read-only statement authorizer: %w; the connection will be closed before any schema or data read; retry after resolving the SQLite initialization error", s.path.String(), err)
	}

	var queryOnly int64
	err := sqlitex.ExecuteTransient(s.conn, "PRAGMA query_only", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			queryOnly = stmt.ColumnInt64(0)
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("open OpenCode SQLite source %q failed while verifying query_only after enforcement was enabled: %w; the connection will be closed before any schema or data read; verify SQLite can report connection pragmas and retry", s.path.String(), err)
	}
	if queryOnly != 1 {
		return fmt.Errorf("open OpenCode SQLite source %q failed while verifying query_only after enforcement was requested: SQLite reported %d instead of 1, so read-only defense in depth is not established; the connection will be closed before schema or data reads; use a supported SQLite runtime", s.path.String(), queryOnly)
	}
	return nil
}

func (s *zombiezenOpenCodeSQLiteSource) authorizeRead(action sqlite.Action) sqlite.AuthResult {
	switch action.Type() {
	case sqlite.OpSelect, sqlite.OpRead, sqlite.OpFunction, sqlite.OpRecursive:
		return sqlite.AuthResultOK
	case sqlite.OpPragma:
		if strings.EqualFold(action.Pragma(), "query_only") && action.PragmaArg() == "" {
			return sqlite.AuthResultOK
		}
		switch strings.ToLower(action.Pragma()) {
		case "table_info", "index_list", "index_info":
			// SQLite's table-valued catalog functions invoke their corresponding
			// read-only PRAGMA authorizer actions. Permit only the three bounded
			// structural probes used by candidate inspection.
			return sqlite.AuthResultOK
		}
	}

	s.denied = action.String()
	return sqlite.AuthResultDeny
}

func (s *zombiezenOpenCodeSQLiteSource) Read(
	ctx context.Context,
	statement string,
	args []any,
	visit func(OpenCodeSQLiteRow) error,
) error {
	if ctx == nil {
		return fmt.Errorf("read OpenCode SQLite source %q failed before preparing the bounded query: context is nil, so cancellation and deadlines cannot be enforced; no statement was prepared; pass a non-nil context", s.path.String())
	}
	if strings.TrimSpace(statement) == "" {
		return fmt.Errorf("read OpenCode SQLite source %q failed before preparing the bounded query: statement is empty, so there is no read operation to authorize; no statement was prepared; supply one SELECT statement or PRAGMA query_only", s.path.String())
	}
	queryCtx, cancel := s.options.clock.WithTimeout(ctx, s.options.queryTimeout)
	defer cancel()

	select {
	case <-queryCtx.Done():
		return fmt.Errorf("read OpenCode SQLite source %q stopped while waiting for its single connection because the caller context or %s deadline ended: %w; no statement was prepared and the source remains untouched; retry with a live bounded context or increase the validated query timeout when a longer bounded wait is intentional", s.path.String(), s.options.queryTimeout, context.Cause(queryCtx))
	case <-s.permit:
	}
	defer func() { s.permit <- struct{}{} }()

	if s.closed {
		return fmt.Errorf("read OpenCode SQLite source %q failed before preparing the bounded query because the source is closed; no statement was prepared; open a new source for further reads", s.path.String())
	}

	oldInterrupt := s.conn.SetInterrupt(queryCtx.Done())
	defer s.conn.SetInterrupt(oldInterrupt)

	s.denied = ""
	prepared, trailingBytes, err := s.conn.PrepareTransient(statement)
	if err != nil {
		return s.prepareError(queryCtx, err)
	}
	finalizeErr := prepared.Finalize()
	if finalizeErr != nil {
		return fmt.Errorf("read OpenCode SQLite source %q failed while finalizing the read-only statement preflight: %w; query execution did not start and no source write was attempted; retry after resolving the SQLite statement error", s.path.String(), finalizeErr)
	}
	if trailingBytes != 0 && strings.TrimSpace(statement[len(statement)-trailingBytes:]) != "" {
		return fmt.Errorf("read OpenCode SQLite source %q rejected a statement during bounded read preflight because it contains trailing SQL; query execution did not start, which prevents hidden follow-up operations; submit exactly one SELECT statement or PRAGMA query_only", s.path.String())
	}

	s.denied = ""
	err = sqlitex.ExecuteTransient(s.conn, statement, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if visit == nil {
				return nil
			}
			return visit(copyOpenCodeSQLiteRow(stmt))
		},
	})
	if err != nil {
		if s.denied != "" {
			return s.deniedStatementError(err)
		}
		if queryCtx.Err() != nil {
			return fmt.Errorf("read OpenCode SQLite source %q stopped during bounded query execution because its context or %s deadline ended: %w; SQLite interrupted the statement and no source write was attempted; retry if the source is healthy, or increase the validated query timeout when a longer bounded read is intentional", s.path.String(), s.options.queryTimeout, context.Cause(queryCtx))
		}
		return fmt.Errorf("read OpenCode SQLite source %q failed during bounded query execution: %w; the requested rows were not fully delivered and no source write was attempted; verify the expected OpenCode schema and statement arguments before retrying", s.path.String(), err)
	}
	return nil
}

func (s *zombiezenOpenCodeSQLiteSource) prepareError(ctx context.Context, err error) error {
	if s.denied != "" {
		return s.deniedStatementError(err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("read OpenCode SQLite source %q stopped during statement preparation because its context or %s deadline ended: %w; query execution did not start and no source write was attempted; retry with a live bounded context", s.path.String(), s.options.queryTimeout, context.Cause(ctx))
	}
	return fmt.Errorf("read OpenCode SQLite source %q failed while preparing the bounded read statement: %w; query execution did not start and no source write was attempted; verify the expected OpenCode schema and submit exactly one read-only statement", s.path.String(), err)
}

func (s *zombiezenOpenCodeSQLiteSource) deniedStatementError(err error) error {
	return fmt.Errorf("read OpenCode SQLite source %q rejected a non-read-only statement during preparation because it requested forbidden SQLite action %q: %w; query execution did not start and query_only remains enabled; use exactly one SELECT statement or the read-only PRAGMA query_only check, never writes, schema changes, ATTACH, checkpoints, maintenance, transactions, or write-effecting pragmas", s.path.String(), s.denied, err)
}

func copyOpenCodeSQLiteRow(stmt *sqlite.Stmt) OpenCodeSQLiteRow {
	columns := make([]OpenCodeSQLiteColumn, stmt.ColumnCount())
	for i := range columns {
		columns[i] = OpenCodeSQLiteColumn{
			Name:  stmt.ColumnName(i),
			Value: copyOpenCodeSQLiteValue(stmt, i),
		}
	}
	return OpenCodeSQLiteRow{columns: columns}
}

func copyOpenCodeSQLiteValue(stmt *sqlite.Stmt, column int) OpenCodeSQLiteValue {
	switch stmt.ColumnType(column) {
	case sqlite.TypeInteger:
		return OpenCodeSQLiteValue{Kind: OpenCodeSQLiteValueInteger, Integer: stmt.ColumnInt64(column)}
	case sqlite.TypeFloat:
		return OpenCodeSQLiteValue{Kind: OpenCodeSQLiteValueFloat, Float: stmt.ColumnFloat(column)}
	case sqlite.TypeText:
		return OpenCodeSQLiteValue{Kind: OpenCodeSQLiteValueText, Text: stmt.ColumnText(column)}
	case sqlite.TypeBlob:
		value := make([]byte, stmt.ColumnLen(column))
		stmt.ColumnBytes(column, value)
		return OpenCodeSQLiteValue{Kind: OpenCodeSQLiteValueBlob, Blob: value}
	default:
		return OpenCodeSQLiteValue{Kind: OpenCodeSQLiteValueNull}
	}
}

func (s *zombiezenOpenCodeSQLiteSource) Close() error {
	<-s.permit
	defer func() { s.permit <- struct{}{} }()

	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.conn.Close(); err != nil {
		return fmt.Errorf("close OpenCode SQLite source %q failed while releasing its single read-only connection: %w; no further reads are allowed from this source; retry process cleanup and inspect SQLite resource diagnostics", s.path.String(), err)
	}
	return nil
}
