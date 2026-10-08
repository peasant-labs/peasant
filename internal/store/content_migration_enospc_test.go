package store

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_enospc.yaml
var contentEnospcYAML []byte

//go:embed testdata/content_enospc.manifest.yaml
var contentEnospcManifestYAML []byte

// contentEnospcMigrationCase is one migration-owned content_enospc case:
// the section-10 name plus the migration step the runner drives under a
// real SQLITE_FULL.
type contentEnospcMigrationCase struct {
	Name  string `yaml:"name"`
	Owner string `yaml:"owner,omitempty"`
	Step  string `yaml:"step,omitempty"`
}

// loadContentEnospcMigrationCases strictly decodes the disk-full family
// and returns the migration-owned cases with their steps. The
// required-name manifest is enforced here over the full six-name family
// — including the cases other owners fill later — so the inventory keeps
// its deletion protection now that the scaffold loader is retired.
func loadContentEnospcMigrationCases(t *testing.T) []contentEnospcMigrationCase {
	t.Helper()
	var fixture struct {
		Cases []contentEnospcMigrationCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contentEnospcYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode content_enospc.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentEnospcManifestYAML)
	if err != nil {
		t.Fatalf("decode content_enospc manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Fatal("content_enospc.yaml: a case has a blank name")
		}
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content disk-full"); err != nil {
		t.Fatal(err)
	}
	var owned []contentEnospcMigrationCase
	for _, c := range fixture.Cases {
		if c.Owner == "migration" {
			owned = append(owned, c)
		}
	}
	return owned
}

// TestContentMigrationENOSPC runs the migration-owned disk-full cases: a
// real SQLITE_FULL at each migration step leaves the old state intact
// with orphans flagged, and the error carries the actionable parts.
func TestContentMigrationENOSPC(t *testing.T) {
	for _, c := range loadContentEnospcMigrationCases(t) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			switch c.Step {
			case "body-subtxn":
				runMigrateBodySubtxnFull(t)
			case "catalog":
				runMigrateCatalogFull(t)
			case "search-consolidation":
				runMigrateSearchConsolidationFull(t)
			default:
				t.Fatalf("unknown enospc step %q", c.Step)
			}
		})
	}
}

// capMigratePages caps the database just above its current page count so
// the next growth fails with a real SQLITE_FULL, returning the restore
// function that lifts the cap. The cap is per-connection, so it goes on
// every pooled checkout: whatever checkout the step runs on enforces it.
// A VACUUM first empties the freelist, so reused pages cannot satisfy the
// growth without allocating. The restore lifts the cap on both checkouts.
func capMigratePages(t *testing.T, s *Store, extra int64) func() {
	t.Helper()
	setCap := func(vacuum bool) {
		t.Helper()
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer s.pool.Put(conn)
		if vacuum {
			if err := sqlitex.ExecuteTransient(conn, `VACUUM`, nil); err != nil {
				t.Fatalf("vacuum before the cap: %v", err)
			}
		}
		var current int64
		if err := sqlitex.ExecuteTransient(conn, `PRAGMA page_count`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				current = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`PRAGMA max_page_count = %d`, current+extra), nil); err != nil {
			t.Fatal(err)
		}
	}
	setCap(true)
	setCap(false)
	return func() {
		for i := 0; i < 2; i++ {
			conn, err := s.pool.Take(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.ExecuteTransient(conn, `PRAGMA max_page_count = 1073741823`, nil); err != nil {
				s.pool.Put(conn)
				t.Fatalf("lift the page cap: %v", err)
			}
			s.pool.Put(conn)
		}
	}
}

// seedMigrateEnospcSession seeds one clean file-backed session for the
// disk-full cases onto the caller's store.
func seedMigrateEnospcSession(t *testing.T, s *Store, root string) (schema.SessionID, string) {
	t.Helper()
	sid, err := schema.NewSessionID("e4e4e4e4-e4e4-44e4-84e4-e4e4e4e4e4e4")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_enospc"
	text, input, output := migrateSeedTexts(genID)
	v2, blobs := buildTestGeneration(t, sid, genID, text, input, output)
	stampSeedMetadataHash(&v2)
	seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
	return sid, genID
}

