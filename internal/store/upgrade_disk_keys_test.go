package store

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// runUpgradeDiskSessionKeysCase exercises the real plan, refusal, and upgrade
// on a synthetic store built by the frozen chain. SQLite's dbstat independently
// measures the live index pages; the upgrade report records the step's WAL.
func runUpgradeDiskSessionKeysCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "keys.db")
	buildBehindStore(t, path, c.From, func(conn *sqlite.Conn) {
		if c.Sessions < 2 || c.KeyBytes < 1 {
			t.Fatalf("the fixture needs at least two sessions and a nonempty key: %+v", c)
		}
		host := strings.Repeat("h", c.KeyBytes)
		project := strings.Repeat("p", c.KeyBytes)
		exec := func(query string, args ...any) {
			t.Helper()
			if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
				t.Fatalf("seed synthetic store: %v", err)
			}
		}
		exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES (?, 'synthetic-host')`, host)
		exec(`INSERT INTO projects(project_hash) VALUES (?)`, project)
		parent := upgradeSyntheticID(0, c.IDBytes, c.MixedIDs)
		digest := strings.Repeat("0", 64)
		end := sqlitex.Transaction(conn)
		var txErr error
		for i := 0; i < c.Sessions && txErr == nil; i++ {
			var parentID any
			if i%2 == 1 && c.EvidenceParentBytes == 0 {
				parentID = parent
			}
			sessionID := upgradeSyntheticID(i, c.IDBytes, c.MixedIDs)
			txErr = sqlitex.Execute(conn, `INSERT INTO sessions(session_id, parent_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES (?, ?, 'claude-code', 'synthetic-model', ?, ?, ?, ?, ?, '/synthetic/session.jsonl', 'jsonl')`, &sqlitex.ExecOptions{Args: []any{sessionID, parentID, host, project, i, i + 1, i + 2}})
			if txErr == nil && c.MetadataParentBytes > 0 && i%2 == 1 {
				parentFromMetadata := upgradeSyntheticID(i, c.MetadataParentBytes, false)
				metadata := `{"parentUuid":"` + parentFromMetadata + `"}`
				txErr = sqlitex.Execute(conn, `INSERT INTO session_publication_metadata(session_id, capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at) VALUES (?, 1, 9, ?, ?, ?, 1)`, &sqlitex.ExecOptions{Args: []any{sessionID, metadata, digest, digest}})
			}
			if txErr == nil && c.EvidenceParentBytes > 0 && i%2 == 1 {
				parentFromEvidence := upgradeSyntheticID(i, c.EvidenceParentBytes, false)
				txErr = seedUpgradeParentEvidence(conn, sessionID, parentFromEvidence, project, digest, c.EvidenceLatestOnly)
			}
		}
		end(&txErr)
		if txErr != nil {
			t.Fatalf("seed synthetic sessions: %v", txErr)
		}
		exec(`INSERT INTO daily_summary_harness(date_utc, model_harness) VALUES (?, 'claude-code')`, strings.Repeat("d", c.KeyBytes))
	})

	conn, err := openReadOnlyNoSidecars(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	truth := dbstatBytesOnConn(t, conn, `tbl_name IN ('sessions', 'daily_summary_harness')`)
	size, measured, err := estimateSessionTablesOnConn(conn, defaults.StoreUpgradeEstimateMaxSessionRows)
	if err != nil || !measured {
		t.Fatalf("measure synthetic session tables: size=%d measured=%v err=%v", size, measured, err)
	}
	if size < truth {
		t.Errorf("session tables estimated at %d bytes, below SQLite's %d bytes of table and index pages", size, truth)
	}
	if c.MaxSessionRows > 0 {
		limited, err := measureTableBytesOnConn(conn, "sessions", c.MaxSessionRows)
		if err != nil || limited.Measured || limited.RowsRead != c.MaxSessionRows+1 {
			t.Errorf("session read past bound: estimate=%+v err=%v, bound=%d", limited, err, c.MaxSessionRows)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		fallback, notice := diskBasisBytesWithLimits(conn, upgradeDiskBasisSessionTables, info.Size(), defaults.StoreUpgradeEstimateMaxEntryRows, c.MaxSessionRows)
		if fallback != info.Size() || notice != "" {
			t.Errorf("session-bound fallback = %d, %q; want the whole %d-byte database", fallback, notice, info.Size())
		}
	}
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	conn = nil
	stubFreeBytes(t, 0, false)
	plan := mustPlan(t, path)
	before := fileHash(t, path)
	if plan.RequiredFreeBytes <= 0 {
		t.Fatal("the synthetic store must need free space")
	}
	stubFreeBytes(t, uint64(plan.RequiredFreeBytes-1), true)
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err == nil {
		t.Error("Upgrade ran below its planned disk requirement")
	} else {
		var diskErr *UpgradeDiskError
		if !errors.As(err, &diskErr) {
			t.Errorf("Upgrade below its planned disk requirement: %v", err)
		}
	}
	assertHashUnchanged(t, path, before, "insufficient disk refusal")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stubFreeBytes(t, math.MaxInt64, true)
	report, err := Upgrade(t.Context(), path, UpgradeOptions{})
	if err != nil {
		t.Fatalf("upgrade synthetic store: %v", err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var stepWAL int64
	for _, step := range report.Steps {
		if step.Step.Version == 78 {
			stepWAL = step.WALBytes
		}
	}
	if c.From == 77 && stepWAL == 0 {
		t.Fatal("the migration did not report its WAL growth")
	}
	// The report is a net WAL change at step completion, not the peak: a
	// checkpoint can hide earlier growth. This assertion only compares the
	// observed values, and does not establish an upper bound on disk usage.
	used := stepWAL + max(afterInfo.Size()-info.Size(), 0)
	if used > plan.RequiredFreeBytes {
		t.Errorf("plan asked for %d bytes, but observed WAL net growth plus database growth was %d bytes", plan.RequiredFreeBytes, used)
	}
	updated, err := openReadOnlyNoSidecars(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if updated != nil {
			_ = updated.Close()
		}
	}()
	updatedTruth := dbstatBytesOnConn(t, updated, `tbl_name IN ('sessions', 'daily_summary_harness')`)
	if c.MetadataParentBytes > 0 || c.EvidenceParentBytes > 0 {
		wantLength := max(c.MetadataParentBytes, c.EvidenceParentBytes)
		var longest int64
		if err := sqlitex.ExecuteTransient(updated, `SELECT coalesce(max(octet_length(resolved_parent_id)), 0) FROM sessions`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error { longest = stmt.ColumnInt64(0); return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if longest != int64(wantLength) {
			t.Errorf("the upgraded parent projection's longest key is %d bytes, want %d from the synthetic source", longest, wantLength)
		}
		if c.EvidenceParentBytes > 0 {
			var matching int64
			if err := sqlitex.ExecuteTransient(updated, `SELECT count(*) FROM sessions WHERE octet_length(resolved_parent_id) = ?`, &sqlitex.ExecOptions{
				Args: []any{c.EvidenceParentBytes}, ResultFunc: func(stmt *sqlite.Stmt) error { matching = stmt.ColumnInt64(0); return nil },
			}); err != nil {
				t.Fatal(err)
			}
			if matching != int64(c.Sessions/2) {
				t.Errorf("the upgrade projected %d evidence parents of %d bytes, want %d", matching, c.EvidenceParentBytes, c.Sessions/2)
			}
		}
	}
	if err := updated.Close(); err != nil {
		t.Fatal(err)
	}
	updated = nil
	if size < updatedTruth {
		t.Errorf("session tables estimated at %d bytes before the upgrade, below the upgraded tables' %d bytes", size, updatedTruth)
	}
	if c.WantBasisMax > 0 {
		largestTruth := max(truth, updatedTruth)
		if float64(size) > c.WantBasisMax*float64(largestTruth) {
			t.Errorf("session table basis %d exceeds %.2f times the larger observed table size %d", size, c.WantBasisMax, largestTruth)
		}
	}
	if floor := int64(math.Ceil(float64(updatedTruth) * defaults.StoreUpgradeSessionTablesRebuildDiskFactor)); plan.RequiredFreeBytes < floor {
		t.Errorf("plan asked for %d bytes; %.2f times the upgraded session tables' %d bytes is %d", plan.RequiredFreeBytes, defaults.StoreUpgradeSessionTablesRebuildDiskFactor, updatedTruth, floor)
	}
	t.Logf("schema %d: %d sessions, %d-byte keys, mixed=%v; old table pages=%d, basis=%d, plan=%d, step WAL=%d, database growth=%d, upgraded table pages=%d", c.From, c.Sessions, c.IDBytes, c.MixedIDs, truth, size, plan.RequiredFreeBytes, stepWAL, max(afterInfo.Size()-info.Size(), 0), updatedTruth)
}

// seedUpgradeParentEvidence creates two real generation records. Only the
// selected generation has the long parent; the other is a short decoy, so the
// upgrade must use the same generation scope as the estimator.
func seedUpgradeParentEvidence(conn *sqlite.Conn, sessionID, parentID, project, digest string, latestOnly bool) error {
	for _, generation := range []string{"other", "used"} {
		if err := sqlitex.Execute(conn, `INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version,
ts_start, ts_end, source_format, project_hash, project_name, host_slug, content_hash, metadata_hash,
redaction_applied, completeness, source_evidence_digest, index_format_version, candidate_digest, installed_at_ms)
VALUES (?, ?, 12, 'claude-code', 'synthetic-model', '', 1, 2, 'jsonl', ?, 'synthetic', 'synthetic-host', ?, ?, 0, 'complete', ?, 2, ?, 1)`,
			&sqlitex.ExecOptions{Args: []any{sessionID, generation, project, digest, digest, digest, digest}}); err != nil {
			return err
		}
	}
	var active any = "used"
	latest := "other"
	if latestOnly {
		active = nil
		latest = "used"
	}
	if err := sqlitex.Execute(conn, `UPDATE sessions SET active_generation_id = ?, latest_attempt_generation_id = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{active, latest, sessionID}}); err != nil {
		return err
	}
	return sqlitex.Execute(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id)
