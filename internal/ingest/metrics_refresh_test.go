package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metrics_refresh.yaml
var metricsRefreshYAML []byte

type metricsRefreshStore struct {
	*store.Store
	fail bool
}

var _ ingest.MetricsStore = (*metricsRefreshStore)(nil)

func (s *metricsRefreshStore) SaveMetricsForInput(ctx context.Context, input *ingest.MetricInput, value *ingest.SessionMetrics) error {
	if s.fail {
		return errors.New("synthetic metric save failure")
	}
	return s.Store.SaveMetricsForInput(ctx, input, value)
}

func TestPersistentHarvestRetriesStoredDownstreamWithoutIndexing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"), store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	meta := makeReindexMeta(t, testutil.TestSessionUUID, "/synthetic/unavailable/session.jsonl")
	meta.Project.Hash = testutil.TestProjectHash
	sid := meta.SessionID
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	entry := schema.SessionEntry{SessionID: sid, Harness: meta.ModelHarness, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser}
	if err := db.IndexSessionEntries(t.Context(), sid, []schema.SessionEntry{entry}); err != nil {
		t.Fatal(err)
	}
	backing := &metricsRefreshStore{Store: db, fail: true}
	classifier := &testutil.StubSessionClassifier{Err: errors.New("synthetic classifier failure")}
	config := makePipelineConfig(testOutputDir)
	// No native or retained files exist. Discovery selection excludes everything;
	// previously stored sessions still receive the invoked downstream maintenance.
	config.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
	pipeline, err := ingest.NewPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(),
		map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}, config,
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithAnalyzer(metrics.NewEngine(backing)), ingest.WithClassifier(classifier))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := pipeline.Run(t.Context())
	if err != nil || failed.Summary.Indexed != 0 || failed.Summary.Computed != 0 || len(classifier.Annotated) != 0 {
		t.Fatalf("failed metrics authorized classification or failed harvest: %+v %v", failed, err)
	}
	backing.fail = false
	retried, err := pipeline.Run(t.Context())
	if err != nil || retried.Summary.Indexed != 0 || retried.Summary.Computed != 1 || len(classifier.Annotated) != 1 {
		t.Fatalf("stored-only metrics were not retried: %+v %v", retried, err)
	}
	before, err := db.GetMetrics(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	input, err := db.ReadMetricInput(t.Context(), sid, true)
	if err != nil || !metrics.MetricsCurrentForInput(input) {
		t.Fatalf("coherent classifier capture did not recognize current default-engine metrics: %v", err)
	}
	classifier.Err = nil
	current, err := pipeline.Run(t.Context())
	if err != nil || current.Summary.Computed != 0 || len(classifier.Annotated) != 2 {
		t.Fatalf("classifier-only retry recomputed metrics or skipped classification: %+v %v", current, err)
	}
	after, err := db.GetMetrics(t.Context(), sid)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("classifier retry changed proven-current metrics")
	}
}

func (s *metricsRefreshStore) SaveMetrics(ctx context.Context, value *ingest.SessionMetrics) error {
	if s.fail {
		return errors.New("synthetic metric save failure")
	}
	return s.Store.SaveMetrics(ctx, value)
}

func TestPipelineRetainsNonfatalMetricRefreshDiagnostics(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		PriorTurns    int      `yaml:"priorTurns"`
		Cases         []struct {
			Name           string `yaml:"name"`
			FailSave       bool   `yaml:"failSave"`
			VersionDelta   int    `yaml:"versionDelta"`
			WantComputed   int    `yaml:"wantComputed"`
			WantDiagnostic string `yaml:"wantDiagnostic"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(metricsRefreshYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("metric refresh fixture requires one document")
	}
	required := []string{"current-index-refreshes-metrics", "failed-save-is-visible", "future-producer-refusal-is-visible"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("metric refresh required-name manifest changed")
	}
	input := loadMetadataReadPolicyFixtures(t)
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid metric refresh fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			db, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			filesystem := testutil.NewMemFS()
			sid := input.SessionID
			meta := makeReindexMeta(t, string(sid), "/synthetic/native/session.jsonl")
			meta.Project.Hash = testutil.TestProjectHash
			_, path := setupPeasantSyncSession(t, filesystem, testOutputDir, testutil.TestHostSlug, string(sid), meta)
			if err := filesystem.WriteFile(path, []byte(input.Transcript), 0600); err != nil {
				t.Fatal(err)
			}
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			version := metrics.CurrentComputeVersion + row.VersionDelta
			if err := db.SaveMetrics(t.Context(), &ingest.SessionMetrics{SessionID: sid, QualityMetrics: schema.QualityMetrics{TurnCount: &fixture.PriorTurns, ComputeVersion: &version}}); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetMetrics(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			config := makePipelineConfig(testOutputDir)
			config.Reindex, config.Force = true, true
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, config,
				ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})),
				ingest.WithAnalyzer(metrics.NewEngine(&metricsRefreshStore{Store: db, fail: row.FailSave})))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil || result.Summary.Errors != 0 || result.Summary.Indexed != 1 || result.Summary.Computed != row.WantComputed {
				t.Fatalf("metric refresh changed nonfatal pipeline outcome: %+v %v", result, err)
			}
			if row.WantDiagnostic == "" {
				if len(result.Diagnostics) != 0 {
					t.Fatalf("successful metrics produced warning: %+v", result.Diagnostics)
				}
			} else {
				found := false
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.ErrorType == "metrics_incomplete" && strings.Contains(diagnostic.Message, row.WantDiagnostic) && diagnostic.Remediation != "" {
						found = true
					}
				}
				if !found {
					t.Fatalf("suppressed-log caller lost metric refusal: %+v", result.Diagnostics)
				}
				after, err := db.GetMetrics(t.Context(), sid)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("failed/refused computation changed last-good metrics: %+v %v", after, err)
				}
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing metric refresh fixture %q", name)
		}
	}
}
