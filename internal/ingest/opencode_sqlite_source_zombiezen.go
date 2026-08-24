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
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	openCodeEnableQueryOnlyStatement         = "PRAGMA query_only=ON"
	openCodeReadQueryOnlyStatement           = "PRAGMA query_only"
	openCodeCatalogTablesStatement           = "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name LIMIT 257"
	openCodeCatalogColumnsStatement          = "SELECT name, \"notnull\", pk FROM pragma_table_info(?1) ORDER BY cid LIMIT 33"
	openCodeCatalogIndexesStatement          = "SELECT il.name, il.\"unique\", il.partial, xi.seqno, xi.cid, xi.name, xi.desc, xi.coll, xi.key FROM pragma_index_list(?1) AS il JOIN pragma_index_xinfo(il.name) AS xi ORDER BY il.name, xi.seqno LIMIT 65"
	openCodeLegacySessionsFirstStatement     = "SELECT DISTINCT session_id FROM message ORDER BY session_id LIMIT ?1"
	openCodeLegacySessionsAfterStatement     = "SELECT DISTINCT session_id FROM message WHERE session_id > ?1 ORDER BY session_id LIMIT ?2"
	openCodeLegacyMessagesFirstStatement     = "SELECT id, session_id, time_created, time_updated, data FROM message WHERE session_id = ?1 ORDER BY time_created, id LIMIT ?2"
	openCodeLegacyMessagesAfterStatement     = "SELECT id, session_id, time_created, time_updated, data FROM message WHERE session_id = ?1 AND (time_created > ?2 OR (time_created = ?2 AND id > ?3)) ORDER BY time_created, id LIMIT ?4"
	openCodeLegacySessionPartsFirstStatement = "SELECT id, message_id, session_id, time_created, time_updated, data FROM part WHERE session_id = ?1 ORDER BY id LIMIT ?2"
	openCodeLegacySessionPartsAfterStatement = "SELECT id, message_id, session_id, time_created, time_updated, data FROM part WHERE session_id = ?1 AND id > ?2 ORDER BY id LIMIT ?3"
	openCodeCurrentSessionsFirstStatement    = "SELECT DISTINCT session_id FROM session_message ORDER BY session_id LIMIT ?1"
	openCodeCurrentSessionsAfterStatement    = "SELECT DISTINCT session_id FROM session_message WHERE session_id > ?1 ORDER BY session_id LIMIT ?2"
	openCodeCurrentMessagesFirstStatement    = "SELECT id, session_id, type, time_created, time_updated, data, seq FROM session_message WHERE session_id = ?1 ORDER BY seq LIMIT ?2"
	openCodeCurrentMessagesAfterStatement    = "SELECT id, session_id, type, time_created, time_updated, data, seq FROM session_message WHERE session_id = ?1 AND seq > ?2 ORDER BY seq LIMIT ?3"

	openCodeLegacyMessageFreshnessBySessionStatement = "SELECT session_id, MAX(MAX(time_created, time_updated)) FROM message GROUP BY session_id"
	openCodeLegacyPartFreshnessBySessionStatement    = "SELECT session_id, MAX(MAX(time_created, time_updated)) FROM part GROUP BY session_id"
	openCodeCurrentFreshnessBySessionStatement       = "SELECT session_id, MAX(MAX(time_created, time_updated)) FROM session_message GROUP BY session_id"
	openCodeSessionRecordsFirstStatement             = "SELECT id, parent_id, time_updated FROM session ORDER BY id LIMIT ?1"
	openCodeSessionRecordsAfterStatement             = "SELECT id, parent_id, time_updated FROM session WHERE id > ?1 ORDER BY id LIMIT ?2"
	openCodeSessionAttributionFirstStatement         = "SELECT id, parent_id, time_updated, directory, title, time_created FROM session ORDER BY id LIMIT ?1"
	openCodeSessionAttributionAfterStatement         = "SELECT id, parent_id, time_updated, directory, title, time_created FROM session WHERE id > ?1 ORDER BY id LIMIT ?2"
	openCodeSessionExtendedFirstStatement            = "SELECT id, parent_id, time_updated, directory, title, time_created, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert FROM session ORDER BY id LIMIT ?1"
	openCodeSessionExtendedAfterStatement            = "SELECT id, parent_id, time_updated, directory, title, time_created, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert FROM session WHERE id > ?1 ORDER BY id LIMIT ?2"
	openCodeEventSequenceFirstStatement              = "SELECT aggregate_id, seq FROM event_sequence ORDER BY aggregate_id LIMIT ?1"
	openCodeEventSequenceAfterStatement              = "SELECT aggregate_id, seq FROM event_sequence WHERE aggregate_id > ?1 ORDER BY aggregate_id LIMIT ?2"
	openCodeEventMaxSeqStatement                     = "SELECT MAX(seq) FROM event WHERE aggregate_id = ?1"
	openCodeProjectFirstStatement                    = "SELECT id, worktree, vcs, name FROM project ORDER BY id LIMIT ?1"
	openCodeProjectAfterStatement                    = "SELECT id, worktree, vcs, name FROM project WHERE id > ?1 ORDER BY id LIMIT ?2"
	openCodeProjectDirectoryFirstStatement           = "SELECT project_id, directory, type FROM project_directory ORDER BY project_id, directory LIMIT ?1"
	openCodeProjectDirectoryAfterStatement           = "SELECT project_id, directory, type FROM project_directory WHERE project_id > ?1 OR (project_id = ?1 AND directory > ?2) ORDER BY project_id, directory LIMIT ?3"
	openCodeSessionParentsFirstStatement             = "SELECT id, parent_id FROM session ORDER BY id LIMIT ?1"
	openCodeSessionParentsAfterStatement             = "SELECT id, parent_id FROM session WHERE id > ?1 ORDER BY id LIMIT ?2"
	openCodeSessionClockFirstStatement               = "SELECT id, time_updated FROM session ORDER BY id LIMIT ?1"
	openCodeSessionClockAfterStatement               = "SELECT id, time_updated FROM session WHERE id > ?1 ORDER BY id LIMIT ?2"
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

	// sessionColumns caches the session table's column support. The schema of a
	// read-only source never changes, so the pragma runs once instead of on
	// every session-record page.
	sessionColumnsChecked bool
	sessionColumns        openCodeSessionColumnSupport
}

