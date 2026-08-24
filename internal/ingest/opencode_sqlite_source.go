package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
)

// OpenCodeCatalogScope identifies one bounded catalog projection.
type OpenCodeCatalogScope string

const (
	OpenCodeCatalogTables  OpenCodeCatalogScope = "tables"
	OpenCodeCatalogColumns OpenCodeCatalogScope = "columns"
	OpenCodeCatalogIndexes OpenCodeCatalogScope = "indexes"
)

// OpenCodeCatalogOverflowError reports that a bounded catalog projection
// contained a sentinel row beyond the retained evidence limit.
type OpenCodeCatalogOverflowError struct {
	Scope OpenCodeCatalogScope
	Table string
	Limit int
}

func (e *OpenCodeCatalogOverflowError) Error() string {
	location := string(e.Scope)
	if e.Table != "" {
		location += " for table " + e.Table
	}
	return fmt.Sprintf("bounded OpenCode catalog %s contained more than %d rows; retained evidence is incomplete", location, e.Limit)
}

const (
	defaultOpenCodeSQLiteBusyTimeout  = 250 * time.Millisecond
	defaultOpenCodeSQLiteQueryTimeout = 5 * time.Second
	// MaxOpenCodeLegacyPageSize is the fixed upper bound for every legacy
	// transcript source page.
	MaxOpenCodeLegacyPageSize = 128
	// MaxOpenCodeCurrentPageSize is the fixed upper bound for every current
	// session message source page.
	MaxOpenCodeCurrentPageSize = 128
)

// OpenCodeCurrentPageSize is a validated, positive current source page size.
type OpenCodeCurrentPageSize struct{ value int }

// NewOpenCodeCurrentPageSize validates a requested current source page size.
func NewOpenCodeCurrentPageSize(value int) (OpenCodeCurrentPageSize, error) {
	if value <= 0 || value > MaxOpenCodeCurrentPageSize {
		return OpenCodeCurrentPageSize{}, fmt.Errorf("validate OpenCode current page size %d failed before source access: the size must be between 1 and the fixed maximum %d, so the read cannot be proven bounded; choose a size within that range", value, MaxOpenCodeCurrentPageSize)
	}
	return OpenCodeCurrentPageSize{value: value}, nil
}

// Value returns the validated integer page size.
func (s OpenCodeCurrentPageSize) Value() int { return s.value }

// OpenCodeCurrentSessionID is a validated current session identifier.
type OpenCodeCurrentSessionID struct{ value string }

// NewOpenCodeCurrentSessionID validates a current session identifier.
func NewOpenCodeCurrentSessionID(value string) (OpenCodeCurrentSessionID, error) {
	if err := validateOpenCodeCurrentToken("session identifier", value); err != nil {
		return OpenCodeCurrentSessionID{}, err
	}
	return OpenCodeCurrentSessionID{value: value}, nil
}

// String returns the validated session identifier.
func (id OpenCodeCurrentSessionID) String() string { return id.value }

// OpenCodeCurrentMessageID is a validated current message identifier.
type OpenCodeCurrentMessageID struct{ value string }

// NewOpenCodeCurrentMessageID validates a current message identifier.
func NewOpenCodeCurrentMessageID(value string) (OpenCodeCurrentMessageID, error) {
	if err := validateOpenCodeCurrentToken("message identifier", value); err != nil {
		return OpenCodeCurrentMessageID{}, err
	}
	return OpenCodeCurrentMessageID{value: value}, nil
}

// String returns the validated message identifier.
func (id OpenCodeCurrentMessageID) String() string { return id.value }

// OpenCodeCurrentMessageType is a validated current projection row type.
type OpenCodeCurrentMessageType struct{ value string }

// NewOpenCodeCurrentMessageType validates a current projection row type.
func NewOpenCodeCurrentMessageType(value string) (OpenCodeCurrentMessageType, error) {
	if err := validateOpenCodeCurrentToken("message type", value); err != nil {
		return OpenCodeCurrentMessageType{}, err
	}
	return OpenCodeCurrentMessageType{value: value}, nil
}

