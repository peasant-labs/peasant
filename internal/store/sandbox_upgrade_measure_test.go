//go:build upgrade_measure

// The measurements in this file build synthetic stores of hundreds of
// megabytes and run for minutes. They prove nothing in CI and never run
// there: the upgrade_measure build tag keeps them out of every gate, and
// each refuses a directory outside /tmp/opencode. Run one by hand with
// -tags upgrade_measure, as each test's comment shows.

package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestSandboxStoreUpgradeMeasure upgrades a synthetic schema-69 store of a
// known text size and logs, per step, the duration and the write-ahead-log
// growth, then the free-space change and the largest log observed at a
// progress event. The sampled maximum can miss the peak. It is
// the measurement behind the upgrade's free-disk factors and the per-step
// split of a long upgrade. It never runs in the gate and never touches a
// real store: it builds its own under /tmp/opencode.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	go test -tags upgrade_measure ./internal/store -run TestSandboxStoreUpgradeMeasure -v -timeout 0
func TestSandboxStoreUpgradeMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	if !sandboxDirUnderTmpOpencode(dir) {
		t.Fatal("the upgrade measurement refuses a directory outside /tmp/opencode")
	}
	var err error
	textMB := 64
	if raw := os.Getenv("PEASANT_SANDBOX_UPGRADE_TEXT_MB"); raw != "" {
		textMB, err = strconv.Atoi(raw)
		if err != nil || textMB <= 0 {
			t.Fatalf("PEASANT_SANDBOX_UPGRADE_TEXT_MB = %q, want a positive whole number of megabytes", raw)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "upgrade-measure.db")
	for _, stale := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(stale)
	}

	const (
		entriesPerSession = 50
		entryBytes        = 4096
		vocabulary        = 50000
	)
	sessions := textMB * 1024 * 1024 / (entriesPerSession * entryBytes)
	words := rand.New(rand.NewPCG(69, 76))
	entryText := func() string {
		var text strings.Builder
		for text.Len() < entryBytes {
			fmt.Fprintf(&text, "w%d ", words.IntN(vocabulary))
		}
		return text.String()
	}
	var textBytes int64
	buildBehindStore(t, path, 69, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('measure-host','measure-host');
INSERT INTO projects(project_hash) VALUES('measure-project');`, nil); err != nil {
			t.Fatal(err)
		}
		for s := 0; s < sessions; s++ {
			end := sqlitex.Transaction(conn)
			var txErr error
			sessionID := fmt.Sprintf("%08x-0000-4000-8000-000000000000", s)
			txErr = sqlitex.ExecuteTransient(conn, `
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code', 'measure-model', 'measure-host', 'measure-project', 1, 2, 3, '/synthetic/measure.jsonl', 'jsonl')`,
				&sqlitex.ExecOptions{Args: []any{sessionID}})
			for e := 0; e < entriesPerSession && txErr == nil; e++ {
				text := entryText()
				textBytes += int64(len(text))
				txErr = sqlitex.ExecuteTransient(conn, `
INSERT INTO session_entries(session_id, entry_index, provider, entry_type, role, depth, content_preview)
VALUES(?, ?, 'claude-code', 'text', 'assistant', 0, ?)`,
					&sqlitex.ExecOptions{Args: []any{sessionID, e, text}})
			}
			end(&txErr)
			if txErr != nil {
				t.Fatal(txErr)
			}
		}
	})

	sizeOf := func(p string) int64 {
		info, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	storeBefore := sizeOf(path)
	freeBefore, freeKnown := migrateFreeBytes(dir)
	plan := mustPlan(t, path)
	t.Logf("store: %d sessions, %d text bytes, database %d bytes; plan asks for %d bytes free", sessions, textBytes, storeBefore, plan.RequiredFreeBytes)

	var walSampleMax int64
	started := time.Now()
	report, err := Upgrade(t.Context(), path, UpgradeOptions{
		Progress: func(event UpgradeEvent) {
			if size := sizeOf(path + "-wal"); size > walSampleMax {
				walSampleMax = size
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range report.Steps {
		t.Logf("step %d  %-9s %-18s %12s  wal +%d bytes  %s", step.Step.Version, step.Step.Kind, step.Step.Cost, step.Duration.Round(time.Millisecond), step.WALBytes, step.Step.Title)
	}
	freeAfter, _ := migrateFreeBytes(dir)
	t.Logf("total %s; database %d -> %d bytes; largest sampled write-ahead log %d bytes (%.3f of the database, %.3f of the text), %d bytes after",
		time.Since(started).Round(time.Millisecond), storeBefore, sizeOf(path), walSampleMax,
		float64(walSampleMax)/float64(storeBefore), float64(walSampleMax)/float64(textBytes), sizeOf(path+"-wal"))
	if freeKnown {
		t.Logf("free space changed by %d bytes", int64(freeAfter)-int64(freeBefore))
	}
}

// TestSandboxStoreUpgradeDiskBasisMeasure builds one synthetic schema-74
// store per entry text layout, plus a store whose text is in the bodies and
// whose entry table holds as many rows as the estimate reads (its worst
// case), compares the free-disk estimate of the entry table rebuild with
// dbstat's page count, times both, then upgrades the store and logs the
// largest log observed at a progress event plus the database's later
// growth. The sampled maximum can miss the peak and the sum is not necessarily
// concurrent usage. It is the measurement behind the entry table rebuild
// factor and the estimate's bounds. It builds its stores under
// /tmp/opencode.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_UPGRADE_TEXT_MB=256 \
//	go test -tags upgrade_measure ./internal/store -run TestSandboxStoreUpgradeDiskBasisMeasure -v -timeout 0
func TestSandboxStoreUpgradeDiskBasisMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	if !sandboxDirUnderTmpOpencode(dir) {
		t.Fatal("the upgrade measurement refuses a directory outside /tmp/opencode")
	}
	var err error
	textMB := 64
	if raw := os.Getenv("PEASANT_SANDBOX_UPGRADE_TEXT_MB"); raw != "" {
		textMB, err = strconv.Atoi(raw)
		if err != nil || textMB <= 0 {
			t.Fatalf("PEASANT_SANDBOX_UPGRADE_TEXT_MB = %q, want a positive whole number of megabytes", raw)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const entriesPerSession, meanEntryBytes = 50, 4096
	sessions := textMB * 1024 * 1024 / (entriesPerSession * meanEntryBytes)
	sizeOf := func(p string) int64 {
		info, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	// The bound store: every entry row the estimate may read, each about a
	// leaf page, every hundredth a hundred times larger.
	const boundRowsPerSession, boundRowBytes = 64, 3800
	boundSessions := int(defaults.StoreUpgradeEstimateMaxEntryRows) / boundRowsPerSession
	specs := []struct {
		name string
		spec layoutStoreSpec
	}{
		{string(entryTextInRows), layoutStoreSpec{Layout: entryTextInRows, Sessions: sessions, TextEntriesPerSession: entriesPerSession, MeanEntryBytes: meanEntryBytes}},
		{string(entryTextInBodies), layoutStoreSpec{Layout: entryTextInBodies, Sessions: sessions, TextEntriesPerSession: entriesPerSession, MeanEntryBytes: meanEntryBytes, RowEntriesPerSession: 2, ContentBytesPerSession: entriesPerSession * meanEntryBytes}},
		{"entry-text-in-bodies-at-the-row-bound", layoutStoreSpec{Layout: entryTextInBodies, Sessions: boundSessions, TextEntriesPerSession: 1, MeanEntryBytes: meanEntryBytes, EntryShape: entrySizeOnePercentHundredfold, RowEntriesPerSession: boundRowsPerSession, RowEntryBytes: boundRowBytes}},
	}
	for _, sized := range specs {
		layout, spec := sized.name, sized.spec
		t.Run(layout, func(t *testing.T) {
			path := filepath.Join(dir, layout+".db")
			for _, stale := range []string{path, path + "-wal", path + "-shm"} {
				_ = os.Remove(stale)
			}
			totals := buildLayoutStore(t, path, spec)
			storeBefore := sizeOf(path)

			conn, err := openReadOnlyNoSidecars(path)
			if err != nil {
				t.Fatal(err)
			}
			timed := func(f func()) time.Duration {
				started := time.Now()
				f()
				return time.Since(started)
			}
			var entriesTruth, searchTruth, bodiesTruth int64
			entriesTime := timed(func() { entriesTruth = dbstatBytesOnConn(t, conn, `tbl_name = 'session_entries'`) })
			searchTime := timed(func() {
				searchTruth = dbstatBytesOnConn(t, conn, `name LIKE 'session_entries_fts%' OR name LIKE 'session_search_fts%'`)
			})
			bodiesTime := timed(func() { bodiesTruth = dbstatBytesOnConn(t, conn, `tbl_name = 'session_entry_bodies'`) })
			// The exact entry table, read in full: every row's octet sum,
			// from the record headers only.
			var fullRows, fullOctets int64
			fullTime := timed(func() {
				columns, colErr := tableColumnsOnConn(conn, entryTableName)
				if colErr != nil {
					t.Fatal(colErr)
				}
				if err := sqlitex.ExecuteTransient(conn, `SELECT count(*), sum(`+octetSumExpr(columns)+`) FROM session_entries`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
					fullRows, fullOctets = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
			})
			// The same rows with their text loaded: what reading the values,
			// overflow pages included, costs.
			var loaded int64
			loadedTime := timed(func() {
				if err := sqlitex.ExecuteTransient(conn, `SELECT sum(length(CAST(content_preview AS BLOB)) + coalesce(length(CAST(tool_output AS BLOB)), 0)) FROM session_entries`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
					loaded = stmt.ColumnInt64(0)
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
			})
			t.Logf("entry table in full: %d rows, %d octets from the record headers in %s; %d text bytes loaded in %s",
				fullRows, fullOctets, fullTime.Round(time.Millisecond), loaded, loadedTime.Round(time.Millisecond))
			var entries tableBytesEstimate
			entriesEstimateTime := timed(func() {
				if entries, err = measureTableBytesOnConn(conn, entryTableName, defaults.StoreUpgradeEstimateMaxEntryRows); err != nil {
					t.Fatal(err)
				}
			})
			var searchEstimate, basis int64
			var measured bool
			for _, index := range entryRebuildSearchIndexes {
				size, err := fts5IndexBytesOnConn(conn, index)
				if err != nil {
					t.Fatal(err)
				}
				searchEstimate += size
			}
			estimateTime := timed(func() {
				if basis, measured, err = estimateDiskBasisOnConn(conn, upgradeDiskBasisEntryTableAndSearchIndex, defaults.StoreUpgradeEstimateMaxEntryRows, defaults.StoreUpgradeEstimateMaxSessionRows); err != nil {
					t.Fatal(err)
				}
			})
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			plan := mustPlan(t, path)
			t.Logf("%s: %d sessions; entry rows %d (text %d), body rows %d (text %d), content %d; database %d bytes",
				layout, spec.Sessions, totals.EntryRows, totals.EntryText, totals.BodyRows, totals.BodyText, totals.ContentBytes, storeBefore)
			t.Logf("dbstat: entry table and indexes %d bytes in %s; search indexes %d in %s; bodies %d in %s",
				entriesTruth, entriesTime.Round(time.Millisecond), searchTruth, searchTime.Round(time.Millisecond), bodiesTruth, bodiesTime.Round(time.Millisecond))
			if !measured {
				basis = storeBefore
			}
			t.Logf("estimate: entry table read in full %d bytes (%d rows read, measured %v) in %s; search indexes %d bytes; basis %d (measured %v, else the database), in %s",
				entries.StoredBytes, entries.RowsRead, entries.Measured, entriesEstimateTime.Round(time.Microsecond), searchEstimate, basis, measured, estimateTime.Round(time.Microsecond))
			t.Logf("plan asks for %d bytes free (%.3f of the database, %.3f of the basis estimate, %.3f of the dbstat entry table and search indexes)",
				plan.RequiredFreeBytes, float64(plan.RequiredFreeBytes)/float64(storeBefore), float64(plan.RequiredFreeBytes)/float64(basis), float64(plan.RequiredFreeBytes)/float64(entriesTruth+searchTruth))

			var walSampleMax int64
			started := time.Now()
			report, err := Upgrade(t.Context(), path, UpgradeOptions{Progress: func(UpgradeEvent) {
				if size := sizeOf(path + "-wal"); size > walSampleMax {
					walSampleMax = size
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range report.Steps {
				t.Logf("step %d  %12s  wal +%d bytes  %s", step.Step.Version, step.Duration.Round(time.Millisecond), step.WALBytes, step.Step.Title)
			}
			growth := sizeOf(path) - storeBefore
			used := walSampleMax + growth
			t.Logf("upgrade %s: largest sampled log %d, database later grew %d, sum %d: %.3f of the database, %.3f of the basis estimate, %.3f of the dbstat entry table and search indexes; the plan asked for %.2f times that sum",
				time.Since(started).Round(time.Millisecond), walSampleMax, growth, used, float64(used)/float64(storeBefore), float64(used)/float64(basis), float64(used)/float64(entriesTruth+searchTruth), float64(plan.RequiredFreeBytes)/float64(used))
		})
	}
}

// sandboxDirUnderTmpOpencode reports whether dir lies strictly inside
// /tmp/opencode, the only place a sandbox measurement may build a store.
func sandboxDirUnderTmpOpencode(dir string) bool {
	rel, err := filepath.Rel("/tmp/opencode", dir)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
