package store

import (
	"context"
	_ "embed"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harmonized_commit_failures.yaml
var harmonizedCommitFailureCases []byte

//go:embed testdata/harmonized_commit_failures_required.yaml
var harmonizedCommitFailureRequired []byte

func TestHarmonizedCommitFailuresWhitebox(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name      string `yaml:"name"`
			Operation string `yaml:"operation"`
		} `yaml:"cases"`
	}
	var manifest struct {
		Names []string `yaml:"names"`
	}
	if err := yaml.Unmarshal(harmonizedCommitFailureCases, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(harmonizedCommitFailureRequired, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("empty or duplicate fixture name %q", c.Name)
		}
		seen[c.Name] = true
		names = append(names, c.Name)
	}
	required := map[string]bool{}
	for _, name := range manifest.Names {
		if name == "" || required[name] {
			t.Fatalf("empty or duplicate required name %q", name)
		}
		required[name] = true
	}
	sort.Strings(names)
	sort.Strings(manifest.Names)
	if !reflect.DeepEqual(names, manifest.Names) {
		t.Fatalf("case names %v differ from required names %v", names, manifest.Names)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Operation == "migrate-body-flush" {
				testMigrationBodyFlushFailure(t)
				return
			}
			if c.Operation == "nondefect-shadow-mismatch" || c.Operation == "migrate-catalog-commit" {
				testMigrationCatalogRollback(t, c.Operation == "migrate-catalog-commit")
				return
			}
			ctx := context.Background()
			s := openSyntheticStore(t, filepath.Join(t.TempDir(), "store.db"))
			var held *sqlite.Conn
			t.Cleanup(func() {
				if held != nil {
					s.pool.Put(held)
				}
				closeSyntheticStore(t, s)
			})
			sid := schema.SessionID("d3d3d3d3-d3d3-43d3-83d3-d3d3d3d3d3d3")
			seedSearchRebuildSession(t, s, string(sid))
			conn, err := s.pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = conn
			exec := func(q string, args ...any) {
				t.Helper()
				if err := sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{Args: args}); err != nil {
					t.Fatal(err)
				}
			}
			scalar := func(q string, args ...any) int64 {
				t.Helper()
				var result int64
				if err := sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{Args: args, ResultFunc: func(st *sqlite.Stmt) error { result = st.ColumnInt64(0); return nil }}); err != nil {
					t.Fatal(err)
				}
				return result
			}
			record := EntryRecord{}
			if c.Operation == "repair-objects" {
				text := "damaged-before-repair"
				record.ContentPreview = &text
			}
			digest := string(bodyDigestForRecord(record))
			switch c.Operation {
			case "sweep-bodies", "repair-objects":
				if err := insertStagedBodyOnConn(conn, sid, &record); err != nil {
					t.Fatal(err)
				}
			case "sweep-blobs":
				exec(`INSERT INTO session_content(session_id,digest,byte_length) VALUES(?,?,1)`, string(sid), strings.Repeat("a", 64))
				exec(`INSERT INTO session_content_chunks(session_id,digest,chunk_index,data) VALUES(?,?,0,?)`, string(sid), strings.Repeat("a", 64), []byte("x"))
			case "skip-bookkeeping":
				exec(`UPDATE sessions SET active_generation_id = ? WHERE session_id = ?`, "generation", string(sid))
			case "consolidate-search":
				s.pool.Put(conn)
				held = nil
				seedLegacySearchIndex(t, s)
				conn, err = s.pool.Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				held = conn
			}
			var query string
			switch c.Operation {
			case "stage-batch":
				query = `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`
			case "migrate-stats", "skip-bookkeeping":
				query = `SELECT COUNT(*) FROM session_captured_stats WHERE session_id = ?`
			case "sweep-bodies", "repair-objects":
				query = `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`
			case "sweep-blobs":
				query = `SELECT COUNT(*) FROM session_content_chunks WHERE session_id = ?`
			case "consolidate-search":
				query = `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_entries_fts' AND ? IS NOT NULL`
			case "retire-projections":
				query = `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('session_projection_generations','session_projection_entries','session_projection_content') AND ? IS NOT NULL`
			default:
				t.Fatalf("unknown operation %q", c.Operation)
			}
			before := scalar(query, string(sid))
			denied := 0
			if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(a sqlite.Action) sqlite.AuthResult {
				if a.Type() == sqlite.OpTransaction && a.Operation() == "COMMIT" {
					denied++
					return sqlite.AuthResultDeny
				}
				return sqlite.AuthResultOK
			})); err != nil {
				t.Fatal(err)
			}
			s.pool.Put(conn)
			held = nil
			prepared := &preparedHarmonized{sessionID: sid}
			var opErr error
			switch c.Operation {
			case "stage-batch":
				conn, err = s.pool.Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				held = conn
				opErr = s.stageBatchTxn(conn, stageBatch{sessions: []schema.SessionID{sid}, units: []batchUnit{{session: sid, unit: stageUnit{body: &record}}}})
				s.pool.Put(conn)
				held = nil
			case "migrate-stats":
				opErr = s.upsertMigrateStats(ctx, &migrateOracle{sessionID: sid, statsRaw: CapturedStats{Source: StatsSourceHarness}, statsCaptureTime: 42})
			case "sweep-bodies":
				_, opErr = s.deleteVerifiedBodies(ctx, sid, []string{digest})
			case "sweep-blobs":
				_, opErr = s.deleteUnreferencedBlobs(ctx, sid, []string{strings.Repeat("a", 64)})
			case "consolidate-search":
				_, opErr = s.migrateConsolidateSearch(ctx)
			case "retire-projections":
				opErr = s.DropRetiredProjectionTables(ctx)
			case "skip-bookkeeping":
				opErr = stampSkippedHarmonized(ctx, s, prepared, "generation", skipStamps{seedJSON: "{}"})
			case "repair-objects":
				text := "repaired-body"
				replacement := record
				replacement.ContentPreview = &text
				prepared.bodies = []EntryRecord{replacement}
				prepared.bodyDigests = []BodyDigest{BodyDigest(digest)}
				opErr = repairHarmonizedObjects(ctx, s, prepared, "generation", skipStamps{seedJSON: "{}"})
			}
			if denied != 1 || opErr == nil || !strings.Contains(opErr.Error(), "authoriz") {
				t.Fatalf("COMMIT denials = %d, error = %v; want one denied COMMIT returned", denied, opErr)
			}
			conn, err = s.pool.Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = conn
			if err := conn.SetAuthorizer(nil); err != nil {
				t.Fatal(err)
			}
			if after := scalar(query, string(sid)); after != before {
				t.Errorf("rollback state = %d, before = %d", after, before)
			}
			if c.Operation == "stage-batch" && scalar(`SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(sid)) != 0 {
				t.Error("staged body survived rollback")
			}
			if c.Operation == "repair-objects" && scalar(`SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ? AND content_preview = 'damaged-before-repair'`, string(sid)) != 1 {
				t.Error("repair changed the damaged body despite rollback")
			}
			if c.Operation == "repair-objects" && scalar(`SELECT needs_rebuild FROM session_search_state WHERE id=1 AND ? IS NOT NULL`, string(sid)) != 0 {
				t.Error("repair rebuild flag survived rollback")
			}
		})
	}
}

// The stage seam fires after the first body batch has committed and released
// its connection. Reinstalling the authorizer there invalidates cached COMMIT
// statements, so the next batch fails at COMMIT rather than the blob batch.
func testMigrationBodyFlushFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := openGenerationStoreWith(t, WithPoolSize(1), tinyStageBudget())
	sid := migrateCaseSessionID(t, "commit-body-flush", 0)
	seedGenerationSession(t, s, string(sid))
	first, second := "first staged body", "second staged body"
	records := []EntryRecord{{ContentPreview: &first}, {ContentPreview: &second}}
	denied := 0
	armMigrateSeamOnce(t, "stage", func(_ *sqlite.Conn, _ string) error {
		conn, err := s.pool.Take(ctx)
		if err != nil {
			return err
		}
		defer s.pool.Put(conn)
		return conn.SetAuthorizer(sqlite.AuthorizeFunc(func(a sqlite.Action) sqlite.AuthResult {
			if a.Type() == sqlite.OpTransaction && a.Operation() == "COMMIT" {
				denied++
				return sqlite.AuthResultDeny
			}
			return sqlite.AuthResultOK
		}))
	})
	err := s.stageMigrateObjects(ctx, &preparedHarmonized{sessionID: sid, bodies: records})
	if denied != 1 || err == nil || !strings.Contains(err.Error(), "authoriz") {
		t.Fatalf("COMMIT denials = %d, error = %v; want the second body batch refused", denied, err)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var firstCount, secondCount int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FILTER (WHERE content_preview = ?), COUNT(*) FILTER (WHERE content_preview = ?) FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{first, second, string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			firstCount, secondCount = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if firstCount != 1 || secondCount != 0 {
		t.Errorf("body counts = %d, %d; want the first batch committed and the second rolled back", firstCount, secondCount)
	}
}

// A data mismatch is returned without an error, but must still undo the
// catalog writes made before verification and retain the legacy generation.
func testMigrationCatalogRollback(t *testing.T, denyCommit bool) {
	ctx := context.Background()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, "commit-shadow-rollback", 0)
	seedGenerationSession(t, s, string(sid))
	const generation = "gen_shadow_rollback"
	seedMigrateProfile(t, s, root, sid, "clean", generation)
	oracle, err := s.readMigrateOracle(ctx, sid, generation)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareHarmonizedCandidate(sid, oracleGeneration(oracle), oracle.blobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.stageMigrateObjects(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	denied := 0
	if denyCommit {
		armMigrateSeamOnce(t, "shadow", func(conn *sqlite.Conn, _ string) error {
			return conn.SetAuthorizer(sqlite.AuthorizeFunc(func(a sqlite.Action) sqlite.AuthResult {
				if a.Type() == sqlite.OpTransaction && a.Operation() == "COMMIT" {
					denied++
					return sqlite.AuthResultDeny
				}
				return sqlite.AuthResultOK
			}))
		})
	} else {
		applyMigrateDamage(t, s, root, sid, generation, "aux-json")
	}
	committed, mismatch, err := s.commitMigrateSession(ctx, oracle, prepared)
	if denyCommit {
		if denied != 1 || err == nil || !strings.Contains(err.Error(), "authoriz") || mismatch != nil {
			t.Fatalf("commit = %v, mismatch = %+v, error = %v, denied = %d; want denied COMMIT", committed, mismatch, err, denied)
		}
	} else if err != nil || committed || mismatch == nil || mismatch.Defect {
		t.Fatalf("commit = %v, mismatch = %+v, error = %v; want non-defect mismatch and nil error", committed, mismatch, err)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	for _, table := range []string{"session_projection_generations", "session_projection_entries", "session_projection_sections"} {
		var count int64
		err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM `+table+` WHERE session_id=? AND generation_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid), generation}, ResultFunc: func(st *sqlite.Stmt) error { count = st.ColumnInt64(0); return nil }})
		if err != nil || count == 0 {
			t.Errorf("legacy %s count = %d, error = %v; want retained rows", table, count, err)
		}
	}
	var count int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM session_generations WHERE session_id=? AND generation_id=?`, &sqlitex.ExecOptions{Args: []any{string(sid), generation}, ResultFunc: func(st *sqlite.Stmt) error { count = st.ColumnInt64(0); return nil }}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("harmonized catalog row survived mismatch: %d", count)
	}
}