// String returns the validated current projection row type.
func (messageType OpenCodeCurrentMessageType) String() string { return messageType.value }

func validateOpenCodeCurrentToken(kind, value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("validate OpenCode current %s %q failed before source access: the value is empty, has surrounding whitespace, or contains a NUL byte and cannot safely participate in a bounded read; use the exact non-empty value returned by the source", kind, value)
	}
	return nil
}

// OpenCodeCurrentSeq is a validated non-negative sequence value.
type OpenCodeCurrentSeq struct{ value int64 }

// NewOpenCodeCurrentSeq validates a current message sequence value.
func NewOpenCodeCurrentSeq(value int64) (OpenCodeCurrentSeq, error) {
	if value < 0 {
		return OpenCodeCurrentSeq{}, fmt.Errorf("validate OpenCode current sequence %d failed before source access: seq must be non-negative but may be sparse; use the exact non-negative seq returned by the source", value)
	}
	return OpenCodeCurrentSeq{value: value}, nil
}

// Value returns the validated sequence value.
func (sequence OpenCodeCurrentSeq) Value() int64 { return sequence.value }

// OpenCodeCurrentCursor resumes current message ordering after one seq value.
type OpenCodeCurrentCursor struct{ sequence OpenCodeCurrentSeq }

// NewOpenCodeCurrentCursor creates an explicit sequence cursor.
func NewOpenCodeCurrentCursor(sequence OpenCodeCurrentSeq) OpenCodeCurrentCursor {
	return OpenCodeCurrentCursor{sequence: sequence}
}

// Seq returns the last sequence represented by the cursor.
func (cursor OpenCodeCurrentCursor) Seq() OpenCodeCurrentSeq { return cursor.sequence }

// OpenCodeCurrentSessionCursor resumes current session enumeration after one
// stable session identifier.
type OpenCodeCurrentSessionCursor struct{ sessionID OpenCodeCurrentSessionID }

// NewOpenCodeCurrentSessionCursor creates a current session cursor.
func NewOpenCodeCurrentSessionCursor(sessionID OpenCodeCurrentSessionID) OpenCodeCurrentSessionCursor {
	return OpenCodeCurrentSessionCursor{sessionID: sessionID}
}

// SessionID returns the last identifier represented by the cursor.
func (cursor OpenCodeCurrentSessionCursor) SessionID() OpenCodeCurrentSessionID {
	return cursor.sessionID
}

// OpenCodeCurrentSessionPageRequest requests one bounded page of current
// session identifiers.
type OpenCodeCurrentSessionPageRequest struct {
	PageSize OpenCodeCurrentPageSize
	After    *OpenCodeCurrentSessionCursor
}

// OpenCodeCurrentSessionPage is a detached bounded session identifier page.
type OpenCodeCurrentSessionPage struct {
	SessionIDs []OpenCodeCurrentSessionID
	Next       *OpenCodeCurrentSessionCursor
}

// OpenCodeCurrentPageRequest requests one bounded page for one current session.
type OpenCodeCurrentPageRequest struct {
	SessionID OpenCodeCurrentSessionID
	PageSize  OpenCodeCurrentPageSize
	After     *OpenCodeCurrentCursor
}

// OpenCodeCurrentMessageRow is one detached row from the materialized current projection.
type OpenCodeCurrentMessageRow struct {
	ID          OpenCodeCurrentMessageID
	SessionID   OpenCodeCurrentSessionID
	Type        OpenCodeCurrentMessageType
	TimeCreated int64
	TimeUpdated int64
	Data        string
	Seq         OpenCodeCurrentSeq
}

// OpenCodeCurrentPage is a detached bounded current message page.
type OpenCodeCurrentPage struct {
	Messages []OpenCodeCurrentMessageRow
	Next     *OpenCodeCurrentCursor
}

// OpenCodeLegacyPageSize is a validated, positive legacy source page size.
type OpenCodeLegacyPageSize struct{ value int }

