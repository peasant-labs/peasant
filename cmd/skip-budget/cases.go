//go:build skipbudget

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/upgrade"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// arm is one side of a case's comparison on the same prepared base.
type arm string

const (
	// armSkip runs the build as shipped, so the planned skip executes.
	armSkip arm = "skip"
	// armBaseline runs the work the skip avoids.
	armBaseline arm = "baseline"
)

// mutation is the change a case applies to the imported base before any round.
type mutation int

const (
	mutateAdapterStamp mutation = iota + 1
	mutateAdapterContext
	mutateIndexer
	mutateAdapterStampSealless
)

// caseSpec is what one corpus case prepares, how its two arms run, and what
// each arm must have executed for the report to be evidence.
type caseSpec struct {
	id       upgrade.MeasurementCase
	mutation mutation
	// skipLabel and baselineLabel describe what each arm executes.
	skipLabel, baselineLabel string
	// baseline adjusts the pipeline config for the baseline arm.
	baseline func(*ingest.PipelineConfig)
	// wantLane is the lane the plan and the skip arm's census must name.
	wantLane ingest.RefreshLane
	// check validates one sample against the case's expected execution.
	check func(sessions int, a arm, s sample) error
	// undeclared plans the base against a table in which the claude-code
	// adapter declares no change scope.
	undeclared bool
}

func disableSkips(cfg *ingest.PipelineConfig) { cfg.DisableRefreshSkips = true }
func forceNative(cfg *ingest.PipelineConfig)  { cfg.Force = true }

func caseSpecs() map[upgrade.MeasurementCase]caseSpec {
	expect := func(a arm, s sample, want executed) error {
		if s.executed != want {
			return fmt.Errorf("%s arm executed %+v, want %+v", a, s.executed, want)
		}
		return nil
	}
	expectLane := func(a arm, s sample, lane ingest.RefreshLane, sessions int) error {
		if s.CensusLanes[lane] != sessions {
			return fmt.Errorf("%s arm census lanes = %v, want %d sessions on %s", a, s.CensusLanes, sessions, lane)
		}
		return nil
	}
	parsesAll := func(n int) executed { return executed{Parses: n} }
	settlesAll := func(n int) executed { return executed{Proofs: n, Proven: n, Settles: n} }
	stampOnly := func(n int, a arm, s sample) error {
		if a == armSkip {
			return expect(a, s, settlesAll(n))
		}
		return expect(a, s, parsesAll(n))
	}
	return map[upgrade.MeasurementCase]caseSpec{
		upgrade.MeasurementCaseStampOnly: {
			id: upgrade.MeasurementCaseStampOnly, mutation: mutateAdapterStamp,
			skipLabel:     "unchanged proof and settle (stored-metadata lane)",
			baselineLabel: "stored-metadata lane with the refresh skips disabled: parse, identical generation not installed",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneStoredMetadata, check: stampOnly,
		},
		upgrade.MeasurementCaseContextChange: {
			id: upgrade.MeasurementCaseContextChange, mutation: mutateAdapterContext,
			skipLabel:     "proof not proven, metadata-only copy refresh (stored-metadata lane)",
			baselineLabel: "stored-metadata lane with the refresh skips disabled: parse and install",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneStoredMetadata,
			check: func(n int, a arm, s sample) error {
				if a == armBaseline {
					return expect(a, s, parsesAll(n))
				}
				return expect(a, s, executed{Proofs: n, Copies: n})
			},
		},
		upgrade.MeasurementCaseIndexerBump: {
			id: upgrade.MeasurementCaseIndexerBump, mutation: mutateIndexer,
			skipLabel:     "stored-index lane: re-index the stored transcript",
			baselineLabel: "native lane (forced run): capture the source and parse",
			baseline:      forceNative, wantLane: ingest.RefreshLaneStoredIndex,
			check: func(n int, a arm, s sample) error {
				if err := expect(a, s, parsesAll(n)); err != nil {
					return err
				}
				if a == armBaseline {
					return expectLane(a, s, ingest.RefreshLaneNative, n)
				}
				return expectLane(a, s, ingest.RefreshLaneStoredIndex, n)
			},
		},
		upgrade.MeasurementCaseLegacyRow: {
			id: upgrade.MeasurementCaseLegacyRow, mutation: mutateAdapterStampSealless,
			skipLabel:     "unchanged proof without a seal (not proven), then the fallback the build takes",
			baselineLabel: "stored-metadata lane with the refresh skips disabled: parse",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneStoredMetadata,
			check: func(n int, a arm, s sample) error {
				if a == armBaseline {
					return expect(a, s, parsesAll(n))
				}
				// The seal is absent on every generation, so no proof may
				// hold. The fallback after it (a copy refresh or a parse) is
				// what this case reports, so it is not fixed here.
				fallback := s
				fallback.Copies, fallback.Parses = 0, 0
				return expect(a, fallback, executed{Proofs: n})
			},
		},
		upgrade.MeasurementCaseUndeclaredProducer: {
			id: upgrade.MeasurementCaseUndeclaredProducer, mutation: mutateAdapterStamp,
			skipLabel:     "declared adapter, as shipped: unchanged proof and settle",
			baselineLabel: "the full step the plan names for the undeclared adapter: native capture and parse (forced run)",
			baseline:      forceNative, wantLane: ingest.RefreshLaneStoredMetadata, undeclared: true,
			check: func(n int, a arm, s sample) error {
				if a == armSkip {
					return expect(a, s, settlesAll(n))
				}
				if err := expect(a, s, parsesAll(n)); err != nil {
					return err
				}
				return expectLane(a, s, ingest.RefreshLaneNative, n)
			},
		},
	}
}

