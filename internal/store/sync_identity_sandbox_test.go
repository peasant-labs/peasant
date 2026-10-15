package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// This local-only shape comparison reuses the sandbox sample configuration.
// It creates no corpus copy, writer, or timing CI gate. Classification and raw
// membership must agree before the local latency budget informs the choice.
func TestSandboxSyncParentIdentities(t *testing.T) {
	s := sandboxSyncIdentityReader(t)
	fixture := loadSandboxMeasures(t)
	info, err := os.Stat(os.Getenv("PEASANT_SANDBOX_DB"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus bytes=%d mtime=%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.ExecuteTransient(conn, `SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM session_metrics), (SELECT count(*) FROM session_relationship_evidence), (SELECT count(*) FROM session_publication_metadata), (SELECT count(*) FROM session_publication_metadata WHERE json_extract(metadata_json,'$.parentUuid') IS NOT NULL)`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		t.Logf("corpus sessions=%d metrics=%d evidence=%d snapshots=%d snapshot_parents=%d", stmt.ColumnInt64(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2), stmt.ColumnInt64(3), stmt.ColumnInt64(4))
		return nil
	}})
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.AllPushableSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := s.SyncParentIdentities(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != len(identities) {
		t.Fatal("identity read changed pushable membership")
	}
	sortRows := func(rows []ParentIdentityRow) {
		sort.Slice(rows, func(i, j int) bool { return rows[i].SessionID < rows[j].SessionID })
	}
	sortRows(identities)
	var baseTimes, derivedTimes, channelTimes []time.Duration
	for n := 0; n < fixture.Samples; n++ {
		start := time.Now()
		gotBase, err := s.AllPushableSessions(t.Context())
		baseTimes = append(baseTimes, time.Since(start))
		if err != nil || !reflect.DeepEqual(gotBase, base) {
			t.Fatalf("baseline changed: %v", err)
		}
		start = time.Now()
		got, err := sandboxIdentityOneShot(t.Context(), s)
		derivedTimes = append(derivedTimes, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
		sortRows(got)
		if !reflect.DeepEqual(got, identities) {
			t.Fatal("one-shot changes identity membership or classification")
		}
		start = time.Now()
		channels, err := s.SyncParentIdentities(t.Context(), base)
		channelTimes = append(channelTimes, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
		sortRows(channels)
		if !reflect.DeepEqual(channels, identities) {
			t.Fatal("channel identities changed")
		}
	}
	logSandboxLatency(t, "base pushable read", baseTimes)
	logSandboxLatency(t, "added one-shot identity read", derivedTimes)
	logSandboxLatency(t, "added production channel fallback (snapshot scan + evidence batches + raw merge)", channelTimes)
}

// The identity corpus predates unrelated migrations. These production readers
// need only the existing columns, so measure them on a read-only pool without
// invoking the normal open/migrate path or changing the corpus schema.
func sandboxSyncIdentityReader(t *testing.T) *Store {
	t.Helper()
	path := os.Getenv("PEASANT_SANDBOX_DB")
	if path == "" {
		t.Skip("local measurement only: set PEASANT_SANDBOX_DB to the approved sandbox copy")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel("/tmp/opencode", path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatal("identity measurement requires a resolved database under /tmp/opencode; no live paths are accepted")
	}
	pool, err := sqlitex.NewPool(path, sqlitex.PoolOptions{Flags: sqlite.OpenReadOnly, PoolSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return &Store{pool: pool}
}

// Retained only for the local comparison against the chosen channel shape.
func sandboxIdentityOneShot(ctx context.Context, s *Store) ([]ParentIdentityRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	var rows []ParentIdentityRow
	err = sqlitex.ExecuteTransient(conn, `SELECT s.session_id,
CASE
 WHEN r.kind IS NOT NULL THEN CASE
   WHEN r.target_state IN ('target_known','target_known_retained') THEN r.target_local_id
   ELSE NULL END
 WHEN x.kind IS NOT NULL THEN NULL
 ELSE COALESCE(NULLIF(json_extract(pm.metadata_json, '$.parentUuid'), ''), NULLIF(s.parent_id, ''))
END,
CASE WHEN r.target_state IN ('unknown','conflicting_current_native_evidence') THEN 1 ELSE 0 END
FROM sessions s
JOIN session_metrics m ON m.session_id = s.session_id
LEFT JOIN session_relationship_evidence r
 ON r.session_id = s.session_id AND r.generation_id = `+parentGenerationScopeSQL("s")+` AND r.kind = 'started_by'
LEFT JOIN session_relationship_evidence x
 ON x.session_id = s.session_id AND x.generation_id = `+parentGenerationScopeSQL("s")+` AND x.kind = 'context_from'
LEFT JOIN session_publication_metadata pm ON pm.session_id = s.session_id`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		rows = append(rows, ParentIdentityRow{SessionID: stmt.ColumnText(0), ParentIdentity: stmt.ColumnText(1), ParentUnresolved: stmt.ColumnInt(2) != 0})
		return nil
	}})
	return rows, err
}
