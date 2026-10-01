package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// This file owns the canonical serialization contract shared by the baseline
// schema generator (row -> SQL INSERT) and the equivalence dump (row ->
// canonical comparison line). One ordering helper and one generated-value
// policy serve both, so the artifact and the equality gate can never drift
// apart.
//
// Policy summary:
//   - Objects are compared and emitted in a stable (type, name) order.
//   - Rows are ordered by rowid for ordinary tables and by the declared
//     primary-key columns, in key order, for WITHOUT ROWID tables.
//   - Identifiers are always double-quoted.
//   - Values are encoded with a type tag so NULL, INTEGER, REAL, TEXT, and BLOB
//     round-trip unambiguously.
//   - A narrowly enumerated set of seed rows gets nondeterministic identities
//     from the shipped chain (randomblob UUIDs in the V25/V36/V39 seeds). Their
//     identity is canonicalized by natural key: the artifact emits a
//     deterministic UUID derived from (table, natural key), and every
//     referencing cell is rewritten to the same literal; the comparison maps
//     both databases' identities to a single natural-key token.
//   - A narrowly enumerated set of seed clock cells (created_at on the seeded
//     annotation_types and annotators rows) stays a runtime expression in the
//     artifact and is normalized to one token in the comparison.

// generatedIdentitySpec enumerates a base-schema row whose identity the
// shipped chain generates nondeterministically, keyed by its natural key.
type generatedIdentitySpec struct {
	Table     string
	IDColumn  string
	KeyColumn string
	Keys      []string
}

// clockCellSpec enumerates the seeded rows whose clock column is written at
// install time with a wall-clock expression.
type clockCellSpec struct {
	Table       string
	KeyColumn   string
	ClockColumn string
	Keys        []string
}

// enumeratedGeneratedIdentities is the complete set of chain-generated seed
// identities. Every entry must resolve to exactly one live row in the chain
// database; the identity fixture in testdata/baseline_open_cases.yaml mirrors
// this list and is cross-checked against it by TestBaselineClockCells.
func enumeratedGeneratedIdentities() []generatedIdentitySpec {
	return []generatedIdentitySpec{
		{Table: "annotation_classes", IDColumn: "id", KeyColumn: "class", Keys: []string{"research"}},
		{Table: "annotation_families", IDColumn: "id", KeyColumn: "family", Keys: []string{"episode_friction"}},
		{Table: "annotation_types", IDColumn: "id", KeyColumn: "type_id", Keys: []string{
			"research.friction_episode",
			"user.custom_label",
			"quality.turn_outcome",
			"quality.turn_flag",
		}},
	}
}

// enumeratedClockCells is the complete set of seeded clock cells.
func enumeratedClockCells() []clockCellSpec {
	return []clockCellSpec{
		{Table: "annotation_types", KeyColumn: "type_id", ClockColumn: "created_at", Keys: []string{
			"quality.session_approval",
			"quality.session_outcome",
			"quality.user_frustration",
			"metadata.session_scope",
			"quality.frustration_signal",
			"quality.resolution_evidence",
			"quality.annotation_approval",
			"research.friction_episode",
			"user.custom_label",
			"quality.turn_outcome",
			"quality.turn_flag",
		}},
		{Table: "annotators", KeyColumn: "name", ClockColumn: "created_at", Keys: []string{
			"outcome-classifier",
			"frustration-classifier",
			"scope-classifier",
			"human-web",
			"frustration-signal-classifier",
			"resolution-evidence-classifier",
		}},
	}
}

// clockCellToken is the comparison placeholder for an install-time clock value.
const clockCellToken = "<now>"

// clockCellSQLExpression is the runtime expression the artifact carries for
// every enumerated clock cell. It matches the expression the shipped V13 seed
// uses, so a baseline install stamps the same wall-clock millisecond value the
// chain would.
const clockCellSQLExpression = "CAST(strftime('%s','now') AS INTEGER) * 1000"

// generatedIdentityToken is the comparison placeholder for a chain-generated
// identity, derived from the row's natural key.
func generatedIdentityToken(table, key string) string {
	return "<generated:" + table + ":" + key + ">"
}

