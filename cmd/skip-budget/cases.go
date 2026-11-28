//go:build skipbudget

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/upgrade"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
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

var allArms = []arm{armSkip, armBaseline}

func newArm(raw string) (arm, error) {
	a := arm(raw)
	if !slices.Contains(allArms, a) {
		return "", fmt.Errorf("arm %q is not one of %v", raw, allArms)
	}
	return a, nil
}

// mutation is the change a case applies to the imported base before any round.
type mutation int

// The zero value is not a mutation, so a case that forgets to name one fails
// to prepare instead of measuring an unchanged base.
const (
	mutateAdapterStamp mutation = iota + 1
	mutateIndexer
	// mutateStaleMetadataHash moves the adapter stamp back by one, as
	// mutateAdapterStamp does, and forges every stored metadata_hash to
	// another non-zero value, so the cheap proof fails on the metadata
	// context while the stored context is unchanged.
	mutateStaleMetadataHash
	// mutateContextChange moves the adapter stamp back by one, as
	// mutateAdapterStamp does, and renames the project in every managed
	// metadata file, so the refreshed capture carries a changed context over
	// unchanged transcript bytes.
	mutateContextChange
	// mutateOpenCodeProjectName moves the adapter stamp back by one and
	// updates only project.name in the synthetic OpenCode SQLite database,
	// leaving worktree, git remote, HostSlug, and transcript bytes fixed.
	mutateOpenCodeProjectName
	// mutateNone leaves the imported base as it is. Only the new-session
	// smoke case uses it: it times the import itself.
	mutateNone
)

// defined reports whether m is one of the mutations above.
func (m mutation) defined() bool { return m >= mutateAdapterStamp && m <= mutateNone }

// caseSpec is what one corpus case prepares, how its two arms run, and what
// each arm must have executed for the report to be evidence.
type caseSpec struct {
	id       upgrade.MeasurementCase
	harness  ingest.Harness
	mutation mutation
	// skipLabel and baselineLabel describe what each arm executes.
	skipLabel, baselineLabel string
	// baseline adjusts the pipeline config for the baseline arm.
	baseline func(*ingest.PipelineConfig)
	// wantLane is the lane the plan and the skip arm's census must name.
	wantLane ingest.RefreshLane
	// check validates one sample against the case's expected execution.
	check func(sessions int, a arm, s sample) error
	// undeclared registers, for the baseline arm and its plan, a claude-code
	// adapter that withholds its change scope. The baseline is then a real
	// run in which the undeclared adapter makes the pipeline take the full
	// step.
	undeclared bool
	// equivalent requires, after every round, that both arms left every
	// session's entries, metrics, captured stats, annotations and session
	// row equal (arm_equivalence_allowlist.yaml names the exceptions).
	equivalent bool
}

func (s caseSpec) targetHarness() ingest.Harness {
	if s.harness == "" {
		return ingest.HarnessClaudeCode
	}
	return s.harness
}

// withholdingClaudeAdapter is the claude-code adapter registered as one that
// withholds its change scope: an undeclared producer.
type withholdingClaudeAdapter struct {
	*ingest.ClaudeAdapter
}

func (*withholdingClaudeAdapter) WithholdsChangeScope() bool { return true }

var _ ingest.ChangeScopeWithholder = (*withholdingClaudeAdapter)(nil)

