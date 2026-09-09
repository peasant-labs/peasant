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
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_recovery_scope.yaml
var contentRecoveryScopeFixtureData []byte

// recoveryScopeOutcome is the observable result of one scoped recovery run.
type recoveryScopeOutcome string

const (
	recoveryScopeRecovered recoveryScopeOutcome = "recovered"
	recoveryScopeUntouched recoveryScopeOutcome = "untouched"
	recoveryScopeRefused   recoveryScopeOutcome = "refused"
)

func newRecoveryScopeOutcome(raw string) (recoveryScopeOutcome, error) {
	switch outcome := recoveryScopeOutcome(raw); outcome {
	case recoveryScopeRecovered, recoveryScopeUntouched, recoveryScopeRefused:
		return outcome, nil
	}
	return "", fmt.Errorf("content recovery scope fixture: unknown expected outcome %q; use recovered, untouched or refused", raw)
}

type contentRecoveryScopeFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name             string `yaml:"name"`
		Harness          string `yaml:"harness"`
		SinceAfterStart  bool   `yaml:"since_after_start"`
		OtherSessionOnly bool   `yaml:"other_session_only"`
		FutureProducer   bool   `yaml:"future_producer"`
		Expect           string `yaml:"expect"`
	} `yaml:"cases"`
}

func loadContentRecoveryScopeFixtures(t *testing.T) contentRecoveryScopeFixtures {
	t.Helper()
	var fixtures contentRecoveryScopeFixtures
	if err := yaml.Unmarshal(contentRecoveryScopeFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate content recovery scope fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing content recovery scope fixture %s", name)
		}
	}
	return fixtures
}

func TestContentRecoveryScope(t *testing.T) {
	fixtures := loadContentRecoveryScopeFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			expect, err := newRecoveryScopeOutcome(fixture.Expect)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			fs := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "scope.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			id, err := ingest.NewSessionID("ses_scopetarget")
			if err != nil {
				t.Fatal(err)
			}
			meta := makeMinimalMeta(t, id.String())
			meta.Project.Hash = testutil.TestProjectHash
			meta.Source.FilePath = "/synthetic/missing.jsonl"
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug)
			path := filepath.Join(dir, id.String(), id.String()+"--transcript.jsonl")
			data := []byte(`{"type":"assistant","message":{"role":"assistant","content":"scoped recovery target"}}`)
			session := ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL}
			indexer := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[ingest.HarnessClaudeCode]
			entries, err := indexer.IndexTranscriptBytes(ctx, session, data)
			if err != nil {
				t.Fatal(err)
			}
			producer := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion
			if fixture.FutureProducer {
				producer++
			}
			// A stored projection without a content capture row is a recovery
			// target; the stored producer stamp decides whether this build may touch it.
			results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: producer, IndexedAtMs: 1700000001000}})
			if len(results) != 1 || results[0].Err != nil || !results[0].Written {
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
			before, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = true
			if fixture.Harness != "" {
				var harness schema.Harness
				if err := harness.UnmarshalText([]byte(fixture.Harness)); err != nil || !harness.IsKnown() {
					t.Fatalf("fixture harness %q is not a known harness: %v", fixture.Harness, err)
				}
				cfg.Harness = &harness
			}
			if fixture.SinceAfterStart {
				since := time.UnixMilli(meta.Timestamp.Start).Add(time.Hour)
				cfg.Since = &since
			}
			if fixture.OtherSessionOnly {
				other, err := ingest.NewSessionID("ses_scopeother")
				if err != nil {
					t.Fatal(err)
				}
				cfg.AllowedSessionIDs = map[ingest.SessionID]bool{other: true}
			}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(database), ingest.WithMetricsStore(database))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("reindex: %v", err)
			}
			diagnostics := make(map[string]ingest.DiagnosticEntry)
			for _, diagnostic := range result.Diagnostics {
				diagnostics[diagnostic.ErrorType] = diagnostic
			}
			capture, found, err := database.GetSessionContentCapture(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			after, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			switch expect {
			case recoveryScopeRecovered:
				if !found || capture.Status != ingest.ContentCaptureComplete {
					t.Fatalf("in-scope session was not recovered: found=%t capture=%+v diagnostics=%+v", found, capture, result.Diagnostics)
				}
				if _, refused := diagnostics["content_recovery_refused"]; refused {
					t.Fatalf("in-scope current session was refused: %+v", diagnostics["content_recovery_refused"])
				}
			case recoveryScopeUntouched:
				if found && capture.Status == ingest.ContentCaptureComplete {
					t.Fatalf("out-of-scope session was recovered: %+v", capture)
				}
				for errorType := range diagnostics {
					if errorType == "content_recovery_refused" || errorType == "content_recovery_unavailable" {
						t.Fatalf("out-of-scope session produced a recovery diagnostic: %+v", diagnostics[errorType])
					}
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("out-of-scope session entries changed: before=%+v after=%+v", before, after)
				}
			case recoveryScopeRefused:
				refused, ok := diagnostics["content_recovery_refused"]
				if !ok {
					t.Fatalf("future producer was not refused: %+v", result.Diagnostics)
				}
				if !containsAll(refused.Message, id.String(), "preserved") {
					t.Fatalf("refusal is not visible and actionable: %+v", refused)
				}
				if found && capture.Status == ingest.ContentCaptureComplete {
					t.Fatalf("refused session was written: %+v", capture)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refused session entries changed: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if needle == "" || !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
