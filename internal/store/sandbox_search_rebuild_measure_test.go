//go:build upgrade_measure

package store

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sandbox_search_rebuild_measure.yaml
var sandboxSearchRebuildMeasureYAML []byte

type sandboxSearchRebuildFixture struct {
	RequiredVariants []string `yaml:"requiredVariants"`
	Variants         []struct {
		Name  string   `yaml:"name"`
		Setup []string `yaml:"setup"`
	} `yaml:"variants"`
	RequiredQueries []string `yaml:"requiredQueries"`
	Queries         []struct {
		Name  string `yaml:"name"`
		Match string `yaml:"match"`
	} `yaml:"queries"`
}

func loadSandboxSearchRebuildFixture(t *testing.T) sandboxSearchRebuildFixture {
	t.Helper()
	var fixture sandboxSearchRebuildFixture
	if err := yaml.Unmarshal(sandboxSearchRebuildMeasureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var variants, queries []string
	for _, v := range fixture.Variants {
		variants = append(variants, v.Name)
	}
	for _, q := range fixture.Queries {
		queries = append(queries, q.Name)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredVariants, variants, "search rebuild variants"); err != nil {
		t.Fatal(err)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredQueries, queries, "search rebuild queries"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestSandboxSearchRebuildMeasure measures how the search index rebuild of
// the search trigger repair (schema 76) scales with the indexed text. For
// each text size it builds a synthetic schema-74 store whose text is in the
// bodies, upgrades it to 75 (timing that step, which also rebuilds the
// index), and leaves the v75 window residue that makes 76 rebuild. Then, on
// fresh copies of that store, it:
//
//   - runs the real Upgrade and logs step 76's duration and log growth
//     (with the widened postings budget the upgrade sets for it);
//   - runs the bare rebuild statement once per fixture variant on a
//     connection with the store's settings, and checks that every fixture
//     query returns the same rows and ranks as after the first variant (the
//     baseline: FTS5's default budget, as migration 76 ran before).
//
// It logs the load average before every timed run: the machine is shared.
// It never runs in the gate and builds its stores only under /tmp/opencode.
//
// Necessity: the rebuild's cost is a property of FTS5's segment merging at
// scale, which no fixture-sized store shows (a few megabytes fit in one
// flush at any budget); the gate tests prove the budget is applied and
// results are unchanged, and this measurement supplies the numbers behind
// defaults.SearchRebuildHashBytes, the estimate rates and the search rebuild
// disk factor. Exit condition: re-run it and update those constants when
// the bundled SQLite (and so FTS5) is re-vendored or FTS5's default hash
// size changes; delete it if the budget ever becomes adaptive at run time.
//
//	PEASANT_SANDBOX_UPGRADE_DIR=/tmp/opencode/<dir> \
//	PEASANT_SANDBOX_REBUILD_TEXT_MB=128,256,512,1024,2048 \
//	PEASANT_SANDBOX_REBUILD_REPEATS=3 \
//	PEASANT_SANDBOX_REBUILD_VARIANTS=baseline,hashsize-16m \
//	go test -tags upgrade_measure ./internal/store -run TestSandboxSearchRebuildMeasure -v -timeout 0
func TestSandboxSearchRebuildMeasure(t *testing.T) {
	dir := os.Getenv("PEASANT_SANDBOX_UPGRADE_DIR")
	if dir == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_UPGRADE_DIR to a directory under /tmp/opencode")
	}
	if !sandboxDirUnderTmpOpencode(dir) {
		t.Fatalf("PEASANT_SANDBOX_UPGRADE_DIR = %q is outside /tmp/opencode; the measurement builds stores only there", dir)
	}
	fixture := loadSandboxSearchRebuildFixture(t)
	sizes := sandboxPositiveInts(t, "PEASANT_SANDBOX_REBUILD_TEXT_MB", []int{64, 128})
	repeats := sandboxPositiveInts(t, "PEASANT_SANDBOX_REBUILD_REPEATS", []int{3})[0]
	wanted := map[string]bool{}
	if raw := os.Getenv("PEASANT_SANDBOX_REBUILD_VARIANTS"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const entriesPerSession, meanEntryBytes = 50, 4096
	for _, textMB := range sizes {
		t.Run(fmt.Sprintf("%d MiB", textMB), func(t *testing.T) {
			base := filepath.Join(dir, fmt.Sprintf("rebuild-%d.v75.db", textMB))
			work := filepath.Join(dir, fmt.Sprintf("rebuild-%d.work.db", textMB))
			removeStoreFiles(base)
			removeStoreFiles(work)
			t.Cleanup(func() {
				removeStoreFiles(base)
				removeStoreFiles(work)
			})
			sessions := textMB * 1024 * 1024 / (entriesPerSession * meanEntryBytes)
			buildStarted := time.Now()
			buildLayoutStore(t, base, layoutStoreSpec{Layout: entryTextInBodies, Sessions: sessions, TextEntriesPerSession: entriesPerSession, MeanEntryBytes: meanEntryBytes, RowEntriesPerSession: 2})
			t.Logf("built the schema-74 store in %s", time.Since(buildStarted).Round(time.Second))

			// 74 -> 75: the entry table copy and both index rebuilds.
			conn, err := sqlite.OpenConn(base, sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			if err := preparePragmas(conn); err != nil {
				t.Fatal(err)
			}
			load := sandboxLoadAverage()
			started := time.Now()
			migrateTo(t, conn, 75)
			t.Logf("migration 75 (entry table copy and both search index rebuilds): %s, load %s", time.Since(started).Round(time.Millisecond), load)
			// The v75 window: a deleted entry row leaves its postings in the
			// union index, the residue that makes migration 76 rebuild.
			if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries WHERE rowid = (SELECT min(rowid) FROM session_entries)`, nil); err != nil {
				t.Fatal(err)
			}
			var textBytes, docs int64
			if err := sqlitex.ExecuteTransient(conn, `SELECT count(*), sum(coalesce(length(CAST(content_preview AS BLOB)), 0) + coalesce(length(CAST(tool_input AS BLOB)), 0) + coalesce(length(CAST(tool_output AS BLOB)), 0)) FROM session_search_source`, &sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					docs, textBytes = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			indexBytes, err := fts5IndexBytesOnConn(conn, "session_search_fts")
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			if err := sqlitex.ExecuteTransient(conn, `EXPLAIN QUERY PLAN SELECT rid, content_preview, tool_input, tool_output FROM session_search_source`, &sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					plan = append(plan, stmt.ColumnText(3))
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			baseInfo, err := os.Stat(base)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("store: %d sessions, %d indexed rows, %d indexed text bytes, search index %d bytes (estimate), database %d bytes; source scan plan %q",
				sessions, docs, textBytes, indexBytes, baseInfo.Size(), plan)

			// The production step, repeated on fresh copies.
			for run := 1; run <= repeats; run++ {
				copyStoreFile(t, base, work)
				load := sandboxLoadAverage()
				report, err := Upgrade(t.Context(), work, UpgradeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, step := range report.Steps {
					if step.Step.Version != searchRepairSchemaVersion {
						continue
					}
					found = true
					if !step.Step.Cost.Long() {
						t.Fatalf("step 76 planned as %s; the synthetic residue did not reach the plan", step.Step.Cost)
					}
					t.Logf("REBUILD upgrade run %d: text %d bytes, step 76 %s (%.2f MB/s), wal +%d bytes (%.3f of the database), load %s",
						run, textBytes, step.Duration.Round(time.Millisecond), float64(textBytes)/step.Duration.Seconds()/1e6, step.WALBytes, float64(step.WALBytes)/float64(baseInfo.Size()), load)
				}
				if !found {
					t.Fatal("the upgrade ran no step 76")
				}
				removeStoreFiles(work)
			}

			// The bare statement per variant, with a result check.
			var baselineDigest string
			for _, variant := range fixture.Variants {
				if len(wanted) > 0 && !wanted[variant.Name] {
					continue
				}
				for run := 1; run <= repeats; run++ {
					copyStoreFile(t, base, work)
					conn, err := sqlite.OpenConn(work, sqlite.OpenReadWrite)
					if err != nil {
						t.Fatal(err)
					}
					if err := preparePragmas(conn); err != nil {
						t.Fatal(err)
					}
					for _, setup := range variant.Setup {
						if err := sqlitex.ExecuteTransient(conn, setup, nil); err != nil {
							t.Fatalf("variant %s setup %q: %v", variant.Name, setup, err)
						}
					}
					load := sandboxLoadAverage()
					started := time.Now()
					for _, statement := range []string{`BEGIN IMMEDIATE`, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, `COMMIT`} {
						if err := sqlitex.ExecuteTransient(conn, statement, nil); err != nil {
							t.Fatalf("%s: %v", statement, err)
						}
					}
					elapsed := time.Since(started)
					// The log only grows inside the transaction, so its size
					// right after the commit is the rebuild's whole log.
					walBytes := sandboxFileBytes(work + "-wal")
					digest := sandboxSearchDigest(t, conn, fixture)
					segments := sandboxFTSSegments(t, conn)
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					removeStoreFiles(work)
					if baselineDigest == "" {
						baselineDigest = digest
					}
					same := digest == baselineDigest
					t.Logf("REBUILD variant %-13s run %d: %s (%.2f MB/s), wal %d bytes (%.3f of the database), %d segments after, results identical to the first variant: %v, load %s",
						variant.Name, run, elapsed.Round(time.Millisecond), float64(textBytes)/elapsed.Seconds()/1e6, walBytes, float64(walBytes)/float64(baseInfo.Size()), segments, same, load)
					if !same {
						t.Errorf("variant %s changed the query results", variant.Name)
					}
				}
			}
		})
	}
}

// sandboxSearchDigest hashes every fixture query's rows and bm25 ranks, in
// rank order, plus the result of FTS5's integrity check.
func sandboxSearchDigest(t *testing.T, conn *sqlite.Conn, fixture sandboxSearchRebuildFixture) string {
	t.Helper()
	if err := ftsContentIntegrityCheck(conn); err != nil {
		t.Fatalf("integrity-check after the rebuild: %v", err)
	}
	hash := sha256.New()
	for _, query := range fixture.Queries {
		rows := 0
		if err := sqlitex.ExecuteTransient(conn, `SELECT rowid, rank FROM session_search_fts WHERE session_search_fts MATCH ? ORDER BY rank, rowid`, &sqlitex.ExecOptions{
			Args: []any{query.Match},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows++
				_, _ = fmt.Fprintf(hash, "%s %d %.12g\n", query.Name, stmt.ColumnInt64(0), stmt.ColumnFloat(1))
				return nil
			},
		}); err != nil {
			t.Fatalf("query %s (%q): %v", query.Name, query.Match, err)
		}
		if rows == 0 {
			t.Fatalf("query %s (%q) matched nothing; the fixture query proves nothing", query.Name, query.Match)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// sandboxFTSSegments counts the segments the union index's structure record
// lists.
func sandboxFTSSegments(t *testing.T, conn *sqlite.Conn) int {
	t.Helper()
	segments := 0
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(DISTINCT segid) FROM session_search_fts_idx`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			segments = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return segments
}

func sandboxPositiveInts(t *testing.T, variable string, fallback []int) []int {
	t.Helper()
	raw := os.Getenv(variable)
	if raw == "" {
		return fallback
	}
	var values []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			t.Fatalf("%s = %q, want positive whole numbers separated by commas", variable, raw)
		}
		values = append(values, n)
	}
	return values
}

// sandboxFileBytes is the size of a file, or 0 when it is missing.
func sandboxFileBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// sandboxLoadAverage is the one-minute load average, or "unknown".
func sandboxLoadAverage() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "unknown"
	}
	return fields[0]
}

func removeStoreFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

// copyStoreFile replaces to with a copy of a closed, checkpointed store.
func copyStoreFile(t *testing.T, from, to string) {
	t.Helper()
	removeStoreFiles(to)
	copySearchRebuildStore(t, from, to)
}
