package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// This file owns the runtime-clock assertion. The opening-interval check has
// exactly one execution owner: assertFreshClockCellsWithinOpenInterval opens a
// real fresh store once and calls verifyClockCellsWithinInterval, and the
// clock-interval case row invokes that helper. TestBaselineClockCells covers the
// orthogonal half (the emitted artifact carries runtime expressions) without
// opening a second fresh database for the same invariant.

// verifyProductionClockCellSet asserts the fixture's clock-cell set and the
// serializer's own enumeration name the same cells, in both directions, so a
// new seeded clock cell fails until the fixture is extended.
func verifyProductionClockCellSet(cells []baselineClockCellFixture) error {
	want := make(map[string]struct{})
	for _, spec := range enumeratedClockCells() {
		for _, key := range spec.Keys {
			want[clockCellKey(spec.Table, spec.KeyColumn, key, spec.ClockColumn)] = struct{}{}
		}
	}
	got := make(map[string]struct{}, len(cells))
	for _, cell := range cells {
		got[clockCellKey(cell.Table, cell.KeyColumn, cell.Key, cell.ClockColumn)] = struct{}{}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			return fmt.Errorf("the serializer enumerates clock cell %s but testdata/baseline_open_cases.yaml does not; extend the fixture in the same change that seeds a new clock cell", key)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			return fmt.Errorf("testdata/baseline_open_cases.yaml names clock cell %s but the serializer does not enumerate it; remove the stale fixture or extend the serializer", key)
		}
	}
	return nil
}

func clockCellKey(table, keyColumn, key, clockColumn string) string {
	return table + "." + clockColumn + "\x00" + keyColumn + "=" + key
}

// baselineInsertRow is one parsed INSERT emitted by the serializer.
type baselineInsertRow struct {
	table  string
	values map[string]string
}

// parseBaselineInsertRows parses the generator's one-row-per-statement INSERT
// format. It understands the exact value shapes the serializer emits: NULL,
// integer, real, quoted text with doubled quotes, X'hex' blobs, and the runtime
// clock expression.
func parseBaselineInsertRows(text string) ([]baselineInsertRow, error) {
	var rows []baselineInsertRow
	for _, rawLine := range strings.Split(text, "\n") {
		if !strings.HasPrefix(rawLine, `INSERT INTO "`) {
			continue
		}
		row, err := parseBaselineInsertLine(rawLine)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseBaselineInsertLine(line string) (baselineInsertRow, error) {
	rest := strings.TrimPrefix(line, "INSERT INTO ")
	if !strings.HasPrefix(rest, `"`) {
		return baselineInsertRow{}, fmt.Errorf("unquoted table in %q", line)
	}
	tableEnd := strings.Index(rest[1:], `"`)
	if tableEnd < 0 {
		return baselineInsertRow{}, fmt.Errorf("unterminated table identifier in %q", line)
	}
	table := rest[1 : 1+tableEnd]
	rest = rest[2+tableEnd:]

	open := strings.Index(rest, "(")
	closeColumns := strings.Index(rest, ")")
	if open < 0 || closeColumns < 0 || closeColumns < open {
		return baselineInsertRow{}, fmt.Errorf("malformed column list in %q", line)
	}
	columnText := rest[open+1 : closeColumns]
	afterColumns := strings.TrimSpace(rest[closeColumns+1:])
	if !strings.HasPrefix(afterColumns, "VALUES ") {
		return baselineInsertRow{}, fmt.Errorf("missing VALUES in %q", line)
	}
	valueText := strings.TrimSpace(strings.TrimPrefix(afterColumns, "VALUES "))
	if !strings.HasPrefix(valueText, "(") || !strings.HasSuffix(valueText, ");") {
		return baselineInsertRow{}, fmt.Errorf("malformed value tuple in %q", line)
	}
	valueText = valueText[1 : len(valueText)-2]

	columns := splitTopLevel(columnText)
	values := splitTopLevel(valueText)
	if len(columns) != len(values) {
		return baselineInsertRow{}, fmt.Errorf("column/value arity mismatch in %q", line)
	}
	row := baselineInsertRow{table: table, values: make(map[string]string, len(columns))}
	for i, column := range columns {
		name := strings.Trim(strings.TrimSpace(column), `"`)
		row.values[name] = strings.TrimSpace(values[i])
	}
	return row, nil
}

// splitTopLevel splits on commas that are not inside a parenthesized expression
// or a single-quoted string.
func splitTopLevel(text string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inQuote {
			if c == '\'' {
				if i+1 < len(text) && text[i+1] == '\'' {
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, text[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, text[start:])
}

// verifyClockCellExpressions asserts every enumerated clock cell's emitted row
// carries the runtime expression, not an integer literal.
func verifyClockCellExpressions(sqlText string, cells []baselineClockCellFixture) error {
	rows, err := parseBaselineInsertRows(sqlText)
	if err != nil {
		return err
	}
	byKey := make(map[string]baselineInsertRow)
	for _, row := range rows {
		for _, cell := range cells {
			if cell.Table != row.table {
				continue
			}
			if row.values[cell.KeyColumn] == quoteText(cell.Key) {
				byKey[clockCellKey(cell.Table, cell.KeyColumn, cell.Key, cell.ClockColumn)] = row
			}
		}
	}
	for _, cell := range cells {
		row, ok := byKey[clockCellKey(cell.Table, cell.KeyColumn, cell.Key, cell.ClockColumn)]
		if !ok {
			return fmt.Errorf("clock cell %s (%s.%s=%q) has no emitted INSERT row", cell.Name, cell.Table, cell.KeyColumn, cell.Key)
		}
		got, ok := row.values[cell.ClockColumn]
		if !ok {
			return fmt.Errorf("clock cell %s: the emitted row omits column %q", cell.Name, cell.ClockColumn)
		}
		if got != clockCellSQLExpression {
			return fmt.Errorf("clock cell %s emitted %q; want the runtime expression %q, never an integer literal", cell.Name, got, clockCellSQLExpression)
		}
	}
	return nil
}

// verifyClockCellsWithinInterval reads every enumerated clock cell raw (before
// any normalization) and asserts it lies within [startSec-1, endSec+1] at
// SQLite second precision.
func verifyClockCellsWithinInterval(conn *sqlite.Conn, cells []baselineClockCellFixture, startSec, endSec int64) error {
	for _, cell := range cells {
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", quoteIdentifier(cell.ClockColumn), quoteIdentifier(cell.Table), quoteIdentifier(cell.KeyColumn))
		var raw int64
		found := false
		err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: []any{cell.Key},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnType(0) != sqlite.TypeInteger {
					return fmt.Errorf("clock cell %s has storage class %s; install-time clock cells must be integer milliseconds", cell.Name, stmt.ColumnType(0))
				}
				raw = stmt.ColumnInt64(0)
				found = true
				return nil
			},
		})
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("clock cell %s (%s.%s=%q) is missing", cell.Name, cell.Table, cell.KeyColumn, cell.Key)
		}
		second := raw / 1000
		if second < startSec-1 || second > endSec+1 {
			return fmt.Errorf("clock cell %s holds %d ms (%d s), outside the open interval [%d, %d] s (with one second of boundary slack)", cell.Name, raw, second, startSec-1, endSec+1)
		}
	}
	return nil
}

// assertFreshClockCellsWithinOpenInterval is the single execution owner of the
// fresh-open interval assertion. It opens one real fresh store and reads the
// cells raw.
func assertFreshClockCellsWithinOpenInterval(t *testing.T, cells []baselineClockCellFixture) {
	t.Helper()
	startSec := time.Now().Unix()
	path := filepath.Join(t.TempDir(), "clock-interval.db")
	s := openFreshStore(t, path)
	endSec := time.Now().Unix()
	conn, err := s.Pool().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection from fresh clock store: %v", err)
	}
	defer s.Pool().Put(conn)
	if err := verifyClockCellsWithinInterval(conn, cells, startSec, endSec); err != nil {
		t.Fatal(err)
	}
}