// NewOpenCodeLegacyPageSize validates a requested legacy source page size.
func NewOpenCodeLegacyPageSize(value int) (OpenCodeLegacyPageSize, error) {
	if value <= 0 || value > MaxOpenCodeLegacyPageSize {
		return OpenCodeLegacyPageSize{}, fmt.Errorf("validate OpenCode legacy page size %d failed before source access: the size must be between 1 and the fixed maximum %d, so the read cannot be proven bounded; choose a size within that range", value, MaxOpenCodeLegacyPageSize)
	}
	return OpenCodeLegacyPageSize{value: value}, nil
}

// Value returns the validated integer page size.
func (s OpenCodeLegacyPageSize) Value() int { return s.value }

// OpenCodeLegacySessionID is a validated legacy session identifier.
type OpenCodeLegacySessionID struct{ value string }

// NewOpenCodeLegacySessionID validates a legacy session identifier.
func NewOpenCodeLegacySessionID(value string) (OpenCodeLegacySessionID, error) {
	if err := validateOpenCodeLegacyIdentifier("session", value); err != nil {
		return OpenCodeLegacySessionID{}, err
	}
	return OpenCodeLegacySessionID{value: value}, nil
}

// String returns the validated session identifier.
func (id OpenCodeLegacySessionID) String() string { return id.value }

// OpenCodeLegacyMessageID is a validated legacy message identifier.
type OpenCodeLegacyMessageID struct{ value string }

// NewOpenCodeLegacyMessageID validates a legacy message identifier.
func NewOpenCodeLegacyMessageID(value string) (OpenCodeLegacyMessageID, error) {
	if err := validateOpenCodeLegacyIdentifier("message", value); err != nil {
		return OpenCodeLegacyMessageID{}, err
	}
	return OpenCodeLegacyMessageID{value: value}, nil
}

// String returns the validated message identifier.
func (id OpenCodeLegacyMessageID) String() string { return id.value }

// OpenCodeLegacyPartID is a validated legacy part identifier.
type OpenCodeLegacyPartID struct{ value string }

// NewOpenCodeLegacyPartID validates a legacy part identifier.
func NewOpenCodeLegacyPartID(value string) (OpenCodeLegacyPartID, error) {
	if err := validateOpenCodeLegacyIdentifier("part", value); err != nil {
		return OpenCodeLegacyPartID{}, err
	}
	return OpenCodeLegacyPartID{value: value}, nil
}

// String returns the validated part identifier.
func (id OpenCodeLegacyPartID) String() string { return id.value }

func validateOpenCodeLegacyIdentifier(kind, value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("validate OpenCode legacy %s identifier %q failed before source access: the identifier is empty, has surrounding whitespace, or contains a NUL byte and cannot safely scope a bounded read; use the exact non-empty identifier returned by the source", kind, value)
	}
	return nil
}

// OpenCodeLegacySessionCursor resumes ordered session-ID enumeration.
type OpenCodeLegacySessionCursor struct{ sessionID OpenCodeLegacySessionID }

// NewOpenCodeLegacySessionCursor validates an explicit session cursor.
func NewOpenCodeLegacySessionCursor(sessionID OpenCodeLegacySessionID) (OpenCodeLegacySessionCursor, error) {
	if err := validateOpenCodeLegacyIdentifier("session cursor", sessionID.value); err != nil {
		return OpenCodeLegacySessionCursor{}, err
	}
	return OpenCodeLegacySessionCursor{sessionID: sessionID}, nil
}

// SessionID returns the last session identifier represented by the cursor.
func (c OpenCodeLegacySessionCursor) SessionID() OpenCodeLegacySessionID { return c.sessionID }

// OpenCodeLegacyMessageCursor resumes message ordering by (time_created, id).
type OpenCodeLegacyMessageCursor struct {
	timeCreated int64
	messageID   OpenCodeLegacyMessageID
}

// NewOpenCodeLegacyMessageCursor validates an explicit message cursor.
func NewOpenCodeLegacyMessageCursor(timeCreated int64, messageID OpenCodeLegacyMessageID) (OpenCodeLegacyMessageCursor, error) {
	if err := validateOpenCodeLegacyIdentifier("message cursor", messageID.value); err != nil {
		return OpenCodeLegacyMessageCursor{}, err
	}
	return OpenCodeLegacyMessageCursor{timeCreated: timeCreated, messageID: messageID}, nil
}

