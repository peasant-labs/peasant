package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_backfill.yaml
var captureBackfillFixtureData []byte

func TestRetainedContentBackfill(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name           string `yaml:"name"`
			Child          bool   `yaml:"child"`
			Corrupt        bool   `yaml:"corrupt"`
			Mismatch       bool   `yaml:"mismatch"`
			Force          bool   `yaml:"force"`
			Count          int    `yaml:"count"`
			Error          bool   `yaml:"expect_error"`
			Cursor         bool   `yaml:"cursor"`
			InvalidProject bool   `yaml:"invalid_project"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(captureBackfillFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()
			database, err := store.Open(t.TempDir() + "/capture.db")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			count := max(1, fixture.Count)
			var ids []ingest.SessionID
			var before [][]schema.SessionEntry
			var captures []ingest.SessionContentCapture
			text := strings.Repeat("safe ", 600) + "RETAINED_TAIL"
			harness := ingest.HarnessClaudeCode
			if fixture.Cursor {
				harness = ingest.HarnessCursor
				text = "  before <user_query>example</user_query> " + text + " \n "
			}
			raw, _ := json.Marshal(text)
			data := []byte(fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":%s}}`, raw))
			if fixture.Cursor {
				data = []byte(fmt.Sprintf(`{"uuid":"stable_cursor_message","role":"assistant","content":%s}`, raw))
			}
			for i := 0; i < count; i++ {
				id, err := ingest.NewSessionID(fmt.Sprintf("ses_backfill%03d", i))
				if err != nil {
					t.Fatal(err)
				}
				meta := makeMinimalMeta(t, id.String())
				meta.Project.Hash = testutil.TestProjectHash
				if fixture.InvalidProject {
					meta.Project.Hash = ""
				}
				meta.ModelHarness = harness
				meta.Source.FilePath = "/synthetic/missing.jsonl"
				meta.Source.Format = ingest.SourceFormatJSONL
				dir := filepath.Join(testOutputDir, testutil.TestHostSlug)
				if fixture.Child && i == 0 {
					parentID, err := ingest.NewSessionID(testutil.TestSessionUUID)
					if err != nil {
						t.Fatal(err)
					}
					parent := makeMinimalMeta(t, parentID.String())
					parent.Project.Hash = testutil.TestProjectHash
					parent.Source.Format = ingest.SourceFormatJSONL
					if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: parent}}); err != nil {
						t.Fatal(err)
					}
					// The parent already has complete empty content; only the child is a recovery target.
					writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: parentID, RequireFullContent: true}})
					if writes[0].Err != nil {
						t.Fatal(writes[0].Err)
					}
					meta.ParentUUID = &parentID
					dir = filepath.Join(dir, parentID.String(), "subagents")
				}
				if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, id.String(), id.String()+"--transcript.jsonl")
				session := ingest.DiscoveredSession{SessionID: id, Harness: harness, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL}
				idx := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[harness]
				entries, err := idx.IndexTranscriptBytes(ctx, session, data)
				if err != nil {
					t.Fatal(err)
				}
				if fixture.Mismatch {
					value := `{"legacy":"anchor"}`
					entries[0].Extra = &value
				}
				if err := database.IndexSessionEntries(ctx, id, entries); err != nil {
					t.Fatal(err)
				}
				if fixture.Cursor {
					if entries[0].ContentPreview == nil || *entries[0].ContentPreview != "example" {
						t.Fatal("tolerant Cursor preview shape changed")
					}
					annotator, err := database.GetAnnotatorIDByName(ctx, "frustration-classifier")
					if err != nil {
						t.Fatal(err)
					}
					typeID, err := database.GetAnnotationTypeID(ctx, "quality.frustration_signal")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := database.CreateEntryAnnotation(ctx, ingest.EntryAnnotationParams{SessionID: id.String(), EntryIndex: 0, EndIndex: 0, AnnotatorID: annotator, AnnotationTypeID: typeID, Value: "detected"}); err != nil {
						t.Fatal(err)
					}
				}
				stored, err := database.ListEntries(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				before = append(before, stored)
				capture, _, err := database.GetSessionContentCapture(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				captures = append(captures, capture)
				metadata, _ := json.Marshal(meta)
				if err := fs.WriteFile(filepath.Join(dir, id.String(), id.String()+"--metadata.json"), metadata, 0600); err != nil {
					t.Fatal(err)
				}
				content := data
				if fixture.Corrupt && i == 0 {
					content = []byte(`{"type":`)
				}
				if err := fs.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = true
			cfg.Force = fixture.Force
			adapters := map[ingest.Harness]ingest.AdapterFactory{harness: makeStubAdapter(nil, nil)}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database))
			if err != nil {
				t.Fatal(err)
			}
			_, err = pipeline.Run(ctx)
			if (err != nil) != fixture.Error {
				t.Fatalf("reindex error=%v expected error=%v", err, fixture.Error)
			}
			if err := fs.RemoveAll(testOutputDir); err != nil {
				t.Fatal(err)
			}
			for i, id := range ids {
				if fixture.Cursor {
					annotations, err := database.GetAnnotationsForEntry(ctx, id.String(), 0)
					if err != nil || len(annotations) != 1 || annotations[0].Value != "detected" {
						t.Fatalf("Cursor annotation anchor lost: %v %v", annotations, err)
					}
				}
				if fixture.Error && i == 0 {
					after, err := database.ListEntries(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					capture, _, err := database.GetSessionContentCapture(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before[i], after) || !reflect.DeepEqual(captures[i], capture) {
						t.Fatal("failed backfill changed prior state")
					}
					continue
				}
				page, err := database.ReadSessionEntries(ctx, id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
				if err != nil {
					t.Fatalf("target %d: %v", i, err)
				}
				if len(page.Entries) == 0 || page.Entries[0].ContentPreview == nil || *page.Entries[0].ContentPreview != text {
					t.Fatalf("target %d full text missing", i)
				}
				// Recovering legacy content does not invent the missing metadata/index
				// publication proof, even when force replaces the canonical entries.
				bundle, err := database.LoadPublicationInput(ctx, id)
				if err != nil || bundle.Readiness != ingest.PublicationNeedsIngest || len(bundle.Entries) != 0 || page.Capture.PublicationCaptureRevision != 0 {
					t.Fatalf("target %d backfill fabricated publication proof: %+v %v", i, bundle, err)
				}
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
}
