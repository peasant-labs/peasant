package ingest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestAdapterBumpRepairsAnAlreadyHarvestedOpenCodeSession pins the whole repair
// path, not only the classifier: a session stamped by the previous adapter
// target is selected, republished once even though its projection is
// byte-identical, and settles without another write. Removing the
// adapter-revision clause leaves the row selected on every harvest.
func TestAdapterBumpRepairsAnAlreadyHarvestedOpenCodeSession(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedOpenCodeEnvironment{}, &ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatal(err)
	}
	config := makePipelineConfig(filepath.Join(t.TempDir(), "managed"))
	config.Sources = map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{root}, Enabled: true}}
	config.StalenessThreshold = 0
	discovered, err := adapter.Discover(t.Context(), config.Sources[ingest.HarnessOpenCode])
	if err != nil || len(discovered) != 1 {
		t.Fatalf("discover synthetic OpenCode source: %d sessions, %v", len(discovered), err)
	}
	sessionID := discovered[0].SessionID

	generationOptions, err := store.ManagedGenerationOptions(string(config.OutputDir))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(storetest.CopyGoldenDB(t), append([]store.OpenOption{store.WithSkipMigrations()}, generationOptions...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
			return adapter
		},
	}
	run := func(options ...ingest.PipelineOption) *ingest.PipelineResult {
		t.Helper()
		options = append([]ingest.PipelineOption{
			ingest.WithStore(database),
			ingest.WithMetricsStore(database),
			ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
				ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true)),
			}),
		}, options...)
		pipeline, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), adapters, config, options...)
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	location := func() ingest.SessionLocation {
		t.Helper()
		locations, err := database.BulkLookupSessionLocations(t.Context(), []ingest.SessionID{sessionID})
		if err != nil {
			t.Fatal(err)
		}
		return locations[sessionID]
	}
	storedRevision := func() int {
		t.Helper()
		if version := location().AdapterVersion; version != nil {
			return *version
		}
		return 0
	}
	selected := func() bool {
		t.Helper()
		// The fixture uses a current schema, so no native-refresh revision list
		// is needed; production passes the refresh set here.
		stale, err := database.ListStaleAdapterSessions(t.Context(), ingest.GenerationTargets(ingest.HarvesterVersionRegistry), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range stale {
			if id == sessionID {
				return true
			}
		}
		return false
	}
	baseline := ingest.HarvesterVersionRegistry[ingest.HarnessOpenCode].AdapterVersion
	// Seed the row at the baseline target. Injecting the registry reproduces the
	// previous target and stamp; the rows themselves are still written by this
	// generation-capable build.
	if result := run(ingest.WithHarvesterVersions(ingest.HarvesterVersionRegistry)); result.Summary.New != 1 {
		t.Fatalf("baseline harvest did not ingest the synthetic session: %+v", result.Summary)
	}
	if got := storedRevision(); got != baseline {
		t.Fatalf("seeded adapter revision = %d, want %d", got, baseline)
	}
	metadataPath := ingest.SessionMetadataPath(string(config.OutputDir), location().HostSlug, string(sessionID), "")
	storedMetadata := func() []byte {
		t.Helper()
		data, err := os.ReadFile(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	storedContentHash := func(data []byte) string {
		t.Helper()
		var decoded ingest.UnifiedMetadata
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded.ContentHash
	}
	baselineContentHash := storedContentHash(storedMetadata())
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessOpenCode].AdapterVersion
	if target <= baseline {
		t.Fatalf("generation target %d does not exceed the baseline %d; the repair path would not be exercised", target, baseline)
	}
	if !selected() {
		t.Fatal("the baseline build's row is not selected by the generation target; the case cannot prove a repair")
	}

	// The ordinary harvest after the bump must republish the identical
	// projection because the stored revision is behind the target.
	if result := run(); result.Summary.Updated != 1 {
		t.Fatalf("adapter bump did not republish the stored session: %+v", result.Summary)
	}
	if got := storedRevision(); got != target {
		t.Fatalf("repaired adapter revision = %d, want %d", got, target)
	}
	if selected() {
		t.Fatal("the repaired session is still selected for adapter refresh")
	}
	repaired := storedMetadata()
	if hash := storedContentHash(repaired); hash != baselineContentHash {
		t.Fatalf("repair replaced the captured projection: content hash %q -> %q", baselineContentHash, hash)
	}

	// And the next harvest settles without another publication.
	if result := run(); result.Summary.Updated != 0 || result.Summary.New != 0 || result.Summary.Unchanged != 1 {
		t.Fatalf("a repaired session did not settle: %+v", result.Summary)
	}
	if selected() {
		t.Fatal("a settled session was selected again")
	}
	if after := storedMetadata(); string(after) != string(repaired) {
		t.Fatal("a settled session was republished")
	}
}