// TimeCreated returns the cursor's message creation time.
func (c OpenCodeLegacyMessageCursor) TimeCreated() int64 { return c.timeCreated }

// MessageID returns the cursor's message identifier tie-breaker.
func (c OpenCodeLegacyMessageCursor) MessageID() OpenCodeLegacyMessageID { return c.messageID }

// OpenCodeLegacyPartCursor resumes part ordering by part identifier.
type OpenCodeLegacyPartCursor struct{ partID OpenCodeLegacyPartID }

// NewOpenCodeLegacyPartCursor validates an explicit part cursor.
func NewOpenCodeLegacyPartCursor(partID OpenCodeLegacyPartID) (OpenCodeLegacyPartCursor, error) {
	if err := validateOpenCodeLegacyIdentifier("part cursor", partID.value); err != nil {
		return OpenCodeLegacyPartCursor{}, err
	}
	return OpenCodeLegacyPartCursor{partID: partID}, nil
}

// PartID returns the last part identifier represented by the cursor.
func (c OpenCodeLegacyPartCursor) PartID() OpenCodeLegacyPartID { return c.partID }

// OpenCodeLegacySessionPageRequest requests one bounded session-ID page.
type OpenCodeLegacySessionPageRequest struct {
	PageSize OpenCodeLegacyPageSize
	After    *OpenCodeLegacySessionCursor
}

// OpenCodeLegacyMessagePageRequest requests one bounded page for one session.
type OpenCodeLegacyMessagePageRequest struct {
	SessionID OpenCodeLegacySessionID
	PageSize  OpenCodeLegacyPageSize
	After     *OpenCodeLegacyMessageCursor
}

// OpenCodeLegacySessionPartPageRequest requests every part row of one session in
// part identifier order, so the projection reads a session's parts once and
// partitions them into message parts and orphans in memory.
type OpenCodeLegacySessionPartPageRequest struct {
	SessionID OpenCodeLegacySessionID
	PageSize  OpenCodeLegacyPageSize
	After     *OpenCodeLegacyPartCursor
}

// OpenCodeSessionRecordPageRequest requests bounded session records from the
// upstream session table. Parent links and the per-session update clock are
// shared by every representation.
type OpenCodeSessionRecordPageRequest struct {
	PageSize OpenCodeCurrentPageSize
	After    *OpenCodeSessionRecordCursor
}

// OpenCodeSessionLinkID is a validated identifier from the shared session
// table. It names either a session or its parent.
type OpenCodeSessionLinkID struct{ value string }

// NewOpenCodeSessionLinkID validates one session table identifier.
func NewOpenCodeSessionLinkID(value string) (OpenCodeSessionLinkID, error) {
	if err := validateOpenCodeCurrentToken("session link identifier", value); err != nil {
		return OpenCodeSessionLinkID{}, err
	}
	return OpenCodeSessionLinkID{value: value}, nil
}

// String returns the validated identifier.
func (id OpenCodeSessionLinkID) String() string { return id.value }

// OpenCodeSessionRecordCursor continues session record enumeration after one
// session identifier.
type OpenCodeSessionRecordCursor struct{ sessionID OpenCodeSessionLinkID }

// OpenCodeSessionRecord is one detached session row. ParentID is the zero
// value for a root session. TimeUpdated is the upstream session clock, which
// OpenCode moves on every session mutation, including revert and undo.
// Directory, Title, and TimeCreated carry the session's working directory,
// title, and creation time when the session table exposes them; they stay
// empty for an older layout that lacks those columns.
type OpenCodeSessionRecord struct {
	SessionID   OpenCodeSessionLinkID
	ParentID    OpenCodeSessionLinkID
	TimeUpdated int64
	Directory   string
	Title       string
	TimeCreated int64
	// The extended record columns are read together only when the session table
	// carries every one of them. An older layout that lacks any of them leaves
	// these fields at their zero value rather than failing the read. Agent labels
	// a subagent session, the token counts and Cost carry the session-level
	// aggregates without folding entries, Version is the harness version, Slug is
	// the display-name fallback, and Revert carries the raw revert marker.
	Agent            string
	TokensInput      int64
	TokensOutput     int64
	TokensReasoning  int64
	TokensCacheRead  int64
	TokensCacheWrite int64
	Cost             float64
	Version          string
	Slug             string
	Revert           string
}

