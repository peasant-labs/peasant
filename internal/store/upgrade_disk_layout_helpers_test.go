package store

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// entryTextLayout names where a schema-74 store keeps its entry text.
type entryTextLayout string

const (
	// entryTextInRows is the layout before the content conversion: the text
	// lives in session_entries and the retired search index covers it.
	entryTextInRows entryTextLayout = "entry-text-in-rows"
	// entryTextInBodies is the converted layout: the text lives in
	// session_entry_bodies, the entry table keeps a few short rows, the
	// search index is consolidated over both, and the stored transcripts
	// sit in the content store beside them.
	entryTextInBodies entryTextLayout = "entry-text-in-bodies"
)

// allEntryTextLayouts is the closed layout set.
var allEntryTextLayouts = []entryTextLayout{entryTextInRows, entryTextInBodies}

// newEntryTextLayout validates a fixture's layout name.
func newEntryTextLayout(raw string) (entryTextLayout, error) {
	for _, layout := range allEntryTextLayouts {
		if string(layout) == raw {
			return layout, nil
		}
	}
	return "", fmt.Errorf("layout %q is outside the closed entry text layout set %q; use one of the listed layouts", raw, allEntryTextLayouts)
}

// entrySizeShape names how the sizes of the entry rows a layout store
// writes are distributed. The set is closed.
type entrySizeShape string

const (
	// entrySizeExponential draws sizes from an exponential distribution
	// with one entry in fifty sixteen times larger: a heavy tail, like tool
	// output. It is the shape when a fixture names none.
	entrySizeExponential entrySizeShape = "exponential"
	// entrySizeOnePercentHundredfold makes every hundredth row a hundred
	// times the mean and every other row the mean.
	entrySizeOnePercentHundredfold entrySizeShape = "one-percent-hundredfold"
	// entrySizeRisingWithRowid makes each row larger than the one before:
	// from near zero at the first rowid to twice the mean at the last.
	entrySizeRisingWithRowid entrySizeShape = "rising-with-rowid"
)

// allEntrySizeShapes is the closed shape set.
var allEntrySizeShapes = []entrySizeShape{entrySizeExponential, entrySizeOnePercentHundredfold, entrySizeRisingWithRowid}

// newEntrySizeShape validates a fixture's shape name; empty is the
// exponential default.
func newEntrySizeShape(raw string) (entrySizeShape, error) {
	if raw == "" {
		return entrySizeExponential, nil
	}
	for _, shape := range allEntrySizeShapes {
		if string(shape) == raw {
			return shape, nil
		}
	}
	return "", fmt.Errorf("entry size shape %q is outside the closed set %q; use one of the listed shapes", raw, allEntrySizeShapes)
}

// layoutStoreSpec sizes a synthetic schema-74 store.
type layoutStoreSpec struct {
	Layout   entryTextLayout
	Sessions int
	// TextEntriesPerSession is how many text-bearing entries each session
	// holds, in the rows or in the bodies, as the layout says.
	TextEntriesPerSession int
	// MeanEntryBytes is the mean entry text size. Sizes are drawn from an
	// exponential distribution with one entry in fifty sixteen times larger,
	// so the tail is heavy, like tool output.
	MeanEntryBytes int
	// EntryShape distributes the sizes of the entry rows: the text entries
	// of the rows layout, and the entry rows of the bodies layout when
	// RowEntryBytes is set.
	EntryShape entrySizeShape
	// RowEntriesPerSession is how many entry rows a converted store keeps
	// in the entry table: the rows of sessions that were never converted.
	RowEntriesPerSession int
	// RowEntryBytes is the mean size of those rows, distributed by
	// EntryShape. Zero writes 600-byte rows.
	RowEntryBytes int
	// ContentBytesPerSession is the stored transcript size per session in
	// the content store (converted layout only).
	ContentBytesPerSession int
}

// layoutStoreTotals is what the builder wrote.
type layoutStoreTotals struct {
	EntryRows, BodyRows   int64
	EntryText, BodyText   int64
	ContentBytes, Entries int64
}

