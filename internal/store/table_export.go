package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// ExportedTable is one table of a store as the parity harness sees it: the
// declared column names in declaration order, and every row as canonical,
// storage-class-tagged cell tokens. Row order is deliberately unspecified: a
// comparison sorts the rows and compares multisets, so no primary key (and no
// created order) is needed, and a table without a natural key compares
// correctly.
//
// The cell token is the same one canonicalSchemaDump uses (canonicalValue), so
// a value that compares equal here compares equal to the baseline dump: NULL,
// INTEGER, REAL, TEXT and BLOB cannot be confused, and a whole-valued REAL
// never compares equal to an INTEGER.
type ExportedTable struct {
	Name    string
	Columns []string
	Rows    [][]string
}

// ExportTableRows reads every ordinary table (sqlite_master type 'table',
// excluding SQLite's own sqlite_* naming) into ExportedTable values. FTS5
// shadow tables are ordinary tables and are included; FTS5 virtual tables are
// not materialized rows and are skipped by the table list.
//
// The caller owns comparison policy: which columns are volatile, which tables
// are exempt, and which rows a case expects. The export computes none of that,
// so the store package never learns a fixture's exemption set.
//
// A nil store is refused: an export of nothing is not an empty store.
func ExportTableRows(ctx context.Context, s *Store) ([]ExportedTable, error) {
	if s == nil {
		return nil, fmt.Errorf("store: ExportTableRows was given a nil store; there is nothing to export, and an empty result would look like an empty database; where: internal/store/table_export.go; when: before any table was read; impact: no rows are returned; fix: pass the Store the run opened or wrote")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: ExportTableRows was cancelled before reading any table: %w; where: internal/store/table_export.go; when: taking a connection; impact: no rows are returned; fix: retry with a live context", err)
	}
	names, err := AllTableNamesFromDB(s)
	if err != nil {
		return nil, fmt.Errorf("store: ExportTableRows could not list the database's tables: %w; where: internal/store/table_export.go; when: reading sqlite_master; impact: no rows are returned; fix: confirm the store is open and not closed", err)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: ExportTableRows could not take a connection: %w; where: internal/store/table_export.go; when: before reading any table; impact: no rows are returned; fix: confirm the store is open and the pool has a free connection", err)
	}
	defer s.pool.Put(conn)

	tables := make([]ExportedTable, 0, len(names))
	for _, name := range names {
		table, err := exportTable(ctx, conn, name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, nil
}

// exportTable reads one table's columns and rows.
func exportTable(ctx context.Context, conn *sqlite.Conn, name string) (ExportedTable, error) {
	if err := ctx.Err(); err != nil {
		return ExportedTable{}, fmt.Errorf("store: exporting table %q was cancelled: %w", name, err)
	}
	columns, err := declaredColumnNames(conn, name)
	if err != nil {
		return ExportedTable{}, err
	}
	table := ExportedTable{Name: name, Columns: columns}
	err = sqlitex.ExecuteTransient(conn,
		fmt.Sprintf("SELECT * FROM %s", quoteIdentifier(name)),
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				values := readRow(stmt)
				if len(values) != len(columns) {
					return fmt.Errorf("store: table %q returned %d columns but declares %d; a SELECT * that does not match the declared columns cannot be compared", name, len(values), len(columns))
				}
				row := make([]string, len(values))
				for i, value := range values {
					row[i] = canonicalValue(value)
				}
				table.Rows = append(table.Rows, row)
				return nil
			},
		})
	if err != nil {
		return ExportedTable{}, fmt.Errorf("store: ExportTableRows could not read table %q: %w; where: internal/store/table_export.go; when: reading every row; impact: the table is missing from the comparison, which would hide a difference; fix: confirm the table is readable and retry", name, err)
	}
	return table, nil
}

// declaredColumnNames returns a table's declared column names in declaration
// order.
func declaredColumnNames(conn *sqlite.Conn, table string) ([]string, error) {
	var columns []string
	err := sqlitex.ExecuteTransient(conn,
		`SELECT name FROM pragma_table_info(?)`,
		&sqlitex.ExecOptions{
			Args: []any{table},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				columns = append(columns, stmt.ColumnText(0))
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("store: ExportTableRows could not read the columns of table %q: %w; where: internal/store/table_export.go; when: after listing the tables; impact: the table cannot be normalized; fix: confirm pragma_table_info accepts the name", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("store: ExportTableRows found table %q with no declared columns; a table that names no column cannot be normalized; where: internal/store/table_export.go; when: reading pragma_table_info; impact: the table is missing from the comparison; fix: check for a corrupt database", table)
	}
	return columns, nil
}

// NormalizedRows returns the table's rows with the named volatile columns
// dropped, rendered as one sorted string per row. It is the multiset the parity
// harness compares: two tables are equal exactly when their slices are equal.
//
// A named volatile column that the table does not declare is a loud error, not
// a no-op: a fixture that exempts a renamed or removed column would silently
// stop normalizing anything and could make two different stores compare equal.
func (t ExportedTable) NormalizedRows(volatileColumns []string) ([]string, error) {
	drop, err := t.volatileIndexes(volatileColumns)
	if err != nil {
		return nil, err
	}
	rows := make([]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		kept := make([]string, 0, len(row)-len(drop))
		for i, cell := range row {
			if drop[i] {
				continue
			}
			kept = append(kept, cell)
		}
		rows = append(rows, strings.Join(kept, "\x1f"))
	}
	return sortedStrings(rows), nil
}

func (t ExportedTable) volatileIndexes(volatileColumns []string) ([]bool, error) {
	drop := make([]bool, len(t.Columns))
	for _, name := range volatileColumns {
		found := false
		for i, column := range t.Columns {
			if column == name {
				drop[i] = true
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("store: table %q declares no volatile column %q; a fixture that exempts a missing column normalizes nothing and could make differing stores compare equal; where: the volatile_columns fixture entry for %q; when: normalizing the exported rows; impact: the table's comparison is refused; fix: name an existing column or remove the entry", t.Name, name, t.Name)
		}
	}
	return drop, nil
}

// sortedStrings returns a sorted copy.
func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
