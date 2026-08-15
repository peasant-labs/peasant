package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	openCodeEnableQueryOnlyStatement      = "PRAGMA query_only=ON"
	openCodeReadQueryOnlyStatement        = "PRAGMA query_only"
	openCodeCatalogTablesStatement        = "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name LIMIT 257"
	openCodeCatalogColumnsStatement       = "SELECT name, \"notnull\", pk FROM pragma_table_info(?1) ORDER BY cid LIMIT 33"
	openCodeCatalogIndexesStatement       = "SELECT il.name, il.\"unique\", il.partial, xi.seqno, xi.cid, xi.name, xi.desc, xi.coll, xi.key FROM pragma_index_list(?1) AS il JOIN pragma_index_xinfo(il.name) AS xi ORDER BY il.name, xi.seqno LIMIT 65"
	openCodeLegacySessionsFirstStatement  = "SELECT DISTINCT session_id FROM message ORDER BY session_id LIMIT ?1"
	openCodeLegacySessionsAfterStatement  = "SELECT DISTINCT session_id FROM message WHERE session_id > ?1 ORDER BY session_id LIMIT ?2"
	openCodeLegacyMessagesFirstStatement  = "SELECT id, session_id, time_created, time_updated, data FROM message WHERE session_id = ?1 ORDER BY time_created, id LIMIT ?2"
	openCodeLegacyMessagesAfterStatement  = "SELECT id, session_id, time_created, time_updated, data FROM message WHERE session_id = ?1 AND (time_created > ?2 OR (time_created = ?2 AND id > ?3)) ORDER BY time_created, id LIMIT ?4"
	openCodeLegacyPartsFirstStatement     = "SELECT id, message_id, session_id, time_created, time_updated, data FROM part WHERE session_id = ?1 AND message_id = ?2 ORDER BY id LIMIT ?3"
	openCodeLegacyPartsAfterStatement     = "SELECT id, message_id, session_id, time_created, time_updated, data FROM part WHERE session_id = ?1 AND message_id = ?2 AND id > ?3 ORDER BY id LIMIT ?4"
	openCodeCurrentSessionsFirstStatement = "SELECT DISTINCT session_id FROM session_message ORDER BY session_id LIMIT ?1"
	openCodeCurrentSessionsAfterStatement = "SELECT DISTINCT session_id FROM session_message WHERE session_id > ?1 ORDER BY session_id LIMIT ?2"
	openCodeCurrentMessagesFirstStatement = "SELECT id, session_id, type, time_created, time_updated, data, seq FROM session_message WHERE session_id = ?1 ORDER BY seq LIMIT ?2"
	openCodeCurrentMessagesAfterStatement = "SELECT id, session_id, type, time_created, time_updated, data, seq FROM session_message WHERE session_id = ?1 AND seq > ?2 ORDER BY seq LIMIT ?3"
)

type zombiezenOpenCodeSQLiteSource struct {
	path    OpenCodeSQLiteSourcePath
	options OpenCodeSQLiteSourceOptions
	conn    *sqlite.Conn
	permit  chan struct{}

	stateMu      sync.Mutex
	closing      bool
	connClosed   bool
	activeCancel context.CancelFunc
	denied       string
}

var _ OpenCodeSQLiteSource = (*zombiezenOpenCodeSQLiteSource)(nil)

// OpenOpenCodeSQLiteSource opens one private zombiezen SQLite connection in
// read-only URI mode, enables query_only, installs a deny-by-default statement
// authorizer, and returns only the restrictive typed source interface.
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

	conn, err := options.openConnection(uri.String(), sqlite.OpenReadOnly|sqlite.OpenURI|sqlite.OpenPrivateCache)
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

	if err := sqlitex.ExecuteTransient(s.conn, openCodeEnableQueryOnlyStatement, nil); err != nil {
		return fmt.Errorf("open OpenCode SQLite source %q failed while enabling PRAGMA query_only=ON before the first schema or data read: %w; the connection will be closed and the source will not be queried; verify that the database is readable and retry without changing the OpenCode-owned files", s.path.String(), err)
	}
	if err := s.conn.SetAuthorizer(sqlite.AuthorizeFunc(s.authorizeRead)); err != nil {
		return fmt.Errorf("open OpenCode SQLite source %q failed after query_only setup while installing the read-only statement authorizer: %w; the connection will be closed before any schema or data read; retry after resolving the SQLite initialization error", s.path.String(), err)
	}

	var queryOnly int64
	err := sqlitex.ExecuteTransient(s.conn, openCodeReadQueryOnlyStatement, &sqlitex.ExecOptions{
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
		case "table_info", "index_list", "index_info", "index_xinfo":
			return sqlite.AuthResultOK
		}
	}

	s.denied = action.String()
	return sqlite.AuthResultDeny
}

