package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pipeline_thinking_level.yaml
var pipelineThinkingLevelYAML []byte

type pipelineThinkingPair struct {
	Level schema.ThinkingLevel    `yaml:"level"`
	Raw   schema.ThinkingLevelRaw `yaml:"raw"`
}

func assertNoThinkingLevelEntries(t *testing.T, entries []schema.SessionEntry) {
	t.Helper()
	if len(entries) == 0 {
		t.Fatal("absence assertion requires indexed content")
	}
	for _, entry := range entries {
		if entry.Extra == nil {
			continue
		}
		var extra map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
			t.Fatal(err)
		}
		if _, ok := extra[ingest.ThinkingLevelExtraKey]; ok {
			t.Fatalf("invented level on entry %d", entry.EntryIndex)
		}
		if _, ok := extra[ingest.ThinkingLevelRawExtraKey]; ok {
			t.Fatalf("invented raw on entry %d", entry.EntryIndex)
		}
	}
}

// assertPipelineThinkingHistory compares a projected history (stored row,
// sidecar metadata, or wire detail) against the fixture expectation. An empty
// expectation means the session never changed level, so the history must be
// absent rather than an empty slice carrying no evidence.
func assertPipelineThinkingHistory(t *testing.T, label string, got []schema.ThinkingLevelChange, want []pipelineThinkingPair) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s history=%+v, want %+v", label, got, want)
	}
	for i, change := range got {
		if change.Level != want[i].Level || change.Raw != want[i].Raw {
			t.Fatalf("%s history[%d]=(%q,%q), want %+v", label, i, change.Level, change.Raw, want[i])
		}
	}
}

func TestPipelineThinkingLevel(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string                 `yaml:"name"`
			Harness    ingest.Harness         `yaml:"harness"`
			Root       ingest.ResolvedPath    `yaml:"root"`
			Seed       pipelineThinkingPair   `yaml:"seed"`
			History    []pipelineThinkingPair `yaml:"history"`
			Turns      []pipelineThinkingPair `yaml:"turns"`
			Diagnostic string                 `yaml:"diagnostic"`
			Files      []struct {
				Path    string `yaml:"path"`
				Content string `yaml:"content"`
			} `yaml:"files"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(pipelineThinkingLevelYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("pipeline thinking level fixture declares no requiredNames inventory; name every case the fixture must retain")
	}
	names := make([]string, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		names[i] = tc.Name
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "pipeline thinking level"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			for _, file := range tc.Files {
				if err := fs.WriteFile(file.Path, []byte(file.Content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			db := storetest.Open(t)
			cfg := makePipelineConfig(testOutputDir)
			cfg.IncludeActive = true
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{tc.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{tc.Root}}}
			pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Errors != 0 {
				t.Fatalf("ingest failed: %+v diagnostics=%+v", result.Summary, result.Diagnostics)
			}
			sid := schema.SessionID(testutil.TestSessionUUID)
			row, err := db.SessionByID(ctx, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			if row == nil {
				t.Fatalf("session missing: %+v diagnostics=%+v", result.Summary, result.Diagnostics)
			}
			if row.ThinkingLevel != tc.Seed.Level || row.ThinkingLevelRaw != tc.Seed.Raw {
				t.Fatalf("stored seed=(%q,%q), want %+v", row.ThinkingLevel, row.ThinkingLevelRaw, tc.Seed)
			}
			assertPipelineThinkingHistory(t, "stored row", row.ThinkingLevelHistory, tc.History)
			entries, err := db.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			observations := 0
			for _, entry := range entries {
				if entry.Role != schema.RoleAssistant || entry.Depth != 0 {
					continue
				}
				if observations >= len(tc.Turns) {
					t.Fatalf("unexpected assistant entry: %+v", entry)
				}
				want := tc.Turns[observations]
				observations++
				extra := map[string]any{}
				if entry.Extra != nil {
					if err := json.Unmarshal([]byte(*entry.Extra), &extra); err != nil {
						t.Fatal(err)
					}
				}
				level, _ := extra[ingest.ThinkingLevelExtraKey].(string)
				raw, _ := extra[ingest.ThinkingLevelRawExtraKey].(string)
				if level != string(want.Level) || raw != string(want.Raw) {
					t.Fatalf("entry evidence=(%q,%q), want %+v", level, raw, want)
				}
				if want.Level == "" {
					if _, ok := extra[ingest.ThinkingLevelExtraKey]; ok {
						t.Fatal("absent level must omit Extra key")
					}
				}
				if want.Raw == "" {
					if _, ok := extra[ingest.ThinkingLevelRawExtraKey]; ok {
						t.Fatal("absent raw must omit Extra key")
					}
				}
			}
			if observations != len(tc.Turns) {
				t.Fatalf("assistant observations=%d, want %d; entries=%+v", observations, len(tc.Turns), entries)
			}
			provider := api.NewStoreDataProvider(db, sessionvisibility.Browsing())
			session, err := provider.SessionByID(ctx, string(sid))
			if err != nil {
				t.Fatal(err)
			}
			detail := api.SessionToDetail(session)
			if detail.ThinkingLevel != tc.Seed.Level || detail.ThinkingLevelRaw != tc.Seed.Raw {
				t.Fatalf("projected seed=(%q,%q), want %+v", detail.ThinkingLevel, detail.ThinkingLevelRaw, tc.Seed)
			}
			assertPipelineThinkingHistory(t, "wire detail", detail.ThinkingLevelHistory, tc.History)
			observations = 0
			for _, turn := range detail.Turns {
				if turn.Role != schema.RoleAssistant || turn.Depth != 0 {
					continue
				}
				if observations >= len(tc.Turns) {
					t.Fatalf("unexpected root assistant turn: %+v", turn)
				}
				want := tc.Turns[observations]
				observations++
				if turn.ThinkingLevel != want.Level || turn.ThinkingLevelRaw != want.Raw {
					t.Fatalf("wire turn=(%q,%q), want %+v", turn.ThinkingLevel, turn.ThinkingLevelRaw, want)
				}
			}
			if observations != len(tc.Turns) {
				t.Fatal("projection lost assistant turns")
			}
			// Extraction diagnostics belong to the persisted metadata, not the
			// pipeline's separate local-maintenance diagnostic list.
			metadataPath := fmt.Sprintf("%s/%s/%s/%s--metadata.json", testOutputDir, row.HostSlug, sid, sid)
			metadataBytes, err := fs.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			var metadata schema.UnifiedMetadata
			if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.ThinkingLevel != tc.Seed.Level || metadata.ThinkingLevelRaw != tc.Seed.Raw {
				t.Fatalf("sidecar seed differs: %+v", metadata)
			}
			assertPipelineThinkingHistory(t, "sidecar metadata", metadata.ThinkingLevelHistory, tc.History)
			found := false
			for _, diagnostic := range metadata.Diagnostics.Warnings {
				if diagnostic.ErrorType == tc.Diagnostic {
					found = true
				}
			}
			if tc.Diagnostic != "" && !found {
				t.Fatalf("missing diagnostic %q: %+v", tc.Diagnostic, metadata.Diagnostics.Warnings)
			}
		})
	}
}