// seedMigrateEnospcBigSession seeds one file-backed session with count
// small text entries: the catalog commit and the search rebuild both
// must grow the database file, so a page cap fails them deterministically
// instead of fitting inside already-allocated pages.
func seedMigrateEnospcBigSession(t *testing.T, s *Store, root string, count int) (schema.SessionID, string) {
	t.Helper()
	sid, err := schema.NewSessionID("e5e5e5e5-e5e5-45e5-85e5-e5e5e5e5e5e5")
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	genID := "gen_migrate_enospc_big"
	entries := make([]schema.SessionEntry, 0, count)
	content := make([]indexformat.ContentRecord, 0, count)
	blobs := make(map[schema.SourceEntryRef][]byte, count)
	for i := 0; i < count; i++ {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_big_%04d", i))
		text := fmt.Sprintf("big entry %04d text ", i) + strings.Repeat("x", 100)
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: schema.Harness("claude-code"),
			EntryType: schema.EntryTypeText, Role: schema.RoleUser,
			ContentPreview: &text, SourceEntryRef: ref,
		})
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
	inputCount := int64(1)
	v2 := indexformat.V2{Generation: indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.Harness("claude-code"),
			Stats:         schema.SessionStats{TurnCount: count, InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{"e_big_0000"},
	}}
	stampSeedMetadataHash(&v2)
	seedFileBackedGeneration(t, s, root, sid, v2, blobs, true)
	return sid, genID
}

// assertMigrateFullError proves the production error path: a real
// SQLITE_FULL carrying the actionable parts.
func assertMigrateFullError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("disk-full step succeeded; want SQLITE_FULL")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "disk is full") && !strings.Contains(err.Error(), "SQLITE_FULL") {
		t.Fatalf("error is not the disk-full path: %v", err)
	}
}

// runMigrateBodySubtxnFull caps the database before staging: the body
// write fails with SQLITE_FULL, the old representation is intact, and
// the sweep flag stays set for the orphan sweep.
func runMigrateBodySubtxnFull(t *testing.T) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid, genID := seedMigrateEnospcSession(t, s, root)
	restore := capMigratePages(t, s, 0)
	defer restore()
	outcome, err := s.MigrateSession(context.Background(), sid)
	assertMigrateFullError(t, err)
	if outcome != "" {
		t.Fatalf("outcome = %q; want no disposition on a disk-full halt", outcome)
	}
	assertMigrateOldIntact(t, s, root, sid, genID)
	if flag := queryMigrateInt(t, s, `SELECT content_sweep_pending FROM sessions WHERE session_id = '`+string(sid)+`'`); flag != 1 {
		t.Fatalf("flag = %d; want it set for the orphan sweep", flag)
	}
}

// runMigrateCatalogFull stages without a cap, then caps before the
// catalog transaction: the commit fails with SQLITE_FULL, the atomic
// transaction leaves the old representation intact, and the flag stays
// set. The re-run after lifting the cap converts.
func runMigrateCatalogFull(t *testing.T) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid, genID := seedMigrateEnospcBigSession(t, s, root, 1500)
	ctx := context.Background()
	release, err := s.sessionLocker.LockExclusive(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	work, err := s.migrateSessionWork(ctx, sid)
	if err != nil || !work.convert {
		_ = release()
		t.Fatalf("work = %+v, err = %v; want conversion work", work, err)
	}
	if err := s.setSweepFlag(ctx, sid); err != nil {
		_ = release()
		t.Fatal(err)
	}
	oracle, err := s.readMigrateOracle(ctx, sid, work.generationID)
	if err != nil {
		_ = release()
		t.Fatal(err)
	}
	prepared, err := prepareHarmonizedCandidate(sid, oracleGeneration(oracle), oracle.blobs)
	if err != nil {
		_ = release()
		t.Fatal(err)
	}
	if err := s.stageMigrateObjects(ctx, prepared); err != nil {
		_ = release()
		t.Fatal(err)
	}
	if err := s.upsertMigrateStats(ctx, oracle); err != nil {
		_ = release()
		t.Fatal(err)
	}
	restore := capMigratePages(t, s, 0)
	converted, mismatch, err := s.commitMigrateSession(ctx, oracle, prepared)
	restore()
	assertMigrateFullError(t, err)
	if converted || mismatch != nil {
		_ = release()
		t.Fatalf("converted = %v, mismatch = %+v; want a failed commit", converted, mismatch)
	}
	assertMigrateOldIntact(t, s, root, sid, genID)
	// The re-run needs the session lock free: release before converting.
	if err := release(); err != nil {
		t.Fatalf("release the catalog-full lock: %v", err)
	}
	if outcome, err := s.MigrateSession(ctx, sid); err != nil || outcome != MigrateOutcomeConverted {
		t.Fatalf("re-run outcome = %q, err = %v; want converted", outcome, err)
	}
	assertSessionConverted(t, s, root, sid, genID)
}