// executed counts the work one run did, so a check compares it as a whole.
type executed struct {
	Proofs  int `json:"proofs"`
	Proven  int `json:"proven"`
	Settles int `json:"settles"`
	Copies  int `json:"copies"`
	Parses  int `json:"parses"`
}

// sample is one timed pipeline run over a fresh copy of the prepared base.
type sample struct {
	Round        int     `json:"round"`
	Arm          arm     `json:"arm"`
	WallMs       float64 `json:"wallMs"`
	ProofMs      float64 `json:"proofMs"`
	IndexStageMs float64 `json:"indexStageMs"`
	ParseMs      float64 `json:"parseMs"`
	WriteMs      float64 `json:"writeMs"`
	executed
	ParsedBytes  int64                       `json:"parsedBytes"`
	Outcomes     map[ingest.IndexOutcome]int `json:"outcomes"`
	Census       []ingest.RefreshCensusRow   `json:"census"`
	CensusLanes  map[ingest.RefreshLane]int  `json:"censusLanes"`
	WriteLane    *ingest.WriteLaneStats      `json:"writeLane,omitempty"`
	DBBytes      int64                       `json:"dbBytesBefore"`
	DBBytesAfter int64                       `json:"dbBytesAfter"`
	WALBytes     int64                       `json:"walBytesAfter"`
}

// armSummary is the order statistics of one arm's samples.
type armSummary struct {
	Label         string   `json:"label"`
	Samples       []sample `json:"samples"`
	MedianWallMs  float64  `json:"medianWallMs"`
	MinWallMs     float64  `json:"minWallMs"`
	MaxWallMs     float64  `json:"maxWallMs"`
	MedianProofMs float64  `json:"medianProofMs"`
	MedianParseMs float64  `json:"medianParseMs"`
	MedianIndexMs float64  `json:"medianIndexStageMs"`
}

type caseShape struct {
	Sessions    int   `json:"sessions"`
	Entries     int   `json:"entriesPerSession"`
	CorpusBytes int64 `json:"corpusBytes"`
	PrepareMs   int64 `json:"prepareMs"`
}

// planEvidence is what the read-only planner says about the prepared base.
type planEvidence struct {
	Sessions       int                        `json:"sessions"`
	ByLane         map[ingest.RefreshLane]int `json:"byLane"`
	FullStep       map[string]int             `json:"fullStep"`
	DeclaredByLane map[ingest.RefreshLane]int `json:"declaredByLane,omitempty"`
}

type caseReport struct {
	Provenance upgrade.BudgetProvenance `json:"provenance"`
	Dirty      bool                     `json:"dirtyWorktree"`
	Case       upgrade.MeasurementCase  `json:"case"`
	Shape      caseShape                `json:"shape"`
	Smoke      bool                     `json:"smoke"`
	Plan       planEvidence             `json:"plan"`
	Skip       armSummary               `json:"skip"`
	Baseline   armSummary               `json:"baseline"`
	// NotProvenRate is the share of attempted proofs that did not hold in
	// the skip arm, summed over rounds.
	NotProvenRate float64  `json:"notProvenRate"`
	Notes         []string `json:"notes"`
}

// harnessOptions is one invocation's shape and provenance.
type harnessOptions struct {
	root     string
	sessions int
	entries  int
	rounds   int
	head     string
	base     string
	command  string
	dirty    bool
}

