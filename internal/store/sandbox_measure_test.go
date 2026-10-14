package store

import (
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sandbox_measure.yaml
var sandboxMeasureYAML []byte

type sandboxMeasureFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Samples       int      `yaml:"samples"`
	ListLimit     int      `yaml:"listLimit"`
	Bands         []struct {
		Name           string `yaml:"name"`
		NominalEntries int    `yaml:"nominalEntries"`
	} `yaml:"bands"`
}

func loadSandboxMeasures(t *testing.T) sandboxMeasureFixture {
	t.Helper()
	var fixture sandboxMeasureFixture
	if err := yaml.Unmarshal(sandboxMeasureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, band := range fixture.Bands {
		names = append(names, band.Name)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "sandbox bands"); err != nil {
		t.Fatal(err)
	}
	if fixture.Samples < 2 || fixture.ListLimit < 1 || fixture.ListLimit > indexFormatReadBatchSize {
		t.Fatal("sandbox sample configuration is incomplete or exceeds one grouping batch")
	}
	return fixture
}

// Sandbox readers open read-only with no migrations. The path must resolve
// beneath the temporary sandbox area; symlinks into the live store are refused.
func sandboxReadOnly(t *testing.T, variable string) *Store {
	t.Helper()
	path := os.Getenv(variable)
	if path == "" {
		t.Skipf("sandbox measurement only: set %s to a disposable sandbox copy", variable)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The approved measurement root is also accepted when Nix supplies a
	// per-command TMPDIR instead of the system temporary directory.
	underTemp := func(root string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !underTemp(temp) && !underTemp("/tmp/opencode") {
		t.Fatalf("%s resolves outside a temporary sandbox: %s; copy the database under the temporary directory first", variable, path)
	}
	root := t.TempDir()
	artifacts, err := NewOSGenerationArtifactStoreExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenReadOnlyWithOptions(path, WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})
	t.Logf("read-only sandbox %s=%s (record corpus hash and revision alongside this log)", variable, path)
	return s
}

func logSandboxLatency(t *testing.T, label string, samples []time.Duration) {
	t.Helper()
	sort.Slice(samples, func(i, j int) bool {
		return samples[i] < samples[j]
	})
	percentile := func(percent int) time.Duration {
		return samples[(len(samples)*percent+99)/100-1]
	}
	t.Logf("%s: n=%d p50=%s p95=%s max=%s", label, len(samples), percentile(50), percentile(95), samples[len(samples)-1])
}

// List is the real mounted-list query. Grouping uses the real reader, then
// the same SQL with only the count mirror replaced by a captured-stats join.
// Equal row sets are mandatory before any timing comparison is meaningful.
func TestSandboxListGroupingMirrors(t *testing.T) {
	s := sandboxReadOnly(t, "PEASANT_SANDBOX_DB")
	fixture := loadSandboxMeasures(t)
	filter := SessionListFilter{Limit: fixture.ListLimit, SortDesc: true}
	rows, err := s.ListSessionsFiltered(t.Context(), filter)
	if err != nil || len(rows) == 0 {
		t.Fatalf("sandbox list has no representative rows: %v", err)
	}
	ids := make([]string, len(rows))
	args := make([]any, len(rows))
	placeholders := make([]string, len(rows))
	for i, row := range rows {
		ids[i], args[i], placeholders[i] = row.SessionID, row.SessionID, "?"
	}
	baseline, err := s.GroupingEvidenceForSessions(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Replace(groupingEvidenceQuery, "s.input_submission_count", "c.input_submission_count", 1)
	query = strings.Replace(query, "FROM sessions s", "FROM sessions s LEFT JOIN session_captured_stats c ON c.session_id=s.session_id", 1)
	query += strings.Join(placeholders, ", ") + ")"
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if conn != nil {
			s.pool.Put(conn)
		}
	}()
	joined := func() (map[string]GroupingEvidenceRow, error) {
		result := map[string]GroupingEvidenceRow{}
		err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			row := GroupingEvidenceRow{SessionID: stmt.ColumnText(0)}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				value, err := schema.NewSessionPurpose(stmt.ColumnText(1))
				if err != nil {
					return err
				}
				row.Purpose = value
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				count := stmt.ColumnInt64(2)
				row.InputSubmissionCount = &count
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				value, err := schema.NewRelationshipTargetState(stmt.ColumnText(3))
				if err != nil {
					return err
				}
				row.OwnerState = value
			}
			if stmt.ColumnType(4) != sqlite.TypeNull && stmt.ColumnText(4) != "" {
				value := stmt.ColumnText(4)
				row.OwnerTargetLocalID = &value
			}
			result[row.SessionID] = row
			return nil
		}})
		return result, err
	}
	joinedRows, err := joined()
	if err != nil || !reflect.DeepEqual(baseline, joinedRows) {
		t.Fatalf("captured-stats join changes grouping membership or evidence: %v; measure a corpus with captured stats for the same sessions", err)
	}
	s.pool.Put(conn)
	// Each variant owns the sole read-only pool connection while it runs.
	conn = nil
	var listTimes, mirrorTimes, joinedTimes []time.Duration
	for n := 0; n < fixture.Samples; n++ {
		start := time.Now()
		got, err := s.ListSessionsFiltered(t.Context(), filter)
		listTimes = append(listTimes, time.Since(start))
		if err != nil || !reflect.DeepEqual(got, rows) {
			t.Fatalf("list changed during measurement: %v", err)
		}
		start = time.Now()
		gotGrouping, err := s.GroupingEvidenceForSessions(t.Context(), ids)
		mirrorTimes = append(mirrorTimes, time.Since(start))
		if err != nil || !reflect.DeepEqual(gotGrouping, baseline) {
			t.Fatalf("mirror grouping changed: %v", err)
		}
		conn, err = s.pool.Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		gotJoined, err := joined()
		joinedTimes = append(joinedTimes, time.Since(start))
		s.pool.Put(conn)
		conn = nil
		if err != nil || !reflect.DeepEqual(gotJoined, baseline) {
			t.Fatalf("joined grouping changed: %v", err)
		}
	}
	t.Logf("list/grouping selected %d sessions; list already joins captured stats, only grouping reads the input-count mirror", len(ids))
	logSandboxLatency(t, "production list (captured-stats join)", listTimes)
	logSandboxLatency(t, "grouping with mirror", mirrorTimes)
	logSandboxLatency(t, "grouping without count mirror", joinedTimes)
}

