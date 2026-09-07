package ingest_test

import (
	"bytes"
	_ "embed"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_child_compatibility.yaml
var metadataChildCompatibilityYAML []byte

func TestMetadataChildCompatibility(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string         `yaml:"requiredNames"`
		ParentID      ingest.SessionID `yaml:"parentID"`
		ChildID       ingest.SessionID `yaml:"childID"`
		Transcript    string           `yaml:"transcript"`
		Cases         []struct {
			Name          string `yaml:"name"`
			ParentSchema  int    `yaml:"parentSchema"`
			ChildSchema   int    `yaml:"childSchema"`
			StoredIndexer int    `yaml:"storedIndexer"`
			Force         bool   `yaml:"force"`
			WantIndexed   int    `yaml:"wantIndexed"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(metadataChildCompatibilityYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			filesystem := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			parent := makeReindexMeta(t, string(fixtures.ParentID), "/synthetic/missing-parent.jsonl")
			parent.SchemaVersion = fixture.ParentSchema
			parent.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			child := makeReindexMeta(t, string(fixtures.ChildID), "/synthetic/missing-child.jsonl")
			child.SchemaVersion = fixture.ChildSchema
			child.Project.Hash = parent.Project.Hash
			child.ParentUUID = &fixtures.ParentID
			parentPath, parentTranscript := setupPeasantSyncSession(t, filesystem, testOutputDir, testutil.TestHostSlug, string(fixtures.ParentID), parent)
			childPath, childTranscript := setupPeasantSyncSession(t, filesystem, testOutputDir, filepath.Join(testutil.TestHostSlug, string(fixtures.ParentID), "subagents"), string(fixtures.ChildID), child)
			if err := filesystem.WriteFile(childTranscript, []byte(fixtures.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			beforeFiles := make(map[string][]byte)
			for _, path := range []string{parentPath, childPath, parentTranscript, childTranscript} {
				beforeFiles[path], err = filesystem.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{
				{Metadata: parent, Session: ingest.DiscoveredSession{SessionID: fixtures.ParentID, Harness: ingest.HarnessClaudeCode}},
				{Metadata: child, Session: ingest.DiscoveredSession{SessionID: fixtures.ChildID, Harness: ingest.HarnessClaudeCode, ParentUUID: &fixtures.ParentID}},
			}); err != nil {
				t.Fatal(err)
			}
			beforeEntries := make(map[ingest.SessionID][]schema.SessionEntry)
			beforeState := make(map[ingest.SessionID]metadataPolicyIndexState)
			for _, sid := range []ingest.SessionID{fixtures.ParentID, fixtures.ChildID} {
				preview := "last-good indexed content"
				writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: []schema.SessionEntry{{SessionID: sid, Harness: ingest.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}, IndexerVersion: fixture.StoredIndexer, IndexedAtMs: 1700000000000}})
				if len(writes) != 1 || !writes[0].Written {
					t.Fatalf("seed index: %+v", writes)
				}
				beforeEntries[sid], err = database.ListEntries(ctx, sid)
				if err != nil {
					t.Fatal(err)
				}
				beforeState[sid] = readMetadataPolicyIndexState(t, database, sid)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex, cfg.Force = true, fixture.Force
			cfg.AllowedSessionIDs = map[ingest.SessionID]bool{fixtures.ChildID: true}
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}, cfg,
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil || result.Summary.Indexed != fixture.WantIndexed {
				t.Fatalf("index result=%+v err=%v, want %d", result, err, fixture.WantIndexed)
			}
			for path, before := range beforeFiles {
				after, err := filesystem.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("managed input %s changed: %v", path, err)
				}
			}
			for sid, before := range beforeEntries {
				if sid == fixtures.ChildID && fixture.WantIndexed > 0 {
					continue
				}
				after, err := database.ListEntries(ctx, sid)
				if err != nil || !reflect.DeepEqual(before, after) || readMetadataPolicyIndexState(t, database, sid) != beforeState[sid] {
					t.Fatalf("unselected/refused session %s index changed: %v", sid, err)
				}
			}
		})
	}
	if err := testutil.RequireFixtureNames("metadata child compatibility", "case", fixtures.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}