// matrixShape is the size the issue fixes for a budget measurement; anything
// smaller is a smoke run and its report says so.
const (
	matrixSessions = 240
	matrixEntries  = 1000
)

func runCase(ctx context.Context, spec caseSpec, opts harnessOptions) (string, error) {
	dir, err := cleanCaseDir(opts.root, spec.id)
	if err != nil {
		return "", err
	}
	baseDir := filepath.Join(dir, "base")
	shape, err := prepareBase(ctx, baseDir, opts.sessions, opts.entries, spec.mutation)
	if err != nil {
		return "", fmt.Errorf("prepare %s base in %s: %w", spec.id, baseDir, err)
	}
	report := caseReport{
		Provenance: upgrade.BudgetProvenance{Head: opts.head, Base: opts.base, Command: opts.command, Date: time.Now().UTC().Format(time.RFC3339), Samples: opts.rounds},
		Dirty:      opts.dirty, Case: spec.id, Shape: shape,
		Smoke: opts.sessions < matrixSessions || opts.entries < matrixEntries,
		Skip:  armSummary{Label: spec.skipLabel}, Baseline: armSummary{Label: spec.baselineLabel},
	}
	if report.Plan, err = planBase(ctx, baseDir, spec, opts.sessions); err != nil {
		return "", fmt.Errorf("plan %s base: %w", spec.id, err)
	}
	for round := 1; round <= opts.rounds; round++ {
		// Alternate the arm order so a warm cache or a slow round does not
		// always favour the same arm.
		order := []arm{armSkip, armBaseline}
		if round%2 == 0 {
			order = []arm{armBaseline, armSkip}
		}
		for _, a := range order {
			s, err := runRound(ctx, spec, baseDir, filepath.Join(dir, fmt.Sprintf("round-%d-%s", round, a)), round, a)
			if err != nil {
				return "", fmt.Errorf("%s round %d %s arm: %w", spec.id, round, a, err)
			}
			if err := spec.check(opts.sessions, a, s); err != nil {
				return "", fmt.Errorf("%s round %d: %w", spec.id, round, err)
			}
			if a == armSkip {
				report.Skip.Samples = append(report.Skip.Samples, s)
			} else {
				report.Baseline.Samples = append(report.Baseline.Samples, s)
			}
		}
	}
	summarize(&report.Skip)
	summarize(&report.Baseline)
	proofs, proven := 0, 0
	for _, s := range report.Skip.Samples {
		proofs += s.Proofs
		proven += s.Proven
	}
	if proofs > 0 {
		report.NotProvenRate = float64(proofs-proven) / float64(proofs)
	}
	report.Notes = append(report.Notes,
		"Each round copies the prepared base outside the timed region and times one pipeline run; arms alternate order by round.",
		"Settle and copy counts come from a store wrapper that counts calls; parses are index-log attempts with a measured parse duration.",
		"INDEX includes settlement; no separate settle duration is persisted.")
	if report.Smoke {
		report.Notes = append(report.Notes, fmt.Sprintf("Smoke shape %d x %d: validates the case, not a budget. Budgets need %d x %d on a quiet machine.", opts.sessions, opts.entries, matrixSessions, matrixEntries))
	}
	if spec.undeclared {
		report.Notes = append(report.Notes, "The pipeline does not read the declaration table; this build declares every adapter. The baseline arm runs the full step the plan names for the undeclared adapter.")
	}
	path := filepath.Join(dir, "report.json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write report %s: %w", path, err)
	}
	return path, nil
}

// cleanCaseDir removes and recreates root/<case>. It refuses a root that is
// not an absolute directory path, so a bad flag cannot remove anything else.
func cleanCaseDir(root string, id upgrade.MeasurementCase) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return "", fmt.Errorf("case root %q must be an absolute directory below /; pass -root", root)
	}
	dir := filepath.Join(filepath.Clean(root), string(id))
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clean case directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create case directory %s: %w", dir, err)
	}
	return dir, nil
}

func summarize(a *armSummary) {
	pick := func(value func(sample) float64) []float64 {
		values := make([]float64, 0, len(a.Samples))
		for _, s := range a.Samples {
			values = append(values, value(s))
		}
		slices.Sort(values)
		return values
	}
	median := func(values []float64) float64 {
		if len(values) == 0 {
			return 0
		}
		return values[(len(values)-1)/2]
	}
	wall := pick(func(s sample) float64 { return s.WallMs })
	if len(wall) > 0 {
		a.MedianWallMs, a.MinWallMs, a.MaxWallMs = median(wall), wall[0], wall[len(wall)-1]
	}
	a.MedianProofMs = median(pick(func(s sample) float64 { return s.ProofMs }))
	a.MedianParseMs = median(pick(func(s sample) float64 { return s.ParseMs }))
	a.MedianIndexMs = median(pick(func(s sample) float64 { return s.IndexStageMs }))
}