// runMigrateSearchConsolidationFull converts, empties the consolidated
// index, then caps before the rebuild: the one transaction fails with
// SQLITE_FULL, the retired index stays present with its triggers and no
// partial postings, and lifting the cap consolidates. The emptying is
// required, not incidental: a rebuild deletes and re-inserts the same
// volume, so it reuses its own freed pages and a page cap alone can
// never fail it; with the index emptied and vacuumed the rebuild must
// allocate and the cap fires.
func runMigrateSearchConsolidationFull(t *testing.T) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid, _ := seedMigrateEnospcBigSession(t, s, root, 1500)
	_ = root
	if outcome, err := s.MigrateSession(context.Background(), sid); err != nil || outcome != MigrateOutcomeConverted {
		t.Fatalf("convert outcome = %q, err = %v; want converted", outcome, err)
	}
	emptySearchIndex(t, s)
	restore := capMigratePages(t, s, 0)
	consolidated, err := s.migrateConsolidateSearch(context.Background())
	restore()
	if err == nil && !consolidated {
		t.Fatalf("consolidation skipped under the cap (want SQLITE_FULL): the retired index state is unexpected")
	}
	assertMigrateFullError(t, err)
	if consolidated {
		t.Fatal("consolidation reported success under SQLITE_FULL")
	}
	check, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(check)
	if !tableExistsOnConn(check, "session_entries_fts") {
		t.Fatal("the old index is gone after a failed consolidation; the transaction must roll back")
	}
	assertSearchIndexMatchesNothing(t, s, "after the failed rebuild")
	consolidated, err = s.migrateConsolidateSearch(context.Background())
	if err != nil || !consolidated {
		t.Fatalf("consolidate after lifting the cap = %v, err = %v; want success", consolidated, err)
	}
}

// emptySearchIndex clears the consolidated index's postings through a
// direct delete, which fires no body trigger: the bodies stay present
// but unindexed until the next rebuild re-tokenizes them. A MATCH query
// proves the emptiness: bare scans of an external-content index read
// through the content mapping and cannot observe it.
func emptySearchIndex(t *testing.T, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_search_fts`, nil); err != nil {
		t.Fatalf("empty the consolidated index: %v", err)
	}
	assertSearchIndexMatchesNothing(t, s, "after the emptying delete")
}

// assertSearchIndexMatchesNothing proves a MATCH query finds no rows:
// the observable emptiness of an external-content index. It names the
// stage so a failure says which step left postings behind.
func assertSearchIndexMatchesNothing(t *testing.T, s *Store, stage string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id FROM session_search_fts WHERE session_search_fts MATCH 'big' LIMIT 1`, &sqlitex.ExecOptions{
		ResultFunc: func(*sqlite.Stmt) error {
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("match the consolidated index %s: %v", stage, err)
	}
	if found {
		t.Fatalf("the consolidated index still matches %s; the transaction must roll back whole", stage)
	}
}