// Catalog returns a detached, bounded structural snapshot. All SQL, row
// collection, and SQLite callbacks complete before the one connection permit
// is released and before the caller can process the result.
func (s *zombiezenOpenCodeSQLiteSource) Catalog(ctx context.Context) (OpenCodeSchemaEvidence, error) {
	if ctx == nil {
		return OpenCodeSchemaEvidence{}, fmt.Errorf("inspect OpenCode SQLite catalog for %q failed before waiting for the single connection: context is nil, so cancellation and deadlines cannot be enforced; no schema was read; pass a non-nil context", s.path.String())
	}
	queryCtx, cancel := s.options.clock.WithTimeout(ctx, s.options.queryTimeout)
	defer cancel()

	select {
	case <-queryCtx.Done():
		return OpenCodeSchemaEvidence{}, fmt.Errorf("inspect OpenCode SQLite catalog for %q stopped while waiting for the single connection because the caller context or %s deadline ended: %w; no schema was read and the source remains untouched; retry with a live bounded context", s.path.String(), s.options.queryTimeout, context.Cause(queryCtx))
	case <-s.permit:
	}
	defer func() { s.permit <- struct{}{} }()

	activeCtx, activeCancel := context.WithCancel(queryCtx)
	s.stateMu.Lock()
	if s.closing || s.connClosed {
		s.stateMu.Unlock()
		activeCancel()
		return OpenCodeSchemaEvidence{}, fmt.Errorf("inspect OpenCode SQLite catalog for %q failed before schema access because the source is closing or closed; no schema was read; open a new source for further catalog inspection", s.path.String())
	}
	s.activeCancel = activeCancel
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		s.activeCancel = nil
		s.stateMu.Unlock()
		activeCancel()
	}()

	oldInterrupt := s.conn.SetInterrupt(activeCtx.Done())
	defer s.conn.SetInterrupt(oldInterrupt)

	evidence, err := s.catalogLocked(activeCtx)
	if err != nil {
		if activeCtx.Err() != nil {
			return evidence, fmt.Errorf("inspect OpenCode SQLite catalog for %q stopped during bounded schema collection because its context or %s deadline ended: %w; no caller code ran while the connection was held and no source write was attempted; retry with a healthy source and live bounded context", s.path.String(), s.options.queryTimeout, context.Cause(activeCtx))
		}
		return evidence, fmt.Errorf("inspect OpenCode SQLite catalog for %q failed during bounded explicit-column schema collection: %w; no transcript rows were exposed and no source write was attempted; verify the OpenCode schema and source health before retrying", s.path.String(), err)
	}
	return evidence, nil
}

// LegacySessionIDs returns distinct session identifiers from the materialized
// legacy message table in stable identifier order.
func (s *zombiezenOpenCodeSQLiteSource) LegacySessionIDs(ctx context.Context, request OpenCodeLegacySessionPageRequest) (OpenCodeLegacySessionPage, error) {
	if err := validateLegacySessionPageRequest(request); err != nil {
		return OpenCodeLegacySessionPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "enumerate bounded legacy session identifiers")
	if err != nil {
		return OpenCodeLegacySessionPage{}, err
	}
	defer lease.release()

	identifiers := make([]OpenCodeLegacySessionID, 0, request.PageSize.value+1)
	decode := func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) != sqlite.TypeText {
			return fmt.Errorf("decode legacy session identifier row: session_id has SQLite type %s instead of text", stmt.ColumnType(0))
		}
		identifier, decodeErr := NewOpenCodeLegacySessionID(stmt.ColumnText(0))
		if decodeErr != nil {
			return fmt.Errorf("decode legacy session identifier row: %w", decodeErr)
		}
		identifiers = append(identifiers, identifier)
		return nil
	}
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacySessionsFirstStatement, []any{request.PageSize.value + 1}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacySessionsAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeLegacySessionPage{}, s.sourceReadError(lease.ctx, "enumerate bounded legacy session identifiers", err, "message(session_id)", "supported legacy message/part")
	}
	hasNext := len(identifiers) > request.PageSize.value
	if hasNext {
		identifiers = identifiers[:request.PageSize.value]
	}
	identifiers = identifiers[:len(identifiers):len(identifiers)]
	page := OpenCodeLegacySessionPage{SessionIDs: identifiers}
	if hasNext {
		cursor := OpenCodeLegacySessionCursor{sessionID: page.SessionIDs[len(page.SessionIDs)-1]}
		page.Next = &cursor
	}
	return page, nil
}