// generatedIdentityLiteral is the deterministic identity the artifact emits for
// an enumerated generated row. It is a version-5-shaped UUID derived from
// (table, natural key), so every fresh install writes the same well-formed
// identity and two independent chain builds serialize byte-identically.
func generatedIdentityLiteral(table, key string) string {
	sum := sha256.Sum256([]byte("peasant.baseline.generated\x00" + table + "\x00" + key))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// tableMeta is the classification pragma_table_list reports for one object.
type tableMeta struct {
	Type       string // table | view | virtual | shadow
	WithoutRow bool
}

// loadTableMeta classifies every table-like object in the database.
func loadTableMeta(conn *sqlite.Conn) (map[string]tableMeta, error) {
	meta := make(map[string]tableMeta)
	err := sqlitex.ExecuteTransient(conn,
		`SELECT name, type, wr FROM pragma_table_list`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				meta[stmt.ColumnText(0)] = tableMeta{Type: stmt.ColumnText(1), WithoutRow: stmt.ColumnBool(2)}
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("store: read table classification: %w; the schema dump needs pragma_table_list to tell shadow and WITHOUT ROWID tables apart", err)
	}
	return meta, nil
}

// tableColumn is one declared column of a table.
type tableColumn struct {
	Name string
	PK   int
}

// loadTableColumns returns the declared columns of table in declaration order.
func loadTableColumns(conn *sqlite.Conn, table string) ([]tableColumn, error) {
	var columns []tableColumn
	err := sqlitex.ExecuteTransient(conn,
		`SELECT name, pk FROM pragma_table_info(?)`,
		&sqlitex.ExecOptions{
			Args: []any{table},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				columns = append(columns, tableColumn{Name: stmt.ColumnText(0), PK: stmt.ColumnInt(1)})
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("store: read columns of table %q: %w; the schema dump needs declared column names to order and emit rows", table, err)
	}
	return columns, nil
}

// canonicalRowOrder returns the ORDER BY clause that makes a table's row order
// reproducible: primary-key columns in key order for a WITHOUT ROWID table,
// rowid otherwise. Identifiers are double-quoted. It is the single ordering
// helper shared by the generator and the comparison dump.
func canonicalRowOrder(conn *sqlite.Conn, table string, meta tableMeta) (string, error) {
	if !meta.WithoutRow {
		return `ORDER BY "rowid"`, nil
	}
	columns, err := loadTableColumns(conn, table)
	if err != nil {
		return "", err
	}
	var keyed []tableColumn
	for _, column := range columns {
		if column.PK > 0 {
			keyed = append(keyed, column)
		}
	}
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].PK < keyed[j].PK })
	if len(keyed) == 0 {
		return "", fmt.Errorf("store: WITHOUT ROWID table %q declares no primary key; the canonical row order cannot be defined", table)
	}
	quoted := make([]string, len(keyed))
	for i, column := range keyed {
		quoted[i] = quoteIdentifier(column.Name)
	}
	return "ORDER BY " + strings.Join(quoted, ", "), nil
}

// cellValue is one typed column value read from a row.
type cellValue struct {
	Type  sqlite.ColumnType
	Int   int64
	Float float64
	Text  string
	Blob  []byte
}

// readRow materializes every column of the current statement row.
func readRow(stmt *sqlite.Stmt) []cellValue {
	values := make([]cellValue, stmt.ColumnCount())
	for i := range values {
		switch stmt.ColumnType(i) {
		case sqlite.TypeNull:
			values[i] = cellValue{Type: sqlite.TypeNull}
		case sqlite.TypeInteger:
			values[i] = cellValue{Type: sqlite.TypeInteger, Int: stmt.ColumnInt64(i)}
		case sqlite.TypeFloat:
			values[i] = cellValue{Type: sqlite.TypeFloat, Float: stmt.ColumnFloat(i)}
		case sqlite.TypeText:
			values[i] = cellValue{Type: sqlite.TypeText, Text: stmt.ColumnText(i)}
		case sqlite.TypeBlob:
			buf := make([]byte, stmt.ColumnLen(i))
			stmt.ColumnBytes(i, buf)
			values[i] = cellValue{Type: sqlite.TypeBlob, Blob: buf}
		}
	}
	return values
}

