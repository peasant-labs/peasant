package storetest

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// SeedManagedInput establishes actual publication and parser evidence for a
// test-owned retained transcript. It never stamps a proof over manual rows.
func SeedManagedInput(t *testing.T, db *store.Store, fs ingest.FileSystem, output string, meta ingest.UnifiedMetadata, data []byte) []schema.SessionEntry {
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
	var entries []schema.SessionEntry
	err = publisher.WithCapture(ctx, meta.SessionID, path, func(current *ingest.ManagedArtifact) error {
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
		return db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: meta.SessionID, Result: result, IndexVersion: result.IndexVersion(), IndexerVersion: target.IndexerVersion, IndexedAtMs: 100, ExpectedState: state, IndexedInputHash: &hash}})[0].Err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