// CurrentSessionIDs returns distinct current session identifiers in stable
// identifier order without reading any message payload.
func (s *zombiezenOpenCodeSQLiteSource) CurrentSessionIDs(ctx context.Context, request OpenCodeCurrentSessionPageRequest) (OpenCodeCurrentSessionPage, error) {
	if err := validateCurrentSessionPageRequest(request); err != nil {
		return OpenCodeCurrentSessionPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "enumerate bounded current session identifiers")
	if err != nil {
		return OpenCodeCurrentSessionPage{}, err
	}
	defer lease.release()

	identifiers := make([]OpenCodeCurrentSessionID, 0, request.PageSize.value+1)
	decode := func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) != sqlite.TypeText {
			return fmt.Errorf("decode current session identifier row: session_id has SQLite type %s instead of text", stmt.ColumnType(0))
		}
		identifier, decodeErr := NewOpenCodeCurrentSessionID(stmt.ColumnText(0))
		if decodeErr != nil {
			return fmt.Errorf("decode current session identifier row: %w", decodeErr)
		}
		identifiers = append(identifiers, identifier)
		return nil
	}
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeCurrentSessionsFirstStatement, []any{request.PageSize.value + 1}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeCurrentSessionsAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeCurrentSessionPage{}, s.sourceReadError(lease.ctx, "enumerate bounded current session identifiers", err, "session_message(session_id)", "supported current session_message")
	}
	hasNext := len(identifiers) > request.PageSize.value
	if hasNext {
		identifiers = identifiers[:request.PageSize.value]
	}
	identifiers = identifiers[:len(identifiers):len(identifiers)]
	page := OpenCodeCurrentSessionPage{SessionIDs: identifiers}
	if hasNext {
		cursor := OpenCodeCurrentSessionCursor{sessionID: page.SessionIDs[len(page.SessionIDs)-1]}
		page.Next = &cursor
	}
	return page, nil
}

// LegacyMessages returns one session's current materialized legacy messages in
// canonical (time_created, id) order.
func (s *zombiezenOpenCodeSQLiteSource) LegacyMessages(ctx context.Context, request OpenCodeLegacyMessagePageRequest) (OpenCodeLegacyMessagePage, error) {
	if err := validateLegacyMessagePageRequest(request); err != nil {
		return OpenCodeLegacyMessagePage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "read bounded legacy message page")
	if err != nil {
		return OpenCodeLegacyMessagePage{}, err
	}
	defer lease.release()

	rows := make([]OpenCodeLegacyMessageRow, 0, request.PageSize.value+1)
	decode := func(stmt *sqlite.Stmt) error {
		row, decodeErr := decodeLegacyMessageRow(stmt)
		if decodeErr != nil {
			return decodeErr
		}
		if row.SessionID != request.SessionID {
			return fmt.Errorf("decode legacy message row %q: projected session %q differs from requested session %q", row.ID.String(), row.SessionID.String(), request.SessionID.String())
		}
		rows = append(rows, row)
		return nil
	}
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacyMessagesFirstStatement, []any{request.SessionID.value, request.PageSize.value + 1}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacyMessagesAfterStatement, []any{request.SessionID.value, request.After.timeCreated, request.After.messageID.value, request.PageSize.value + 1}, decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeLegacyMessagePage{}, s.sourceReadError(lease.ctx, "read bounded legacy message page", err, "message(id, session_id, time_created, time_updated, data)", "supported legacy message/part")
	}
	hasNext := len(rows) > request.PageSize.value
	if hasNext {
		rows = rows[:request.PageSize.value]
	}
	rows = rows[:len(rows):len(rows)]
	page := OpenCodeLegacyMessagePage{Messages: rows}
	if hasNext {
		last := page.Messages[len(page.Messages)-1]
		cursor := OpenCodeLegacyMessageCursor{timeCreated: last.TimeCreated, messageID: last.ID}
		page.Next = &cursor
	}
	return page, nil
}