// OpenCodeSessionRecordSkip records one session row the bounded read could not
// decode. Reason explains why the row was dropped and names the row's
// best-effort raw identifier when the identifier itself decoded.
type OpenCodeSessionRecordSkip struct {
	Reason string
}

// OpenCodeSessionRecordPage is one bounded page of session records. Supported
// is false when the database has no session table at all; the page is then
// empty. HasParent and HasClock report which of the parent_id and time_updated
// columns the session table carries, so parent links are read whether or not the
// clock column exists. When the session table exists but carries neither column,
// Supported is true while HasParent and HasClock are both false and the page is
// empty. Skipped names the rows that could not be decoded; an undecodable row is
// dropped rather than failing the whole read.
type OpenCodeSessionRecordPage struct {
	Supported bool
	HasParent bool
	HasClock  bool
	Records   []OpenCodeSessionRecord
	// PresentSessionIDs names every row on this page whose identifier decoded,
	// including a row whose parent link or clock was dropped. A session that
	// still has a row here exists in OpenCode; a discovered session missing from
	// every page was deleted from the session table and its historical message
	// or session_message rows are stale.
	PresentSessionIDs []OpenCodeSessionLinkID
	Skipped           []OpenCodeSessionRecordSkip
	Next              *OpenCodeSessionRecordCursor
}

// OpenCodeLegacyMessageRow is one detached current row from legacy message.
type OpenCodeLegacyMessageRow struct {
	ID          OpenCodeLegacyMessageID
	SessionID   OpenCodeLegacySessionID
	TimeCreated int64
	TimeUpdated int64
	Data        string
}

// OpenCodeLegacyPartRow is one detached current row from legacy part.
type OpenCodeLegacyPartRow struct {
	ID          OpenCodeLegacyPartID
	MessageID   OpenCodeLegacyMessageID
	SessionID   OpenCodeLegacySessionID
	TimeCreated int64
	TimeUpdated int64
	Data        string
}

// OpenCodeLegacySessionPage is a detached bounded session-ID page.
type OpenCodeLegacySessionPage struct {
	SessionIDs []OpenCodeLegacySessionID
	Next       *OpenCodeLegacySessionCursor
}

// OpenCodeLegacyMessagePage is a detached bounded message page.
type OpenCodeLegacyMessagePage struct {
	Messages []OpenCodeLegacyMessageRow
	Next     *OpenCodeLegacyMessageCursor
}

// OpenCodeRawRowIdentifier is a best-effort identifier read directly from a text
// column of a row the bounded read could not decode. It is deliberately not
// validated, so it stays distinct from the validated identity handles and never
// drives a read; it only names a dropped row in a diagnostic.
type OpenCodeRawRowIdentifier struct{ value string }

// NewOpenCodeRawRowIdentifier wraps a raw column value with no validation.
func NewOpenCodeRawRowIdentifier(value string) OpenCodeRawRowIdentifier {
	return OpenCodeRawRowIdentifier{value: value}
}

// String returns the raw identifier text, which may be empty.
func (r OpenCodeRawRowIdentifier) String() string { return r.value }

// IsEmpty reports whether the source column was absent or not text.
func (r OpenCodeRawRowIdentifier) IsEmpty() bool { return r.value == "" }