VALUES (?, 'other', 0, 'started_by', 'target_known', 'short-parent'), (?, 'used', 0, 'started_by', 'target_known', ?)`,
		&sqlitex.ExecOptions{Args: []any{sessionID, sessionID, parentID}})
}

// runUpgradeDiskIndexInventoryCase pins the modeled index columns to the
// actual schema after the frozen copy step. A new, removed, or changed index
// must not silently leave the preflight's key model behind.
func runUpgradeDiskIndexInventoryCase(t *testing.T, c storeUpgradeCase) {
	path := filepath.Join(t.TempDir(), "index-inventory.db")
	if c.StopAt != 0 && c.StopAt != c.From {
		t.Fatalf("the frozen target fixture must set from and stopAt to the same schema: %+v", c)
	}
	buildBehindStore(t, path, c.From, nil)
	stubFreeBytes(t, math.MaxInt64, true)
	if c.StopAt == 0 {
		if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
			t.Fatalf("upgrade synthetic store: %v", err)
		}
	}
	conn, err := openReadOnlyNoSidecars(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var names []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_index_list(?) ORDER BY name`, &sqlitex.ExecOptions{
		Args: []any{c.Table}, ResultFunc: func(stmt *sqlite.Stmt) error {
			names = append(names, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, name := range names {
		var columns []string
		if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, &sqlitex.ExecOptions{
			Args: []any{name}, ResultFunc: func(stmt *sqlite.Stmt) error {
				columns = append(columns, stmt.ColumnText(0))
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, strings.Join(columns, ","))
	}
	keys, indexes := upgradeIndexDefinitions(c.Table)
	var modeled []string
	for _, index := range indexes {
		var columns []string
		for _, i := range index {
			columns = append(columns, keys[i])
		}
		modeled = append(modeled, strings.Join(columns, ","))
	}
	slices.Sort(actual)
	slices.Sort(modeled)
	if len(actual) == 0 || !slices.Equal(actual, modeled) {
		t.Errorf("%s indexes after the upgrade = %v; disk estimate models %v", c.Table, actual, modeled)
	}
}