// LegacyParts returns one message's current materialized legacy parts in part
// identifier order while retaining both session and message scope.
func (s *zombiezenOpenCodeSQLiteSource) LegacyParts(ctx context.Context, request OpenCodeLegacyPartPageRequest) (OpenCodeLegacyPartPage, error) {
	if err := validateLegacyPartPageRequest(request); err != nil {
		return OpenCodeLegacyPartPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "read bounded legacy part page")
	if err != nil {
		return OpenCodeLegacyPartPage{}, err
	}
	defer lease.release()

	rows := make([]OpenCodeLegacyPartRow, 0, request.PageSize.value+1)
	decode := func(stmt *sqlite.Stmt) error {
		row, decodeErr := decodeLegacyPartRow(stmt)
		if decodeErr != nil {
			return decodeErr
		}
		if row.SessionID != request.SessionID || row.MessageID != request.MessageID {
			return fmt.Errorf("decode legacy part row %q: projected session/message %q/%q differs from requested scope %q/%q", row.ID.String(), row.SessionID.String(), row.MessageID.String(), request.SessionID.String(), request.MessageID.String())
		}
		rows = append(rows, row)
		return nil
	}
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacyPartsFirstStatement, []any{request.SessionID.value, request.MessageID.value, request.PageSize.value + 1}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacyPartsAfterStatement, []any{request.SessionID.value, request.MessageID.value, request.After.partID.value, request.PageSize.value + 1}, decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeLegacyPartPage{}, s.sourceReadError(lease.ctx, "read bounded legacy part page", err, "part(id, message_id, session_id, time_created, time_updated, data)", "supported legacy message/part")
	}
	hasNext := len(rows) > request.PageSize.value
	if hasNext {
		rows = rows[:request.PageSize.value]
	}
	rows = rows[:len(rows):len(rows)]
	page := OpenCodeLegacyPartPage{Parts: rows}
	if hasNext {
		cursor := OpenCodeLegacyPartCursor{partID: page.Parts[len(page.Parts)-1].ID}
		page.Next = &cursor
	}
	return page, nil
}

// CurrentMessages returns one session's materialized current rows in seq order.
func (s *zombiezenOpenCodeSQLiteSource) CurrentMessages(ctx context.Context, request OpenCodeCurrentPageRequest) (OpenCodeCurrentPage, error) {
	if err := validateCurrentPageRequest(request); err != nil {
		return OpenCodeCurrentPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "read bounded current session_message page")
	if err != nil {
		return OpenCodeCurrentPage{}, err
	}
	defer lease.release()

	rows := make([]OpenCodeCurrentMessageRow, 0, request.PageSize.value+1)
	decode := func(stmt *sqlite.Stmt) error {
		row, decodeErr := decodeCurrentMessageRow(stmt)
		if decodeErr != nil {
			return decodeErr
		}
		if row.SessionID != request.SessionID {
			return fmt.Errorf("decode current message row %q: projected session %q differs from requested session %q", row.ID.String(), row.SessionID.String(), request.SessionID.String())
		}
		rows = append(rows, row)
		pending := openCodeCurrentPendingPageState{row: rows[len(rows)-1], count: len(rows)}
		if checkpointErr := s.options.cancellationCheckpoint.AfterPendingRow(lease.ctx, pending); checkpointErr != nil {
			return fmt.Errorf("check current page cancellation after collecting row %q into %d-row pending atomic page: %w", pending.row.ID.String(), pending.count, checkpointErr)
		}
		return nil
	}
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeCurrentMessagesFirstStatement, []any{request.SessionID.value, request.PageSize.value + 1}, decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeCurrentMessagesAfterStatement, []any{request.SessionID.value, request.After.sequence.value, request.PageSize.value + 1}, decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeCurrentPage{}, s.sourceReadError(lease.ctx, "read bounded current session_message page", err, "session_message(id, session_id, type, time_created, time_updated, data, seq)", "supported current session_message")
	}
	hasNext := len(rows) > request.PageSize.value
	if hasNext {
		rows = rows[:request.PageSize.value]
	}
	rows = rows[:len(rows):len(rows)]
	page := OpenCodeCurrentPage{Messages: rows}
	if hasNext {
		cursor := OpenCodeCurrentCursor{sequence: page.Messages[len(page.Messages)-1].Seq}
		page.Next = &cursor
	}
	return page, nil
}

type openCodeReadLease struct {
	source       *zombiezenOpenCodeSQLiteSource
	ctx          context.Context
	cancel       context.CancelFunc
	oldInterrupt <-chan struct{}
}