// buildLayoutStore builds a schema-74 store in the given layout at path, in
// WAL mode, and closes it so the file is checkpointed with no sidecar.
func buildLayoutStore(t testing.TB, path string, spec layoutStoreSpec) layoutStoreTotals {
	t.Helper()
	var totals layoutStoreTotals
	rng := rand.New(rand.NewPCG(74, 75))
	textOf := func(size int) string {
		var text strings.Builder
		for text.Len() < size {
			fmt.Fprintf(&text, "w%d ", rng.IntN(50000))
		}
		return text.String()[:size]
	}
	// sizeOf is the size of the row-th of rows entry rows of the given mean.
	sizeOf := func(row, rows, mean int) int {
		var size int
		switch spec.EntryShape {
		case entrySizeOnePercentHundredfold:
			size = mean
			if row%100 == 99 {
				size = 100 * mean
			}
		case entrySizeRisingWithRowid:
			size = 2 * mean * (row + 1) / max(rows, 1)
		default:
			size = int(rng.ExpFloat64() * float64(mean) * 0.75)
			if rng.IntN(50) == 0 {
				size *= 16
			}
		}
		return max(size, 16)
	}
	textRows := spec.Sessions * spec.TextEntriesPerSession
	shortRows := spec.Sessions * spec.RowEntriesPerSession
	rowsWritten, shortWritten := 0, 0
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	exec := func(query string, args ...any) {
		t.Helper()
		if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(query), err)
		}
	}
	exec(`PRAGMA journal_mode = WAL`)
	migrateTo(t, conn, 74)
	if spec.Layout == entryTextInBodies {
		// The converted store ran the search consolidation: the entry
		// triggers feed the union index and the retired index is gone.
		if err := sqlitex.ExecuteScript(conn, "DROP TRIGGER IF EXISTS session_entries_ai;\n"+
			"DROP TRIGGER IF EXISTS session_entries_ad;\n"+
			"DROP TRIGGER IF EXISTS session_entries_au;\n"+
			migrateSearchTriggerAI+";\n"+migrateSearchTriggerAD+";\n"+migrateSearchTriggerAU+";\n"+
			"DROP TABLE session_entries_fts;\n"+
			"UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1;", nil); err != nil {
			t.Fatalf("consolidate the search index of the converted store: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('layout-host','layout-host')`)
	exec(`INSERT INTO projects(project_hash) VALUES('layout-project')`)
	nextBody := int64(BodyRowIDBase)
	for s := 0; s < spec.Sessions; s++ {
		end := sqlitex.Transaction(conn)
		var txErr error
		step := func(query string, args ...any) {
			if txErr == nil {
				txErr = sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args})
			}
		}
		sessionID := fmt.Sprintf("%08x-0000-4000-8000-000000000074", s)
		step(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code', 'layout-model', 'layout-host', 'layout-project', 1, 2, 3, '/synthetic/layout.jsonl', 'jsonl')`, sessionID)
		switch spec.Layout {
		case entryTextInRows:
			for e := 0; e < spec.TextEntriesPerSession; e++ {
				preview, output := textOf(sizeOf(rowsWritten, textRows, spec.MeanEntryBytes)), ""
				rowsWritten++
				if e%2 == 1 {
					preview, output = textOf(min(len(preview), 200)), preview
				}
				totals.EntryText += int64(len(preview) + len(output))
				totals.EntryRows++
				step(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview, tool_output)
VALUES(?, ?, 'claude-code', 'text', 'assistant', 0, ?, nullif(?, ''))`, sessionID, e, preview, output)
			}
		case entryTextInBodies:
			for e := 0; e < spec.TextEntriesPerSession; e++ {
				text := textOf(sizeOf(e, spec.TextEntriesPerSession, spec.MeanEntryBytes))
				totals.BodyText += int64(len(text))
				totals.BodyRows++
				step(`INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES(?, ?, printf('%064x', ?), ?, 'claude-code', 'text', 'assistant', ?, 0, 0, 0, 0)`, nextBody, sessionID, nextBody, e, text)
				nextBody++
			}
			for e := 0; e < spec.RowEntriesPerSession; e++ {
				// A never-converted session's row: a bounded preview and,
				// when the fixture sizes them, tool output of any length.
				preview, output := textOf(600), ""
				if spec.RowEntryBytes > 0 {
					size := sizeOf(shortWritten, shortRows, spec.RowEntryBytes)
					preview, output = textOf(min(size, 200)), textOf(size)
				}
				shortWritten++
				totals.EntryText += int64(len(preview) + len(output))
				totals.EntryRows++
				step(`INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview, tool_output)
VALUES(?, ?, 'claude-code', 'text', 'user', 0, ?, nullif(?, ''))`, sessionID, spec.TextEntriesPerSession+e, preview, output)
			}
			if spec.ContentBytesPerSession > 0 {
				digest := fmt.Sprintf("%064x", s)
				step(`INSERT INTO session_content(session_id, digest, byte_length) VALUES(?, ?, ?)`, sessionID, digest, spec.ContentBytesPerSession)
				for chunk, left := 0, spec.ContentBytesPerSession; left > 0; chunk++ {
					size := min(left, 65536)
					step(`INSERT INTO session_content_chunks(session_id, digest, chunk_index, data) VALUES(?, ?, ?, randomblob(?))`, sessionID, digest, chunk, size)
					left -= size
				}
				totals.ContentBytes += int64(spec.ContentBytesPerSession)
			}
		}
		end(&txErr)
		if txErr != nil {
			t.Fatalf("seed session %d of the %s store: %v", s, spec.Layout, txErr)
		}
	}
	return totals
}

// migrateTo applies the frozen chain up to version on conn.
func migrateTo(t testing.TB, conn *sqlite.Conn, version int) {
	t.Helper()
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(version)); err != nil {
		t.Fatalf("migrate the synthetic store to schema %d: %v", version, err)
	}
}

// dbstatBytesOnConn is the ground truth for a set of schema objects: the
// pages dbstat counts for every name the predicate selects. It reads every
// page of those objects, so it is for tests and measurements only.
func dbstatBytesOnConn(t testing.TB, conn *sqlite.Conn, where string) int64 {
	t.Helper()
	var bytes int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name IN (SELECT name FROM sqlite_schema WHERE `+where+`)`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bytes = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("dbstat over %s: %v", where, err)
	}
	return bytes
}