// OpenCodeOrphanPartDrop records one part row the bounded read could not decode
// into a typed row. PartID and MessageID are the row's best-effort raw
// identifiers, read directly from the text columns even when the rest of the
// row does not decode; either is empty when its column is not text. Reason
// explains why the row was dropped. The read always drops such a row rather
// than failing; the caller decides whether the drop is tolerable, because a
// part whose message is present is a session failure, not an orphan.
type OpenCodeOrphanPartDrop struct {
	PartID    OpenCodeRawRowIdentifier
	MessageID OpenCodeRawRowIdentifier
	Reason    string
}

// OpenCodeLegacyPartPage is a detached bounded part page. Dropped is populated
// only by the orphan read and names orphan rows that could not be decoded.
type OpenCodeLegacyPartPage struct {
	Parts   []OpenCodeLegacyPartRow
	Dropped []OpenCodeOrphanPartDrop
	Next    *OpenCodeLegacyPartCursor
}

// OpenCodeSQLiteSourcePath is an absolute path to an OpenCode-owned SQLite
// database. Construct paths with NewOpenCodeSQLiteSourcePath so the source
// opener never interprets a caller-supplied SQLite URI.
type OpenCodeSQLiteSourcePath struct {
	path string
}

// NewOpenCodeSQLiteSourcePath validates an OpenCode SQLite source path without
// opening or otherwise inspecting the source.
func NewOpenCodeSQLiteSourcePath(path string) (OpenCodeSQLiteSourcePath, error) {
	if path == "" {
		return OpenCodeSQLiteSourcePath{}, fmt.Errorf("validate OpenCode SQLite source path failed before opening the source: the path is empty, so no database location can be identified; supply an absolute path to a synthetic or explicitly selected OpenCode database")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return OpenCodeSQLiteSourcePath{}, fmt.Errorf("validate OpenCode SQLite source path %q failed before opening the source: the path contains a NUL byte and cannot name a filesystem entry; no database was accessed; supply an absolute filesystem path without control bytes", path)
	}
	if !filepath.IsAbs(path) {
		return OpenCodeSQLiteSourcePath{}, fmt.Errorf("validate OpenCode SQLite source path %q failed before opening the source: relative paths depend on process state and could select the wrong database; no database was accessed; resolve the path to an absolute location first", path)
	}

	return OpenCodeSQLiteSourcePath{path: filepath.Clean(path)}, nil
}

// String returns the validated filesystem path. It is not a SQLite URI.
func (p OpenCodeSQLiteSourcePath) String() string {
	return p.path
}