// openCodeSessionColumnSupport records which session-record columns the session
// table carries. present is false when the session table is absent. The
// attribution columns (directory, title, time_created) are read together only
// when all three exist alongside the parent link and clock, so an older layout
// that lacks any of them simply yields empty attribution rather than failing.
type openCodeSessionColumnSupport struct {
	present         bool
	hasParent       bool
	hasClock        bool
	hasDirectory    bool
	hasTitle        bool
	hasCreated      bool
	hasAgent        bool
	hasTokensInput  bool
	hasTokensOutput bool
	hasTokensReason bool
	hasCacheRead    bool
	hasCacheWrite   bool
	hasCost         bool
	hasVersion      bool
	hasSlug         bool
	hasRevert       bool
}

// attribution reports whether the session table carries the working directory,
// title, and creation time alongside the parent link and clock, so one bounded
// read can attribute every session to its directory, title, and creation time.
func (s openCodeSessionColumnSupport) attribution() bool {
	return s.hasParent && s.hasClock && s.hasDirectory && s.hasTitle && s.hasCreated
}

// extendedAttribution reports whether the session table carries every column of
// the extended record read: the base attribution columns plus the agent label,
// the five token aggregates, cost, version, slug, and revert. When any of those
// columns is absent the read falls back to the base attribution statement and
// the extended fields stay empty or zero, so an older layout never fails.
func (s openCodeSessionColumnSupport) extendedAttribution() bool {
	return s.attribution() && s.hasAgent && s.hasTokensInput && s.hasTokensOutput &&
		s.hasTokensReason && s.hasCacheRead && s.hasCacheWrite && s.hasCost &&
		s.hasVersion && s.hasSlug && s.hasRevert
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

// LegacyFreshnessBySession returns the newest row time of every legacy session
// in one database with one GROUP BY aggregate per table, so freshness reads are
// bounded by table count rather than by session count. A session absent from
// the result has no legacy rows and is left to the caller's floor.
func (s *zombiezenOpenCodeSQLiteSource) LegacyFreshnessBySession(ctx context.Context) (map[string]time.Time, error) {
	lease, err := s.beginSourceRead(ctx, "read legacy freshness by session")
	if err != nil {
		return nil, err
	}
	defer lease.release()
	result := make(map[string]time.Time)
	decode := newFreshnessBySessionDecoder(result)
	if err := s.executeRowsLocked(lease.ctx, openCodeLegacyMessageFreshnessBySessionStatement, nil, decode); err != nil || lease.ctx.Err() != nil {
		return nil, s.sourceReadError(lease.ctx, "read legacy message freshness by session", err, "message(session_id,time_created,time_updated)", "supported legacy message/part")
	}
	if err := s.executeRowsLocked(lease.ctx, openCodeLegacyPartFreshnessBySessionStatement, nil, decode); err != nil || lease.ctx.Err() != nil {
		return nil, s.sourceReadError(lease.ctx, "read legacy part freshness by session", err, "part(session_id,time_created,time_updated)", "supported legacy message/part")
	}
	return result, nil
}

// CurrentFreshnessBySession returns the newest row time of every current session
// in one database with one GROUP BY aggregate, so freshness reads are bounded by
// table count rather than by session count. A session absent from the result has
// no current rows and is left to the caller's floor.
func (s *zombiezenOpenCodeSQLiteSource) CurrentFreshnessBySession(ctx context.Context) (map[string]time.Time, error) {
	lease, err := s.beginSourceRead(ctx, "read current freshness by session")
	if err != nil {
		return nil, err
	}
	defer lease.release()
	result := make(map[string]time.Time)
	decode := newFreshnessBySessionDecoder(result)
	if err := s.executeRowsLocked(lease.ctx, openCodeCurrentFreshnessBySessionStatement, nil, decode); err != nil || lease.ctx.Err() != nil {
		return nil, s.sourceReadError(lease.ctx, "read current freshness by session", err, "session_message(session_id,time_created,time_updated)", "supported current session_message")
	}
	return result, nil
}

// newFreshnessBySessionDecoder returns a row decoder for a
// (session_id, MAX(MAX(time_created,time_updated))) GROUP BY aggregate. It keeps
// the newest millisecond per session across every statement it decodes, so the
// legacy message and part statements merge into one map. A null aggregate
// contributes nothing.
func newFreshnessBySessionDecoder(result map[string]time.Time) func(*sqlite.Stmt) error {
	return func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) != sqlite.TypeText {
			return fmt.Errorf("decode freshness by session: session_id has SQLite type %s instead of text", stmt.ColumnType(0))
		}
		sessionID := stmt.ColumnText(0)
		if sessionID == "" {
			return fmt.Errorf("decode freshness by session: session_id is empty")
		}
		switch stmt.ColumnType(1) {
		case sqlite.TypeNull:
			return nil
		case sqlite.TypeInteger:
			candidate := time.UnixMilli(stmt.ColumnInt64(1))
			if existing, ok := result[sessionID]; !ok || candidate.After(existing) {
				result[sessionID] = candidate
			}
			return nil
		default:
			return fmt.Errorf("decode freshness by session %q: aggregate has SQLite type %s instead of integer", sessionID, stmt.ColumnType(1))
		}
	}
}

