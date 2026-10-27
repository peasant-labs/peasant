package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
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
	db, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations(), store.WithPoolSize(1))
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
	pipeline, err := newTestPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(),
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

// TestStoredMetricRefreshAppliesTheRunSessionFilter pins the stored metrics
// refresh to the run's shared scope at the call site. The scope rule itself is
// covered case by case where it is stated (the index inventory and the pair
// repair); this test answers the separate question of whether the refresh asks
// it at all, because a refresh that stopped asking would keep computing
// sessions the run excluded while every rule-level test stayed green.
func TestStoredMetricRefreshAppliesTheRunSessionFilter(t *testing.T) {
	t.Parallel()
	db, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations(), store.WithPoolSize(1))
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
	// Discovery selects nothing, so the only downstream work available to the
	// run is the stored maintenance this test is about.
	run := func(scope map[ingest.SessionID]bool) ingest.PipelineResult {
		t.Helper()
		config := makePipelineConfig(testOutputDir)
		config.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
		config.AllowedSessionIDs = scope
		pipeline, err := newTestPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(),
			map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}, config,
			ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithAnalyzer(metrics.NewEngine(&metricsRefreshStore{Store: db})))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(t.Context())
		if err != nil {
			t.Fatalf("stored metrics refresh run: %v", err)
		}
		return *result
	}
	outside := run(map[ingest.SessionID]bool{"22222222-2222-4222-8222-222222222222": true})
	if outside.Summary.Computed != 0 {
		t.Fatalf("the refresh computed metrics for %d sessions outside the run's session filter, want 0", outside.Summary.Computed)
	}
	// The second run is the control for the first: the same stored session under
	// a scope that covers it, so a zero above means the filter excluded it rather
	// than that the refresh had no work to do.
	inside := run(map[ingest.SessionID]bool{sid: true})
	if inside.Summary.Computed != 1 {
		t.Fatalf("the refresh computed metrics for %d sessions inside the run's session filter, want 1", inside.Summary.Computed)
	}
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
	if err := testutil.DecodeFixtureYAML(metricsRefreshYAML, &fixture); err != nil {
		t.Fatal(err)
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
			db, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations(), store.WithPoolSize(1))
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
			pipeline, err := newTestPipeline(filesystem, testutil.DefaultGitResolver(), ingest.DefaultAdapterRegistry, config,
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
				// The forced run tries native input first; the fixture's recorded
				// source is synthetic and absent, so that warning is expected and
				// is not a metrics diagnostic.
				var unexpected []ingest.DiagnosticEntry
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.ErrorType != "native_refresh_unavailable" {
						unexpected = append(unexpected, diagnostic)
					}
				}
				if len(unexpected) != 0 {
					t.Fatalf("successful metrics produced warning: %+v", unexpected)
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
