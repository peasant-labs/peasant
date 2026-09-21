package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"gopkg.in/yaml.v3"
)

type unknownFailingStore struct {
	*store.Store
	fail bool
}

func (s *unknownFailingStore) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	if !s.fail {
		return s.Store.IndexSessionEntryBatch(ctx, writes)
	}
	results := make([]ingest.SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = ingest.SessionEntryWriteResult{SessionID: write.SessionID, Err: fmt.Errorf("synthetic evidence write failure")}
	}
	return results
}

var _ ingest.SessionEntryBatchStore = (*unknownFailingStore)(nil)

//go:embed testdata/retained_unknown.yaml
var retainedUnknownYAML []byte

type retainedUnknownCase struct {
	Name      string         `yaml:"name"`
	Harness   ingest.Harness `yaml:"harness"`
	Source    string         `yaml:"source"`
	Positions []int          `yaml:"positions"`
	Pointers  []string       `yaml:"pointers"`
	Kinds     []string       `yaml:"kinds"`
	Texts     []string       `yaml:"texts"`
	Error     bool           `yaml:"error"`
	Pipeline  bool           `yaml:"pipeline"`
}

func loadRetainedUnknownFixtures(t *testing.T) []retainedUnknownCase {
	t.Helper()
	var doc struct {
		Required []string              `yaml:"required_names"`
		Cases    []retainedUnknownCase `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(retainedUnknownYAML))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing YAML: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || c.Source == "" || len(c.Positions) != len(c.Kinds) || len(c.Pointers) != len(c.Kinds) {
			t.Fatalf("invalid fixture %q", c.Name)
		}
		seen[c.Name] = true
	}
	if len(doc.Required) == 0 {
		t.Fatal("missing required-name manifest")
	}
	for _, name := range doc.Required {
		if !seen[name] {
			t.Fatalf("missing required fixture %q", name)
		}
	}
	return doc.Cases
}

func TestRetainedUnknownCapture(t *testing.T) {
	t.Parallel()
	for _, c := range loadRetainedUnknownFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			payload := strings.Repeat("synthetic-large-value-", 1024) + "FULL_UNKNOWN_TAIL"
			data := strings.ReplaceAll(c.Source, "PAYLOAD", payload)
			session := makeClaudeSession(t, fs, testutil.TestSessionUUID, data)
			session.Harness = c.Harness
			idx := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})[c.Harness].(ingest.AuthoritativeTranscriptIndexer)
			result, err := idx.IndexTranscriptForCapture(t.Context(), session)
			if c.Error {
				var refused *ingest.UnrepresentedRecordError
				if err == nil || errors.As(err, &refused) || len(result.Entries) > 0 {
					t.Fatalf("corruption must win without replacement: %+v %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(result.RetainedUnknown) != len(c.Kinds) {
				t.Fatalf("occurrences %d want %d", len(result.RetainedUnknown), len(c.Kinds))
			}
			for i, record := range result.RetainedUnknown {
				if record.Harness != c.Harness || record.Kind != c.Kinds[i] || record.Position.Line != c.Positions[i] || record.Position.JSONPointer != c.Pointers[i] || !bytes.Contains(record.Payload, []byte(payload)) {
					t.Fatalf("evidence lost its payload or source position: %s %+v", record.Kind, record.Position)
				}
			}
			var visible strings.Builder
			for _, entry := range result.Entries {
				if entry.ContentPreview != nil {
					visible.WriteString(*entry.ContentPreview)
				}
				if entry.ToolOutput != nil {
					visible.WriteString(*entry.ToolOutput)
				}
			}
			if strings.Contains(visible.String(), payload) {
				t.Fatal("unknown evidence fabricated conversation content")
			}
			for _, text := range c.Texts {
				if !strings.Contains(visible.String(), text) {
					t.Fatalf("known sibling %q lost", text)
				}
			}
			turns := transcript.EntriesToTurns(result.Entries)
			for _, entry := range result.Entries {
				if !ingest.IsRetainedUnknownCarrier(entry) {
					continue
				}
				for _, turn := range turns {
					if turn.Index == entry.EntryIndex {
						t.Fatal("private unknown evidence became a displayed turn")
					}
				}
			}
		})
	}
}

func TestRetainedUnknownStreamAndRetainedBatch(t *testing.T) {
	t.Parallel()
	fixtures := loadRetainedUnknownFixtures(t)
	corrupt := map[ingest.Harness]string{}
	for _, c := range fixtures {
		if c.Error {
			corrupt[c.Harness] = c.Source
		}
	}
	for _, c := range fixtures {
		if !c.Pipeline {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fs := testutil.NewMemFS()
			// A synthetic token is constructed, not copied from any live source.
			secret := "ghp_" + strings.Repeat("A", 36)
			payload := strings.Repeat("synthetic-large-value-", 1024) + " " + secret + " FULL_UNKNOWN_TAIL"
			session := makeClaudeSession(t, fs, testutil.TestSessionUUID, strings.ReplaceAll(c.Source, "PAYLOAD", payload))
			session.Harness, session.ModTime = c.Harness, time.Now().Add(-time.Hour)
			meta := makeMinimalMeta(t, session.SessionID.String())
			meta.Project.Hash, meta.ModelHarness, meta.Source.FilePath = testutil.TestProjectHash, c.Harness, session.SourcePath.String()
			database, err := store.Open(t.TempDir() + "/unknown.db")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			writer := &unknownFailingStore{Store: database}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Sources = map[ingest.Harness]ingest.SourceConfig{c.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{session.SourcePath}}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{c.Harness: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta})}
			run := func() *ingest.PipelineResult {
				pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithStore(writer), ingest.WithMetricsStore(writer))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			check := func(result *ingest.PipelineResult) {
				if result.Summary.Indexed != 1 || result.Summary.StoreError != nil {
					t.Fatalf("index failed: %+v logs %+v", result.Summary, result.IndexLog)
				}
				counts := result.Summary.RetainedUnknownKinds
				if len(counts) != 1 || counts[0].Harness != c.Harness || counts[0].Kind != c.Kinds[0] || counts[0].Occurrences != len(c.Kinds) || counts[0].Sessions != 1 {
					t.Fatalf("counts: %+v", counts)
				}
				if len(result.Summary.RefusedRecordKinds) != 0 {
					t.Fatal("unknown data was incorrectly reported as refusal")
				}
				capture, found, err := database.GetSessionContentCapture(ctx, session.SessionID)
				if err != nil || !found || capture.FailureCode != ingest.ContentCaptureUnknownDataRetained || capture.Status != ingest.ContentCaptureIncomplete {
					t.Fatalf("capture: %+v %v", capture, err)
				}
				if store.PublishableWithOmissions(capture) {
					t.Fatal("local Extra must not certify outbound projection")
				}
				if _, err := database.ReadSessionEntries(ctx, session.SessionID, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent}); !errors.Is(err, store.ErrContentCaptureIncomplete) {
					t.Fatalf("full-content reader failed to hold unprojected evidence: %v", err)
				}
				entries, err := database.ListEntries(ctx, session.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, entry := range entries {
					records, err := ingest.RetainedUnknownOf(entry)
					if err != nil {
						t.Fatal(err)
					}
					for _, record := range records {
						n++
						if bytes.Contains(record.Payload, []byte(secret)) || !bytes.Contains(record.Payload, []byte("FULL_UNKNOWN_TAIL")) || len(record.Payload) < 8192 {
							t.Fatal("unknown payload bypassed redaction or was truncated")
						}
					}
				}
				if n != len(c.Kinds) {
					t.Fatalf("stored occurrences %d want %d", n, len(c.Kinds))
				}
			}
			check(run())
			before, err := database.ListEntries(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			// Remove the provider source and discovery results: force reindex
			// must take the retained batch path, not streamed native extraction.
			if err := fs.Remove(session.SourcePath.String()); err != nil {
				t.Fatal(err)
			}
			adapters[c.Harness] = makeStubAdapter(nil, nil)
			cfg.Reindex, cfg.Force = true, true
			check(run())
			after, err := database.ListEntries(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(after)
			if !bytes.Equal(a, b) {
				t.Fatal("retained fallback changed evidence")
			}
			// Transaction failure must not certify accounting for this run, nor
			// replace any previously committed evidence.
			writer.fail = true
			failed := run()
			if failed.Summary.Indexed != 0 || len(failed.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("failed writes certified evidence: %+v", failed.Summary)
			}
			after, err = database.ListEntries(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			a, _ = json.Marshal(after)
			if !bytes.Equal(a, b) {
				t.Fatal("failed write changed prior evidence")
			}
			writer.fail = false
			if corrupt[c.Harness] == "" {
				t.Fatal("missing corruption fixture for production-path regression")
			}
			if err := fs.WriteFile(session.SourcePath.String(), []byte(corrupt[c.Harness]), 0600); err != nil {
				t.Fatal(err)
			}
			adapters[c.Harness] = makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta})
			cfg.Reindex = false
			damaged := run()
			if damaged.Summary.Indexed != 0 || len(damaged.Summary.RetainedUnknownKinds) != 0 {
				t.Fatalf("damaged source certified: %+v", damaged.Summary)
			}
			foundError := false
			for _, log := range damaged.IndexLog {
				if log.Outcome == ingest.IndexOutcomeError {
					foundError = true
				}
			}
			if !foundError {
				t.Fatalf("damaged source did not exercise capture validation: %+v", damaged.IndexLog)
			}
			after, err = database.ListEntries(ctx, session.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			a, _ = json.Marshal(after)
			if !bytes.Equal(a, b) {
				t.Fatal("corruption replaced prior good evidence")
			}
		})
	}
}
