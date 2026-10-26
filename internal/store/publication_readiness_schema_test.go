package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// TestPublicationReadinessAcrossRefreshFreeSchemaBump pins the publication
// readiness rule to the declared refresh-free policy. A capture recorded at a
// schema this build reads as it stands stays ready after a schema bump; only a
// version this build must rebuild reads as needing a re-ingest. Before the rule
// was shared, an exact comparison against the current version flipped every
// stored capture to needs_ingest on a bump whose only differences were optional
// fields, which re-extracted the whole corpus on the next ordinary harvest.
func TestPublicationReadinessAcrossRefreshFreeSchemaBump(t *testing.T) {
	t.Parallel()
	current := int(ingest.CurrentSchemaVersion)
	older := current - 1
	if !ingest.MetadataSchemaVersionIsCurrent(older) {
		t.Skipf("schema %d is not readable by this build; no refresh-free predecessor to exercise", older)
	}
	if ingest.MetadataSchemaVersionIsCurrent(1) {
		t.Fatal("schema 1 reads as current; the unreadable-version control cannot be exercised")
	}
	s, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations(), store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	id := e.Metadata.SessionID
	revision := capturePublication(t, s, e)
	entries := contentEntries(id, contentCase{Prefix: "synthetic prose ", Repeats: 10000, Tail: "SAFE FULL TAIL", Entries: 2})
	if r := indexPublication(t, s, e, revision, entries); r.Err != nil {
		t.Fatal(r.Err)
	}

	// Every readiness reader must agree, so a selection driven by one of them
	// cannot disagree with the publication path that serves the capture.
	assertReadiness := func(want ingest.PublicationReadiness) {
		t.Helper()
		locations, err := s.BulkLookupSessionLocations(ctx, []ingest.SessionID{id})
		if err != nil {
			t.Fatal(err)
		}
		if got := locations[id].PublicationReadiness; got != want {
			t.Fatalf("bulk location readiness = %s, want %s", got, want)
		}
		projection, err := s.LoadPublicationMetadata(ctx, []ingest.SessionID{id})
		if err != nil {
			t.Fatal(err)
		}
		if got := projection[id].Readiness; got != want {
			t.Fatalf("publication projection readiness = %s, want %s", got, want)
		}
		bundle, err := s.LoadPublicationInput(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := bundle.Readiness; got != want {
			t.Fatalf("publication bundle readiness = %s, want %s", got, want)
		}
	}

	assertReadiness(ingest.PublicationReady)
	storetest.SetStoredMetadataSchema(t, s, id, older)
	assertReadiness(ingest.PublicationReady)
	storetest.SetStoredMetadataSchema(t, s, id, 1)
	assertReadiness(ingest.PublicationNeedsIngest)
}