// undeclaredAdapters is the shipped registry with the claude-code adapter
// replaced by one that withholds its change scope.
func undeclaredAdapters() map[ingest.Harness]ingest.AdapterFactory {
	adapters := maps.Clone(ingest.DefaultAdapterRegistry)
	adapters[ingest.HarnessClaudeCode] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
		return &withholdingClaudeAdapter{ClaudeAdapter: ingest.NewClaudeAdapter(fs, git, s)}
	}
	return adapters
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
	validatesAll := func(n int) executed {
		return executed{Proofs: n, Candidates: n, Validations: n, Validated: n, Settles: n}
	}
	referencesAll := func(n int) executed {
		return executed{Proofs: n, Candidates: n, Validations: n, Plans: n, References: n}
	}
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
		upgrade.MeasurementCaseUndeclaredProducer: {
			id: upgrade.MeasurementCaseUndeclaredProducer, mutation: mutateAdapterStamp,
			skipLabel:     "declared adapter, as shipped: unchanged proof and settle",
			baselineLabel: "adapter registered as undeclared: the pipeline takes the full native step and parses",
			wantLane:      ingest.RefreshLaneStoredMetadata, undeclared: true,
			check: func(n int, a arm, s sample) error {
				if a == armSkip {
					return expect(a, s, settlesAll(n))
				}
				if err := expect(a, s, parsesAll(n)); err != nil {
					return err
				}
				if s.UndeclaredDecisions != n {
					return fmt.Errorf("%s arm: %d session decisions name the undeclared adapter, want %d", a, s.UndeclaredDecisions, n)
				}
				return expectLane(a, s, ingest.RefreshLaneNative, n)
			},
		},
		upgrade.MeasurementCaseHashDrift: {
			id: upgrade.MeasurementCaseHashDrift, mutation: mutateStaleMetadataHash,
			skipLabel:     "stale stored metadata hash, as shipped: cheap proof fails on the metadata context, metadata candidate, validation proven, validated settle",
			baselineLabel: "stored-metadata lane with the refresh skips disabled: parse, identical generation not installed",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneStoredMetadata, equivalent: true,
			check: func(n int, a arm, s sample) error {
				if a == armSkip {
					return expect(a, s, validatesAll(n))
				}
				return expect(a, s, parsesAll(n))
			},
		},
		upgrade.MeasurementCaseContextReference: {
			id: upgrade.MeasurementCaseContextReference, mutation: mutateContextChange,
			skipLabel:     "renamed project, as shipped: cheap proof and validator fail on the context, metadata candidate, reference planned and installed",
			baselineLabel: "stored-metadata lane with the refresh skips disabled: parse and install the changed generation",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneStoredMetadata, equivalent: true,
			check: func(n int, a arm, s sample) error {
				if a == armSkip {
					return expect(a, s, referencesAll(n))
				}
				return expect(a, s, parsesAll(n))
			},
		},
		upgrade.MeasurementCaseOpenCodeHashDrift: {
			id: upgrade.MeasurementCaseOpenCodeHashDrift, harness: ingest.HarnessOpenCode, mutation: mutateStaleMetadataHash,
			skipLabel:     "opencode stale stored metadata hash, as shipped: cheap proof downgraded, mandatory validation proven, validated settle",
			baselineLabel: "opencode native lane with the refresh skips disabled: parse, identical generation not installed",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneNative, equivalent: true,
			check: func(n int, a arm, s sample) error {
				if err := expectLane(a, s, ingest.RefreshLaneNative, n); err != nil {
					return err
				}
				if a == armSkip {
					return expect(a, s, validatesAll(n))
				}
				return expect(a, s, parsesAll(n))
			},
		},
		upgrade.MeasurementCaseOpenCodeContextReference: {
			id: upgrade.MeasurementCaseOpenCodeContextReference, harness: ingest.HarnessOpenCode, mutation: mutateOpenCodeProjectName,
			skipLabel:     "opencode renamed project.name, as shipped: mandatory validation fails on the context, metadata candidate, reference planned and installed",
			baselineLabel: "opencode native lane with the refresh skips disabled: parse and install the changed generation",
			baseline:      disableSkips, wantLane: ingest.RefreshLaneNative, equivalent: true,
			check: func(n int, a arm, s sample) error {
				if err := expectLane(a, s, ingest.RefreshLaneNative, n); err != nil {
					return err
				}
				if a == armSkip {
					return expect(a, s, referencesAll(n))
				}
				return expect(a, s, parsesAll(n))
			},
		},
	}
}

// executed counts the work one run did, so a check compares it as a whole.
type executed struct {
	Proofs int `json:"proofs"`
	Proven int `json:"proven"`
	// Candidates counts metadata candidates built, Validations the
	// validator's answers, and Validated its proven answers.
	Candidates  int `json:"candidates"`
	Validations int `json:"validations"`
	Validated   int `json:"validated"`
	// Settles counts settles the store accepted.
	Settles int `json:"settles"`
	// Plans counts reference plans the store accepted, and References the
	// references it installed.
	Plans      int `json:"plans"`
	References int `json:"references"`
	Parses     int `json:"parses"`
}