func (s *zombiezenOpenCodeSQLiteSource) beginSourceRead(parent context.Context, operation string) (*openCodeReadLease, error) {
	if parent == nil {
		return nil, fmt.Errorf("%s from OpenCode SQLite source %q failed before waiting for the single connection: context is nil, so cancellation and deadlines cannot be enforced; no transcript row was read; pass a non-nil context", operation, s.path.String())
	}
	queryCtx, queryCancel := s.options.clock.WithTimeout(parent, s.options.queryTimeout)
	select {
	case <-queryCtx.Done():
		queryCancel()
		return nil, fmt.Errorf("%s from OpenCode SQLite source %q stopped while waiting for the single connection because the caller context or %s deadline ended: %w; no transcript row was read and no source write was attempted; retry with a live bounded context", operation, s.path.String(), s.options.queryTimeout, context.Cause(queryCtx))
	case <-s.permit:
	}

	activeCtx, activeCancel := context.WithCancel(queryCtx)
	s.stateMu.Lock()
	if s.closing || s.connClosed {
		s.stateMu.Unlock()
		activeCancel()
		queryCancel()
		s.permit <- struct{}{}
		return nil, fmt.Errorf("%s from OpenCode SQLite source %q failed before transcript access because the source is closing or closed; no row was read; open a new source for further bounded reads", operation, s.path.String())
	}
	s.activeCancel = activeCancel
	s.stateMu.Unlock()
	return &openCodeReadLease{
		source:       s,
		ctx:          activeCtx,
		cancel:       func() { activeCancel(); queryCancel() },
		oldInterrupt: s.conn.SetInterrupt(activeCtx.Done()),
	}, nil
}

func (l *openCodeReadLease) release() {
	l.source.conn.SetInterrupt(l.oldInterrupt)
	l.source.stateMu.Lock()
	l.source.activeCancel = nil
	l.source.stateMu.Unlock()
	l.cancel()
	l.source.permit <- struct{}{}
}

func (s *zombiezenOpenCodeSQLiteSource) sourceReadError(ctx context.Context, operation string, err error, projection, supportedSchema string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s from OpenCode SQLite source %q stopped during explicit-column keyset collection because its caller context or %s deadline ended: %w; no partial page or continuation cursor was returned and no source write was attempted; retry the same request and cursor with a live bounded context", operation, s.path.String(), s.options.queryTimeout, context.Cause(ctx))
	}
	return fmt.Errorf("%s from OpenCode SQLite source %q failed while decoding the scoped %s projection: %w; no partial page or continuation cursor was returned and no history or external-output source was consulted; verify that this is a %s schema and repair the source with OpenCode before retrying", operation, s.path.String(), projection, err, supportedSchema)
}

func validateCurrentPageRequest(request OpenCodeCurrentPageRequest) error {
	if request.PageSize.value <= 0 || request.PageSize.value > MaxOpenCodeCurrentPageSize {
		return fmt.Errorf("validate OpenCode current page request failed before source access: page size %d was not created by NewOpenCodeCurrentPageSize, so the read cannot be proven bounded; construct the page size with the validator", request.PageSize.value)
	}
	if err := validateOpenCodeCurrentToken("session identifier", request.SessionID.value); err != nil {
		return err
	}
	if request.After != nil && request.After.sequence.value < 0 {
		return fmt.Errorf("validate OpenCode current page cursor failed before source access: seq %d is negative, so keyset continuation cannot preserve the non-negative source contract; construct the cursor from NewOpenCodeCurrentSeq", request.After.sequence.value)
	}
	return nil
}

func validateCurrentSessionPageRequest(request OpenCodeCurrentSessionPageRequest) error {
	if request.PageSize.value <= 0 || request.PageSize.value > MaxOpenCodeCurrentPageSize {
		return fmt.Errorf("validate OpenCode current session page request failed before source access: page size %d was not created by NewOpenCodeCurrentPageSize, so enumeration cannot be proven bounded; construct the page size with the validator", request.PageSize.value)
	}
	if request.After != nil {
		if err := validateOpenCodeCurrentToken("session cursor", request.After.sessionID.value); err != nil {
			return err
		}
	}
	return nil
}

func decodeCurrentMessageRow(stmt *sqlite.Stmt) (OpenCodeCurrentMessageRow, error) {
	if err := requireOpenCodeColumnTypes(stmt, []sqlite.ColumnType{sqlite.TypeText, sqlite.TypeText, sqlite.TypeText, sqlite.TypeInteger, sqlite.TypeInteger, sqlite.TypeText, sqlite.TypeInteger}); err != nil {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row: %w", err)
	}
	messageID, err := NewOpenCodeCurrentMessageID(stmt.ColumnText(0))
	if err != nil {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row identifier: %w", err)
	}
	sessionID, err := NewOpenCodeCurrentSessionID(stmt.ColumnText(1))
	if err != nil {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row session: %w", err)
	}
	messageType, err := NewOpenCodeCurrentMessageType(stmt.ColumnText(2))
	if err != nil {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row type: %w", err)
	}
	data := stmt.ColumnText(5)
	if !json.Valid([]byte(data)) {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row %q: data is not valid JSON", messageID.String())
	}
	sequence, err := NewOpenCodeCurrentSeq(stmt.ColumnInt64(6))
	if err != nil {
		return OpenCodeCurrentMessageRow{}, fmt.Errorf("decode current session_message row %q sequence: %w", messageID.String(), err)
	}
	return OpenCodeCurrentMessageRow{
		ID:          messageID,
		SessionID:   sessionID,
		Type:        messageType,
		TimeCreated: stmt.ColumnInt64(3),
		TimeUpdated: stmt.ColumnInt64(4),
		Data:        data,
		Seq:         sequence,
	}, nil
}

