package store

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestParentCacheReverseLookupSeeksTargetIndexes is the query-plan/scaling
// guard for the reverse logical-target lookup.
//
// The two production statements are explained directly (production and its
// guard share parentCacheLookupSQL), and the plans must drive from the named
// target list and seek the target indexes. A restored harness-population scan
// changes the plan and fails the first guarded row and the index assertion.
// Seeding a large unrelated library must not change the plan at all: the
// target-driven statement reads nothing per unrelated stored session.
func TestParentCacheReverseLookupSeeksTargetIndexes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(filepath.Join(t.TempDir(), "parent-reconcile-plan.db"), WithPoolSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)

	harnesses := []ingest.Harness{ingest.HarnessCodex, ingest.HarnessOpenCode}
	harnessArgs := []any{"codex", "opencode"}
	const (
		libraryTarget = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		absentTarget  = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	legacySQL := parentCacheLookupSQL(parentCacheLegacyLookupTemplate, len(harnesses))
	durableSQL := parentCacheLookupSQL(parentCacheDurableLookupTemplate, len(harnesses))

	explain := func(sql string, target string) []string {
		t.Helper()
		args := append([]any{fmt.Sprintf(`[%q]`, target)}, harnessArgs...)
		var rows []string
		if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+sql, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = append(rows, stmt.ColumnText(3))
				return nil
			},
		}); err != nil {
			t.Fatalf("explain %q: %v", sql, err)
		}
		if len(rows) == 0 {
			t.Fatalf("explain %q produced no plan rows", sql)
		}
		return rows
	}

	// Seed a large unrelated library through the real write path. Every root
	// retains the library target as its legacy logical parent, so the old
	// harness-scan query would evaluate all of them for any target.
	const librarySize = 512
	library := make([]ingest.StoreEntry, 0, librarySize)
	for i := 0; i < librarySize; i++ {
		id := ingest.SessionID(fmt.Sprintf("eeeeeeee-eeee-4eee-8eee-%012d", i))
		parent := ingest.SessionID(libraryTarget)
		meta := planLibraryMeta(id, ingest.HarnessCodex, &parent)
		library = append(library, ingest.StoreEntry{
			Metadata:           &meta,
			PublicationCapture: true,
			CWDProvenance:      ingest.CWDSourceAbsent,
		})
	}
	if err := db.InsertSessions(ctx, library); err != nil {
		t.Fatalf("seed unrelated library: %v", err)
	}

	legacyPlan := explain(legacySQL, absentTarget)
	durablePlan := explain(durableSQL, absentTarget)

	assertTargetDrivenPlan(t, "legacy", legacyPlan, "idx_session_publication_parent_uuid")
	assertTargetDrivenPlan(t, "durable", durablePlan, "idx_relationship_evidence_started_by_target")

	// The plan must be independent of the unrelated library size: the seeded
	// books and the empty database must choose the identical loop nest.
	if empty, seeded := explainPlansOnEmptyStore(t, legacySQL, durableSQL, harnessArgs), map[string][]string{
		"legacy":  legacyPlan,
		"durable": durablePlan,
	}; !reflect.DeepEqual(empty["legacy"], seeded["legacy"]) || !reflect.DeepEqual(empty["durable"], seeded["durable"]) {
		t.Fatalf("reverse lookup plan depends on the unrelated library:\nlegacy empty=%v seeded=%v\ndurable empty=%v seeded=%v",
			empty["legacy"], seeded["legacy"], empty["durable"], seeded["durable"])
	}

	// Behavioral control: the fixed absent target still returns nothing with
	// the library present, and the library target resolves through the index.
	updates, err := db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{ingest.SessionID(absentTarget)}, harnesses)
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents(absent): %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("absent target returned %d updates, want none", len(updates))
	}
	updates, err = db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{ingest.SessionID(libraryTarget)}, harnesses)
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents(library): %v", err)
	}
	if len(updates) != librarySize {
		t.Fatalf("library target returned %d updates, want %d", len(updates), librarySize)
	}
}

// assertTargetDrivenPlan requires the target list to be the outer loop and the
// named target index to carry every stored-evidence probe. A harness-population
// scan is refused by the first-row and no-scan assertions.
func assertTargetDrivenPlan(t *testing.T, label string, plan []string, index string) {
	t.Helper()
	if !strings.Contains(plan[0], "SCAN target") {
		t.Errorf("%s lookup outer loop is not the named target list; plan=%v", label, plan)
	}
	indexSeek := false
	for _, row := range plan {
		if strings.Contains(row, "USING INDEX "+index) {
			indexSeek = true
		}
		if strings.Contains(row, "USING INDEX idx_sessions_harness") {
			t.Errorf("%s lookup still drives from the harness index: %q", label, row)
		}
		if strings.HasPrefix(row, "SCAN ") && !strings.Contains(row, "target") {
			t.Errorf("%s lookup scans a stored table instead of seeking by target: %q", label, row)
		}
	}
	if !indexSeek {
		t.Errorf("%s lookup does not seek %s; plan=%v", label, index, plan)
	}
}

// explainPlansOnEmptyStore explains both statements on a second, unseeded
// database. The plan shape must match the seeded database exactly.
func explainPlansOnEmptyStore(t *testing.T, legacySQL, durableSQL string, harnessArgs []any) map[string][]string {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "parent-reconcile-empty.db"), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)
	out := make(map[string][]string, 2)
	for label, sql := range map[string]string{"legacy": legacySQL, "durable": durableSQL} {
		args := append([]any{`["dddddddd-dddd-4ddd-8ddd-dddddddddddd"]`}, harnessArgs...)
		var rows []string
		if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+sql, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = append(rows, stmt.ColumnText(3))
				return nil
			},
		}); err != nil {
			t.Fatalf("explain %s on empty store: %v", label, err)
		}
		out[label] = rows
	}
	return out
}

// planLibraryMeta builds the minimal valid stored metadata for one unrelated
// library root. It is synthetic and carries no private content.
func planLibraryMeta(id ingest.SessionID, harness ingest.Harness, logicalParent *ingest.SessionID) ingest.UnifiedMetadata {
	meta := ingest.NewUnifiedMetadata()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.SessionID = id
	meta.HostSlug = ingest.HostSlug("github.com--synthetic--parent-reconcile-plan")
	meta.Model = ingest.ModelID("claude-opus-4-6")
	meta.ModelHarness = harness
	ingested := int64(1700000001000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: ingested, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{
		Hash: ingest.ProjectHash("abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345678"),
		Name: "synthetic-parent-reconcile-plan",
	}
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/synthetic/parent-reconcile-plan.jsonl"}
	meta.ParentUUID = logicalParent
	body := []byte("synthetic parent-reconcile-plan transcript " + string(id))
	meta.ContentHash = schema.ComputeTranscriptHash(body)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	return meta
}