// sample is one timed pipeline run over a fresh copy of the prepared base.
type sample struct {
	// PID is the child process that ran the sample.
	PID          int     `json:"pid"`
	Round        int     `json:"round"`
	Arm          arm     `json:"arm"`
	WallMs       float64 `json:"wallMs"`
	ProofMs      float64 `json:"proofMs"`
	IndexStageMs float64 `json:"indexStageMs"`
	ParseMs      float64 `json:"parseMs"`
	WriteMs      float64 `json:"writeMs"`
	// ValidateMs is the refresh_validate stage: the metadata half and the
	// validator, summed over sessions.
	ValidateMs float64 `json:"validateMs"`
	// ReferenceMs is the refresh_reference stage: the plan and the install,
	// summed over sessions.
	ReferenceMs float64 `json:"referenceMs"`
	executed
	// Unsupported counts sessions whose validation was unsupported: no
	// candidate, or a validator that could not answer.
	Unsupported int `json:"unsupported"`
	// Traces is what each session went through on the index stage.
	Traces []sessionTrace `json:"traces,omitempty"`
	// Equivalence digests each session's state after the run, per table,
	// for the arm-equivalence check.
	Equivalence map[string]map[string]string `json:"equivalence,omitempty"`
	// UndeclaredDecisions counts session decisions that name the adapter as
	// undeclared.
	UndeclaredDecisions int                         `json:"undeclaredDecisions"`
	ParsedBytes         int64                       `json:"parsedBytes"`
	Outcomes            map[ingest.IndexOutcome]int `json:"outcomes"`
	Census              []censusRow                 `json:"census"`
	CensusLanes         map[ingest.RefreshLane]int  `json:"censusLanes"`
	WriteLane           *ingest.WriteLaneStats      `json:"writeLane,omitempty"`
	DBBytes             int64                       `json:"dbBytesBefore"`
	DBBytesAfter        int64                       `json:"dbBytesAfter"`
	WALBytes            int64                       `json:"walBytesAfter"`
}

// censusRow is one census row as a sample carries it from the child to the
// parent. It writes the row as the census does, reason as its stable code,
// and reads that form back through the reason's validating constructor.
//
// The decoder lives here rather than as RefreshCensusRow.UnmarshalJSON:
// the census is an output the pipeline writes into reports and never reads
// back, and this build-tagged harness is the only reader. A production
// decoder would be a new trust boundary in internal/ingest that nothing
// shipped exercises.
type censusRow struct {
	ingest.RefreshCensusRow
}