func validateLegacySessionPageRequest(request OpenCodeLegacySessionPageRequest) error {
	if request.PageSize.value <= 0 || request.PageSize.value > MaxOpenCodeLegacyPageSize {
		return fmt.Errorf("validate OpenCode legacy session page request failed before source access: page size %d was not created by NewOpenCodeLegacyPageSize, so the enumeration cannot be proven bounded; construct the page size with the validator", request.PageSize.value)
	}
	if request.After != nil {
		if err := validateOpenCodeLegacyIdentifier("session cursor", request.After.sessionID.value); err != nil {
			return err
		}
	}
	return nil
}

func validateLegacyMessagePageRequest(request OpenCodeLegacyMessagePageRequest) error {
	if err := validateLegacySessionPageRequest(OpenCodeLegacySessionPageRequest{PageSize: request.PageSize}); err != nil {
		return fmt.Errorf("validate OpenCode legacy message page request: %w", err)
	}
	if err := validateOpenCodeLegacyIdentifier("session", request.SessionID.value); err != nil {
		return err
	}
	if request.After != nil {
		if err := validateOpenCodeLegacyIdentifier("message cursor", request.After.messageID.value); err != nil {
			return err
		}
	}
	return nil
}

func validateLegacyPartPageRequest(request OpenCodeLegacyPartPageRequest) error {
	if err := validateLegacySessionPageRequest(OpenCodeLegacySessionPageRequest{PageSize: request.PageSize}); err != nil {
		return fmt.Errorf("validate OpenCode legacy part page request: %w", err)
	}
	if err := validateOpenCodeLegacyIdentifier("session", request.SessionID.value); err != nil {
		return err
	}
	if err := validateOpenCodeLegacyIdentifier("message", request.MessageID.value); err != nil {
		return err
	}
	if request.After != nil {
		if err := validateOpenCodeLegacyIdentifier("part cursor", request.After.partID.value); err != nil {
			return err
		}
	}
	return nil
}

func decodeLegacyMessageRow(stmt *sqlite.Stmt) (OpenCodeLegacyMessageRow, error) {
	if err := requireOpenCodeColumnTypes(stmt, []sqlite.ColumnType{sqlite.TypeText, sqlite.TypeText, sqlite.TypeInteger, sqlite.TypeInteger, sqlite.TypeText}); err != nil {
		return OpenCodeLegacyMessageRow{}, fmt.Errorf("decode legacy message row: %w", err)
	}
	messageID, err := NewOpenCodeLegacyMessageID(stmt.ColumnText(0))
	if err != nil {
		return OpenCodeLegacyMessageRow{}, fmt.Errorf("decode legacy message row identifier: %w", err)
	}
	sessionID, err := NewOpenCodeLegacySessionID(stmt.ColumnText(1))
	if err != nil {
		return OpenCodeLegacyMessageRow{}, fmt.Errorf("decode legacy message row session: %w", err)
	}
	data := stmt.ColumnText(4)
	if !json.Valid([]byte(data)) {
		return OpenCodeLegacyMessageRow{}, fmt.Errorf("decode legacy message row %q: data is not valid JSON", messageID.String())
	}
	return OpenCodeLegacyMessageRow{ID: messageID, SessionID: sessionID, TimeCreated: stmt.ColumnInt64(2), TimeUpdated: stmt.ColumnInt64(3), Data: data}, nil
}

func decodeLegacyPartRow(stmt *sqlite.Stmt) (OpenCodeLegacyPartRow, error) {
	if err := requireOpenCodeColumnTypes(stmt, []sqlite.ColumnType{sqlite.TypeText, sqlite.TypeText, sqlite.TypeText, sqlite.TypeInteger, sqlite.TypeInteger, sqlite.TypeText}); err != nil {
		return OpenCodeLegacyPartRow{}, fmt.Errorf("decode legacy part row: %w", err)
	}
	partID, err := NewOpenCodeLegacyPartID(stmt.ColumnText(0))
	if err != nil {
		return OpenCodeLegacyPartRow{}, fmt.Errorf("decode legacy part row identifier: %w", err)
	}
	messageID, err := NewOpenCodeLegacyMessageID(stmt.ColumnText(1))
	if err != nil {
		return OpenCodeLegacyPartRow{}, fmt.Errorf("decode legacy part row message: %w", err)
	}
	sessionID, err := NewOpenCodeLegacySessionID(stmt.ColumnText(2))
	if err != nil {
		return OpenCodeLegacyPartRow{}, fmt.Errorf("decode legacy part row session: %w", err)
	}
	data := stmt.ColumnText(5)
	if !json.Valid([]byte(data)) {
		return OpenCodeLegacyPartRow{}, fmt.Errorf("decode legacy part row %q: data is not valid JSON", partID.String())
	}
	return OpenCodeLegacyPartRow{ID: partID, MessageID: messageID, SessionID: sessionID, TimeCreated: stmt.ColumnInt64(3), TimeUpdated: stmt.ColumnInt64(4), Data: data}, nil
}