// countingStore counts the two store calls the profile cannot tell apart: an
// unchanged settle and a metadata-only copy refresh. Every other capability is
// promoted from the embedded store unchanged.
type countingStore struct {
	*store.Store
	settles atomic.Int64
	copies  atomic.Int64
}

var (
	_ ingest.ManagedGenerationUnchangedProver = (*countingStore)(nil)
	_ ingest.ManagedGenerationRefresher       = (*countingStore)(nil)
)

func (s *countingStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationRefreshOutcome, error) {
	s.settles.Add(1)
	return s.Store.SettleCapturedGenerationUnchanged(ctx, request)
}

func (s *countingStore) RefreshManagedGenerationWithoutParse(ctx context.Context, request ingest.ManagedGenerationRefreshRequest) (ingest.ManagedGenerationRefreshOutcome, error) {
	s.copies.Add(1)
	return s.Store.RefreshManagedGenerationWithoutParse(ctx, request)
}

func openStore(dbPath, output string) (*store.Store, error) {
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		return nil, fmt.Errorf("managed-generation options for %s: %w", output, err)
	}
	return store.Open(dbPath, options...)
}

// newPipeline wires the pipeline as `peasant harvest` does for one harness,
// with the metrics engine and classifier.
func newPipeline(db *store.Store, metricsStore ingest.MetricsStore, source, output string, profiler *ingest.IndexProfiler, configure func(*ingest.PipelineConfig)) (*ingest.Pipeline, error) {
	fsys := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
		},
		OutputDir:     ingest.ResolvedPath(output),
		IndexProfiler: profiler,
	}
	if configure != nil {
		configure(&cfg)
	}
	return ingest.NewPipeline(fsys, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(metricsStore), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fsys, ingest.IndexerRegistryOptions{})),
		ingest.WithLogger(db), ingest.WithIndexLogger(db),
		ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
		ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)))
}

// prepareBase writes the synthetic corpus, imports it once into a managed
// store, and applies the case's mutation. Every round copies this base.
func prepareBase(ctx context.Context, baseDir string, sessions, entries int, m mutation) (caseShape, error) {
	source, output, dbDir := filepath.Join(baseDir, "source"), filepath.Join(baseDir, "output"), filepath.Join(baseDir, "db")
	for _, dir := range []string{source, output, dbDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return caseShape{}, err
		}
	}
	corpus, err := writeCorpus(source, sessions, entries)
	if err != nil {
		return caseShape{}, err
	}
	db, err := openStore(filepath.Join(dbDir, "peasant.db"), output)
	if err != nil {
		return caseShape{}, err
	}
	defer db.Close()
	pipeline, err := newPipeline(db, db, source, output, &ingest.IndexProfiler{}, nil)
	if err != nil {
		return caseShape{}, err
	}
	start := time.Now()
	result, err := pipeline.Run(ctx)
	if err != nil {
		return caseShape{}, fmt.Errorf("initial import: %w", err)
	}
	prepareMs := time.Since(start).Milliseconds()
	if result.Summary.New != sessions || result.Summary.Errors != 0 {
		return caseShape{}, fmt.Errorf("initial import summary %+v, want %d new sessions and no errors", result.Summary, sessions)
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode]
	switch m {
	case mutateAdapterStamp, mutateAdapterContext, mutateAdapterStampSealless:
		project := ""
		if m == mutateAdapterContext {
			project = "skip budget forced context"
		}
		if err := execSQL(ctx, db, `UPDATE sessions SET adapter_version = ?`, target.AdapterVersion-1); err != nil {
			return caseShape{}, err
		}
		stamped, err := stampMetadata(output, target.AdapterVersion-1, project)
		if err != nil {
			return caseShape{}, err
		}
		if stamped != sessions {
			return caseShape{}, fmt.Errorf("stamped %d metadata files, want %d", stamped, sessions)
		}
		if m == mutateAdapterStampSealless {
			// The generation rows are immutable by trigger; a legacy row
			// predates the seal, so lift the trigger, zero the seal, and
			// restore the trigger exactly as the schema declares it.
			if err := execSQL(ctx, db, `SAVEPOINT sealless;
DROP TRIGGER session_generations_immutable;
UPDATE session_generations SET candidate_digest = '`+strings.Repeat("0", 64)+`';
CREATE TRIGGER session_generations_immutable BEFORE UPDATE ON session_generations
BEGIN
  SELECT RAISE(ABORT, 'generation rows are immutable; a new generation gets a new row');
END;
RELEASE sealless;`); err != nil {
				return caseShape{}, err
			}
		}
	case mutateIndexer:
		if target.IndexerVersion < 2 {
			return caseShape{}, fmt.Errorf("claude indexer target %d cannot be lowered to a positive revision", target.IndexerVersion)
		}
		if err := execSQL(ctx, db, `UPDATE sessions SET index_version = ?`, target.IndexerVersion-1); err != nil {
			return caseShape{}, err
		}
	default:
		return caseShape{}, fmt.Errorf("mutation %d is not defined", m)
	}
	return caseShape{Sessions: sessions, Entries: entries, CorpusBytes: corpus, PrepareMs: prepareMs}, nil
}

