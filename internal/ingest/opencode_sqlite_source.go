package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
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
)

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

// OpenCodeLegacyPartPageRequest requests one bounded page for one message.
type OpenCodeLegacyPartPageRequest struct {
	SessionID OpenCodeLegacySessionID
	MessageID OpenCodeLegacyMessageID
	PageSize  OpenCodeLegacyPageSize
	After     *OpenCodeLegacyPartCursor
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

// OpenCodeLegacyPartPage is a detached bounded part page.
type OpenCodeLegacyPartPage struct {
	Parts []OpenCodeLegacyPartRow
	Next  *OpenCodeLegacyPartCursor
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
	busyTimeout  time.Duration
	queryTimeout time.Duration
	clock        OpenCodeSQLiteDeadlineClock
}

// DefaultOpenCodeSQLiteSourceOptions returns the bounded production policy.
func DefaultOpenCodeSQLiteSourceOptions() OpenCodeSQLiteSourceOptions {
	return OpenCodeSQLiteSourceOptions{
		busyTimeout:  defaultOpenCodeSQLiteBusyTimeout,
		queryTimeout: defaultOpenCodeSQLiteQueryTimeout,
		clock:        systemOpenCodeSQLiteDeadlineClock{},
	}
}

// NewOpenCodeSQLiteSourceOptions validates an explicit timing policy.
func NewOpenCodeSQLiteSourceOptions(
	busyTimeout time.Duration,
	queryTimeout time.Duration,
	clock OpenCodeSQLiteDeadlineClock,
) (OpenCodeSQLiteSourceOptions, error) {
	options := OpenCodeSQLiteSourceOptions{
		busyTimeout:  busyTimeout,
		queryTimeout: queryTimeout,
		clock:        clock,
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
	return nil
}

type systemOpenCodeSQLiteDeadlineClock struct{}

func (systemOpenCodeSQLiteDeadlineClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// OpenCodeSQLiteSource exposes bounded, detached catalog and legacy transcript
// pages plus bounded cleanup. It deliberately provides no raw connection, SQL,
// arguments, transactions, query kinds, table names, writable surfaces, or
// callbacks.
type OpenCodeSQLiteSource interface {
	Catalog(context.Context) (OpenCodeSchemaEvidence, error)
	LegacySessionIDs(context.Context, OpenCodeLegacySessionPageRequest) (OpenCodeLegacySessionPage, error)
	LegacyMessages(context.Context, OpenCodeLegacyMessagePageRequest) (OpenCodeLegacyMessagePage, error)
	LegacyParts(context.Context, OpenCodeLegacyPartPageRequest) (OpenCodeLegacyPartPage, error)
	Close(context.Context) error
}