// quoteIdentifier wraps a SQL identifier in double quotes, doubling any
// embedded quote.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteText renders a SQL text literal, doubling embedded single quotes.
func quoteText(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// sqlLiteral renders a typed value as a SQL literal that round-trips its
// storage class.
func sqlLiteral(value cellValue) string {
	switch value.Type {
	case sqlite.TypeInteger:
		return strconv.FormatInt(value.Int, 10)
	case sqlite.TypeFloat:
		text := strconv.FormatFloat(value.Float, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0" // keep the REAL storage class on a whole-valued float
		}
		return text
	case sqlite.TypeText:
		return quoteText(value.Text)
	case sqlite.TypeBlob:
		return "X'" + hex.EncodeToString(value.Blob) + "'"
	default:
		return "NULL"
	}
}

// canonicalValue renders a typed value as a comparison token that keeps the
// storage class visible.
func canonicalValue(value cellValue) string {
	switch value.Type {
	case sqlite.TypeInteger:
		return "int:" + strconv.FormatInt(value.Int, 10)
	case sqlite.TypeFloat:
		return "real:" + strconv.FormatFloat(value.Float, 'g', -1, 64)
	case sqlite.TypeText:
		return "text:" + strconv.Quote(value.Text)
	case sqlite.TypeBlob:
		return "blob:" + hex.EncodeToString(value.Blob)
	default:
		return "null"
	}
}

// generatedIdentityMap resolves the enumerated generated identities in one
// database. literalByID maps a raw stored identity literal to the deterministic
// artifact literal; idValue maps it to its comparison token.
type generatedIdentityMap struct {
	idValue     map[string]string // raw id literal -> comparison token
	literalByID map[string]string // raw id literal -> artifact literal
}

// isCanonicalUUID reports whether value is a lowercase 8-4-4-4-12 hex UUID.
func isCanonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			c := value[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// resolveGeneratedIdentities looks up every enumerated generated row by natural
// key. It asserts the row exists, that the identity is unique among the
// enumerated keys, and that the stored identity is either the deterministic
// artifact literal (baseline-created database) or a canonical UUID
// (chain-created database).
func resolveGeneratedIdentities(conn *sqlite.Conn) (*generatedIdentityMap, error) {
	resolved := &generatedIdentityMap{
		idValue:     make(map[string]string),
		literalByID: make(map[string]string),
	}
	for _, spec := range enumeratedGeneratedIdentities() {
		for _, key := range spec.Keys {
			literal := generatedIdentityLiteral(spec.Table, key)
			token := generatedIdentityToken(spec.Table, key)
			var id string
			found := false
			err := sqlitex.ExecuteTransient(conn,
				fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, quoteIdentifier(spec.IDColumn), quoteIdentifier(spec.Table), quoteIdentifier(spec.KeyColumn)),
				&sqlitex.ExecOptions{
					Args: []any{key},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						if found {
							return fmt.Errorf("store: generated identity fixture %q.%s=%q matches more than one row; the natural key must be unique", spec.Table, spec.KeyColumn, key)
						}
						id = stmt.ColumnText(0)
						found = true
						return nil
					},
				})
			if err != nil {
				return nil, fmt.Errorf("store: resolve generated identity %s.%s=%q: %w; the enumerated seed row must exist in the schema snapshot", spec.Table, spec.KeyColumn, key, err)
			}
			if !found {
				return nil, fmt.Errorf("store: enumerated generated seed row %s.%s=%q is missing from the schema snapshot; extend testdata/baseline_open_cases.yaml and the serializer together when a migration stops seeding it", spec.Table, spec.KeyColumn, key)
			}
			if id != literal && !isCanonicalUUID(id) {
				return nil, fmt.Errorf("store: generated identity %s.%s=%q has value %q; expected the deterministic baseline UUID or a chain-generated canonical UUID", spec.Table, spec.KeyColumn, key, id)
			}
			if prior, duplicate := resolved.idValue[id]; duplicate && prior != token {
				return nil, fmt.Errorf("store: generated identity value %q is shared by two enumerated rows; generated identities must stay unique", id)
			}
			resolved.idValue[id] = token
			resolved.literalByID[id] = literal
		}
	}
	return resolved, nil
}

// schemaObject is one sqlite_master row.
type schemaObject struct {
	Type string
	Name string
	SQL  string
}