// LegacySessionParts returns every part row of one session in identifier order,
// so the projection reads a session's parts once and partitions them into
// message parts and orphans in memory instead of scanning parts per message and
// again for orphans. It tolerates a row it cannot decode by dropping it, so one
// malformed part never fails the read.
func (s *zombiezenOpenCodeSQLiteSource) LegacySessionParts(ctx context.Context, request OpenCodeLegacySessionPartPageRequest) (OpenCodeLegacyPartPage, error) {
	if err := validateLegacySessionPartPageRequest(request); err != nil {
		return OpenCodeLegacyPartPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "read bounded legacy session part page")
	if err != nil {
		return OpenCodeLegacyPartPage{}, err
	}
	defer lease.release()
	collector := newTolerantLegacyPartPageCollector(request.PageSize.value, func(row OpenCodeLegacyPartRow) error {
		if row.SessionID != request.SessionID {
			return fmt.Errorf("decode legacy session part row %q: projected session %q differs from requested session %q", row.ID.String(), row.SessionID.String(), request.SessionID.String())
		}
		return nil
	})
	if request.After == nil {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacySessionPartsFirstStatement, []any{request.SessionID.value, request.PageSize.value + 1}, collector.decode)
	} else {
		err = s.executeRowsLocked(lease.ctx, openCodeLegacySessionPartsAfterStatement, []any{request.SessionID.value, request.After.partID.value, request.PageSize.value + 1}, collector.decode)
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeLegacyPartPage{}, s.sourceReadError(lease.ctx, "read bounded legacy session part page", err, "part(id, message_id, session_id, time_created, time_updated, data)", "supported legacy message/part")
	}
	return collector.page(), nil
}

func validateLegacySessionPartPageRequest(request OpenCodeLegacySessionPartPageRequest) error {
	if err := validateLegacySessionPageRequest(OpenCodeLegacySessionPageRequest{PageSize: request.PageSize}); err != nil {
		return fmt.Errorf("validate OpenCode legacy session part page request: %w", err)
	}
	if err := validateOpenCodeLegacyIdentifier("session", request.SessionID.value); err != nil {
		return err
	}
	if request.After != nil {
		if err := validateOpenCodeLegacyIdentifier("part cursor", request.After.partID.value); err != nil {
			return err
		}
	}
	return nil
}