func (r *censusRow) UnmarshalJSON(data []byte) error {
	var wire struct {
		Reason      string             `json:"reason"`
		Lane        ingest.RefreshLane `json:"lane"`
		Sessions    int                `json:"sessions"`
		SourceBytes int64              `json:"source_bytes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("decode census row: %w", err)
	}
	reason, err := ingest.NewOutcomeReason(wire.Reason)
	if err != nil {
		return fmt.Errorf("decode census row on lane %s: %w", wire.Lane, err)
	}
	r.RefreshCensusRow = ingest.RefreshCensusRow{Reason: reason, Lane: wire.Lane, Sessions: wire.Sessions, SourceBytes: wire.SourceBytes}
	return nil
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
	// MedianValidateMs is the median refresh_validate stage time.
	MedianValidateMs float64 `json:"medianValidateMs"`
	// MedianReferenceMs is the median refresh_reference stage time.
	MedianReferenceMs float64 `json:"medianReferenceMs"`
}

type caseShape struct {
	Sessions    int   `json:"sessions"`
	Entries     int   `json:"entriesPerSession"`
	CorpusBytes int64 `json:"corpusBytes"`
	PrepareMs   int64 `json:"prepareMs"`
	// ImportedEntries is the entry count the initial import indexed, and
	// ActiveGenerations the sessions it left with an active managed
	// generation.
	ImportedEntries   int   `json:"importedEntries"`
	ActiveGenerations int64 `json:"activeGenerations"`
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
	NotProvenRate float64 `json:"notProvenRate"`
	// Breakdown is the skip arm's sessions by outcome, over every round.
	Breakdown []outcomeRow `json:"breakdown,omitempty"`
	Notes     []string     `json:"notes"`
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

// runCase prepares the case's base once, then runs each arm sample in its own
// child process through command, alternating the arm order by round. A sample
// that did not execute what the case expects fails the case before any
// report is written.
func runCase(ctx context.Context, spec caseSpec, opts harnessOptions, command sampleCommand) (string, error) {
	dir, err := cleanCaseDir(opts.root, spec.id)
	if err != nil {
		return "", err
	}
	baseDir := filepath.Join(dir, "base")
	shape, err := prepareBase(ctx, baseDir, spec.targetHarness(), opts.sessions, opts.entries, spec.mutation)
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
		byArm := map[arm]sample{}
		for _, a := range order {
			s, err := collectRunSample(sampleFlags{Kind: string(sampleKindRun), Dir: filepath.Join(dir, fmt.Sprintf("round-%d-%s", round, a)),
				Base: baseDir, Case: string(spec.id), Arm: string(a), Round: round}, command)
			if err != nil {
				return "", fmt.Errorf("%s round %d %s arm: %w", spec.id, round, a, err)
			}
			if err := spec.check(opts.sessions, a, s); err != nil {
				return "", fmt.Errorf("%s round %d: %w", spec.id, round, err)
			}
			byArm[a] = s
			if a == armSkip {
				report.Skip.Samples = append(report.Skip.Samples, s)
			} else {
				report.Baseline.Samples = append(report.Baseline.Samples, s)
			}
		}
		if spec.equivalent {
			if err := armsEquivalent(byArm[armSkip], byArm[armBaseline]); err != nil {
				return "", fmt.Errorf("%s round %d: %w", spec.id, round, err)
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
		"Each sample runs in its own child process on a fresh heap: it copies the prepared base outside the timed region and times one pipeline run; arms alternate order by round.",
		"Settle counts are settles the store accepted, counted by a store wrapper; candidates, validations and validated answers come from the same wrapper and the traced indexer; parses are index-log attempts with a measured parse duration.",
		"INDEX includes settlement; no separate settle duration is persisted.")
	if spec.equivalent {
		report.Breakdown = breakdown(report.Skip.Samples)
		report.Notes = append(report.Notes,
			"After every round both arms left each session's entries, metrics, captured stats, annotations and session row equal, apart from the columns arm_equivalence_allowlist.yaml names with a reason.",
			"secondScanMs is, for a session that built a metadata candidate and then parsed, that session's own measured metadata half: BuildGeneration runs the same half over the same input again.")
	}
	if report.Smoke {
		report.Notes = append(report.Notes, fmt.Sprintf("Smoke shape %d x %d: validates the case, not a budget. Budgets need %d x %d on a quiet machine.", opts.sessions, opts.entries, matrixSessions, matrixEntries))
	}
	if spec.undeclared {
		report.Notes = append(report.Notes, "The baseline arm registers the claude-code adapter as undeclared, for the plan and the run alike. Both read the declaration through the same refresh decision, so the run takes the full native step the plan names and each session decision names the adapter.")
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
	a.MedianValidateMs = median(pick(func(s sample) float64 { return s.ValidateMs }))
	a.MedianReferenceMs = median(pick(func(s sample) float64 { return s.ReferenceMs }))
}

// countingStore records, per session, the cheap proof, the validator's
// answer and the settle, which the stored profile folds into the INDEX stage.
// Every other capability is promoted from the embedded store unchanged.
type countingStore struct {
	*store.Store
	trace *runTrace
}

var (
	_ ingest.ManagedGenerationUnchangedProver = (*countingStore)(nil)
	_ ingest.ManagedGenerationValidator       = (*countingStore)(nil)
	_ ingest.ManagedGenerationReferencer      = (*countingStore)(nil)
)

// refuseSettlesForTest makes every settle refuse as stale work and every
// reference install refuse as moved, so a test can show a case whose skip arm
// cannot skip fails its executed counts. Only a test child sets it.
var refuseSettlesForTest bool

func (s *countingStore) PlanManagedGenerationReference(ctx context.Context, request ingest.ManagedGenerationReferenceRequest) (ingest.ManagedGenerationReferencePlan, error) {
	s.trace.planning(request.Proof.SessionID)
	plan, err := s.Store.PlanManagedGenerationReference(ctx, request)
	s.trace.planned(request.Proof.SessionID, err)
	return plan, err
}

func (s *countingStore) InstallManagedGenerationReference(ctx context.Context, plan ingest.ManagedGenerationReferencePlan) (ingest.ManagedGenerationReferenceOutcome, error) {
	if refuseSettlesForTest {
		err := &store.ManagedGenerationReferenceError{SessionID: plan.SessionID(), SourceGenerationID: plan.SourceGenerationID(), Reason: ingest.ReferenceRefusalSourceMoved, Detail: "refused by the test store", Where: "skip-budget test store"}
		s.trace.installed(plan.SessionID(), err)
		return ingest.ManagedGenerationReferenceOutcome{}, err
	}
	start := time.Now()
	outcome, err := s.Store.InstallManagedGenerationReference(ctx, plan)
	s.trace.installed(plan.SessionID(), err)
	s.trace.addReferenceTime(plan.SessionID(), time.Since(start))
	return outcome, err
}

func (s *countingStore) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	proof, err := s.Store.ProveCapturedGenerationUnchanged(ctx, request)
	s.trace.proved(request.SessionID, proof, err)
	return proof, err
}

func (s *countingStore) ValidateManagedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	start := time.Now()
	result, err := s.Store.ValidateManagedGenerationUnchanged(ctx, request)
	s.trace.validated(request.SessionID, result, err, time.Since(start))
	return result, err
}

func (s *countingStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	if refuseSettlesForTest {
		err := &ingest.StaleIndexWorkError{SessionID: request.SessionID}
		s.trace.settled(request.SessionID, err)
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, err
	}
	outcome, err := s.Store.SettleCapturedGenerationUnchanged(ctx, request)
	s.trace.settled(request.SessionID, err)
	return outcome, err
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
//
// A traced run (an arm sample) installs its generations under
// armGenerationID; the untraced initial import keeps the random identities.
func newPipeline(db *store.Store, metricsStore ingest.MetricsStore, harness ingest.Harness, adapters map[ingest.Harness]ingest.AdapterFactory, source, output string, profiler *ingest.IndexProfiler, configure func(*ingest.PipelineConfig), trace *runTrace) (*ingest.Pipeline, error) {
	fsys := &ingest.OSFileSystem{}
	indexers := ingest.NewIndexerRegistry(fsys, ingest.IndexerRegistryOptions{})
	if trace != nil {
		switch harness {
		case ingest.HarnessClaudeCode:
			claude, ok := indexers[ingest.HarnessClaudeCode].(*ingest.ClaudeIndexer)
			if !ok {
				return nil, fmt.Errorf("the registered claude-code indexer is %T, not the Claude indexer the trace wraps", indexers[ingest.HarnessClaudeCode])
			}
			indexers[ingest.HarnessClaudeCode] = tracingClaudeIndexer{ClaudeIndexer: claude, trace: trace}
		case ingest.HarnessOpenCode:
			opencode, ok := indexers[ingest.HarnessOpenCode].(*ingest.OpenCodeIndexer)
			if !ok {
				return nil, fmt.Errorf("the registered opencode indexer is %T, not the OpenCode indexer the trace wraps", indexers[ingest.HarnessOpenCode])
			}
			indexers[ingest.HarnessOpenCode] = tracingOpenCodeIndexer{OpenCodeIndexer: opencode, trace: trace}
		default:
			return nil, fmt.Errorf("unsupported traced harness %q", harness)
		}
	}
	var generationIDs func(ingest.DiscoveredSession) string
	if trace != nil {
		generationIDs = armGenerationID
	}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			harness: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
		},
		OutputDir:     ingest.ResolvedPath(output),
		IndexProfiler: profiler,
	}
	if configure != nil {
		configure(&cfg)
	}
	return ingest.NewPipeline(fsys, testutil.NoGitResolver(), adapters, cfg,
		ingest.WithStore(db), ingest.WithMetricsStore(metricsStore), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(indexers), ingest.WithGenerationID(generationIDs),
		ingest.WithLogger(db), ingest.WithIndexLogger(db),
		ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
		ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)))
}

// prepareBase writes the synthetic corpus, imports it once into a managed
// store, and applies the case's mutation. Every round copies this base.
func prepareBase(ctx context.Context, baseDir string, harness ingest.Harness, sessions, entries int, m mutation) (caseShape, error) {
	if !m.defined() {
		return caseShape{}, fmt.Errorf("mutation %d is not defined; name the case's mutation", m)
	}
	source, output, dbDir := filepath.Join(baseDir, "source"), filepath.Join(baseDir, "output"), filepath.Join(baseDir, "db")
	for _, dir := range []string{source, output, dbDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return caseShape{}, err
		}
	}
	var (
		corpus int64
		err    error
	)
	switch harness {
	case ingest.HarnessClaudeCode:
		corpus, err = writeCorpus(source, sessions, entries)
	case ingest.HarnessOpenCode:
		corpus, err = writeOpenCodeCorpus(source, sessions, entries)
	default:
		return caseShape{}, fmt.Errorf("unsupported corpus harness %q", harness)
	}
	if err != nil {
		return caseShape{}, err
	}
	db, err := openStore(filepath.Join(dbDir, "peasant.db"), output)
	if err != nil {
		return caseShape{}, err
	}
	defer db.Close()
	pipeline, err := newPipeline(db, db, harness, ingest.DefaultAdapterRegistry, source, output, &ingest.IndexProfiler{}, nil, nil)
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
	shape := caseShape{Sessions: sessions, Entries: entries, CorpusBytes: corpus, PrepareMs: prepareMs}
	for _, entry := range result.IndexLog {
		shape.ImportedEntries += entry.EntriesCount
	}
	if shape.ActiveGenerations, err = queryInt(ctx, db, `SELECT count(*) FROM sessions WHERE active_generation_id IS NOT NULL`); err != nil {
		return caseShape{}, err
	}
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[harness]
	switch m {
	case mutateNone:
	case mutateAdapterStamp, mutateStaleMetadataHash, mutateContextChange, mutateOpenCodeProjectName:
		if err := execSQL(ctx, db, `UPDATE sessions SET adapter_version = ?`, target.AdapterVersion-1); err != nil {
			return caseShape{}, err
		}
		stamped, err := stampMetadata(output, target.AdapterVersion-1)
		if err != nil {
			return caseShape{}, err
		}
		if stamped != sessions {
			return caseShape{}, fmt.Errorf("stamped %d metadata files, want %d", stamped, sessions)
		}
		if m == mutateContextChange {
			renamed, err := renameProject(output, contextChangeProjectName)
			if err != nil {
				return caseShape{}, err
			}
			if renamed != sessions {
				return caseShape{}, fmt.Errorf("renamed the project in %d metadata files, want %d", renamed, sessions)
			}
		}
		if m == mutateOpenCodeProjectName {
			priorHostSlugs, err := querySessionHostSlugs(ctx, db)
			if err != nil {
				return caseShape{}, err
			}
			if len(priorHostSlugs) != sessions {
				return caseShape{}, fmt.Errorf("read %d prior host slugs, want %d", len(priorHostSlugs), sessions)
			}
			changed, err := renameOpenCodeSQLiteProject(source, contextChangeProjectName)
			if err != nil {
				return caseShape{}, err
			}
			if changed == 0 {
				return caseShape{}, fmt.Errorf("updated 0 project rows in synthetic opencode db")
			}
			if err := assertOpenCodeHostSlugUnchanged(ctx, source, db.InstallationSalt(), priorHostSlugs, contextChangeProjectName); err != nil {
				return caseShape{}, err
			}
		}
		if m == mutateStaleMetadataHash {
			forged, err := forgeStoredMetadataHashes(ctx, db)
			if err != nil {
				return caseShape{}, err
			}
			if forged != int64(sessions) {
				return caseShape{}, fmt.Errorf("forged %d stored metadata hashes, want %d", forged, sessions)
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
	return shape, nil
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
		if plan, err = upgrade.PlanRefreshWithAdapters(ctx, path, undeclaredAdapters()); err != nil {
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
	trace := newRunTrace()
	counted := &countingStore{Store: db, trace: trace}
	profiler := &ingest.IndexProfiler{}
	var configure func(*ingest.PipelineConfig)
	adapters := ingest.DefaultAdapterRegistry
	if a == armBaseline {
		configure = spec.baseline
		if spec.undeclared {
			adapters = undeclaredAdapters()
		}
	}
	// Discovery reads the prepared source in place: it is never written.
	pipeline, err := newPipeline(db, counted, spec.targetHarness(), adapters, filepath.Join(baseDir, "source"), output, profiler, configure, trace)
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
	trace.count(&s.executed, &s.Unsupported)
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
		case ingest.StageRefreshValidate:
			s.ValidateMs = float64(stage.Duration.Microseconds()) / 1000
		case ingest.StageRefreshReference:
			s.ReferenceMs = float64(stage.Duration.Microseconds()) / 1000
		}
	}
	for _, session := range result.Sessions {
		if session.Decision.AdapterUndeclared {
			s.UndeclaredDecisions++
		}
	}
	if result.Census != nil {
		for _, row := range result.Census.Rows {
			s.Census = append(s.Census, censusRow{row})
			s.CensusLanes[row.Lane] += row.Sessions
		}
	}
	if result.WriteLane != nil {
		lane := *result.WriteLane
		s.WriteLane = &lane
	}
	s.Traces = trace.sessions(result)
	if spec.equivalent {
		if s.Equivalence, err = digestArmState(ctx, db); err != nil {
			return sample{}, fmt.Errorf("digest the %s arm's state for the equivalence check: %w", a, err)
		}
	}
	return s, nil
}

func execSQL(ctx context.Context, db *store.Store, query string, args ...any) error {
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return err
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		return fmt.Errorf("prepare statement %q: %w", strings.SplitN(query, "\n", 2)[0], err)
	}
	return nil
}

func queryInt(ctx context.Context, db *store.Store, query string) (int64, error) {
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return 0, err
	}
	defer db.Pool().Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		value = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		return 0, fmt.Errorf("query %q: %w", query, err)
	}
	return value, nil
}

func querySessionHostSlugs(ctx context.Context, db *store.Store) (map[ingest.SessionID]string, error) {
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Pool().Put(conn)
	slugs := map[ingest.SessionID]string{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT s.session_id, h.host_slug FROM sessions s JOIN host_slugs h ON s.opaque_host_id = h.opaque_id`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		slugs[ingest.SessionID(stmt.ColumnText(0))] = stmt.ColumnText(1)
		return nil
	}}); err != nil {
		return nil, fmt.Errorf("query session host slugs: %w", err)
	}
	return slugs, nil
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

