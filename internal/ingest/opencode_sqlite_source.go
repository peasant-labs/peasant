package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultOpenCodeSQLiteBusyTimeout  = 250 * time.Millisecond
	defaultOpenCodeSQLiteQueryTimeout = 5 * time.Second
)

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

// OpenCodeSQLiteValueKind identifies a copied SQLite result value.
type OpenCodeSQLiteValueKind uint8

const (
	OpenCodeSQLiteValueNull OpenCodeSQLiteValueKind = iota
	OpenCodeSQLiteValueInteger
	OpenCodeSQLiteValueFloat
	OpenCodeSQLiteValueText
	OpenCodeSQLiteValueBlob
)

// OpenCodeSQLiteValue is a detached result value. Exactly one value field is
// meaningful according to Kind.
type OpenCodeSQLiteValue struct {
	Kind    OpenCodeSQLiteValueKind
	Integer int64
	Float   float64
	Text    string
	Blob    []byte
}

// OpenCodeSQLiteColumn is a named, detached result column.
type OpenCodeSQLiteColumn struct {
	Name  string
	Value OpenCodeSQLiteValue
}

// OpenCodeSQLiteRow is a detached query result row. It does not expose the
// underlying SQLite statement or connection.
type OpenCodeSQLiteRow struct {
	columns []OpenCodeSQLiteColumn
}

// Columns returns an independent copy of the row's columns.
func (r OpenCodeSQLiteRow) Columns() []OpenCodeSQLiteColumn {
	columns := make([]OpenCodeSQLiteColumn, len(r.columns))
	copy(columns, r.columns)
	for i := range columns {
		columns[i].Value.Blob = append([]byte(nil), columns[i].Value.Blob...)
	}
	return columns
}

// OpenCodeSQLiteSource exposes only bounded reads and resource cleanup. It
// deliberately provides no raw connection, transaction, or prepare method.
type OpenCodeSQLiteSource interface {
	Read(context.Context, string, []any, func(OpenCodeSQLiteRow) error) error
	Close() error
}