// SessionRecords returns session rows in identifier order with their parent
// link and update clock. A database without a session table, or without the
// parent_id and time_updated columns, reports Supported=false with no rows, so
// older layouts stay discoverable as roots with file-based freshness.
func (s *zombiezenOpenCodeSQLiteSource) SessionRecords(ctx context.Context, request OpenCodeSessionRecordPageRequest) (OpenCodeSessionRecordPage, error) {
	var cursor *string
	if request.After != nil {
		cursor = &request.After.sessionID.value
	}
	if err := validateOpenCodeCurrentBoundedPage(request.PageSize.value, cursor, "session record cursor"); err != nil {
		return OpenCodeSessionRecordPage{}, err
	}
	lease, err := s.beginSourceRead(ctx, "read bounded session record page")
	if err != nil {
		return OpenCodeSessionRecordPage{}, err
	}
	defer lease.release()
	support, err := s.sessionColumnSupportLocked(lease.ctx)
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeSessionRecordPage{}, s.sourceReadError(lease.ctx, "read bounded session record page", err, "pragma_table_info(session)", "supported OpenCode session")
	}
	if !support.present {
		// The session table is absent, so there is nothing to read. Older
		// layouts stay discoverable as roots with file-based freshness.
		return OpenCodeSessionRecordPage{}, nil
	}
	hasParent, hasClock := support.hasParent, support.hasClock
	readAttribution := support.attribution()
	readExtended := support.extendedAttribution()
	page := OpenCodeSessionRecordPage{Supported: true, HasParent: hasParent, HasClock: hasClock}
	if !hasParent && !hasClock {
		// The session table exists but carries neither the parent link nor the
		// changed clock, so it can supply nothing. The caller records a
		// diagnostic and keeps the sessions as roots.
		return page, nil
	}
	bounded := newOpenCodeBoundedPage[OpenCodeSessionRecord](request.PageSize.value)
	var skipped []OpenCodeSessionRecordSkip
	var present []OpenCodeSessionLinkID
	var cursorID *OpenCodeSessionLinkID
	// An undecodable row is dropped with a skip note rather than failing the
	// whole read, so one bad row never loses every session's parent link and
	// clock. A row whose identifier is valid still advances the cursor even when
	// the rest of the row is dropped, so pagination makes progress.
	decode := func(stmt *sqlite.Stmt) error {
		bounded.observe()
		if stmt.ColumnType(0) != sqlite.TypeText {
			skipped = append(skipped, OpenCodeSessionRecordSkip{Reason: fmt.Sprintf("a session row was dropped because id has SQLite type %s instead of text", stmt.ColumnType(0))})
			return nil
		}
		rawID := stmt.ColumnText(0)
		sessionID, err := NewOpenCodeSessionLinkID(rawID)
		if err != nil {
			skipped = append(skipped, OpenCodeSessionRecordSkip{Reason: fmt.Sprintf("a session row was dropped because id %q is not a valid identifier: %v", rawID, err)})
			return nil
		}
		linkID := sessionID
		cursorID = &linkID
		// The row exists, so record its identifier before any column-level drop.
		// A session whose row is present is not deleted, even when its parent
		// link or clock cannot be decoded.
		present = append(present, sessionID)
		record := OpenCodeSessionRecord{SessionID: sessionID}
		// The parent link is column 1 whenever it is selected. The clock is the
		// last selected column: column 2 when both are present, otherwise
		// column 1.
		if hasParent {
			if stmt.ColumnType(1) == sqlite.TypeText && stmt.ColumnText(1) != "" {
				parentID, err := NewOpenCodeSessionLinkID(stmt.ColumnText(1))
				if err != nil {
					skipped = append(skipped, OpenCodeSessionRecordSkip{Reason: fmt.Sprintf("session row %q was dropped because parent_id is not a valid identifier: %v", rawID, err)})
					return nil
				}
				record.ParentID = parentID
			}
		}
		if hasClock {
			clockColumn := 1
			if hasParent {
				clockColumn = 2
			}
			switch stmt.ColumnType(clockColumn) {
			case sqlite.TypeNull:
			case sqlite.TypeInteger:
				record.TimeUpdated = stmt.ColumnInt64(clockColumn)
			default:
				skipped = append(skipped, OpenCodeSessionRecordSkip{Reason: fmt.Sprintf("session row %q was dropped because time_updated has SQLite type %s instead of integer", rawID, stmt.ColumnType(clockColumn))})
				return nil
			}
		}
		if readAttribution {
			// The attribution statement selects directory, title, and time_created
			// as columns 3, 4, and 5 after id, parent_id, and time_updated.
			// Attribution is best effort: a null or non-text directory or title
			// yields an empty field, and a null or non-integer time_created yields
			// zero, so a bad attribution column never drops a row whose parent link
			// and clock are usable.
			if stmt.ColumnType(3) == sqlite.TypeText {
				record.Directory = stmt.ColumnText(3)
			}
			if stmt.ColumnType(4) == sqlite.TypeText {
				record.Title = stmt.ColumnText(4)
			}
			if stmt.ColumnType(5) == sqlite.TypeInteger {
				record.TimeCreated = stmt.ColumnInt64(5)
			}
		}
		if readExtended {
			// The extended statement appends agent, the five token aggregates,
			// cost, version, slug, and revert as columns 6 through 15. Each column
			// is best effort: a null or type-mismatched value yields the zero field
			// so a bad extended column never drops a row whose parent link, clock,
			// and attribution are usable.
			if stmt.ColumnType(6) == sqlite.TypeText {
				record.Agent = stmt.ColumnText(6)
			}
			if stmt.ColumnType(7) == sqlite.TypeInteger {
				record.TokensInput = stmt.ColumnInt64(7)
			}
			if stmt.ColumnType(8) == sqlite.TypeInteger {
				record.TokensOutput = stmt.ColumnInt64(8)
			}
			if stmt.ColumnType(9) == sqlite.TypeInteger {
				record.TokensReasoning = stmt.ColumnInt64(9)
			}
			if stmt.ColumnType(10) == sqlite.TypeInteger {
				record.TokensCacheRead = stmt.ColumnInt64(10)
			}
			if stmt.ColumnType(11) == sqlite.TypeInteger {
				record.TokensCacheWrite = stmt.ColumnInt64(11)
			}
			switch stmt.ColumnType(12) {
			case sqlite.TypeFloat, sqlite.TypeInteger:
				record.Cost = stmt.ColumnFloat(12)
			}
			if stmt.ColumnType(13) == sqlite.TypeText {
				record.Version = stmt.ColumnText(13)
			}
			if stmt.ColumnType(14) == sqlite.TypeText {
				record.Slug = stmt.ColumnText(14)
			}
			if stmt.ColumnType(15) == sqlite.TypeText {
				record.Revert = stmt.ColumnText(15)
			}
		}
		bounded.keep(record)
		return nil
	}
	// The statements stay compile-time constants at each call site so the
	// read-only source-statement guard resolves the fixed statement set.
	projection := "session(id, parent_id, time_updated)"
	switch {
	case readExtended:
		projection = "session(id, parent_id, time_updated, directory, title, time_created, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert)"
		if request.After == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionExtendedFirstStatement, []any{request.PageSize.value + 1}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionExtendedAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
		}
	case readAttribution:
		projection = "session(id, parent_id, time_updated, directory, title, time_created)"
		if request.After == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionAttributionFirstStatement, []any{request.PageSize.value + 1}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionAttributionAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
		}
	case hasParent && hasClock:
		if request.After == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionRecordsFirstStatement, []any{request.PageSize.value + 1}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionRecordsAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
		}
	case hasParent:
		projection = "session(id, parent_id)"
		if request.After == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionParentsFirstStatement, []any{request.PageSize.value + 1}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionParentsAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
		}
	default:
		projection = "session(id, time_updated)"
		if request.After == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionClockFirstStatement, []any{request.PageSize.value + 1}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeSessionClockAfterStatement, []any{request.After.sessionID.value, request.PageSize.value + 1}, decode)
		}
	}
	if err != nil || lease.ctx.Err() != nil {
		return OpenCodeSessionRecordPage{}, s.sourceReadError(lease.ctx, "read bounded session record page", err, projection, "supported OpenCode session")
	}
	// The shared bounded page counts every row seen, valid or skipped, so dropped
	// rows never shrink a page below its bound and hide later sessions.
	records, hasNext := bounded.assemble()
	page.Records = records
	page.PresentSessionIDs = present
	page.Skipped = skipped
	if hasNext {
		switch {
		case bounded.overflowKept():
			// Every fetched row was kept and one was trimmed, so re-fetch from the
			// last kept record. The trimmed row returns on the next page and is
			// never lost. No row was dropped on this page.
			page.Next = &OpenCodeSessionRecordCursor{sessionID: records[len(records)-1].SessionID}
		case cursorID != nil:
			// A row was dropped, so advance past the last identifier this page
			// observed. A dropped tail row is not re-fetched, so each dropped row
			// is reported once rather than again on the next page.
			page.Next = &OpenCodeSessionRecordCursor{sessionID: *cursorID}
		case len(records) > 0:
			page.Next = &OpenCodeSessionRecordCursor{sessionID: records[len(records)-1].SessionID}
		}
	}
	return page, nil
}