func requireOpenCodeColumnTypes(stmt *sqlite.Stmt, expected []sqlite.ColumnType) error {
	for index, expectedType := range expected {
		if actual := stmt.ColumnType(index); actual != expectedType {
			return fmt.Errorf("column %d has SQLite type %s instead of %s", index, actual, expectedType)
		}
	}
	return nil
}

func (s *zombiezenOpenCodeSQLiteSource) catalogLocked(ctx context.Context) (OpenCodeSchemaEvidence, error) {
	var evidence OpenCodeSchemaEvidence
	tables := make(map[string]bool)
	tableOverflow := false
	if err := s.executeRowsLocked(ctx, openCodeCatalogTablesStatement, nil, func(stmt *sqlite.Stmt) error {
		if len(evidence.Tables) == openCodeCatalogRowLimit {
			tableOverflow = true
			return nil
		}
		name := stmt.ColumnText(0)
		tables[name] = true
		evidence.Tables = append(evidence.Tables, name)
		return nil
	}); err != nil {
		return evidence, fmt.Errorf("read sqlite_schema table-name projection with %d-row retained limit: %w", openCodeCatalogRowLimit, err)
	}
	if tableOverflow {
		return evidence, &OpenCodeCatalogOverflowError{Scope: OpenCodeCatalogTables, Limit: openCodeCatalogRowLimit}
	}
	sort.Strings(evidence.Tables)

	var err error
	if tables["message"] {
		evidence.LegacyMessageColumns, err = s.columnsLocked(ctx, "message")
		if err != nil {
			return evidence, err
		}
	}
	if tables["part"] {
		evidence.LegacyPartColumns, err = s.columnsLocked(ctx, "part")
		if err != nil {
			return evidence, err
		}
	}
	if tables["session_message"] {
		evidence.CurrentMessageColumns, err = s.columnsLocked(ctx, "session_message")
		if err != nil {
			return evidence, err
		}
		evidence.CurrentIndexes, err = s.indexesLocked(ctx, "session_message")
		if err != nil {
			return evidence, err
		}
	}
	return evidence, nil
}

func (s *zombiezenOpenCodeSQLiteSource) columnsLocked(ctx context.Context, table string) ([]OpenCodeColumnEvidence, error) {
	columns := make([]OpenCodeColumnEvidence, 0)
	overflow := false
	err := s.executeRowsLocked(ctx, openCodeCatalogColumnsStatement, []any{table}, func(stmt *sqlite.Stmt) error {
		if len(columns) == openCodeColumnRowLimit {
			overflow = true
			return nil
		}
		columns = append(columns, OpenCodeColumnEvidence{
			Name:    stmt.ColumnText(0),
			NotNull: stmt.ColumnInt64(1) != 0,
			Primary: stmt.ColumnInt64(2) != 0,
		})
		return nil
	})
	if err != nil {
		return columns, fmt.Errorf("read pragma_table_info explicit columns for %q with %d-row retained limit: %w", table, openCodeColumnRowLimit, err)
	}
	if overflow {
		return columns, &OpenCodeCatalogOverflowError{Scope: OpenCodeCatalogColumns, Table: table, Limit: openCodeColumnRowLimit}
	}
	return columns, nil
}

