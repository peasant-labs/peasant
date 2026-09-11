package storetest

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// SeedManagedArtifact writes a test-owned retained pair under output and
// records it in the database with MirrorArtifacts, then stops. It leaves the
// state a completed harvest leaves before indexing: files on disk and a row,
// but no index evidence, which is the retained-but-not-yet-indexed session a
// harvest still has real work to do on.
//
// It returns the discovered session and the managed metadata path, so a caller
// that needs the rest of a completed harvest can continue from the same pair.
func SeedManagedArtifact(t *testing.T, db *store.Store, filesystem ingest.FileSystem, output string, meta ingest.UnifiedMetadata, data []byte) (ingest.DiscoveredSession, string) {
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
	parent := ""
	if meta.ParentUUID != nil {
		parent = string(*meta.ParentUUID)
	}
	sessionDir := ingest.SessionDir(output, string(meta.HostSlug), string(meta.SessionID), parent)
	if err := filesystem.MkdirAll(sessionDir, defaults.PrivateDirPerm); err != nil {
		t.Fatal(err)
	}
	transcriptName := string(meta.SessionID) + "--transcript." + string(meta.Source.Format)
	if err := filesystem.WriteFile(filepath.Join(sessionDir, transcriptName), data, defaults.PrivateFilePerm); err != nil {
		t.Fatal(err)
	}
	path := ingest.SessionMetadataPath(output, string(meta.HostSlug), string(meta.SessionID), parent)
	if err := filesystem.WriteFile(path, encoded, defaults.PrivateFilePerm); err != nil {
		t.Fatal(err)
	}
	results := db.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("seed managed artifact mirror: %+v", results)
	}
	session := ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness, SourcePath: ingest.ResolvedPath(meta.Source.FilePath)}
	return session, path
}

// MirrorRetainedPair reads a retained pair already written under output and
// records its row in the database with MirrorArtifacts. It is the replacement
// for the old reconcile-from-files step: a test that has written the pair and
// inserted the base row calls this to establish the pair's artifact identity.
func MirrorRetainedPair(t *testing.T, db *store.Store, filesystem ingest.FileSystem, output, metadataPath string, sid ingest.SessionID) {
	t.Helper()
	artifact, err := ingest.ReadManagedPair(filesystem, output, metadataPath, sid)
	if err != nil {
		t.Fatal(err)
	}
	results := db.MirrorArtifacts(t.Context(), []ingest.ArtifactMirrorRequest{{Artifact: artifact}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("mirror retained pair for %s: %+v", sid, results)
	}
}

// SeedManagedInput establishes actual publication and parser evidence for a
// test-owned retained transcript. It never stamps a proof over manual rows.
func SeedManagedInput(t *testing.T, db *store.Store, filesystem ingest.FileSystem, output string, meta ingest.UnifiedMetadata, data []byte) []schema.SessionEntry {
	t.Helper()
	ctx := t.Context()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	session, _ := SeedManagedArtifact(t, db, filesystem, output, meta, data)
	state, err := db.ReadIndexState(ctx, meta.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sameMeta(t, meta, data))
	if err != nil {
		t.Fatal(err)
	}
	current, err := ingest.NewManagedArtifact(encoded, data)
	if err != nil {
		t.Fatal(err)
	}
	indexer := ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[meta.ModelHarness]
	input, err := ingest.CaptureIndexInput(ctx, indexer, session, current)
	if err != nil {
		t.Fatal(err)
	}
	result, err := input.Parse(ctx, indexer)
	if err != nil {
		t.Fatal(err)
	}
	entries := result.(indexformat.V1).Entries
	hash := input.Hash()
	target := ingest.HarvesterVersionRegistry[meta.ModelHarness]
	// Seed the state a completed harvest leaves, not a half of it. Without a
	// complete content capture the session stays pending content work forever,
	// so a test that seeds a "current" session and then asserts nothing touched
	// it is asserting against a session production would still be working on.
	if err := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: meta.SessionID, Result: result, IndexVersion: result.IndexVersion(),
		IndexerVersion: target.IndexerVersion, IndexedAtMs: 100, ExpectedState: state,
		IndexedInputHash: &hash, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: session.TranscriptOrigin, CaptureFormat: ingest.ContentCaptureFormatFull,
			CapturedAtMs: 100,
		},
	}})[0].Err; err != nil {
		t.Fatal(err)
	}
	// Metrics are the last thing a completed harvest leaves behind. Without them
	// the next run has real work to do on this session, so a test that seeds it
	// as "current" and asserts nothing touched it is asserting about a session
	// production has not finished.
	if _, err := metrics.NewEngine(db).ComputeMetrics(ctx, []ingest.SessionID{meta.SessionID}); err != nil {
		t.Fatal(err)
	}
	return entries
}

// sameMeta re-derives the checksums the seeded metadata carries, so the
// managed artifact the index step captures matches the row that was mirrored.
func sameMeta(t *testing.T, meta ingest.UnifiedMetadata, data []byte) ingest.UnifiedMetadata {
	t.Helper()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.ContentHash = schema.ComputeTranscriptHash(data)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	return meta
}
