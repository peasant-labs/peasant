package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
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
			Name           string   `yaml:"name"`
			Child          bool     `yaml:"child"`
			Corrupt        bool     `yaml:"corrupt"`
			Mismatch       bool     `yaml:"mismatch"`
			Force          bool     `yaml:"force"`
			Count          int      `yaml:"count"`
			Reported       bool     `yaml:"recovery_reported"`
			Unchanged      bool     `yaml:"unchanged"`
			Cursor         bool     `yaml:"cursor"`
			InvalidProject bool     `yaml:"invalid_project"`
			Diagnostics    []string `yaml:"expected_diagnostics"`
			Preservation   string   `yaml:"preservation_phrase"`
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
					writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: parentID, Result: indexformat.V1{}, IndexVersion: 1, RequireFullContent: true}})
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
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("a failed recovery must not abort the run: %v", err)
			}
			// A refused or unavailable recovery is visible per session and names
			// the session it preserved; every other session still recovers.
			// A refused or unavailable recovery is visible per session, names the
			// session, and tells the user its stored state survived. WHICH words
			// carry that promise depends on the reporter that owns the cause, so
			// the fixture declares the phrase rather than the test assuming one:
			// a refusal that stops stating preservation goes red whichever
			// reporter it comes from.
			if fixture.Reported == (fixture.Preservation == "") {
				t.Fatalf("fixture %s: a reported recovery failure must declare the preservation phrase the user reads, and an unreported one must declare none", fixture.Name)
			}
			reported := false
			for _, diagnostic := range result.Diagnostics {
				if strings.Contains(diagnostic.Location, ids[0].String()) && strings.Contains(diagnostic.Message, fixture.Preservation) && fixture.Preservation != "" {
					reported = true
				}
			}
			// The exact multiset of diagnostic types naming the first session.
			// The boolean above says a refusal is visible and preserves; this
			// says the user is told ONCE, by one reporter, which a boolean
			// cannot see: a cause reported by two paths reads as two warnings.
			var naming []string
			for _, diagnostic := range result.Diagnostics {
				if strings.Contains(diagnostic.Location, ids[0].String()) || strings.Contains(diagnostic.Message, ids[0].String()) {
					naming = append(naming, diagnostic.ErrorType)
				}
			}
			sort.Strings(naming)
			want := append([]string(nil), fixture.Diagnostics...)
			sort.Strings(want)
			if len(naming)+len(want) > 0 && !reflect.DeepEqual(naming, want) {
				t.Fatalf("diagnostics naming the session = %v, want %v: %+v", naming, want, result.Diagnostics)
			}
			if reported != fixture.Reported {
				t.Fatalf("recovery failure reported=%t, expected=%t; diagnostics=%+v", reported, fixture.Reported, result.Diagnostics)
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
				if fixture.Unchanged && i == 0 {
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
