package store

import (
	"context"
	"fmt"
	"reflect"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// IndexFormat persists a concrete representation using the caller's connection
// and savepoint. Write returns its faithful canonical relational projection;
// Store alone replaces that projection, repairs anchors, and commits stamps.
// Implementations must not obtain another connection or commit independently.
type IndexFormat interface {
	Version() int
	Validate(indexformat.Result) error
	Write(context.Context, *sqlite.Conn, schema.SessionID, indexformat.Result) ([]schema.SessionEntry, error)
	Delete(context.Context, *sqlite.Conn, schema.SessionID) error
}

type relationalIndexFormat struct{}

var _ IndexFormat = relationalIndexFormat{}

func (relationalIndexFormat) Version() int { return 1 }

func (relationalIndexFormat) Validate(result indexformat.Result) error {
	if _, ok := result.(indexformat.V1); !ok {
		return fmt.Errorf("index format 1 requires an indexformat.V1 result, got %T; no index was replaced; use the matching concrete indexer result", result)
	}
	return nil
}

func (relationalIndexFormat) Write(_ context.Context, _ *sqlite.Conn, _ schema.SessionID, result indexformat.Result) ([]schema.SessionEntry, error) {
	value, ok := result.(indexformat.V1)
	if !ok {
		return nil, relationalIndexFormat{}.Validate(result)
	}
	// V1 is the canonical representation itself. The common writer stores these
	// rows directly, without a second payload table or an extra replacement.
	return value.Entries, nil
}

func (relationalIndexFormat) Delete(context.Context, *sqlite.Conn, schema.SessionID) error {
	// Canonical rows are replaced by the common writer, with annotation repair.
	return nil
}

// WithIndexFormats adds concrete format support to one Store instance. Built-in
// V1 support is always present. Duplicate registrations fail before opening the
// database, so tests and callers cannot silently replace another handler.
func WithIndexFormats(formats ...IndexFormat) OpenOption {
	owned := append([]IndexFormat(nil), formats...)
	return func(options *openOptions) { options.indexFormats = append(options.indexFormats, owned...) }
}

func newIndexFormats(extra []IndexFormat) (map[int]IndexFormat, error) {
	formats := map[int]IndexFormat{1: relationalIndexFormat{}}
	for _, format := range extra {
		if nilIndexValue(format) || format.Version() < 1 {
			return nil, fmt.Errorf("store: invalid index format registration before opening database; register a non-nil handler with a positive format version")
		}
		version := format.Version()
		if _, exists := formats[version]; exists {
			return nil, fmt.Errorf("store: duplicate index format %d before opening database; each representation must have exactly one handler", version)
		}
		formats[version] = format
	}
	return formats, nil
}

// SupportsIndexFormat reports local read/write support, not a harness target or
// evidence that any session was processed by this build.
func (s *Store) SupportsIndexFormat(version int) bool {
	_, supported := s.indexFormats[version]
	return supported
}

func nilIndexValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// UnsupportedIndexFormatError prevents an unknown representation from appearing
// as a complete but empty transcript, search, or annotation result.
type UnsupportedIndexFormatError struct {
	SessionID schema.SessionID
	Version   int
}

func (e *UnsupportedIndexFormatError) Error() string {
	if e.Version == 0 {
		return fmt.Sprintf("store: session %s has stored entries but no recorded index format; this build cannot verify their representation, so reads and replacement were refused and the index was preserved; restore valid format evidence or use a compatible Peasant build", e.SessionID)
	}
	return fmt.Sprintf("store: session %s has unsupported index format %d; before reading or replacing its transcript projection, this build refused the operation and preserved its index; use a Peasant build that supports this format", e.SessionID, e.Version)
}

type storedIndexState struct {
	IndexerVersion int
	IndexVersion   *int
	IndexedAt      *int64
	Harness        schema.Harness
}

func readIndexStateOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (*storedIndexState, error) {
	var state *storedIndexState
	err := sqlitex.ExecuteTransient(conn, `SELECT index_version, index_format_version, indexed_at, model_harness FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var harness schema.Harness
			if err := harness.UnmarshalText([]byte(stmt.ColumnText(3))); err != nil || !harness.IsKnown() {
				return fmt.Errorf("stored harness %q is not recognized; restore valid session metadata before indexing", stmt.ColumnText(3))
			}
			state = &storedIndexState{IndexerVersion: stmt.ColumnInt(0), Harness: harness}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				version := stmt.ColumnInt(1)
				state.IndexVersion = &version
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				at := stmt.ColumnInt64(2)
				state.IndexedAt = &at
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read index state for %s before accessing its projection: %w; no replacement was authorized; restore database access and retry", sessionID, err)
	}
	return state, nil
}

func (s *Store) validateIndexWriteOnConn(conn *sqlite.Conn, write ingest.SessionEntryWrite) (IndexFormat, *storedIndexState, error) {
	if nilIndexValue(write.Result) || write.IndexVersion < 1 || write.Result.IndexVersion() != write.IndexVersion {
		return nil, nil, fmt.Errorf("store: index result for session %s does not match declared format %d; refused before replacement; provide one concrete result matching the indexer's declared output", write.SessionID, write.IndexVersion)
	}
	format, supported := s.indexFormats[write.IndexVersion]
	if !supported {
		return nil, nil, &UnsupportedIndexFormatError{SessionID: write.SessionID, Version: write.IndexVersion}
	}
	if err := format.Validate(write.Result); err != nil {
		return nil, nil, fmt.Errorf("store: validate index result for %s before replacement: %w", write.SessionID, err)
	}
	state, err := readIndexStateOnConn(conn, write.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if state == nil {
		return nil, nil, fmt.Errorf("store: cannot index session %s before its metadata is stored; import the session and retry", write.SessionID)
	}
	if state.IndexVersion == nil {
		hasEntries := false
		if err := sqlitex.ExecuteTransient(conn, sqlSessionEntriesExist, &sqlitex.ExecOptions{Args: []any{string(write.SessionID)}, ResultFunc: func(*sqlite.Stmt) error { hasEntries = true; return nil }}); err != nil {
			return nil, nil, fmt.Errorf("store: verify absent index format for %s before replacement: %w; existing entries were preserved; restore database access and retry", write.SessionID, err)
		}
		if hasEntries {
			return nil, nil, &UnsupportedIndexFormatError{SessionID: write.SessionID}
		}
	}
	if state.IndexVersion != nil {
		if !s.SupportsIndexFormat(*state.IndexVersion) {
			return nil, nil, &UnsupportedIndexFormatError{SessionID: write.SessionID, Version: *state.IndexVersion}
		}
		if *state.IndexVersion > write.IndexVersion {
			return nil, nil, fmt.Errorf("store: session %s has index format %d, newer than output format %d; replacement was refused, including forced indexing; use a compatible newer indexer", write.SessionID, *state.IndexVersion, write.IndexVersion)
		}
	}
	producer := write.IndexerVersion
	if producer == 0 {
		// Legacy entry-only callers do not claim a parser run. The current
		// harness target is only a safety ceiling, never a provenance stamp.
		target, registered := ingest.HarvesterVersionRegistry[state.Harness]
		if !registered {
			return nil, nil, fmt.Errorf("store: session %s harness %q has no current indexer; entry-only replacement was refused; register a compatible indexer before retrying", write.SessionID, state.Harness)
		}
		producer = target.IndexerVersion
	}
	if producer < 1 || state.IndexerVersion > producer {
		return nil, nil, fmt.Errorf("store: session %s was indexed by revision %d, newer than this writer's revision %d; use a newer Peasant build to refresh it without losing parser output", write.SessionID, state.IndexerVersion, producer)
	}
	return format, state, nil
}