// sessionColumnSupportLocked returns the session table's column support and
// caches it after the first read, so the pragma runs once per source rather
// than on every session-record page. The caller holds the single connection.
func (s *zombiezenOpenCodeSQLiteSource) sessionColumnSupportLocked(ctx context.Context) (openCodeSessionColumnSupport, error) {
	s.stateMu.Lock()
	if s.sessionColumnsChecked {
		support := s.sessionColumns
		s.stateMu.Unlock()
		return support, nil
	}
	s.stateMu.Unlock()

	columns, err := s.columnsLocked(ctx, "session")
	if err != nil {
		return openCodeSessionColumnSupport{}, err
	}
	support := openCodeSessionColumnSupport{present: len(columns) > 0}
	for _, column := range columns {
		switch column.Name {
		case "parent_id":
			support.hasParent = true
		case "time_updated":
			support.hasClock = true
		case "directory":
			support.hasDirectory = true
		case "title":
			support.hasTitle = true
		case "time_created":
			support.hasCreated = true
		case "agent":
			support.hasAgent = true
		case "tokens_input":
			support.hasTokensInput = true
		case "tokens_output":
			support.hasTokensOutput = true
		case "tokens_reasoning":
			support.hasTokensReason = true
		case "tokens_cache_read":
			support.hasCacheRead = true
		case "tokens_cache_write":
			support.hasCacheWrite = true
		case "cost":
			support.hasCost = true
		case "version":
			support.hasVersion = true
		case "slug":
			support.hasSlug = true
		case "revert":
			support.hasRevert = true
		}
	}
	s.stateMu.Lock()
	s.sessionColumnsChecked = true
	s.sessionColumns = support
	s.stateMu.Unlock()
	return support, nil
}

// projectColumnsPresent reports whether a table carries every named column, so
// an older layout that lacks the project or project_directory shape yields no
// attribution instead of failing the read.
func projectColumnsPresent(columns []OpenCodeColumnEvidence, required ...string) bool {
	if len(columns) == 0 {
		return false
	}
	have := make(map[string]bool, len(columns))
	for _, column := range columns {
		have[column.Name] = true
	}
	for _, name := range required {
		if !have[name] {
			return false
		}
	}
	return true
}

