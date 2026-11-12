package ingest_test

import (
	"context"
	_ "embed"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/import_profile.yaml
var importProfileYAML []byte

//go:embed testdata/import_profile.manifest.yaml
var importProfileManifestYAML []byte

type importProfileFixtures struct {
	Sessions []struct {
		Name    string `yaml:"name"`
		ID      string `yaml:"id"`
		Entries int    `yaml:"entries"`
	} `yaml:"sessions"`
	EntryLine  string `yaml:"entryLine"`
	Structural struct {
		SmallEntries int    `yaml:"smallEntries"`
		LargeEntries int    `yaml:"largeEntries"`
		EntryLine    string `yaml:"entryLine"`
	} `yaml:"structural"`
	Discover struct {
		StrayFile string `yaml:"strayFile"`
	} `yaml:"discover"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func LoadImportProfileFixtures(t *testing.T) importProfileFixtures {
	t.Helper()
	var fixtures importProfileFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(importProfileYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode import_profile.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(importProfileManifestYAML, &manifest); err != nil {
		t.Fatalf("decode import_profile manifest: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if present[c.Name] {
			t.Fatalf("import_profile.yaml repeats case %q", c.Name)
		}
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("import_profile.yaml", "cases", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// importProfileBench is one synthetic source tree, one real store, and the
// production pipeline over them.
type importProfileBench struct {
	t         *testing.T
	fixtures  importProfileFixtures
	root      string
	db        *store.Store
	versions  map[ingest.Harness]ingest.HarvesterVersions
	stamp     time.Time
	entryLine string
	// classify wires the metrics engine and the classifier annotator, as
	// `peasant harvest` does.
	classify bool
}

func newImportProfileBench(t *testing.T, fixtures importProfileFixtures) *importProfileBench {
	t.Helper()
	db, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &importProfileBench{
		t: t, fixtures: fixtures, root: t.TempDir(), db: db,
		versions: maps.Clone(ingest.HarvesterVersionRegistry), stamp: time.Unix(1_700_000_000, 0),
		entryLine: fixtures.EntryLine,
	}
}

// writeSession writes one session's source file with the given entry count and
// a modification time that moves forward on every write.
func (b *importProfileBench) writeSession(id string, entries int) {
	b.t.Helper()
	source := filepath.Join(b.root, "source", "-workspace", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		b.t.Fatal(err)
	}
	var content strings.Builder
	for n := range entries {
		line := strings.ReplaceAll(b.entryLine, "{{session}}", id)
		content.WriteString(strings.ReplaceAll(line, "{{n}}", strconv.Itoa(n)))
		content.WriteByte('\n')
	}
	if err := os.WriteFile(source, []byte(content.String()), 0o600); err != nil {
		b.t.Fatal(err)
	}
	b.touch(id)
}

func (b *importProfileBench) touch(id string) {
	b.t.Helper()
	b.stamp = b.stamp.Add(time.Hour)
	source := filepath.Join(b.root, "source", "-workspace", id+".jsonl")
	if err := os.Chtimes(source, b.stamp, b.stamp); err != nil {
		b.t.Fatal(err)
	}
}

func (b *importProfileBench) pipeline(profiler *ingest.IndexProfiler, configure func(*ingest.PipelineConfig)) *ingest.Pipeline {
	b.t.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources:       map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join(b.root, "source"))}}},
		OutputDir:     ingest.ResolvedPath(filepath.Join(b.root, "output")),
		Parallelism:   1,
		IndexProfiler: profiler,
	}
	if configure != nil {
		configure(&cfg)
	}
	options := []ingest.PipelineOption{
		ingest.WithStore(b.db), ingest.WithMetricsStore(b.db), ingest.WithLogger(b.db), ingest.WithIndexLogger(b.db),
		ingest.WithHarvesterVersions(b.versions),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: ingest.NewClaudeIndexer(fs)}),
	}
	if b.classify {
		options = append(options,
			ingest.WithAnalyzer(metrics.NewEngineWithModels(b.db, b.db)),
			ingest.WithClassifier(metrics.NewClassifierAnnotator(b.db, b.db)))
	}
	pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, options...)
	if err != nil {
		b.t.Fatal(err)
	}
	return pipeline
}

// run is one harvest with the always-on profiler, as every import entry point
// runs.
func (b *importProfileBench) run(configure func(*ingest.PipelineConfig)) (*ingest.PipelineResult, *ingest.IndexProfiler) {
	b.t.Helper()
	return b.runWith(&ingest.IndexProfiler{}, configure)
}

func (b *importProfileBench) runWith(profiler *ingest.IndexProfiler, configure func(*ingest.PipelineConfig)) (*ingest.PipelineResult, *ingest.IndexProfiler) {
	b.t.Helper()
	result, err := b.pipeline(profiler, configure).Run(b.t.Context())
	if err != nil {
		b.t.Fatal(err)
	}
	return result, profiler
}

func (b *importProfileBench) queryInt(query string, args ...any) int64 {
	b.t.Helper()
	conn, err := b.db.Pool().Take(b.t.Context())
	if err != nil {
		b.t.Fatal(err)
	}
	defer b.db.Pool().Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		value = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		b.t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func (b *importProfileBench) lastRun() int64 {
	return b.queryInt(`SELECT COALESCE(max(id), 0) FROM ingest_log`)
}

// refreshRows returns the stored refresh reason of each session in one run and
// fails when a session has more than one.
func (b *importProfileBench) refreshRows(runID int64) map[ingest.SessionID]ingest.OutcomeReason {
	b.t.Helper()
	outcomes, err := b.db.ListIngestRunOutcomes(b.t.Context(), runID)
	if err != nil {
		b.t.Fatal(err)
	}
	rows := make(map[ingest.SessionID]ingest.OutcomeReason)
	for _, outcome := range outcomes {
		if outcome.Reason.Kind() != ingest.OutcomeKindRefresh {
			continue
		}
		if _, twice := rows[outcome.SessionID]; twice {
			b.t.Fatalf("session %s has more than one refresh row in run %d", outcome.SessionID, runID)
		}
		rows[outcome.SessionID] = outcome.Reason
	}
	return rows
}

func (b *importProfileBench) wantRefreshRows(label string, want map[string]ingest.OutcomeReason) {
	b.t.Helper()
	got := b.refreshRows(b.lastRun())
	if len(got) != len(want) {
		b.t.Fatalf("%s: refresh rows = %v, want %v", label, got, want)
	}
	for id, reason := range want {
		if got[ingest.SessionID(id)] != reason {
			b.t.Fatalf("%s: session %s refreshed for %q, want %q (all rows %v)", label, id, got[ingest.SessionID(id)].Code(), reason.Code(), got)
		}
	}
}

func TestImportProfileOnTheProductionPipeline(t *testing.T) {
	fixtures := LoadImportProfileFixtures(t)
	bench := newImportProfileBench(t, fixtures)
	first, second := fixtures.Sessions[0], fixtures.Sessions[1]
	for _, session := range fixtures.Sessions {
		bench.writeSession(session.ID, session.Entries)
	}
	firstResult, _ := bench.run(nil)
	firstRun := bench.lastRun()

	runners := map[string]func(t *testing.T){
		"new-session-writes-a-refresh-row": func(t *testing.T) {
			if firstResult.Summary.New != len(fixtures.Sessions) {
				t.Fatalf("first harvest: %+v", firstResult.Summary)
			}
			bench.wantRefreshRows("first harvest", map[string]ingest.OutcomeReason{first.ID: ingest.OutcomeReasonRefreshNew, second.ID: ingest.OutcomeReasonRefreshNew})
		},
		"each-refreshed-session-has-exactly-one-refresh-row-per-run": func(t *testing.T) {
			// refreshRows fails on a second row for one session.
			if rows := bench.refreshRows(firstRun); len(rows) != len(fixtures.Sessions) {
				t.Fatalf("%d sessions have a refresh row, want %d", len(rows), len(fixtures.Sessions))
			}
		},
		"census-by-run-counts-every-refreshed-session": func(t *testing.T) {
			census := bench.queryInt(`SELECT count(*) FROM ingest_run_outcomes o
JOIN ingest_outcome_reasons r ON r.id = o.reason_id JOIN ingest_outcome_kinds k ON k.id = r.kind_id
WHERE k.name = 'refresh' AND o.run_id = ?`, firstRun)
			refreshed := firstResult.Summary.New + firstResult.Summary.Updated
			if int(census) != refreshed || firstResult.Census == nil || firstResult.Census.Sessions() != refreshed {
				t.Fatalf("stored census %d, run census %+v, refreshed %d", census, firstResult.Census, refreshed)
			}
			for _, row := range firstResult.Census.Rows {
				if row.Lane != ingest.RefreshLaneNative || row.Reason != ingest.OutcomeReasonRefreshNew {
					t.Fatalf("census row = %+v, want new sessions on the native lane", row)
				}
			}
		},
		"run-stores-its-stage-rows-and-index-timings": func(t *testing.T) {
			stages, err := bench.db.ListIngestRunStages(t.Context(), firstRun)
			if err != nil {
				t.Fatal(err)
			}
			seen := make(map[ingest.Stage]ingest.IndexProfileStage, len(stages))
			for _, stage := range stages {
				seen[stage.Stage] = stage
			}
			for _, required := range []ingest.Stage{ingest.StageDiscover, ingest.StageDiff, ingest.StageFilter, ingest.StageExtract, ingest.StageIndex, ingest.StageReport} {
				if _, ok := seen[required]; !ok {
					t.Fatalf("run %d stored no %s stage row; stored %+v", firstRun, required, stages)
				}
			}
			if seen[ingest.StageIndex].Bytes <= 0 {
				t.Fatalf("INDEX row carries no bytes: %+v", seen[ingest.StageIndex])
			}
			if firstResult.WriteLane == nil || firstResult.WriteLane.Jobs == 0 || firstResult.WriteLane.Queued != 0 {
				t.Fatalf("write lane totals = %+v, want finished jobs and none waiting", firstResult.WriteLane)
			}
			for _, session := range fixtures.Sessions {
				timings, err := bench.db.ListIndexLogTimings(t.Context(), ingest.SessionID(session.ID))
				if err != nil {
					t.Fatal(err)
				}
				if len(timings) != 1 ||
					timings[0].ParseDuration == nil ||
					timings[0].WriteDuration == nil ||
					timings[0].Bytes == nil ||
					*timings[0].Bytes <= 0 {
					t.Fatalf("session %s index timings = %+v, want one measured attempt", session.ID, timings)
				}
			}
		},
		"discover-stage-row-counts-sessions-not-files": func(t *testing.T) {
			// The live discovery display counts files; the stored row counts
			// the sessions discovery found. A .jsonl file that is not a
			// session must not move it.
			stray := filepath.Join(bench.root, "source", "-workspace", fixtures.Discover.StrayFile)
			if err := os.WriteFile(stray, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(stray) }()
			bench.run(nil)
			stages, err := bench.db.ListIngestRunStages(t.Context(), bench.lastRun())
			if err != nil {
				t.Fatal(err)
			}
			for _, stage := range stages {
				if stage.Stage != ingest.StageDiscover {
					continue
				}
				if stage.Done != len(fixtures.Sessions) || stage.Total != len(fixtures.Sessions) {
					t.Fatalf("DISCOVER row = %+v, want done and total %d sessions (the tree holds %d .jsonl files)", stage, len(fixtures.Sessions), len(fixtures.Sessions)+1)
				}
				return
			}
			t.Fatalf("the run stored no DISCOVER row: %+v", stages)
		},
		"unchanged-session-writes-no-outcome-row": func(t *testing.T) {
			result, _ := bench.run(nil)
			if result.Summary.Unchanged != len(fixtures.Sessions) {
				t.Fatalf("repeat harvest: %+v", result.Summary)
			}
			run := bench.lastRun()
			if run == firstRun {
				t.Fatal("the unchanged harvest stored no audit row")
			}
			if rows := bench.queryInt(`SELECT count(*) FROM ingest_run_outcomes WHERE run_id=?`, run); rows != 0 {
				t.Fatalf("an unchanged harvest wrote %d outcome rows", rows)
			}
			if result.Census == nil || result.Census.Sessions() != 0 {
				t.Fatalf("unchanged harvest census = %+v", result.Census)
			}
			if stages := bench.queryInt(`SELECT count(*) FROM ingest_run_stages WHERE run_id=?`, run); stages == 0 {
				t.Fatal("the unchanged harvest stored no stage rows")
			}
		},
		"compare-only-session-writes-no-outcome-row": func(t *testing.T) {
			// The source's clock moves and its bytes do not: the run must read
			// it to prove it unchanged, and that is not a refresh.
			bench.touch(first.ID)
			result, _ := bench.run(nil)
			if result.Summary.Unchanged != len(fixtures.Sessions) {
				t.Fatalf("compare-only harvest: %+v", result.Summary)
			}
			if rows := bench.queryInt(`SELECT count(*) FROM ingest_run_outcomes WHERE run_id=?`, bench.lastRun()); rows != 0 {
				t.Fatalf("a compare-only harvest wrote %d outcome rows", rows)
			}
		},
		"native-refresh-writes-a-refresh-row": func(t *testing.T) {
			bench.writeSession(second.ID, second.Entries+2)
			result, _ := bench.run(nil)
			if result.Summary.Updated != 1 {
				t.Fatalf("changed-source harvest: %+v", result.Summary)
			}
			bench.wantRefreshRows("changed source", map[string]ingest.OutcomeReason{second.ID: ingest.OutcomeReasonRefreshSourceChanged})
		},
		"stored-index-refresh-writes-a-refresh-row-and-an-index-row": func(t *testing.T) {
			target := bench.versions[ingest.HarnessClaudeCode]
			target.IndexerVersion++
			bench.versions[ingest.HarnessClaudeCode] = target
			before := bench.queryInt(`SELECT count(*) FROM index_log`)
			result, _ := bench.run(nil)
			if result.Summary.Indexed != len(fixtures.Sessions) {
				t.Fatalf("indexer-bump harvest: %+v", result.Summary)
			}
			bench.wantRefreshRows("indexer bump", map[string]ingest.OutcomeReason{first.ID: ingest.OutcomeReasonRefreshIndexerBehind, second.ID: ingest.OutcomeReasonRefreshIndexerBehind})
			if added := bench.queryInt(`SELECT count(*) FROM index_log`) - before; int(added) != len(fixtures.Sessions) {
				t.Fatalf("indexer bump added %d index rows, want %d", added, len(fixtures.Sessions))
			}
			for _, row := range result.Census.Rows {
				if row.Lane != ingest.RefreshLaneStoredIndex || row.SourceBytes <= 0 {
					t.Fatalf("census row = %+v, want the stored-index lane with parsed bytes", row)
				}
			}
		},
		"adapter-bump-refresh-writes-a-refresh-row": func(t *testing.T) {
			target := bench.versions[ingest.HarnessClaudeCode]
			target.AdapterVersion++
			bench.versions[ingest.HarnessClaudeCode] = target
			// The source is unchanged, so the stored transcript serves the
			// refresh and the reason names the stamp, not the source.
			result, _ := bench.run(nil)
			if result.Summary.Updated != len(fixtures.Sessions) {
				t.Fatalf("adapter-bump harvest: %+v", result.Summary)
			}
			bench.wantRefreshRows("adapter bump", map[string]ingest.OutcomeReason{first.ID: ingest.OutcomeReasonRefreshAdapterBehind, second.ID: ingest.OutcomeReasonRefreshAdapterBehind})
		},
		"forced-run-names-every-refresh-forced": func(t *testing.T) {
			bench.run(func(cfg *ingest.PipelineConfig) { cfg.Force = true })
			bench.wantRefreshRows("forced", map[string]ingest.OutcomeReason{first.ID: ingest.OutcomeReasonRefreshForced, second.ID: ingest.OutcomeReasonRefreshForced})
		},
		"dry-run-stores-nothing": func(t *testing.T) {
			before := bench.lastRun()
			bench.writeSession(first.ID, first.Entries+5)
			result, _ := bench.run(func(cfg *ingest.PipelineConfig) { cfg.DryRun = true })
			if result.Summary.Updated != 1 {
				t.Fatalf("dry run: %+v", result.Summary)
			}
			if bench.lastRun() != before {
				t.Fatal("a dry run stored an audit row")
			}
		},
		"cancelled-run-stores-the-profile-it-measured": func(t *testing.T) {
			before := bench.lastRun()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			profiler := &ingest.IndexProfiler{}
			// The run is cancelled once discovery has finished and been timed.
			pipeline := bench.pipeline(profiler, func(cfg *ingest.PipelineConfig) {
				cfg.PrepareSessionFilter = func(context.Context, []ingest.DiscoveredSession) error {
					cancel()
					return nil
				}
			})
			if _, err := pipeline.Run(ctx); err == nil {
				t.Fatal("the cancelled run reported success")
			}
			run := bench.lastRun()
			if run == before {
				t.Fatal("the cancelled run stored no audit row")
			}
			stages, err := bench.db.ListIngestRunStages(t.Context(), run)
			if err != nil {
				t.Fatal(err)
			}
			discovered := false
			for _, stage := range stages {
				if stage.Stage == ingest.StageIndex || stage.Stage == ingest.StageReport {
					t.Fatalf("the cancelled run stored a stage it never reached: %+v", stage)
				}
				discovered = discovered || stage.Stage == ingest.StageDiscover
			}
			if !discovered {
				t.Fatalf("the cancelled run lost the stage it measured: %+v", stages)
			}
			if marked := bench.queryInt(`SELECT count(*) FROM ingest_log WHERE id=? AND error_message LIKE '%cancelled%' AND finished_at IS NOT NULL`, run); marked != 1 {
				t.Fatal("the cancelled run's audit row does not say it was cancelled")
			}
		},
		"profiler-observations-scale-with-batches-sessions-and-stages": func(t *testing.T) {
			observe := func(entries int, detail bool) (int64, int, ingest.IndexProfileSnapshot) {
				sized := newImportProfileBench(t, fixtures)
				sized.classify = true
				sized.entryLine = fixtures.Structural.EntryLine
				for _, session := range fixtures.Sessions {
					sized.writeSession(session.ID, entries)
				}
				result, profiler := sized.runWith(&ingest.IndexProfiler{AnnotationResultDetail: detail}, nil)
				if result.Summary.Indexed != len(fixtures.Sessions) {
					t.Fatalf("import of %d-entry sessions: %+v", entries, result.Summary)
				}
				indexed := 0
				for _, entry := range result.IndexLog {
					indexed += entry.EntriesCount
				}
				return profiler.Observations(), indexed, profiler.Snapshot()
			}
			small, smallEntries, smallProfile := observe(fixtures.Structural.SmallEntries, false)
			large, largeEntries, largeProfile := observe(fixtures.Structural.LargeEntries, false)
			if largeEntries <= smallEntries*10 {
				t.Fatalf("the large import indexed %d entries against %d: the corpus does not grow enough to prove the bound", largeEntries, smallEntries)
			}
			// Same sessions, same stages: the observation count is a function
			// of those and of the drain batches, so it must not move with
			// entries. A drain batch can split on timing (a partial-batch flush
			// wait), at most once per session, so that is the only slack.
			slack := int64(len(fixtures.Sessions))
			if small == 0 || large > small+slack || small > large+slack {
				t.Fatalf("always-on profiler observations grew with entries: %d for %d entries, %d for %d entries (slack %d)", small, smallEntries, large, largeEntries, slack)
			}
			for label, profile := range map[string]ingest.IndexProfileSnapshot{"small": smallProfile, "large": largeProfile} {
				if len(profile.Annotation.AnnotationResults) != 0 {
					t.Fatalf("the always-on %s import timed %d annotation result groups; per-result detail is for --profile-index only", label, len(profile.Annotation.AnnotationResults))
				}
			}
			// The control: with per-result detail the same corpus must reach
			// the per-entry path, one result per added reply, or the bound
			// above proves nothing.
			detailSmall, _, _ := observe(fixtures.Structural.SmallEntries, true)
			detailLarge, _, detailProfile := observe(fixtures.Structural.LargeEntries, true)
			addedReplies := int64(len(fixtures.Sessions) * (fixtures.Structural.LargeEntries - fixtures.Structural.SmallEntries))
			if detailLarge-detailSmall < addedReplies || len(detailProfile.Annotation.AnnotationResults) == 0 {
				t.Fatalf("per-result detail did not grow with entries (%d for %d entries, %d for %d entries): the corpus no longer makes one annotation result per entry", detailSmall, smallEntries, detailLarge, largeEntries)
			}
		},
	}
	for _, c := range fixtures.Cases {
		run, ok := runners[c.Name]
		if !ok {
			t.Fatalf("import_profile.yaml: case %q has no runner; add it to the runner", c.Name)
		}
		if !t.Run(c.Name, run) {
			// The cases share one store in order; a failed case leaves it in
			// a state the later ones do not expect.
			t.FailNow()
		}
	}
}