// OpenCodeSQLiteDeadlineClock creates bounded operation contexts. Tests can
// provide a controlled implementation; production uses the system clock.
type OpenCodeSQLiteDeadlineClock interface {
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// OpenCodeSQLiteSourceOptions contains validated source timing policy.
// Construct options with DefaultOpenCodeSQLiteSourceOptions or
// NewOpenCodeSQLiteSourceOptions.
type OpenCodeSQLiteSourceOptions struct {
	busyTimeout            time.Duration
	queryTimeout           time.Duration
	clock                  OpenCodeSQLiteDeadlineClock
	cancellationCheckpoint openCodeSQLiteCancellationCheckpoint
	openConnection         openCodeSQLiteConnectionOpener
}

// openCodeSQLiteConnectionOpener remains package-private so production callers
// cannot replace the restrictive source connection with an arbitrary executor.
// It exists solely to prove the one-connection lifecycle against the same
// production opener path used by Catalog and page reads.
type openCodeSQLiteConnectionOpener func(string, ...sqlite.OpenFlags) (*sqlite.Conn, error)

type openCodeSQLiteCancellationCheckpoint interface {
	AfterPendingRow(context.Context, openCodeCurrentPendingPageState) error
}

type openCodeCurrentPendingPageState struct {
	row   OpenCodeCurrentMessageRow
	count int
}

type contextOpenCodeSQLiteCancellationCheckpoint struct{}

func (contextOpenCodeSQLiteCancellationCheckpoint) AfterPendingRow(ctx context.Context, _ openCodeCurrentPendingPageState) error {
	return ctx.Err()
}

// DefaultOpenCodeSQLiteSourceOptions returns the bounded production policy.
func DefaultOpenCodeSQLiteSourceOptions() OpenCodeSQLiteSourceOptions {
	return OpenCodeSQLiteSourceOptions{
		busyTimeout:            defaultOpenCodeSQLiteBusyTimeout,
		queryTimeout:           defaultOpenCodeSQLiteQueryTimeout,
		clock:                  systemOpenCodeSQLiteDeadlineClock{},
		cancellationCheckpoint: contextOpenCodeSQLiteCancellationCheckpoint{},
		openConnection:         sqlite.OpenConn,
	}
}

// NewOpenCodeSQLiteSourceOptions validates an explicit timing policy.
func NewOpenCodeSQLiteSourceOptions(
	busyTimeout time.Duration,
	queryTimeout time.Duration,
	clock OpenCodeSQLiteDeadlineClock,
) (OpenCodeSQLiteSourceOptions, error) {
	options := OpenCodeSQLiteSourceOptions{
		busyTimeout:            busyTimeout,
		queryTimeout:           queryTimeout,
		clock:                  clock,
		cancellationCheckpoint: contextOpenCodeSQLiteCancellationCheckpoint{},
		openConnection:         sqlite.OpenConn,
	}
	if err := options.validate(); err != nil {
		return OpenCodeSQLiteSourceOptions{}, err
	}
	return options, nil
}

func (o OpenCodeSQLiteSourceOptions) validate() error {
	if o.busyTimeout <= 0 {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: busy timeout %s is not positive, so lock waits would be unbounded or disabled; use a positive timeout no longer than the query timeout", o.busyTimeout)
	}
	if o.queryTimeout <= 0 {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: query timeout %s is not positive, so source reads would not have a policy deadline; use a positive bounded timeout", o.queryTimeout)
	}
	if o.busyTimeout > o.queryTimeout {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: busy timeout %s exceeds query timeout %s, so lock waiting could outlive the read operation; reduce the busy timeout to the query timeout or less", o.busyTimeout, o.queryTimeout)
	}
	if o.clock == nil {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: deadline clock is nil, so bounded reads cannot create cancellation contexts; provide a clock or use DefaultOpenCodeSQLiteSourceOptions")
	}
	if o.cancellationCheckpoint == nil {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: cancellation checkpoint is nil, so a context ending after row decode could escape the atomic-page lifecycle; use a source options constructor with the default context checkpoint")
	}
	if o.openConnection == nil {
		return fmt.Errorf("validate OpenCode SQLite source options failed before opening the source: connection opener is nil, so the single restrictive source lifecycle cannot be created; use DefaultOpenCodeSQLiteSourceOptions or NewOpenCodeSQLiteSourceOptions")
	}
	return nil
}

type systemOpenCodeSQLiteDeadlineClock struct{}

func (systemOpenCodeSQLiteDeadlineClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// OpenCodeSQLiteSource exposes bounded, detached catalog and transcript
// pages plus bounded cleanup. It deliberately provides no raw connection, SQL,
// arguments, transactions, query kinds, table names, writable surfaces, or
// callbacks.
type OpenCodeSQLiteSource interface {
	Catalog(context.Context) (OpenCodeSchemaEvidence, error)
	CurrentSessionIDs(context.Context, OpenCodeCurrentSessionPageRequest) (OpenCodeCurrentSessionPage, error)
	LegacySessionIDs(context.Context, OpenCodeLegacySessionPageRequest) (OpenCodeLegacySessionPage, error)
	CurrentFreshnessBySession(context.Context) (map[string]time.Time, error)
	LegacyFreshnessBySession(context.Context) (map[string]time.Time, error)
	LegacyMessages(context.Context, OpenCodeLegacyMessagePageRequest) (OpenCodeLegacyMessagePage, error)
	LegacySessionParts(context.Context, OpenCodeLegacySessionPartPageRequest) (OpenCodeLegacyPartPage, error)
	SessionRecords(context.Context, OpenCodeSessionRecordPageRequest) (OpenCodeSessionRecordPage, error)
	CurrentMessages(context.Context, OpenCodeCurrentPageRequest) (OpenCodeCurrentPage, error)
	Close(context.Context) error
}