// contextChangeProjectName is the project name mutateContextChange writes.
const contextChangeProjectName = "skip-budget-renamed-project"

// armGenerationID names the generation an arm installs for a session. Both
// arms of a round install under the same identity, so the session row they
// leave compares equal; the prepared base's generations keep the random
// identities the import minted, so the names never collide.
func armGenerationID(session ingest.DiscoveredSession) string {
	return "g_arm_" + strings.ReplaceAll(string(session.SessionID), "-", "")
}

// renameProject rewrites the project name of every managed metadata file,
// then recomputes the metadata hash, as a renamed repository leaves the
// recorded context.
func renameProject(output, name string) (int, error) {
	return rewriteMetadata(output, func(meta *schema.UnifiedMetadata) { meta.Project.Name = name })
}

// stampMetadata rewrites every managed metadata file's adapter revision, then
// recomputes the metadata hash, as an older build would have left the pair.
func stampMetadata(output string, revision int) (int, error) {
	return rewriteMetadata(output, func(meta *schema.UnifiedMetadata) { meta.AdapterVersion = &revision })
}

// rewriteMetadata applies change to every managed metadata file, then
// recomputes its metadata hash and replaces the file atomically.
func rewriteMetadata(output string, change func(*schema.UnifiedMetadata)) (int, error) {
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
		change(&meta)
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
