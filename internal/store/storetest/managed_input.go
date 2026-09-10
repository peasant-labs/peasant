package storetest

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// SeedManagedArtifact publishes and mirrors a test-owned managed artifact and
// stops there. It leaves the state a completed publication leaves behind and no
// index evidence at all, which is the retained-but-not-yet-indexed session a
// harvest still has real work to do on.
//
// It returns the publisher, the discovered session, and the managed metadata
// path, so a caller that needs the rest of a completed harvest can continue
// from the same publication.
func SeedManagedArtifact(t *testing.T, db *store.Store, fs ingest.FileSystem, output string, meta ingest.UnifiedMetadata, data []byte) (*ingest.ArtifactPublisher, ingest.DiscoveredSession, string) {
	t.Helper()
	ctx := t.Context()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.ContentHash = schema.ComputeTranscriptHash(data)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(encoded, data)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := ingest.NewArtifactPublisher(fs, output, ingest.ArtifactPublisherOptions{Mirror: db})
	if err != nil {
		t.Fatal(err)
	}
	parent := ""
	if meta.ParentUUID != nil {
		parent = string(*meta.ParentUUID)
	}
	path := ingest.SessionMetadataPath(output, string(meta.HostSlug), string(meta.SessionID), parent)
	session := ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness, SourcePath: ingest.ResolvedPath(meta.Source.FilePath)}
	observation, err := publisher.Observe(ctx, session, path)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := publisher.Publish(ctx, ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Reconcile(ctx, committed); err != nil {
		t.Fatal(err)
	}
	return publisher, session, path
}

// SeedManagedInput establishes actual publication and parser evidence for a
// test-owned retained transcript. It never stamps a proof over manual rows.
func SeedManagedInput(t *testing.T, db *store.Store, fs ingest.FileSystem, output string, meta ingest.UnifiedMetadata, data []byte) []schema.SessionEntry {
	t.Helper()
	ctx := t.Context()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	publisher, session, path := SeedManagedArtifact(t, db, fs, output, meta, data)
	var entries []schema.SessionEntry
	err := publisher.WithCapture(ctx, meta.SessionID, path, func(current *ingest.ManagedArtifact) error {
		state, err := db.ReadIndexState(ctx, meta.SessionID)
		if err != nil {
			return err
		}
		indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[meta.ModelHarness]
		input, err := ingest.CaptureIndexInput(ctx, indexer, session, current)
		if err != nil {
			return err
		}
		result, err := input.Parse(ctx, indexer)
		if err != nil {
			return err
		}
		entries = result.(indexformat.V1).Entries
		hash := input.Hash()
		target := ingest.HarvesterVersionRegistry[meta.ModelHarness]
		// Seed the state a completed harvest leaves, not a half of it. Without
		// a complete content capture the session stays pending content work
		// forever, so a test that seeds a "current" session and then asserts
		// nothing touched it is asserting against a session production would
		// still be working on.
		return db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
			SessionID: meta.SessionID, Result: result, IndexVersion: result.IndexVersion(),
			IndexerVersion: target.IndexerVersion, IndexedAtMs: 100, ExpectedState: state,
			IndexedInputHash: &hash, RequireFullContent: true,
			ContentCapture: ingest.SessionContentCaptureWrite{
				Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
				TranscriptOrigin: session.TranscriptOrigin, CaptureFormat: ingest.ContentCaptureFormatFull,
				CapturedAtMs: 100,
			},
		}})[0].Err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Metrics are the last thing a completed harvest leaves behind. Without
	// them the next run has real work to do on this session, so a test that
	// seeds it as "current" and asserts nothing touched it is asserting about a
	// session production has not finished.
	if _, err := metrics.NewEngine(db).ComputeMetrics(ctx, []ingest.SessionID{meta.SessionID}); err != nil {
		t.Fatal(err)
	}
	return entries
}
