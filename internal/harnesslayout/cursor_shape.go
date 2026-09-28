package harnesslayout

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Cursor keeps its stores in SQLite files that a running Cursor may hold
// open. The probes read at most cursorMaxRows rows per session query and
// decode no value larger than cursorMaxValueBytes.
const (
	cursorMaxRows       = 20000
	cursorMaxValueBytes = 16 << 20
	cursorBusyTimeout   = 5 * time.Second
)

// cursorIdentifierKey matches an object key that names a field. Any other key
// (a file URI, a UUID, a content hash) is data, and the shape records it as
// "*" so that no path, identifier, or content reaches a field path.
var (
	cursorIdentifierKey = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]{0,63}$`)
	cursorHexRun        = regexp.MustCompile(`[0-9a-fA-F]{12,}|[0-9]{6,}`)
)

func cursorFieldKey(key string) string {
	if !cursorIdentifierKey.MatchString(key) || cursorHexRun.MatchString(key) {
		return "*"
	}
	return key
}

// cursorWalk records a decoded JSON value like ShapeRecorder.AddRecord does,
// but under path and with data-valued object keys collapsed to "*".
func cursorWalk(rec *ShapeRecorder, path string, value any) {
	switch v := value.(type) {
	case map[string]any:
		rec.observe(path, JSONObject)
		for key, child := range v {
			cursorWalk(rec, path+"."+cursorFieldKey(key), child)
		}
	case []any:
		rec.observe(path, JSONArray)
		for _, child := range v {
			cursorWalk(rec, path+"[]", child)
		}
	default:
		rec.walk(path, v)
	}
}

// cursorRecord counts one record of kind and records its value under path.
// An empty path counts the record without recording fields.
func cursorRecord(rec *ShapeRecorder, kind, path string, value any) {
	rec.shape.Records++
	if kind != "" {
		rec.shape.Kinds[kind]++
	}
	if path != "" {
		cursorWalk(rec, path, value)
	}
}

// cursorDecode decodes one stored JSON value and counts it as malformed when
// it does not decode.
func cursorDecode(rec *ShapeRecorder, raw []byte) (any, bool) {
	value, err := decodeValue(raw)
	if err != nil {
		rec.shape.Malformed++
		return nil, false
	}
	return value, true
}

// cursorKind returns a record-kind name read from the store, or other when
// the stored value is not a plain identifier.
func cursorKind(raw, other string) string {
	if raw == "" || cursorFieldKey(raw) == "*" {
		return other
	}
	return raw
}

func cursorOpenReadOnly(ctx context.Context, src Source, rel string) (*sqlite.Conn, error) {
	if src.Dir == "" {
		return nil, fmt.Errorf("%s: reading SQLite needs an operating system directory", rel)
	}
	conn, err := sqlite.OpenConn(filepath.Join(src.Dir, filepath.FromSlash(rel)), sqlite.OpenReadOnly)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", rel, err)
	}
	conn.SetBusyTimeout(cursorBusyTimeout)
	conn.SetInterrupt(ctx.Done())
	return conn, nil
}

// cursorShapeTables records the columns of each named table that exists as
// "$.<table>.<column>" and returns the tables it found. It reads the schema
// only, never a row.
func cursorShapeTables(conn *sqlite.Conn, rec *ShapeRecorder, tables ...string) (map[string]bool, error) {
	found := map[string]bool{}
	for _, table := range tables {
		err := sqlitex.ExecuteTransient(conn, `SELECT name, type FROM pragma_table_info(?)`,
			&sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(stmt *sqlite.Stmt) error {
				found[table] = true
				rec.AddField("$."+table+"."+stmt.ColumnText(0), cursorColumnType(stmt.ColumnText(1)))
				return nil
			}})
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// cursorColumnType maps a declared column type to a JSON type by SQLite
// affinity. TEXT, BLOB, and untyped columns are recorded as strings.
func cursorColumnType(declared string) JSONType {
	upper := strings.ToUpper(declared)
	for _, numeric := range []string{"INT", "REAL", "FLOA", "DOUB", "NUM", "DEC"} {
		if strings.Contains(upper, numeric) {
			return JSONNumber
		}
	}
	return JSONString
}

// cursorBoundedValue reads column col, selected as
// "CASE WHEN length(v) <= cursorMaxValueBytes THEN v END" so that SQLite
// never hands an oversized value to Go. It returns false for NULL.
func cursorBoundedValue(stmt *sqlite.Stmt, col int) ([]byte, bool) {
	if stmt.ColumnIsNull(col) {
		return nil, false
	}
	buf := make([]byte, stmt.ColumnLen(col))
	stmt.ColumnBytes(col, buf)
	return buf, true
}
