package store_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestListUncachedChildrenOfParentsIsTargetScoped proves the reverse
// reconciliation lookup reads the persisted logical-parent evidence of exactly
// the named targets: an unrelated stored root is never considered, a child
// whose cache is already populated is never returned, and the harness filter
// holds.
func TestListUncachedChildrenOfParentsIsTargetScoped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := store.Open(filepath.Join(t.TempDir(), "parent-reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	parentID := ingest.SessionID("11111111-1111-4111-8111-111111111111")
	unrelatedID := ingest.SessionID("22222222-2222-4222-8222-222222222222")
	childID := ingest.SessionID("33333333-3333-4333-8333-333333333333")
	unrelatedChild := ingest.SessionID("44444444-4444-4434-8434-444444444444")
	alreadyCachedID := ingest.SessionID("55555555-5555-4535-8535-555555555555")

	insert := func(id ingest.SessionID, harness ingest.Harness, logicalParent *ingest.SessionID) {
		t.Helper()
		meta := parentReconcileMeta(t, id, harness, logicalParent)
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{{
			Metadata:           &meta,
			PublicationCapture: true,
			CWDProvenance:      ingest.CWDSourceAbsent,
		}}); err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}
	}

	// Children first: their logical parents are absent, so each stores a nil
	// cache while its publication metadata snapshot keeps the logical parent.
	insert(childID, ingest.HarnessCodex, &parentID)
	insert(unrelatedChild, ingest.HarnessCodex, &unrelatedID)
	insert(parentID, ingest.HarnessCodex, nil)
	insert(unrelatedID, ingest.HarnessCodex, nil)
	// Stored while its parent is already available: the populated cache is
	// authoritative and this reverse pass must never return it.
	insert(alreadyCachedID, ingest.HarnessCodex, &parentID)

	got, err := db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{parentID}, []ingest.Harness{ingest.HarnessCodex})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents: %v", err)
	}
	want := []ingest.ParentCacheReconcile{{Child: childID, Parent: parentID}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targeted lookup = %+v, want exactly %+v (unrelated roots and populated caches must be excluded)", got, want)
	}

	got, err = db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{unrelatedID}, []ingest.Harness{ingest.HarnessCodex})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents(unrelated): %v", err)
	}
	want = []ingest.ParentCacheReconcile{{Child: unrelatedChild, Parent: unrelatedID}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unrelated-target lookup = %+v, want %+v", got, want)
	}

	got, err = db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{parentID}, []ingest.Harness{ingest.HarnessOpenCode})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents(opencode): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("harness filter returned %+v, want nothing for a codex-only child", got)
	}

	got, err = db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{parentID, unrelatedID}, []ingest.Harness{ingest.HarnessCodex})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents(multi): %v", err)
	}
	want = []ingest.ParentCacheReconcile{{Child: childID, Parent: parentID}, {Child: unrelatedChild, Parent: unrelatedID}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-target lookup = %+v, want %+v", got, want)
	}
}

func parentReconcileMeta(t *testing.T, id ingest.SessionID, harness ingest.Harness, logicalParent *ingest.SessionID) ingest.UnifiedMetadata {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.SessionID = id
	meta.HostSlug = ingest.HostSlug(testutil.TestHostSlug)
	meta.Model = testutil.TestModel
	meta.ModelHarness = harness
	ingested := int64(1700000001000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: ingested, Ingested: &ingested}
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "synthetic-reconcile"}
	meta.Source = ingest.SourceInfo{Format: ingest.SourceFormatJSONL, FilePath: "/synthetic/reconcile.jsonl"}
	meta.ParentUUID = logicalParent
	body := []byte("synthetic reconcile transcript " + string(id))
	meta.ContentHash = schema.ComputeTranscriptHash(body)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	return meta
}