// planBase runs the read-only planner on the prepared base. Every session
// must take the case's lane; an undeclared case must instead name the adapter
// and charge the full step.
func planBase(ctx context.Context, baseDir string, spec caseSpec, sessions int) (planEvidence, error) {
	path := filepath.Join(baseDir, "db", "peasant.db")
	declared, err := upgrade.PlanRefresh(ctx, path)
	if err != nil {
		return planEvidence{}, err
	}
	if declared.Estimate.ByLane[spec.wantLane] != sessions {
		return planEvidence{}, fmt.Errorf("plan lanes %v, want %d sessions on %s", declared.Estimate.ByLane, sessions, spec.wantLane)
	}
	plan := declared
	evidence := planEvidence{}
	if spec.undeclared {
		declarations, err := upgrade.ProducerDeclarations()
		if err != nil {
			return planEvidence{}, err
		}
		for i := range declarations {
			if declarations[i].Kind == upgrade.ProducerKindAdapter && declarations[i].Name == string(ingest.HarnessClaudeCode) {
				declarations[i].ChangeScope = ""
			}
		}
		if plan, err = upgrade.PlanRefreshFor(ctx, path, declarations); err != nil {
			return planEvidence{}, err
		}
		want := upgrade.UndeclaredProducer{Kind: upgrade.ProducerKindAdapter, Name: string(ingest.HarnessClaudeCode), Skipped: ingest.RefreshLaneStoredMetadata, Required: upgrade.ChangeScopeMetadataOnly}
		if plan.Estimate.FullStep[want] != sessions || plan.Estimate.ByLane[ingest.RefreshLaneNative] != sessions || plan.Estimate.Undecided != 0 {
			return planEvidence{}, fmt.Errorf("undeclared plan full step %v lanes %v undecided %d, want %d sessions on the full step naming %s", plan.Estimate.FullStep, plan.Estimate.ByLane, plan.Estimate.Undecided, sessions, want)
		}
		evidence.DeclaredByLane = declared.Estimate.ByLane
	}
	evidence.Sessions, evidence.ByLane, evidence.FullStep = plan.Estimate.Sessions, plan.Estimate.ByLane, map[string]int{}
	for producer, count := range plan.Estimate.FullStep {
		evidence.FullStep[producer.String()] = count
	}
	return evidence, nil
}

