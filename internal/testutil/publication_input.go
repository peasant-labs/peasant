package testutil

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

var _ ingest.PublicationInputReader = (*StubPushStore)(nil)

func (s *StubPushStore) LoadPublicationInput(ctx context.Context, id ingest.SessionID) (ingest.PublicationInputBundle, error) {
	s.mu.Lock()
	s.PublicationInputCalls++
	s.mu.Unlock()
	entries, err := s.ListEntries(ctx, id)
	if err != nil {
		return ingest.PublicationInputBundle{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PublicationInputErr != nil {
		return ingest.PublicationInputBundle{}, s.PublicationInputErr
	}
	if s.GetQualityMetricsErr != nil {
		return ingest.PublicationInputBundle{}, s.GetQualityMetricsErr
	}
	if s.AssociationsErr != nil {
		return ingest.PublicationInputBundle{}, s.AssociationsErr
	}
	input := s.PublicationInputs[id]
	input.Entries = entries
	input.Quality = s.Metrics[id]
	for _, association := range s.Associations[id] {
		input.Associations = append(input.Associations, schema.PublishedAssociation{ID: association.ID, ObservedCommitHash: association.ObservedCommitHash})
	}
	return input, nil
}

var _ ingest.PublicationMetadataReader = (*StubPushStore)(nil)

func (s *StubPushStore) LoadPublicationMetadata(_ context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.PublicationMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PublicationMetadataCalls++
	if s.PublicationInputErr != nil {
		return nil, s.PublicationInputErr
	}
	result := make(map[ingest.SessionID]ingest.PublicationMetadata, len(ids))
	for _, id := range ids {
		input := s.PublicationInputs[id]
		result[id] = ingest.PublicationMetadata{Metadata: input.Metadata, Readiness: input.Readiness, CaptureRevision: input.CaptureRevision}
	}
	return result, nil
}

// SeedReadyPublication persists a synthetic source capture and indexes its exact
// entries through the production transactions. It creates no source or sidecar.
//
// The metadata argument is the ONE seed. Any candidate row a test pairs with it
// must take its session, project, and harness from this same metadata: the
// production read refuses a candidate whose recorded identity moved after
// selection, and a fixture that names the session in one place and its project in
// another trips that guard while looking like a product defect.
//
// Both stamps come from HarvesterVersionRegistry for this harness, so a registry
// change cannot leave the fixture claiming a producer or a stored format the
// build no longer targets.
func SeedReadyPublication(t testing.TB, db *store.Store, meta *schema.UnifiedMetadata, entries []schema.SessionEntry) {
	t.Helper()
	content, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentHash = schema.ComputeTranscriptHash(content)
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	kind := ingest.CWDSourceAbsent
	if meta.CWD != "" {
		kind = ingest.CWDSourceExact
	}
	revisions, err := db.InsertSessionsWithRevisions(context.Background(), []ingest.StoreEntry{{Metadata: meta, PublicationCapture: true, CWDProvenance: kind}})
	if err != nil {
		t.Fatal(err)
	}
	versions := HarvesterVersionsForSeed(t, meta.ModelHarness)
	results := db.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: meta.SessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: versions.IndexVersion, RequireFullContent: true, CaptureRevision: revisions[meta.SessionID], IndexerVersion: versions.IndexerVersion, IndexedAtMs: meta.Timestamp.Start}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("index captured publication: %+v", results)
	}
}

// HarvesterVersionsForSeed returns the registry targets for one harness and
// fails the test when the harness is not registered, so a fixture can never
// silently seed the zero versions of an unregistered harness.
func HarvesterVersionsForSeed(t testing.TB, harness ingest.Harness) ingest.HarvesterVersions {
	t.Helper()
	versions, registered := ingest.HarvesterVersionRegistry[harness]
	if !registered {
		t.Fatalf("seed harness %q is not in HarvesterVersionRegistry, so it has no adapter, indexer, or index-format target to seed", harness)
	}
	return versions
}

// SeedPublicationInputs migrates existing synthetic metadata fixtures into an
// explicit bundle double during test arrangement, before production runs.
// Missing fixtures remain incomplete; the production reader never sees this FS.
func SeedPublicationInputs(s *StubPushStore, fs ingest.FileSystem, base string) {
	if s.PublicationInputs == nil {
		s.PublicationInputs = make(map[ingest.SessionID]ingest.PublicationInputBundle)
	}
	rows := append(append([]ingest.PushSessionRow(nil), s.Sessions...), s.AllSessions...)
	for _, row := range rows {
		data, err := fs.ReadFile(ingest.SessionMetadataPath(base, row.HostSlug, row.SessionID, row.ParentID))
		if err != nil {
			continue
		}
		id := ingest.SessionID(row.SessionID)
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		hash, err := schema.NewProjectHash(row.ProjectHash)
		if err != nil {
			continue
		}
		s.PublicationInputs[id] = ingest.PublicationInputBundle{Metadata: meta, ReceiptProjectHash: hash, SessionOrigin: sessionorigin.Origin(row.SessionOrigin), CaptureRevision: 1, Readiness: ingest.PublicationReady}
	}
}