func (s *zombiezenOpenCodeSQLiteSource) indexesLocked(ctx context.Context, table string) ([]OpenCodeIndexEvidence, error) {
	indexes := make([]OpenCodeIndexEvidence, 0)
	byName := make(map[string]int)
	rowCount := 0
	overflow := false
	err := s.executeRowsLocked(ctx, openCodeCatalogIndexesStatement, []any{table}, func(stmt *sqlite.Stmt) error {
		if rowCount == openCodeIndexRowLimit {
			overflow = true
			return nil
		}
		rowCount++
		name := stmt.ColumnText(0)
		index, ok := byName[name]
		if !ok {
			index = len(indexes)
			byName[name] = index
			indexes = append(indexes, OpenCodeIndexEvidence{Name: name, Unique: stmt.ColumnInt64(1) != 0, Partial: stmt.ColumnInt64(2) != 0})
		}
		indexes[index].Keys = append(indexes[index].Keys, OpenCodeIndexKeyEvidence{
			Sequence:   stmt.ColumnInt64(3),
			ColumnID:   stmt.ColumnInt64(4),
			Name:       stmt.ColumnText(5),
			Descending: stmt.ColumnInt64(6) != 0,
			Collation:  stmt.ColumnText(7),
			Key:        stmt.ColumnInt64(8) != 0,
		})
		return nil
	})
	if err != nil {
		return indexes, fmt.Errorf("read index-list/index-xinfo explicit ordering evidence for %q with %d-row retained limit: %w", table, openCodeIndexRowLimit, err)
	}
	if overflow {
		return indexes, &OpenCodeCatalogOverflowError{Scope: OpenCodeCatalogIndexes, Table: table, Limit: openCodeIndexRowLimit}
	}
	return indexes, nil
}

func (s *zombiezenOpenCodeSQLiteSource) executeRowsLocked(ctx context.Context, statement string, args []any, result func(*sqlite.Stmt) error) error {
	s.denied = ""
	prepared, trailingBytes, err := s.conn.PrepareTransient(statement)
	if err != nil {
		return s.prepareError(ctx, err)
	}
	if err := prepared.Finalize(); err != nil {
		return fmt.Errorf("finalize read-only source statement preflight: %w", err)
	}
	if trailingBytes != 0 && strings.TrimSpace(statement[len(statement)-trailingBytes:]) != "" {
		return fmt.Errorf("reject source statement with trailing SQL before execution")
	}

	s.denied = ""
	err = sqlitex.ExecuteTransient(s.conn, statement, &sqlitex.ExecOptions{Args: args, ResultFunc: result})
	if err != nil && s.denied != "" {
		return s.deniedStatementError(err)
	}
	return err
}

func (s *zombiezenOpenCodeSQLiteSource) prepareError(ctx context.Context, err error) error {
	if s.denied != "" {
		return s.deniedStatementError(err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("prepare bounded read-only source statement stopped because its context ended: %w", context.Cause(ctx))
	}
	return fmt.Errorf("prepare bounded read-only source statement: %w", err)
}

func (s *zombiezenOpenCodeSQLiteSource) deniedStatementError(err error) error {
	return fmt.Errorf("read OpenCode SQLite source %q rejected a forbidden statement during private bounded-read preparation because it requested forbidden SQLite action %q: %w; execution did not start and query_only remains enabled; keep source operations on the fixed catalog/message/part projections and never write, migrate, checkpoint, repair, maintain, copy, truncate, or alter OpenCode-owned files", s.path.String(), s.denied, err)
}

// Close prevents new source reads, cancels an active SQLite operation, and
// waits for the one private connection only within the injected deadline.
func (s *zombiezenOpenCodeSQLiteSource) Close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("close OpenCode SQLite source %q failed before shutdown: context is nil, so cleanup cannot be bounded; pass a non-nil context and retry", s.path.String())
	}
	closeCtx, cancel := s.options.clock.WithTimeout(ctx, s.options.queryTimeout)
	defer cancel()

	s.stateMu.Lock()
	s.closing = true
	activeCancel := s.activeCancel
	alreadyClosed := s.connClosed
	s.stateMu.Unlock()
	if alreadyClosed {
		return nil
	}
	if activeCancel != nil {
		activeCancel()
	}

	select {
	case <-closeCtx.Done():
		return fmt.Errorf("close OpenCode SQLite source %q stopped while waiting for the active bounded source operation because the caller context or %s cleanup deadline ended: %w; new reads remain disabled but the connection may still require a retry to release; retry Close with a live context after investigating source responsiveness", s.path.String(), s.options.queryTimeout, context.Cause(closeCtx))
	case <-s.permit:
	}
	defer func() { s.permit <- struct{}{} }()

	s.stateMu.Lock()
	if s.connClosed {
		s.stateMu.Unlock()
		return nil
	}
	s.stateMu.Unlock()
	if err := s.conn.Close(); err != nil {
		return fmt.Errorf("close OpenCode SQLite source %q failed while releasing its single read-only connection: %w; no further reads are allowed; retry bounded cleanup and inspect SQLite resource diagnostics", s.path.String(), err)
	}
	s.stateMu.Lock()
	s.connClosed = true
	s.stateMu.Unlock()
	return nil
}