// loadSchemaObjects returns every sqlite_master object in canonical
// (type, name) order.
func loadSchemaObjects(conn *sqlite.Conn) ([]schemaObject, error) {
	var objects []schemaObject
	err := sqlitex.ExecuteTransient(conn,
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				objects = append(objects, schemaObject{Type: stmt.ColumnText(0), Name: stmt.ColumnText(1), SQL: stmt.ColumnText(2)})
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("store: read sqlite_master: %w; the schema dump enumerates every schema object", err)
	}
	return objects, nil
}

// dataTables returns the tables the artifact serializer reads rows from, in
// name order. SQLite-owned tables are excluded: sqlite_sequence's rows arise
// from the replayed AUTOINCREMENT inserts, and sqlite_schema is the catalog
// compared through its objects.
func dataTables(meta map[string]tableMeta) []string {
	var tables []string
	for name, m := range meta {
		if m.Type != "table" && m.Type != "virtual" {
			continue
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		tables = append(tables, name)
	}
	sort.Strings(tables)
	return tables
}

// shadowTables returns the FTS5 shadow tables, in name order.
func shadowTables(meta map[string]tableMeta) []string {
	var tables []string
	for name, m := range meta {
		if m.Type == "shadow" {
			tables = append(tables, name)
		}
	}
	sort.Strings(tables)
	return tables
}

// dumpTables returns the tables the comparison dump reads rows from, in name
// order. Unlike the serializer it includes sqlite_sequence, whose single row
// is part of the compared state; the catalog table itself stays excluded
// because its objects are compared directly.
func dumpTables(meta map[string]tableMeta) []string {
	tables := dataTables(meta)
	if _, ok := meta["sqlite_sequence"]; ok {
		tables = append(tables, "sqlite_sequence")
	}
	sort.Strings(tables)
	return tables
}

// clockKey builds the lookup key for one clock cell.
func clockKey(table, key, column string) string { return table + "\x00" + key + "\x00" + column }

// clockCellsByColumn indexes enumerated clock cells by (table, key, column).
func clockCellsByColumn() map[string]struct{} {
	index := make(map[string]struct{})
	for _, spec := range enumeratedClockCells() {
		for _, key := range spec.Keys {
			index[clockKey(spec.Table, key, spec.ClockColumn)] = struct{}{}
		}
	}
	return index
}

// clockKeyColumnFor returns the natural-key column of a clock-cell table.
func clockKeyColumnFor(table string) string {
	for _, spec := range enumeratedClockCells() {
		if spec.Table == table {
			return spec.KeyColumn
		}
	}
	return ""
}

// validateEnumeratedSeeds asserts every enumerated generated identity and clock
// cell resolves to exactly one row in the database, so completeness gaps fail
// loudly instead of silently dropping a cell.
func validateEnumeratedSeeds(conn *sqlite.Conn) error {
	if _, err := resolveGeneratedIdentities(conn); err != nil {
		return err
	}
	for _, spec := range enumeratedClockCells() {
		for _, key := range spec.Keys {
			count := 0
			err := sqlitex.ExecuteTransient(conn,
				fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, quoteIdentifier(spec.Table), quoteIdentifier(spec.KeyColumn)),
				&sqlitex.ExecOptions{
					Args: []any{key},
					ResultFunc: func(stmt *sqlite.Stmt) error {
						count = stmt.ColumnInt(0)
						return nil
					},
				})
			if err != nil {
				return fmt.Errorf("store: check clock cell %s.%s=%q: %w; the enumerated seed row must exist in the schema snapshot", spec.Table, spec.KeyColumn, key, err)
			}
			if count != 1 {
				return fmt.Errorf("store: enumerated clock cell %s.%s=%q matches %d rows; each clock cell must name exactly one seeded row", spec.Table, spec.KeyColumn, key, count)
			}
		}
	}
	return nil
}

