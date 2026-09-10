package ingest_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestUpgradeMeetsThePendingIntentAnOlderBuildLeft holds what a user sees on
// the first harvest AFTER upgrading past the build that could leave a
// publication intent for a session it could not mirror.
//
// The pre-check keeps a fresh installation from ever staging one. It cannot
// help someone who already has one on disk, and that is a real state: the
// build that wrote it refused the session at the mirror, AFTER staging. For
// them the only thing standing between one actionable refusal and a pile of
// entries telling them to inspect and delete recovery evidence is where the
// pending recovery's failures are reported.
//
// The older build is arranged by hiding the newer stored schema from the
// compatibility CHECK. The mirror reads the stored version straight from the
// row, so it still refuses and still leaves the intent behind, which is
// exactly the state that build produced.
func TestUpgradeMeetsThePendingIntentAnOlderBuildLeft(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	database, err := store.Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	id, err := ingest.NewSessionID("ses_upgradetarget")
	if err != nil {
		t.Fatal(err)
	}
	meta := makeMinimalMeta(t, id.String())
	meta.Project.Hash = testutil.TestProjectHash
	meta.Source.FilePath = "/synthetic/missing.jsonl"
	stored := *meta
	stored.SchemaVersion = ingest.CurrentSchemaVersion + 1
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: &stored}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(testOutputDir, testutil.TestHostSlug)
	path := filepath.Join(dir, id.String(), id.String()+"--transcript.jsonl")
	data := []byte(`{"type":"assistant","message":{"role":"assistant","content":"a session an older build could not mirror"}}`)
	session := ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL}
	indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[ingest.HarnessClaudeCode]
	entries, err := indexer.IndexTranscriptBytes(ctx, session, data)
	if err != nil {
		t.Fatal(err)
	}
	results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000001000,
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed projection: %+v", results)
	}
	metadata, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(dir, id.String(), id.String()+"--metadata.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg := makePipelineConfig(testOutputDir)
	cfg.Reindex = true
	adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
	harvest := func(sessionStore ingest.SessionStore) *ingest.PipelineResult {
		t.Helper()
		pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			ingest.WithStore(sessionStore), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}

	// The older build: it staged an intent and the mirror refused it.
	harvest(&preUpgradeLookupStore{Store: database, target: id})
	if !hasPendingArtifactIntent(t, fs) {
		t.Fatal("the arrangement left no pending intent, so this test would prove nothing about the upgrade path")
	}

	// The upgrade. The leftover intent is met first, and must reach the user as
	// the one refusal that names what to do about it.
	after := harvest(database)
	var naming []ingest.DiagnosticEntry
	for _, diagnostic := range after.Diagnostics {
		if strings.Contains(diagnostic.Location, id.String()) || strings.Contains(diagnostic.Message, id.String()) {
			naming = append(naming, diagnostic)
		}
	}
	if len(naming) != 1 {
		t.Fatalf("the first harvest after the upgrade reported %d diagnostics for one refused session, want 1: %+v", len(naming), after.Diagnostics)
	}
	if naming[0].ErrorType != "metadata_refused" {
		t.Fatalf("the leftover intent was reported as %q rather than the refusal it is, so it cannot collapse with the same refusal from the selection: %+v", naming[0].ErrorType, naming[0])
	}
	if !strings.Contains(naming[0].Remediation, "Upgrade Peasant") {
		t.Fatalf("the refusal carries a remedy that does not lift it: %+v", naming[0])
	}
	for _, diagnostic := range after.Diagnostics {
		if strings.Contains(diagnostic.Remediation, "removing stale temporary evidence") {
			t.Fatalf("the user is still told to inspect and delete recovery evidence for a session an upgrade fixes: %+v", diagnostic)
		}
	}
}

// hasPendingArtifactIntent reports whether any publication intent is still
// staged under the managed transactions directory.
func hasPendingArtifactIntent(t *testing.T, fs *testutil.CountingFS) bool {
	t.Helper()
	entries, err := fs.ReadDir(filepath.Join(testOutputDir, ".peasant-state", "transactions"))
	if err != nil {
		return false
	}
	return len(entries) > 0
}