// TestBaselineClockCells asserts the fixture and serializer agree on the clock
// cell set, and that the committed and freshly generated artifacts carry the
// runtime expression for every enumerated cell. It intentionally does not open
// a fresh database for the interval assertion; the clock-interval case owns
// that.
func TestBaselineClockCells(t *testing.T) {
	fixtures, err := LoadBaselineOpenCaseFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBaselineOpenCaseFixtures(fixtures); err != nil {
		t.Fatal(err)
	}
	if err := verifyProductionClockCellSet(fixtures.ClockCells.Cells); err != nil {
		t.Fatal(err)
	}
	if err := verifyClockCellExpressions(baselineSchemaSQL, fixtures.ClockCells.Cells); err != nil {
		t.Fatalf("committed baseline artifact: %v", err)
	}
	fresh, err := GenerateSchemaBaselineSQL()
	if err != nil {
		t.Fatalf("GenerateSchemaBaselineSQL: %v", err)
	}
	if err := verifyClockCellExpressions(string(fresh), fixtures.ClockCells.Cells); err != nil {
		t.Fatalf("freshly generated artifact: %v", err)
	}
}

// TestBaselineClockCellMutations demonstrates that replacing an emitted runtime
// expression with 0 or with a frozen literal fails both clock checks even
// though the pin and independent-build gates are satisfied.
func TestBaselineClockCellMutations(t *testing.T) {
	fixtures, err := LoadBaselineOpenCaseFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBaselineOpenCaseFixtures(fixtures); err != nil {
		t.Fatal(err)
	}
	cells := fixtures.ClockCells.Cells
	artifact, err := GenerateSchemaBaselineSQL()
	if err != nil {
		t.Fatalf("GenerateSchemaBaselineSQL: %v", err)
	}
	if err := verifyClockCellExpressions(string(artifact), cells); err != nil {
		t.Fatalf("unmutated artifact must pass the shape check: %v", err)
	}
	for _, mutation := range []struct {
		name        string
		replacement string
	}{
		{name: "zero", replacement: "0"},
		{name: "frozen-literal", replacement: "1000000000000"},
	} {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			mutated := strings.ReplaceAll(string(artifact), clockCellSQLExpression, mutation.replacement)
			if mutated == string(artifact) {
				t.Fatal("the mutation changed nothing; the artifact no longer carries the runtime expression")
			}
			if err := verifyClockCellExpressions(mutated, cells); err == nil {
				t.Fatalf("the shape check accepted an artifact whose clock cells were replaced with %s", mutation.name)
			}
			path := filepath.Join(t.TempDir(), "mutated.db")
			conn, err := sqlite.OpenConn(path, 0)
			if err != nil {
				t.Fatalf("open scratch database: %v", err)
			}
			defer conn.Close()
			if err := sqlitex.ExecuteScript(conn, mutated, nil); err != nil {
				t.Fatalf("apply mutated artifact: %v", err)
			}
			startSec := time.Now().Unix()
			if err := verifyClockCellsWithinInterval(conn, cells, startSec, time.Now().Unix()); err == nil {
				t.Fatalf("the interval check accepted a database whose clock cells were replaced with %s", mutation.name)
			}
		})
	}
}