// runRound copies the base, runs one arm, and removes the copy. Only the
// pipeline run is timed.
func runRound(ctx context.Context, spec caseSpec, baseDir, roundDir string, round int, a arm) (sample, error) {
	defer os.RemoveAll(roundDir)
	for _, part := range []string{"db", "output"} {
		if err := copyTree(filepath.Join(baseDir, part), filepath.Join(roundDir, part)); err != nil {
			return sample{}, fmt.Errorf("copy base %s: %w", part, err)
		}
	}
	dbPath, output := filepath.Join(roundDir, "db", "peasant.db"), filepath.Join(roundDir, "output")
	db, err := openStore(dbPath, output)
	if err != nil {
		return sample{}, err
	}
	defer db.Close()
	counted := &countingStore{Store: db}
	profiler := &ingest.IndexProfiler{}
	var configure func(*ingest.PipelineConfig)
	if a == armBaseline {
		configure = spec.baseline
	}
	// Discovery reads the prepared source in place: it is never written.
	pipeline, err := newPipeline(db, counted, filepath.Join(baseDir, "source"), output, profiler, configure)
	if err != nil {
		return sample{}, err
	}
	s := sample{Round: round, Arm: a, Outcomes: map[ingest.IndexOutcome]int{}, CensusLanes: map[ingest.RefreshLane]int{}}
	s.DBBytes = fileSize(dbPath)
	start := time.Now()
	result, err := pipeline.Run(ctx)
	s.WallMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		return sample{}, err
	}
	if result.Summary.Errors != 0 {
		return sample{}, fmt.Errorf("run reported %d errors", result.Summary.Errors)
	}
	s.DBBytesAfter, s.WALBytes = fileSize(dbPath), fileSize(dbPath+"-wal")
	s.Settles, s.Copies = int(counted.settles.Load()), int(counted.copies.Load())
	for _, entry := range result.IndexLog {
		s.Outcomes[entry.Outcome]++
		if entry.ParseDuration != nil && *entry.ParseDuration > 0 {
			s.Parses++
			s.ParseMs += float64(entry.ParseDuration.Microseconds()) / 1000
		}
		if entry.WriteDuration != nil {
			s.WriteMs += float64(entry.WriteDuration.Microseconds()) / 1000
		}
		if entry.Bytes != nil {
			s.ParsedBytes += *entry.Bytes
		}
	}
	for _, stage := range profiler.StageTotals() {
		switch stage.Stage {
		case ingest.StageRefreshProof:
			s.Proofs, s.Proven = stage.Total, stage.Done
			s.ProofMs = float64(stage.Duration.Microseconds()) / 1000
		case ingest.StageIndex:
			s.IndexStageMs = float64(stage.Duration.Microseconds()) / 1000
		}
	}
	if result.Census != nil {
		s.Census = result.Census.Rows
		for _, row := range result.Census.Rows {
			s.CensusLanes[row.Lane] += row.Sessions
		}
	}
	if result.WriteLane != nil {
		lane := *result.WriteLane
		s.WriteLane = &lane
	}
	return s, nil
}

func execSQL(ctx context.Context, db *store.Store, query string, args ...any) error {
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return err
	}
	defer db.Pool().Put(conn)
	run := func() error { return sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}) }
	if len(args) == 0 && strings.Count(query, ";") > 1 {
		run = func() error { return sqlitex.ExecuteScript(conn, query, nil) }
	}
	if err := run(); err != nil {
		return fmt.Errorf("prepare statement %q: %w", strings.SplitN(query, "\n", 2)[0], err)
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// writeCorpus writes one deterministic synthetic Claude JSONL file per
// session. The text is synthetic and carries no user data.
func writeCorpus(root string, sessions, entries int) (int64, error) {
	filler := strings.Repeat("synthetic transcript text for the skip budget measurement. ", 12)
	workspace := filepath.Join(root, "-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return 0, err
	}
	var total int64
	for n := range sessions {
		id := fmt.Sprintf("5b000000-0000-4000-8000-%012d", n+1)
		var content strings.Builder
		for e := range entries {
			at := time.Unix(1_700_000_000+int64(e), 0).UTC().Format(time.RFC3339)
			if e%2 == 0 {
				fmt.Fprintf(&content, `{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"prompt %d: %s"}}`+"\n", id, e, at, e, filler)
				continue
			}
			fmt.Fprintf(&content, `{"type":"assistant","sessionId":"%s","uuid":"a%d","parentUuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"reply %d: %s"}],"usage":{"input_tokens":120,"output_tokens":80}}}`+"\n", id, e, e-1, at, e, filler)
		}
		path := filepath.Join(workspace, id+".jsonl")
		if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
			return 0, err
		}
		stamp := time.Unix(1_700_000_000+int64(n), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return 0, err
		}
		total += int64(content.Len())
	}
	return total, nil
}

// stampMetadata rewrites every managed metadata file's adapter revision, and
// its project name when one is given, then recomputes the metadata hash, as
// an older build would have left the pair.
func stampMetadata(output string, revision int, project string) (int, error) {
	count := 0
	err := filepath.WalkDir(output, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), defaults.MetadataSuffix) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("decode managed metadata %s: %w", path, err)
		}
		meta.AdapterVersion = &revision
		if project != "" {
			meta.Project.Name = project
		}
		meta.MetadataHash = schema.ComputeMetadataHash(&meta)
		encoded, err := json.Marshal(&meta)
		if err != nil {
			return err
		}
		temp := path + ".stamp"
		if err := os.WriteFile(temp, encoded, 0o600); err != nil {
			return err
		}
		if err := os.Rename(temp, path); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}