// ProjectAttribution reads the project and project_directory tables once per
// database. It pages each table by its key so the read stays bounded, and it
// reports the tables absent when their required columns are missing, so
// discovery keeps its git resolution for an older layout. Only the allowlisted
// columns are projected; the read never touches the event stream.
func (s *zombiezenOpenCodeSQLiteSource) ProjectAttribution(ctx context.Context) (OpenCodeProjectAttribution, error) {
	lease, err := s.beginSourceRead(ctx, "read project attribution")
	if err != nil {
		return OpenCodeProjectAttribution{}, err
	}
	defer lease.release()
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return OpenCodeProjectAttribution{}, err
	}
	limit := pageSize.value
	var attribution OpenCodeProjectAttribution

	projectColumns, err := s.columnsLocked(lease.ctx, "project")
	if err != nil {
		return OpenCodeProjectAttribution{}, s.sourceReadError(lease.ctx, "read project columns", err, "pragma_table_info(project)", "supported OpenCode project")
	}
	if projectColumnsPresent(projectColumns, "id", "worktree", "name") {
		attribution.ProjectsPresent = true
		var cursor *string
		for {
			batch := 0
			var lastID string
			decode := func(stmt *sqlite.Stmt) error {
				batch++
				lastID = stmt.ColumnText(0)
				projectID, idErr := NewOpenCodeProjectID(stmt.ColumnText(0))
				if idErr != nil {
					// A project row whose identifier cannot be validated is skipped;
					// no session can match it, so it never drives attribution.
					return nil
				}
				attribution.Projects = append(attribution.Projects, OpenCodeProjectRecord{
					ID:       projectID,
					Worktree: stmt.ColumnText(1),
					VCS:      stmt.ColumnText(2),
					Name:     stmt.ColumnText(3),
				})
				return nil
			}
			if cursor == nil {
				err = s.executeRowsLocked(lease.ctx, openCodeProjectFirstStatement, []any{limit}, decode)
			} else {
				err = s.executeRowsLocked(lease.ctx, openCodeProjectAfterStatement, []any{*cursor, limit}, decode)
			}
			if err != nil || lease.ctx.Err() != nil {
				return OpenCodeProjectAttribution{}, s.sourceReadError(lease.ctx, "read bounded project page", err, "project(id, worktree, vcs, name)", "supported OpenCode project")
			}
			if batch < limit {
				break
			}
			next := lastID
			cursor = &next
		}
	}

	directoryColumns, err := s.columnsLocked(lease.ctx, "project_directory")
	if err != nil {
		return OpenCodeProjectAttribution{}, s.sourceReadError(lease.ctx, "read project_directory columns", err, "pragma_table_info(project_directory)", "supported OpenCode project_directory")
	}
	if projectColumnsPresent(directoryColumns, "project_id", "directory", "type") {
		attribution.DirectoriesPresent = true
		var cursorProject, cursorDirectory *string
		for {
			batch := 0
			var lastProject, lastDirectory string
			decode := func(stmt *sqlite.Stmt) error {
				batch++
				lastProject = stmt.ColumnText(0)
				lastDirectory = stmt.ColumnText(1)
				projectID, idErr := NewOpenCodeProjectID(stmt.ColumnText(0))
				if idErr != nil {
					// A directory row with an unvalidatable project identifier is
					// skipped; it cannot join to a project, so it never attributes.
					return nil
				}
				attribution.Directories = append(attribution.Directories, OpenCodeProjectDirectoryRecord{
					ProjectID: projectID,
					Directory: stmt.ColumnText(1),
					Type:      stmt.ColumnText(2),
				})
				return nil
			}
			if cursorProject == nil {
				err = s.executeRowsLocked(lease.ctx, openCodeProjectDirectoryFirstStatement, []any{limit}, decode)
			} else {
				err = s.executeRowsLocked(lease.ctx, openCodeProjectDirectoryAfterStatement, []any{*cursorProject, *cursorDirectory, limit}, decode)
			}
			if err != nil || lease.ctx.Err() != nil {
				return OpenCodeProjectAttribution{}, s.sourceReadError(lease.ctx, "read bounded project_directory page", err, "project_directory(project_id, directory, type)", "supported OpenCode project_directory")
			}
			if batch < limit {
				break
			}
			nextProject, nextDirectory := lastProject, lastDirectory
			cursorProject, cursorDirectory = &nextProject, &nextDirectory
		}
	}
	return attribution, nil
}

// EventSequenceBySession reads the newest event sequence for every session from
// the event_sequence table in one bounded pass. It never touches the event table
// or its payload. When the event_sequence table or its required columns are
// absent it reports Present false, so the caller falls back to the per-session
// MAX(seq) seek.
func (s *zombiezenOpenCodeSQLiteSource) EventSequenceBySession(ctx context.Context) (OpenCodeEventSequence, error) {
	lease, err := s.beginSourceRead(ctx, "read event sequence by session")
	if err != nil {
		return OpenCodeEventSequence{}, err
	}
	defer lease.release()
	columns, err := s.columnsLocked(lease.ctx, "event_sequence")
	if err != nil {
		return OpenCodeEventSequence{}, s.sourceReadError(lease.ctx, "read event_sequence columns", err, "pragma_table_info(event_sequence)", "supported OpenCode event_sequence")
	}
	if !projectColumnsPresent(columns, "aggregate_id", "seq") {
		return OpenCodeEventSequence{}, nil
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return OpenCodeEventSequence{}, err
	}
	limit := pageSize.value
	result := OpenCodeEventSequence{Present: true, BySession: make(map[string]int64)}
	var cursor *string
	for {
		batch := 0
		var lastAggregate string
		decode := func(stmt *sqlite.Stmt) error {
			batch++
			lastAggregate = stmt.ColumnText(0)
			result.BySession[stmt.ColumnText(0)] = stmt.ColumnInt64(1)
			return nil
		}
		if cursor == nil {
			err = s.executeRowsLocked(lease.ctx, openCodeEventSequenceFirstStatement, []any{limit}, decode)
		} else {
			err = s.executeRowsLocked(lease.ctx, openCodeEventSequenceAfterStatement, []any{*cursor, limit}, decode)
		}
		if err != nil || lease.ctx.Err() != nil {
			return OpenCodeEventSequence{}, s.sourceReadError(lease.ctx, "read bounded event_sequence page", err, "event_sequence(aggregate_id, seq)", "supported OpenCode event_sequence")
		}
		if batch < limit {
			break
		}
		next := lastAggregate
		cursor = &next
	}
	return result, nil
}