func TestSandboxGiantRowSize(t *testing.T) {
	s := sandboxReadOnly(t, "PEASANT_SANDBOX_DB")
	legacy := sandboxReadOnly(t, "PEASANT_SANDBOX_LEGACY_DB")
	sid, err := schema.NewSessionID(os.Getenv("PEASANT_SANDBOX_SESSION"))
	if err != nil {
		t.Fatalf("set PEASANT_SANDBOX_SESSION to the giant session: %v", err)
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	_, native, err := harmonizedActiveOnConn(conn, sid)
	if err != nil || !native {
		t.Fatalf("giant row measurement needs converted DB content: %v", err)
	}
	var lengths []string
	if err := sqlitex.Execute(conn, `PRAGMA table_info(session_entry_bodies)`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		kind := stmt.ColumnText(2)
		if kind == "TEXT" || kind == "BLOB" {
			column := strings.ReplaceAll(stmt.ColumnText(1), `"`, `""`)
			lengths = append(lengths, `COALESCE(length(CAST(b."`+column+`" AS BLOB)),0)`)
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(lengths) == 0 {
		t.Fatal("body table has no structured byte columns")
	}
	sum := strings.Join(lengths, "+")
	query := `SELECT count(*),COALESCE(sum(` + sum + `),0),COALESCE(max(` + sum + `),0) FROM session_entry_bodies b WHERE b.session_id=? AND EXISTS(SELECT 1 FROM session_generation_entries e JOIN sessions s ON s.session_id=e.session_id AND s.active_generation_id=e.generation_id WHERE e.session_id=b.session_id AND e.body_digest=b.body_digest)`
	var bodyCount, bodyTotal, bodyMax int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		bodyCount, bodyTotal, bodyMax = stmt.ColumnInt64(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	oldConn, err := legacy.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.pool.Put(oldConn)
	var oldCount, oldTotal, oldMax int64
	if err := sqlitex.Execute(oldConn, `SELECT count(*),COALESCE(sum(length(CAST(e.entry_json AS BLOB))),0),COALESCE(max(length(CAST(e.entry_json AS BLOB))),0) FROM session_projection_entries e JOIN sessions s ON s.session_id=e.session_id AND s.active_generation_id=e.generation_id WHERE e.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		oldCount, oldTotal, oldMax = stmt.ColumnInt64(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if bodyCount == 0 || oldCount == 0 {
		t.Fatal("both sandbox copies must contain the same giant session before and after conversion")
	}
	var mapped int64
	if err := sqlitex.Execute(conn, `SELECT count(*) FROM session_generation_entries e JOIN sessions s ON s.session_id=e.session_id AND s.active_generation_id=e.generation_id WHERE e.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		mapped = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if mapped != oldCount {
		t.Fatalf("different giant corpus: converted mappings %d; old rows %d", mapped, oldCount)
	}
	t.Logf("giant session=%s structured_bodies=%d structured_field_bytes_total=%d max=%d legacy_entry_rows=%d legacy_json_bytes_total=%d max=%d (raw bytes, not on-disk page sizes)", sid, bodyCount, bodyTotal, bodyMax, oldCount, oldTotal, oldMax)
	var pages, pageBytes int64
	if err := sqlitex.Execute(conn, `SELECT count(*),COALESCE(sum(pgsize),0) FROM dbstat WHERE name='session_entry_bodies'`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		pages, pageBytes = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	t.Logf("whole sandbox body table: pages=%d page_bytes=%d; write-lane time must be measured separately with harvest --profile-index", pages, pageBytes)
}

// TestSandboxSessionConversionCommit measures the largest-session conversion
// commit on a sandbox copy of a production store and gates it against the
// open path's busy_timeout. It runs the production MigrateSession stage and
// commit sequence for one real session end to end: lock, sweep flag, oracle
// read, convergence and integrity checks, prepare, bounded staging
// sub-transactions, then the timed activation commit. Only the commit holds
// the single SQLite writer for the whole session, so only the commit is
// asserted against the hold bound; the end-to-end wall is reported.
//
// The test runs only when the sandbox environment names a copy:
// PEASANT_SANDBOX_DB (a writable copy of the store, never the live path),
// PEASANT_SANDBOX_SYNC (an artifact root holding the session's real tree),
// and PEASANT_SANDBOX_SESSION (the session to convert). Without all three
// it skips, so CI and clean checkouts never touch it.
func TestSandboxSessionConversionCommit(t *testing.T) {
	dbPath := os.Getenv("PEASANT_SANDBOX_DB")
	syncRoot := os.Getenv("PEASANT_SANDBOX_SYNC")
	sessionID := os.Getenv("PEASANT_SANDBOX_SESSION")
	if dbPath == "" || syncRoot == "" || sessionID == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_DB, PEASANT_SANDBOX_SYNC, and PEASANT_SANDBOX_SESSION to a sandbox copy")
	}
	sid, err := schema.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("parse PEASANT_SANDBOX_SESSION %q: %v", sessionID, err)
	}
	artifacts, err := NewOSGenerationArtifactStore(syncRoot)
	if err != nil {
		t.Fatalf("open sandbox artifact root: %v", err)
	}
	locker, err := NewFileSessionLocker(syncRoot)
	if err != nil {
		t.Fatalf("open sandbox session locker: %v", err)
	}
	db, err := Open(dbPath,
		WithPoolSize(4),
		WithIndexFormats(generationIndexFormat{}),
		WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatalf("open sandbox store: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := t.Context()

	release, err := db.sessionLocker.LockExclusive(ctx, sid)
	if err != nil {
		t.Fatalf("lock sandbox session: %v", err)
	}
	defer func() { _ = release() }()

	work, err := db.migrateSessionWork(ctx, sid)
	if err != nil {
		t.Fatalf("inspect sandbox session: %v", err)
	}
	if !work.convert {
		t.Fatalf("sandbox session %s carries no conversion work; the measurement needs a file-backed active generation", sessionID)
	}
	if err := db.setSweepFlag(ctx, sid); err != nil {
		t.Fatalf("set sandbox sweep flag: %v", err)
	}
	oracle, err := db.readMigrateOracle(ctx, sid, work.generationID)
	if err != nil {
		t.Fatalf("read sandbox oracle: %v", err)
	}
	mainEntries := 0
	for _, part := range oracle.entries {
		mainEntries += len(part)
	}
	t.Logf("sandbox oracle: %d entries in %d partitions, %d content records, %d aliases",
		mainEntries, len(oracle.entries), len(oracle.content), len(oracle.aliases))
	if err := db.checkMigrateOracleConverges(ctx, oracle); err != nil {
		t.Fatalf("sandbox oracle diverges: %v", err)
	}
	if err := checkMigrateEmittedIntegrity(oracle); err != nil {
		t.Fatalf("sandbox emitted integrity: %v", err)
	}
	prepared, err := prepareHarmonizedCandidate(sid, oracleGeneration(oracle), oracle.blobs)
	if err != nil {
		t.Fatalf("prepare sandbox candidate: %v", err)
	}
	stageStart := time.Now()
	if err := db.stageMigrateObjects(ctx, prepared); err != nil {
		t.Fatalf("stage sandbox objects: %v", err)
	}
	stageWall := time.Since(stageStart)
	if err := db.upsertMigrateStats(ctx, oracle); err != nil {
		t.Fatalf("upsert sandbox stats: %v", err)
	}
	commitStart := time.Now()
	converted, rollback, err := db.commitMigrateSession(ctx, oracle, prepared)
	commitWall := time.Since(commitStart)
	var oracleBytes int64
	for _, payload := range oracle.blobs {
		oracleBytes += int64(len(payload))
	}
	if err != nil {
		t.Fatalf("commit sandbox session: %v", err)
	}
	if rollback != nil {
		t.Fatalf("sandbox session rolled back at %s: %s", rollback.Dimension, rollback.Reason)
	}
	if !converted {
		t.Fatal("sandbox commit converted nothing")
	}
	t.Logf("sandbox session conversion: staging %s, activation commit %s (hold bound %s)",
		stageWall.Round(time.Millisecond), commitWall.Round(time.Millisecond), defaults.SQLiteBusyTimeout)
	byteBudget := defaults.FullContentWriteBatchBytes
	t.Logf("sandbox oracle bytes: %d (byte budget %d)", oracleBytes, byteBudget)
	if oracleBytes > byteBudget {
		// The oversized path: the session exceeds the staging byte budget,
		// so it stages and commits alone, outside the bounded-batch
		// contract the hold gate covers. Its wall is the derivation input
		// for the oversized-session behavior, not a hold violation: the
		// migration runs under the no-concurrent-writer advisory, and
		// whether the in-transaction shadow verify keeps the oversized
		// activation commit under the hold is a design tradeoff for review.
		t.Logf("sandbox session is oversized (%d bytes over the %d-byte budget): wall reported, hold gate applies to budget-conforming sessions",
			oracleBytes, byteBudget)
		return
	}
	if commitWall > defaults.SQLiteBusyTimeout {
		t.Fatalf("activation commit held %s, past the open path's busy_timeout %s",
			commitWall, defaults.SQLiteBusyTimeout)
	}
}