// serializeBaselineSQL renders the chain-built database connection as the
// committed baseline artifact: DDL in tables -> indexes -> views -> triggers
// order, then seed rows, then the FTS5 shadow rows, then the user_version
// stamp. Autoindexes and sqlite_sequence are never emitted; their owners create
// them.
func serializeBaselineSQL(conn *sqlite.Conn) ([]byte, error) {
	if err := validateEnumeratedSeeds(conn); err != nil {
		return nil, err
	}
	meta, err := loadTableMeta(conn)
	if err != nil {
		return nil, err
	}
	identities, err := resolveGeneratedIdentities(conn)
	if err != nil {
		return nil, err
	}
	objects, err := loadSchemaObjects(conn)
	if err != nil {
		return nil, err
	}
	clocks := clockCellsByColumn()

	var b strings.Builder
	b.WriteString("-- Generated by scripts/store-baseline-gen; do not edit by hand.\n")
	b.WriteString("-- Regenerate with: go generate ./internal/store\n")
	b.WriteString("--\n")
	b.WriteString("-- A consolidated head-schema snapshot for brand-new databases. Existing\n")
	b.WriteString("-- databases keep the shipped migration chain as their only upgrade path.\n\n")

	// DDL: tables, then indexes, then views, then triggers.
	for _, group := range []string{"table", "index", "view", "trigger"} {
		for _, object := range objects {
			if object.Type != group || object.SQL == "" {
				continue
			}
			if group == "table" {
				if m, ok := meta[object.Name]; ok && m.Type == "shadow" {
					continue // created by its virtual table
				}
				if strings.HasPrefix(object.Name, "sqlite_") {
					continue // sqlite_sequence and friends are created automatically
				}
			}
			b.WriteString(object.SQL)
			b.WriteString(";\n\n")
		}
	}

	// Seed rows, one INSERT per row, tables in name order.
	for _, table := range dataTables(meta) {
		if meta[table].Type == "virtual" {
			continue // its content is restored through its shadow tables
		}
		columns, err := loadTableColumns(conn, table)
		if err != nil {
			return nil, err
		}
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = quoteIdentifier(column.Name)
		}
		keyColumn := clockKeyColumnFor(table)
		order, err := canonicalRowOrder(conn, table, meta[table])
		if err != nil {
			return nil, err
		}
		err = sqlitex.ExecuteTransient(conn,
			fmt.Sprintf("SELECT * FROM %s %s", quoteIdentifier(table), order),
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					values := readRow(stmt)
					rowKey, hasKey := "", false
					if keyColumn != "" {
						for i, column := range columns {
							if column.Name == keyColumn && i < len(values) && values[i].Type == sqlite.TypeText {
								rowKey, hasKey = values[i].Text, true
								break
							}
						}
					}
					literals := make([]string, len(values))
					for i, value := range values {
						switch {
						case hasKey && clockIndexHas(clocks, table, rowKey, columns[i].Name):
							literals[i] = clockCellSQLExpression
						case value.Type == sqlite.TypeText:
							if literal, ok := identities.literalByID[value.Text]; ok {
								literals[i] = quoteText(literal)
							} else {
								literals[i] = sqlLiteral(value)
							}
						default:
							literals[i] = sqlLiteral(value)
						}
					}
					b.WriteString("INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(literals, ", ") + ");\n")
					return nil
				},
			})
		if err != nil {
			return nil, fmt.Errorf("store: serialize rows of table %q: %w; the baseline artifact must carry every seed row", table, err)
		}
	}

	// FTS5 shadow storage: the virtual table's CREATE pre-populates an empty
	// index, so the chain snapshot's post-rebuild rows are restored explicitly.
	for _, table := range shadowTables(meta) {
		columns, err := loadTableColumns(conn, table)
		if err != nil {
			return nil, err
		}
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = quoteIdentifier(column.Name)
		}
		order, err := canonicalRowOrder(conn, table, meta[table])
		if err != nil {
			return nil, err
		}
		var inserts strings.Builder
		rows := 0
		err = sqlitex.ExecuteTransient(conn,
			fmt.Sprintf("SELECT * FROM %s %s", quoteIdentifier(table), order),
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					values := readRow(stmt)
					literals := make([]string, len(values))
					for i, value := range values {
						literals[i] = sqlLiteral(value)
					}
					rows++
					inserts.WriteString("INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(literals, ", ") + ");\n")
					return nil
				},
			})
		if err != nil {
			return nil, fmt.Errorf("store: serialize shadow rows of table %q: %w; FTS5 shadow state must round-trip", table, err)
		}
		if rows == 0 {
			continue
		}
		b.WriteString("DELETE FROM " + quoteIdentifier(table) + ";\n")
		b.WriteString(inserts.String())
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("PRAGMA user_version = %d;\n", CurrentSchemaVersion()))
	return []byte(b.String()), nil
}