// MaxEventSeq reads one session's newest event sequence directly from the event
// table with the payload-free indexed MAX(seq) aggregate. It projects no payload
// column. Present is false when the session has no event rows, so the caller
// keeps the clock-only signal for it. This is the fallback for a database with no
// event_sequence table.
func (s *zombiezenOpenCodeSQLiteSource) MaxEventSeq(ctx context.Context, sessionID OpenCodeSessionLinkID) (OpenCodeSessionSeq, error) {
	lease, err := s.beginSourceRead(ctx, "read max event sequence for session")
	if err != nil {
		return OpenCodeSessionSeq{}, err
	}
	defer lease.release()
	columns, err := s.columnsLocked(lease.ctx, "event")
	if err != nil {
		return OpenCodeSessionSeq{}, s.sourceReadError(lease.ctx, "read event columns", err, "pragma_table_info(event)", "supported OpenCode event")
	}
	if !projectColumnsPresent(columns, "aggregate_id", "seq") {
		// No event table to seek, so the session keeps its clock-only signal.
		return OpenCodeSessionSeq{}, nil
	}
	result := OpenCodeSessionSeq{}
	decode := func(stmt *sqlite.Stmt) error {
		if stmt.ColumnType(0) == sqlite.TypeInteger {
			result.Present = true
			result.Seq = stmt.ColumnInt64(0)
		}
		return nil
	}
	if err := s.executeRowsLocked(lease.ctx, openCodeEventMaxSeqStatement, []any{sessionID.value}, decode); err != nil || lease.ctx.Err() != nil {
		return OpenCodeSessionSeq{}, s.sourceReadError(lease.ctx, "read max event sequence", err, "event(seq) where aggregate_id", "supported OpenCode event")
	}
	return result, nil
}

// openCodeBoundedPage assembles one bounded page of rows for any keyset read.
// It counts every row observed, valid or dropped, so a dropped row never shrinks
// a page below its bound and hides a later page, then trims the kept rows to the
// requested size. The legacy part reads and the session-record read share it.
type openCodeBoundedPage[Row any] struct {
	pageSize int
	seen     int
	rows     []Row
}

func newOpenCodeBoundedPage[Row any](pageSize int) *openCodeBoundedPage[Row] {
	return &openCodeBoundedPage[Row]{pageSize: pageSize, rows: make([]Row, 0, pageSize+1)}
}

// observe counts one row read from the source, whether or not it is kept.
func (p *openCodeBoundedPage[Row]) observe() { p.seen++ }

// keep retains one decoded row for the page.
func (p *openCodeBoundedPage[Row]) keep(row Row) { p.rows = append(p.rows, row) }

// overflowKept reports whether more valid rows were kept than the page bound, so
// a kept row was trimmed and must be re-fetched on the next page. It is only
// true when every fetched row was kept, so no row was dropped on this page.
func (p *openCodeBoundedPage[Row]) overflowKept() bool { return len(p.rows) > p.pageSize }

// assemble returns the kept rows trimmed to the page bound and whether a further
// page exists.
func (p *openCodeBoundedPage[Row]) assemble() ([]Row, bool) {
	hasNext := p.seen > p.pageSize
	rows := p.rows
	if len(rows) > p.pageSize {
		rows = rows[:p.pageSize]
	}
	return rows[:len(rows):len(rows)], hasNext
}

// legacyPartPageCollector decodes part rows, checks their scope, and
// assembles one bounded page with its continuation cursor. LegacyParts and
// LegacyOrphanParts share it; each keeps its constant statements in its body.
type legacyPartPageCollector struct {
	bounded     *openCodeBoundedPage[OpenCodeLegacyPartRow]
	requireJSON bool
	tolerant    bool
	check       func(OpenCodeLegacyPartRow) error
	dropped     []OpenCodeOrphanPartDrop
	cursorID    *OpenCodeLegacyPartID
}

func newLegacyPartPageCollector(pageSize int, requireJSON bool, check func(OpenCodeLegacyPartRow) error) *legacyPartPageCollector {
	return &legacyPartPageCollector{bounded: newOpenCodeBoundedPage[OpenCodeLegacyPartRow](pageSize), requireJSON: requireJSON, check: check}
}

