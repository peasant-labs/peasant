package store

import (
	"fmt"
	"math"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// upgradeDiskBasis is what a catalog entry's disk factor multiplies: the
// objects the step rewrites. A step that copies a table needs space in
// proportion to that table, not to the database. The set is closed.
type upgradeDiskBasis string

const (
	// upgradeDiskBasisDatabase is the whole database file. A step whose
	// writes are small, or not yet measured against a narrower basis, uses
	// it.
	upgradeDiskBasisDatabase upgradeDiskBasis = "database"
	// upgradeDiskBasisEntryTable is the entry table, session_entries, with
	// its indexes: what a step that copies that table rewrites.
	upgradeDiskBasisEntryTable upgradeDiskBasis = "entry_table"
	// upgradeDiskBasisEntryTableAndSearchIndex is the entry table with its
	// indexes plus the two search indexes rebuilt over the stored text:
	// session_entries_fts over the entry rows and session_search_fts over
	// the entry rows and the entry bodies. Schema 75 rewrites exactly these.
	upgradeDiskBasisEntryTableAndSearchIndex upgradeDiskBasis = "entry_table_and_search_index"
)

// allUpgradeDiskBases is the closed basis set.
var allUpgradeDiskBases = []upgradeDiskBasis{upgradeDiskBasisDatabase, upgradeDiskBasisEntryTable, upgradeDiskBasisEntryTableAndSearchIndex}

// newUpgradeDiskBasis validates a raw basis. The empty basis is refused, so
// a catalog entry that names none fails the catalog test.
func newUpgradeDiskBasis(raw string) (upgradeDiskBasis, error) {
	for _, basis := range allUpgradeDiskBases {
		if string(basis) == raw {
			return basis, nil
		}
	}
	return "", fmt.Errorf("store.newUpgradeDiskBasis: value %q is outside the closed disk basis set %q; the step's free-disk need cannot be computed; use one of the listed bases", raw, allUpgradeDiskBases)
}

func (b upgradeDiskBasis) String() string { return string(b) }

const (
	// entryTableName is the table schema 11 and schema 75 copy.
	entryTableName = "session_entries"
	// fts5DefaultPageSize is SQLite's default target size of one FTS5 leaf
	// page, used when an index's _config table sets no pgsz.
	fts5DefaultPageSize = 4050
	// fts5StructureRowID is the rowid of an FTS5 index's structure record
	// in its _data table, and fts5AveragesRowID of its averages record.
	fts5StructureRowID = 10
	fts5AveragesRowID  = 1
	// fts5DocsizeRowBytes bounds one row of an FTS5 _docsize table: a rowid
	// of up to nine bytes, a few token-count varints, the record and cell
	// headers, and a cell pointer.
	fts5DocsizeRowBytes = 24
	// fts5MaxSegments is FTS5's own bound on the segments of one index.
	fts5MaxSegments = 2000
)

// entryRebuildSearchIndexes are the search indexes schema 75 drops and
// rebuilds: session_entries_fts over the entry rows and session_search_fts
// over the entry rows and the entry bodies.
var entryRebuildSearchIndexes = []string{"session_entries_fts", "session_search_fts"}

// tableBytesEstimate is a read-only, bounded estimate of one rowid table.
type tableBytesEstimate struct {
	// Rows is an upper bound on the row count: the span of the table's
	// rowids. Deleted rows leave gaps, so it errs high.
	Rows int64
	// RowsRead is how many rows the estimate read: the two rowid bounds
	// plus the sample. It never exceeds the sample bound plus two.
	RowsRead int64
	// Exact is true when the table had no more rows than the sample bound,
	// so every row was read.
	Exact bool
	// StoredBytes estimates the bytes the table and its indexes occupy.
	StoredBytes int64
}

// estimateTableBytesOnConn estimates the stored size of a rowid table
// without reading its values. It reads the smallest and the largest rowid,
// then at most sampleRows rows at evenly spaced rowids. Each sampled row is
// measured with octet_length, which SQLite answers from the record header
// without loading the value or its overflow pages, so the cost is bounded
// by the sample and not by the table: a 40 GB entry table costs what a
// small one does.
//
// A table with no more rowids than sampleRows is read in full and is
// exact. A larger one is the rowid span times the mean sampled row, plus a
// fixed per-row overhead for the record, the cell and the index entries
// (storedRowBytes), times a safety margin. A missing or empty table is zero.
func estimateTableBytesOnConn(conn *sqlite.Conn, table string, sampleRows int64) (tableBytesEstimate, error) {
	var est tableBytesEstimate
	if !tableExistsOnConn(conn, table) {
		return est, nil
	}
	columns, err := tableColumnsOnConn(conn, table)
	if err != nil {
		return est, err
	}
	rowExpr := octetSumExpr(columns)
	quoted := quoteIdentifier(table)

	pageSize, err := readPragmaInt(conn, "PRAGMA page_size")
	if err != nil || pageSize <= 0 {
		return est, fmt.Errorf("store: read the page size for the free-disk estimate of %s: %v", table, err)
	}

	// Every b-tree, the table's and each index's, takes at least one page.
	var trees int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM sqlite_schema WHERE tbl_name = ? AND type IN ('table', 'index') AND rootpage > 0`, &sqlitex.ExecOptions{
		Args:       []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error { trees = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		return est, fmt.Errorf("store: count the indexes of %s for the free-disk estimate: %w", table, err)
	}
	treePages := trees * pageSize

	var lo, hi int64
	empty := false
	for _, bound := range []struct {
		query string
		into  *int64
	}{
		// Two statements: SQLite answers a lone min or max of the rowid
		// from one end of the tree, and both in one statement by a scan.
		{`SELECT min(rowid) FROM ` + quoted, &lo},
		{`SELECT max(rowid) FROM ` + quoted, &hi},
	} {
		if err := sqlitex.ExecuteTransient(conn, bound.query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			empty = empty || stmt.ColumnType(0) == sqlite.TypeNull
			*bound.into = stmt.ColumnInt64(0)
			return nil
		}}); err != nil {
			return est, fmt.Errorf("store: read the rowid bounds of %s for the free-disk estimate: %w", table, err)
		}
		est.RowsRead++
	}
	if empty {
		return est, nil
	}
	span := hi - lo + 1

	if span <= sampleRows {
		var rows, stored int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+rowExpr+` FROM `+quoted, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			rows++
			stored += storedRowBytes(stmt.ColumnInt64(0), pageSize)
			return nil
		}}); err != nil {
			return est, fmt.Errorf("store: measure the rows of %s for the free-disk estimate: %w", table, err)
		}
		est.Exact = true
		est.Rows = rows
		est.RowsRead += rows
		est.StoredBytes = stored + treePages
		return est, nil
	}

	est.Rows = span
	stmt, _, err := conn.PrepareTransient(`SELECT ` + rowExpr + ` FROM ` + quoted + ` WHERE rowid >= ? ORDER BY rowid LIMIT 1`)
	if err != nil {
		return est, fmt.Errorf("store: prepare the row sample of %s for the free-disk estimate: %w", table, err)
	}
	defer stmt.Finalize()
	var stored, read int64
	for i := int64(0); i < sampleRows; i++ {
		// The midpoint of the i-th of sampleRows equal slices of the span.
		at := lo + int64(float64(2*i+1)/float64(2*sampleRows)*float64(span))
		stmt.BindInt64(1, at)
		hasRow, err := stmt.Step()
		if err != nil {
			return est, fmt.Errorf("store: sample a row of %s for the free-disk estimate: %w", table, err)
		}
		if hasRow {
			stored += storedRowBytes(stmt.ColumnInt64(0), pageSize)
			read++
		}
		if err := stmt.Reset(); err != nil {
			return est, fmt.Errorf("store: sample a row of %s for the free-disk estimate: %w", table, err)
		}
	}
	est.RowsRead += read
	if read > 0 {
		meanRow := float64(stored) / float64(read)
		est.StoredBytes = int64(math.Ceil(defaults.StoreUpgradeEstimateSampleMargin*float64(span)*meanRow)) + treePages
	}
	return est, nil
}

// storedRowBytes is the space one row of a table occupies, from the bytes
// of its columns, by SQLite's file format for a table leaf page with no
// reserved bytes: the part of a large row that does not fit on the leaf
// goes to overflow pages, which are whole pages; the cell left on the leaf
// takes its share of a leaf page, which holds as many cells of that size as
// fit and wastes the rest. A fixed overhead adds the record header, the
// rowid and the row's index entries. Neighbouring rows of other sizes can
// fill a page better than rows of one size, so the share errs high.
func storedRowBytes(payload, pageSize int64) int64 {
	usable := pageSize
	maxLocal := usable - 35
	local, overflow := payload, int64(0)
	if payload > maxLocal {
		minLocal := (usable-12)*32/255 - 23
		local = minLocal + (payload-minLocal)%(usable-4)
		if local > maxLocal {
			local = minLocal
		}
		overflow = (payload - local + usable - 5) / (usable - 4)
		local += 4 // the first overflow page number
	}
	// A cell is its local payload, its size and rowid varints (at most
	// nine bytes each), and a two-byte pointer; the page header is eight.
	cell := local + 18 + 2
	perPage := max((usable-8)/cell, 1)
	return pageSize/perPage + overflow*pageSize + defaults.StoreUpgradeEstimateRowOverheadBytes
}

// fts5IndexBytesOnConn estimates the size of an FTS5 index from two
// records FTS5 keeps current on every write, each one row of the index's
// _data table, so the cost is three rows however large the index is:
//   - the structure record (rowid 10) lists every segment's leaves; a leaf
//     is one row of about the index's page size and takes whole database
//     pages;
//   - the averages record (rowid 1) starts with the document count; each
//     document has one small row in the _docsize table.
//
// Each shadow table adds its first page. A missing index is zero. A record
// that does not decode is an error.
func fts5IndexBytesOnConn(conn *sqlite.Conn, index string) (int64, error) {
	data, config := index+"_data", index+"_config"
	if !tableExistsOnConn(conn, data) {
		return 0, nil
	}
	records := map[int64][]byte{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT id, block FROM `+quoteIdentifier(data)+` WHERE id IN (?, ?)`, &sqlitex.ExecOptions{
		Args: []any{fts5AveragesRowID, fts5StructureRowID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			record := make([]byte, stmt.ColumnLen(1))
			stmt.ColumnBytes(1, record)
			records[stmt.ColumnInt64(0)] = record
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read the structure record of the search index %s for the free-disk estimate: %w", index, err)
	}
	var leaves int64
	if record, ok := records[fts5StructureRowID]; ok {
		var err error
		if leaves, err = fts5StructureLeafPages(record); err != nil {
			return 0, fmt.Errorf("store: decode the structure record of the search index %s for the free-disk estimate: %w", index, err)
		}
	}
	var documents uint64
	if record, ok := records[fts5AveragesRowID]; ok && len(record) > 0 {
		var n int
		if documents, n = sqliteVarint(record); n == 0 {
			return 0, fmt.Errorf("store: decode the averages record of the search index %s for the free-disk estimate: the %d-byte record ends inside its document count", index, len(record))
		}
	}
	leafSize := int64(fts5DefaultPageSize)
	if tableExistsOnConn(conn, config) {
		if err := sqlitex.ExecuteTransient(conn, `SELECT v FROM `+quoteIdentifier(config)+` WHERE k = 'pgsz'`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if size := stmt.ColumnInt64(0); size > 0 {
					leafSize = size
				}
				return nil
			},
		}); err != nil {
			return 0, fmt.Errorf("store: read the page size of the search index %s for the free-disk estimate: %w", index, err)
		}
	}
	dbPage, err := readPragmaInt(conn, "PRAGMA page_size")
	if err != nil || dbPage <= 0 {
		return 0, fmt.Errorf("store: read the page size for the free-disk estimate of the search index %s: %v", index, err)
	}
	var shadows int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM sqlite_schema WHERE name IN (?, ?, ?, ?, ?) AND rootpage > 0`, &sqlitex.ExecOptions{
		Args:       []any{data, index + "_idx", index + "_docsize", config, index + "_content"},
		ResultFunc: func(stmt *sqlite.Stmt) error { shadows = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		return 0, fmt.Errorf("store: count the tables of the search index %s for the free-disk estimate: %w", index, err)
	}
	leafBytes := leaves * ((leafSize + dbPage - 1) / dbPage) * dbPage
	return leafBytes + int64(documents)*fts5DocsizeRowBytes + shadows*dbPage, nil
}

// fts5StructureLeafPages sums the leaf pages of every segment an FTS5
// structure record lists. The record is a 4-byte cookie, an optional
// 4-byte version-2 marker, then varints: the level count, the segment
// count, the write counter, and per level the merge count and the segment
// count, then per segment its id and its first and last leaf page (a
// version-2 record adds five varints per segment). The varints are
// SQLite's own encoding.
func fts5StructureLeafPages(record []byte) (int64, error) {
	if len(record) < 4 {
		return 0, fmt.Errorf("the record is %d bytes, shorter than its 4-byte cookie", len(record))
	}
	at := 4
	v2 := len(record) >= 8 && string(record[4:8]) == "\xff\x00\x00\x01"
	if v2 {
		at = 8
	}
	next := func() (uint64, error) {
		value, n := sqliteVarint(record[at:])
		if n == 0 {
			return 0, fmt.Errorf("the record ends inside a varint at byte %d of %d", at, len(record))
		}
		at += n
		return value, nil
	}
	nLevels, err := next()
	if err != nil {
		return 0, err
	}
	nSegments, err := next()
	if err != nil {
		return 0, err
	}
	if nLevels > fts5MaxSegments || nSegments > fts5MaxSegments {
		return 0, fmt.Errorf("the record lists %d levels and %d segments, above FTS5's bound of %d", nLevels, nSegments, fts5MaxSegments)
	}
	if _, err := next(); err != nil { // the write counter
		return 0, err
	}
	perSegment := 3
	if v2 {
		perSegment = 8
	}
	var leaves int64
	var seen uint64
	for level := uint64(0); level < nLevels; level++ {
		if _, err := next(); err != nil { // segments in an ongoing merge
			return 0, err
		}
		count, err := next()
		if err != nil {
			return 0, err
		}
		for segment := uint64(0); segment < count; segment++ {
			var fields [8]uint64
			for f := 0; f < perSegment; f++ {
				if fields[f], err = next(); err != nil {
					return 0, err
				}
			}
			first, last := fields[1], fields[2]
			if last < first {
				return 0, fmt.Errorf("segment %d lists leaf pages %d to %d", fields[0], first, last)
			}
			leaves += int64(last - first + 1)
			seen++
		}
	}
	if seen != nSegments {
		return 0, fmt.Errorf("the record lists %d segments in its levels and %d in its header", seen, nSegments)
	}
	return leaves, nil
}

// sqliteVarint decodes one SQLite varint: big-endian groups of seven bits,
// high bit set on every byte but the last, and a ninth byte that carries
// eight bits. n is 0 when b ends before the varint does.
func sqliteVarint(b []byte) (value uint64, n int) {
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		value = value<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return value, i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return value<<8 | uint64(b[8]), 9
}

// diskBasisBytes is the size a step's disk factor multiplies, for a store
// whose database file is storeBytes long. The objects a step rewrites live
// in the database, so the database size caps every basis: an estimate that
// errs high never asks for more than the whole-database basis would. A
// store whose tables cannot be measured falls back to the whole database,
// with a notice.
func diskBasisBytes(conn *sqlite.Conn, basis upgradeDiskBasis, storeBytes int64) (int64, string) {
	if basis == upgradeDiskBasisDatabase {
		return storeBytes, ""
	}
	size, err := estimateDiskBasisOnConn(conn, basis)
	if err != nil {
		return storeBytes, fmt.Sprintf("warning: could not measure the %s for the free-disk check (%v); the upgrade asks for the share of the whole database instead", strings.ReplaceAll(basis.String(), "_", " "), err)
	}
	return min(size, storeBytes), ""
}

// estimateDiskBasisOnConn measures a table basis: the entry table by its
// bounded sample, and each search index the step rebuilds by its structure
// record. A rebuild writes an index about the size of the one it replaces.
func estimateDiskBasisOnConn(conn *sqlite.Conn, basis upgradeDiskBasis) (int64, error) {
	entries, err := estimateTableBytesOnConn(conn, entryTableName, defaults.StoreUpgradeEstimateSampleRows)
	if err != nil {
		return 0, err
	}
	switch basis {
	case upgradeDiskBasisEntryTable:
		return entries.StoredBytes, nil
	case upgradeDiskBasisEntryTableAndSearchIndex:
		total := entries.StoredBytes
		for _, index := range entryRebuildSearchIndexes {
			size, err := fts5IndexBytesOnConn(conn, index)
			if err != nil {
				return 0, err
			}
			total += size
		}
		return total, nil
	default:
		return 0, fmt.Errorf("store: the disk basis %q has no table measure", basis)
	}
}

// tableColumnsOnConn lists a table's column names.
func tableColumnsOnConn(conn *sqlite.Conn, table string) ([]string, error) {
	var columns []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_table_info(?)`, &sqlitex.ExecOptions{
		Args: []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			columns = append(columns, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list the columns of %s for the free-disk estimate: %w", table, err)
	}
	return columns, nil
}

// octetSumExpr is the SQL sum of the stored byte lengths of columns, or 0.
func octetSumExpr(columns []string) string {
	if len(columns) == 0 {
		return "0"
	}
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = "coalesce(octet_length(" + quoteIdentifier(column) + "), 0)"
	}
	return strings.Join(parts, " + ")
}