// clockIndexHas reports whether (table, key, column) is an enumerated clock
// cell.
func clockIndexHas(index map[string]struct{}, table, key, column string) bool {
	_, ok := index[clockKey(table, key, column)]
	return ok
}

// canonicalSchemaDump renders one database as the comparison text the
// equivalence gate compares. It covers every sqlite_master object (including
// NULL-SQL autoindexes, sqlite_sequence, and FTS5 shadow tables) and every
// table's rows, plus user_version and application_id, under the generated-value
// policy. The only excluded data is the random _install_salt row; that table's
// schema is still compared.
func canonicalSchemaDump(conn *sqlite.Conn) ([]byte, error) {
	meta, err := loadTableMeta(conn)
	if err != nil {
		return nil, err
	}
	identities, err := resolveGeneratedIdentities(conn)
	if err != nil {
		return nil, err
	}
	objects, err := loadSchemaObjects(conn)
	if err != nil {
		return nil, err
	}
	version, err := readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		return nil, err
	}
	appID, err := readPragmaInt(conn, "PRAGMA application_id")
	if err != nil {
		return nil, err
	}
	clocks := clockCellsByColumn()

	var b strings.Builder
	fmt.Fprintf(&b, "user_version %d\napplication_id %d\n", version, appID)
	for _, object := range objects {
		fmt.Fprintf(&b, "object %s %s %s\n", object.Type, strconv.Quote(object.Name), strconv.Quote(object.SQL))
	}
	for _, table := range dumpTables(meta) {
		columns, err := loadTableColumns(conn, table)
		if err != nil {
			return nil, err
		}
		names := make([]string, len(columns))
		for i, column := range columns {
			names[i] = strconv.Quote(column.Name)
		}
		fmt.Fprintf(&b, "data %s %s\n", strconv.Quote(table), strings.Join(names, " "))
		if table == "_install_salt" {
			continue // random per install; the schema above is still compared
		}
		keyColumn := clockKeyColumnFor(table)
		order, err := canonicalRowOrder(conn, table, meta[table])
		if err != nil {
			return nil, err
		}
		err = sqlitex.ExecuteTransient(conn,
			fmt.Sprintf("SELECT * FROM %s %s", quoteIdentifier(table), order),
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					values := readRow(stmt)
					rowKey, hasKey := "", false
					if keyColumn != "" {
						for i, column := range columns {
							if column.Name == keyColumn && i < len(values) && values[i].Type == sqlite.TypeText {
								rowKey, hasKey = values[i].Text, true
								break
							}
						}
					}
					tokens := make([]string, len(values))
					for i, value := range values {
						switch {
						case hasKey && clockIndexHas(clocks, table, rowKey, columns[i].Name):
							if value.Type != sqlite.TypeInteger {
								return fmt.Errorf("store: clock cell %s.%s=%q.%s has storage class %d; install-time clock cells must be integer milliseconds", table, keyColumn, rowKey, columns[i].Name, value.Type)
							}
							tokens[i] = "clock:" + clockCellToken
						case value.Type == sqlite.TypeText:
							if token, ok := identities.idValue[value.Text]; ok {
								tokens[i] = "generated:" + token
							} else {
								tokens[i] = canonicalValue(value)
							}
						default:
							tokens[i] = canonicalValue(value)
						}
					}
					fmt.Fprintf(&b, "row %s\n", strings.Join(tokens, "\t"))
					return nil
				},
			})
		if err != nil {
			return nil, fmt.Errorf("store: dump rows of table %q: %w; the equivalence gate compares every seed row", table, err)
		}
	}
	return []byte(b.String()), nil
}

// readPragmaInt reads a single integer from a pragma statement.
func readPragmaInt(conn *sqlite.Conn, statement string) (int64, error) {
	var value int64
	if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read %q: %w; the schema dump records the database identity", statement, err)
	}
	return value, nil
}