// newTolerantLegacyPartPageCollector collects orphan part rows and drops any
// row it cannot decode or scope instead of failing the whole read, so one
// malformed orphan row never fails the session.
func newTolerantLegacyPartPageCollector(pageSize int, check func(OpenCodeLegacyPartRow) error) *legacyPartPageCollector {
	collector := newLegacyPartPageCollector(pageSize, false, check)
	collector.tolerant = true
	return collector
}

func (c *legacyPartPageCollector) decode(stmt *sqlite.Stmt) error {
	c.bounded.observe()
	rawPartID, rawMessageID := bestEffortLegacyPartIdentifiers(stmt)
	// A row whose part id is a valid identifier advances the cursor even when the
	// rest of the row is dropped, so a page of all-dropped rows still makes
	// progress instead of ending pagination.
	if rawPartID != "" {
		if partID, idErr := NewOpenCodeLegacyPartID(rawPartID); idErr == nil {
			c.cursorID = &partID
		}
	}
	row, err := decodeLegacyPartRow(stmt, c.requireJSON)
	if err != nil {
		if c.tolerant {
			c.dropped = append(c.dropped, OpenCodeOrphanPartDrop{PartID: NewOpenCodeRawRowIdentifier(rawPartID), MessageID: NewOpenCodeRawRowIdentifier(rawMessageID), Reason: fmt.Sprintf("part row with id %q could not be decoded: %v", rawPartID, err)})
			return nil
		}
		return err
	}
	if err := c.check(row); err != nil {
		if c.tolerant {
			c.dropped = append(c.dropped, OpenCodeOrphanPartDrop{PartID: NewOpenCodeRawRowIdentifier(row.ID.String()), MessageID: NewOpenCodeRawRowIdentifier(row.MessageID.String()), Reason: fmt.Sprintf("part row %q was out of scope: %v", row.ID.String(), err)})
			return nil
		}
		return err
	}
	c.bounded.keep(row)
	return nil
}

// bestEffortLegacyPartIdentifiers reads the part and message identifiers
// directly from their text columns, so a dropped row still names its message.
// Either identifier is empty when its column is not text.
func bestEffortLegacyPartIdentifiers(stmt *sqlite.Stmt) (partID string, messageID string) {
	if stmt.ColumnType(0) == sqlite.TypeText {
		partID = stmt.ColumnText(0)
	}
	if stmt.ColumnType(1) == sqlite.TypeText {
		messageID = stmt.ColumnText(1)
	}
	return partID, messageID
}

func (c *legacyPartPageCollector) page() OpenCodeLegacyPartPage {
	rows, hasNext := c.bounded.assemble()
	page := OpenCodeLegacyPartPage{Parts: rows, Dropped: c.dropped}
	if hasNext {
		// Prefer the last kept row so the sentinel row beyond it is re-fetched on
		// the next page. When a page keeps no valid row, advance past the last
		// decodable part id so a page of all-dropped rows still moves.
		switch {
		case len(rows) > 0:
			page.Next = &OpenCodeLegacyPartCursor{partID: rows[len(rows)-1].ID}
		case c.cursorID != nil:
			page.Next = &OpenCodeLegacyPartCursor{partID: *c.cursorID}
		}
	}
	return page
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
	var cursor *string
	if request.After != nil {
		cursor = &request.After.sessionID.value
	}
	return validateOpenCodeCurrentBoundedPage(request.PageSize.value, cursor, "session cursor")
}

// validateOpenCodeCurrentBoundedPage validates the shared bounds every current
// page request has: a page size within the fixed maximum and, when present, a
// keyset cursor token. Current session enumeration and session-record
// enumeration both delegate to it instead of duplicating the checks.
func validateOpenCodeCurrentBoundedPage(pageSize int, cursor *string, cursorKind string) error {
	if pageSize <= 0 || pageSize > MaxOpenCodeCurrentPageSize {
		return fmt.Errorf("validate OpenCode bounded page request failed before source access: page size %d was not created by NewOpenCodeCurrentPageSize, so enumeration cannot be proven bounded; construct the page size with the validator", pageSize)
	}
	if cursor != nil {
		if err := validateOpenCodeCurrentToken(cursorKind, *cursor); err != nil {
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

// decodeLegacyPartRow decodes one part row. Ordinary parts require valid JSON
// data in a text column. Orphan parts (requireJSON false) keep malformed or
// non-text data so the projection can drop them with a warning instead of
// failing the whole session.
func decodeLegacyPartRow(stmt *sqlite.Stmt, requireJSON bool) (OpenCodeLegacyPartRow, error) {
	expected := []sqlite.ColumnType{sqlite.TypeText, sqlite.TypeText, sqlite.TypeText, sqlite.TypeInteger, sqlite.TypeInteger}
	if requireJSON {
		// Only ordinary parts require a text data column; an orphan row with BLOB
		// data is tolerated and dropped later with a warning.
		expected = append(expected, sqlite.TypeText)
	}
	if err := requireOpenCodeColumnTypes(stmt, expected); err != nil {
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
	if requireJSON && !json.Valid([]byte(data)) {
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
