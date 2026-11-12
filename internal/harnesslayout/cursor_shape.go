package harnesslayout

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// cursorIdentifierKey matches an object key that names a field. Any other key
// (a file URI, a UUID, a content hash) is data, and the shape records it as
// "*" so that no path, identifier, or content reaches a field path.
func cursorFieldKey(key string) string {
	if !cursorIdentifierKey(key) || cursorHasHexRun(key) {
		return "*"
	}
	return key
}

// cursorIdentifierKey reports whether key is a short identifier segment. It is
// the hand-written form of ^[A-Za-z_$][A-Za-z0-9_$]{0,63}$; the owned-code
// guard forbids stdlib regexp.
func cursorIdentifierKey(key string) bool {
	if len(key) == 0 || len(key) > 64 || !cursorIdentifierStart(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		if !cursorIdentifierPart(key[i]) {
			return false
		}
	}
	return true
}

func cursorIdentifierStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func cursorIdentifierPart(c byte) bool {
	return cursorIdentifierStart(c) || (c >= '0' && c <= '9')
}

// cursorHasHexRun reports whether key contains a long hex or digit run, the
// hand-written form of [0-9a-fA-F]{12,}|[0-9]{6,}.
func cursorHasHexRun(key string) bool {
	hexRun, digitRun := 0, 0
	for i := 0; i < len(key); i++ {
		c := key[i]
		if cursorHex(c) {
			hexRun++
		} else {
			hexRun = 0
		}
		if c >= '0' && c <= '9' {
			digitRun++
		} else {
			digitRun = 0
		}
		if hexRun >= 12 || digitRun >= 6 {
			return true
		}
	}
	return false
}

func cursorHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// cursorFieldPath renders an object key in the saved-report grammar: an
// identifier key stays a dotted segment, and a collapsed key is a quoted
// bracket segment, never a dotted "*".
func cursorFieldPath(path, key string) string {
	if field := cursorFieldKey(key); field != "*" {
		return path + "." + field
	}
	return path + `["*"]`
}

// cursorWalk records a decoded JSON value like ShapeRecorder.AddRecord does,
// but under path and with data-valued object keys collapsed to "*".
func cursorWalk(rec *ShapeRecorder, path string, value any) {
	switch v := value.(type) {
	case map[string]any:
		rec.observe(path, JSONObject)
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			cursorWalk(rec, cursorFieldPath(path, key), v[key])
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
	conn.SetBusyTimeout(sqliteProbeBusyTimeout)
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
// "CASE WHEN length(v) <= sqliteProbeMaxValueBytes THEN v END" so that SQLite
// never hands an oversized value to Go. It returns false for NULL.
func cursorBoundedValue(stmt *sqlite.Stmt, col int) ([]byte, bool) {
	if stmt.ColumnIsNull(col) {
		return nil, false
	}
	buf := make([]byte, stmt.ColumnLen(col))
	stmt.ColumnBytes(col, buf)
	return buf, true
}
