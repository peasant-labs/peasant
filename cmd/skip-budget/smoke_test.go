//go:build skipbudget

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/upgrade"
)

//go:embed testdata/smoke_samples.yaml
var smokeSamplesYAML []byte

//go:embed testdata/sample_flags.yaml
var sampleFlagsYAML []byte

// The test binary doubles as the sample child. The environment selects the
// child behaviour; the sample itself arrives as the harness flags after
// "--", parsed by the same flag set the harness binds, so the runner is
// driven through real child processes and the real argv path.
const (
	childModeEnv     = "SKIP_BUDGET_TEST_CHILD"
	childSessionsEnv = "SKIP_BUDGET_TEST_CHILD_SESSIONS"
)

type childMode string

const (
	// childModeImport and childModeRun run the sample the flags name, as the
	// harness child does. childModeImport imports SKIP_BUDGET_TEST_CHILD_SESSIONS
	// sessions instead of the smoke count.
	childModeImport childMode = "import"
	childModeRun    childMode = "run"
	// childModeGarbage prints a line that is not a sample.
	childModeGarbage childMode = "garbage"
	// The remaining modes run the case-arm sample, then misreport it.
	childModeRunMiscount     childMode = "run-miscount"
	childModeRunWrongRound   childMode = "run-wrong-round"
	childModeRunWrongArm     childMode = "run-wrong-arm"
	childModeRunParentPID    childMode = "run-parent-pid"
	childModeRunUnknownCause childMode = "run-unknown-reason"
	childModeRunTwoSamples   childMode = "run-two-samples"
	// childModeRunRefusingStore runs the sample against a store that refuses
	// every settle as stale work.
	childModeRunRefusingStore childMode = "run-refusing-store"
	// childModeRunUnequalArms runs the sample, then changes one session's
	// state digest in the skip arm only.
	childModeRunUnequalArms childMode = "run-unequal-arms"
)

// misreports rewrites an honest case-arm sample before the child prints it.
var misreports = map[childMode]func(*sample){
	childModeRunMiscount:   func(s *sample) { s.Parses++ },
	childModeRunWrongRound: func(s *sample) { s.Round++ },
	childModeRunWrongArm: func(s *sample) {
		s.Arm = map[arm]arm{armSkip: armBaseline, armBaseline: armSkip}[s.Arm]
	},
	childModeRunParentPID: func(s *sample) { s.PID = os.Getppid() },
	// A reason id this build does not define prints an empty code.
	childModeRunUnknownCause: func(s *sample) { s.Census[0].Reason = ingest.OutcomeReason(-1) },
	childModeRunUnequalArms: func(s *sample) {
		if s.Arm != armSkip {
			return
		}
		for sid := range s.Equivalence {
			s.Equivalence[sid]["entries"] = "0:unrouted"
			return
		}
	},
}

func init() { runTestChild = runChildIfAsked }

// runChildIfAsked runs the child behaviour the environment selects and exits;
// with no child mode set it returns and the tests run.
func runChildIfAsked() {
	mode := childMode(os.Getenv(childModeEnv))
	if mode == "" {
		return
	}
	if mode == childModeGarbage {
		fmt.Println("not a sample result")
		os.Exit(0)
	}
	fs := flag.NewFlagSet("skip-budget", flag.ContinueOnError)
	f := bindSampleFlags(fs)
	err := fs.Parse(argsAfterDashes(os.Args))
	if err == nil {
		err = runTestChildMode(mode, *f)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func argsAfterDashes(args []string) []string {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[i+1:]
	}
	return nil
}

func runTestChildMode(mode childMode, f sampleFlags) error {
	ctx := context.Background()
	switch mode {
	case childModeImport:
		sessions, err := strconv.Atoi(os.Getenv(childSessionsEnv))
		if err != nil {
			return fmt.Errorf("%s: %w", childSessionsEnv, err)
		}
		return runSampleChild(ctx, f, sessions, smokeEntries, os.Stdout)
	case childModeRun:
		return runSampleChild(ctx, f, smokeSessions, smokeEntries, os.Stdout)
	case childModeRunRefusingStore:
		refuseSettlesForTest = true
		return runSampleChild(ctx, f, smokeSessions, smokeEntries, os.Stdout)
	}
	var out bytes.Buffer
	if err := runSampleChild(ctx, f, smokeSessions, smokeEntries, &out); err != nil {
		return err
	}
	if mode == childModeRunTwoSamples {
		_, err := os.Stdout.Write(append(out.Bytes(), out.Bytes()...))
		return err
	}
	misreport, ok := misreports[mode]
	if !ok {
		return fmt.Errorf("%s=%q is not a child mode", childModeEnv, mode)
	}
	var s sample
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		return err
	}
	misreport(&s)
	return json.NewEncoder(os.Stdout).Encode(s)
}

// testChildCommand launches the test binary as the child for one sample,
// with the sample's harness flags after "--". It records each launch.
type testChildCommand struct {
	mode     childMode
	sessions int
	mu       sync.Mutex
	launched []sampleFlags
}

func (c *testChildCommand) command(f sampleFlags) *exec.Cmd {
	c.mu.Lock()
	c.launched = append(c.launched, f)
	c.mu.Unlock()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^$", "--"}, f.args()...)...)
	cmd.Env = append(os.Environ(), childModeEnv+"="+string(c.mode), childSessionsEnv+"="+strconv.Itoa(c.sessions))
	return cmd
}

type smokeSampleCase struct {
	Name   string     `yaml:"name"`
	Sample sampleKind `yaml:"sample"`
	// Case is the corpus case a case-arm row runs; empty means C1.
	Case      upgrade.MeasurementCase `yaml:"case"`
	Child     childMode               `yaml:"child"`
	Sessions  int                     `yaml:"sessions"`
	Rounds    int                     `yaml:"rounds"`
	WantOrder []arm                   `yaml:"wantOrder"`
	WantError string                  `yaml:"wantError"`
}

// TestSmokeSamplesRunInChildProcesses drives the new-session smoke runner
// and a corpus case's arm runner with the test binary as their child: each
// sample must come from its own process and directory, the arm order must
// alternate by round, and a child that imported the wrong shape, misreported
// its sample, or printed anything but one result fails the run before any
// report.
func TestSmokeSamplesRunInChildProcesses(t *testing.T) {
	var fixture struct {
		RequiredNames []string          `yaml:"requiredNames"`
		Cases         []smokeSampleCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(smokeSamplesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("smoke_samples.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		kind, err := newSampleKind(string(c.Sample))
		if err != nil {
			t.Fatalf("case %s: %v", c.Name, err)
		}
		if kind == sampleKindRun {
			t.Run(c.Name, func(t *testing.T) { testRunSamples(t, c) })
			continue
		}
		t.Run(c.Name, func(t *testing.T) { testImportSamples(t, c) })
	}
}

func testImportSamples(t *testing.T, c smokeSampleCase) {
	const samples = 2
	dir := t.TempDir()
	child := &testChildCommand{mode: c.Child, sessions: c.Sessions}
	results, err := runSmoke(dir, harnessOptions{}, samples, child.command)
	if c.WantError != "" {
		if err == nil || !strings.Contains(err.Error(), c.WantError) {
			t.Fatalf("runSmoke error = %v, want one containing %q", err, c.WantError)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != samples {
		t.Fatalf("runSmoke returned %d samples, want %d", len(results), samples)
	}
	pids, dirs := map[int]bool{os.Getpid(): true}, map[string]bool{}
	for _, r := range results {
		if pids[r.PID] || dirs[r.Dir] {
			t.Fatalf("sample %+v shares a process or directory with the runner or another sample", r)
		}
		pids[r.PID], dirs[r.Dir] = true, true
		if _, err := os.Stat(filepath.Join(r.Dir, "db", "peasant.db")); err != nil {
			t.Fatalf("sample %+v left no store in its directory: %v", r, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
		t.Fatalf("runSmoke wrote no report: %v", err)
	}
}

// testRunSamples runs c.Rounds smoke rounds (default one) of the row's case
// (default the stamp-only case), both arms, with the test binary as each
// arm's child.
func testRunSamples(t *testing.T, c smokeSampleCase) {
	root := t.TempDir()
	rounds := max(c.Rounds, 1)
	id := upgrade.MeasurementCaseStampOnly
	if c.Case != "" {
		var err error
		if id, err = upgrade.NewMeasurementCase(string(c.Case)); err != nil {
			t.Fatal(err)
		}
	}
	child := &testChildCommand{mode: c.Child}
	opts := harnessOptions{root: root, sessions: smokeSessions, entries: smokeEntries, rounds: rounds}
	path, err := runCase(context.Background(), caseSpecs()[id], opts, child.command)
	reportPath := filepath.Join(root, string(id), "report.json")
	if c.WantError != "" {
		if err == nil || !strings.Contains(err.Error(), c.WantError) {
			t.Fatalf("runCase error = %v, want one containing %q", err, c.WantError)
		}
		if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
			t.Fatalf("a failed case left a report at %s: %v", reportPath, statErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if c.WantOrder != nil {
		var order []arm
		for i, f := range child.launched {
			if want := i/samplesPerRound + 1; f.Round != want {
				t.Fatalf("launch %d ran round %d, want round %d", i+1, f.Round, want)
			}
			order = append(order, arm(f.Arm))
		}
		if !slices.Equal(order, c.WantOrder) {
			t.Fatalf("children ran arms in order %v, want %v", order, c.WantOrder)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report caseReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	all := append(report.Skip.Samples, report.Baseline.Samples...)
	if len(all) != samplesPerRound*rounds {
		t.Fatalf("report holds %d samples, want both arms of %d rounds", len(all), rounds)
	}
	if caseSpecs()[id].equivalent {
		assertHashDriftBreakdown(t, report, rounds)
	}
	pids := map[int]bool{os.Getpid(): true}
	for _, s := range all {
		if pids[s.PID] {
			t.Fatalf("sample %s round %d ran in pid %d, which the runner or another sample used", s.Arm, s.Round, s.PID)
		}
		pids[s.PID] = true
	}
}

// assertHashDriftBreakdown requires the skip arm's breakdown to name every
// session of every round as validated, with a measured validation time.
func assertHashDriftBreakdown(t *testing.T, report caseReport, rounds int) {
	t.Helper()
	if len(report.Breakdown) != 1 {
		t.Fatalf("breakdown %+v, want only validated sessions", report.Breakdown)
	}
	row := report.Breakdown[0]
	if row.Outcome != outcomeValidated || row.Sessions != smokeSessions*rounds || row.MedianValidateMs <= 0 || row.MedianParseMs != 0 {
		t.Fatalf("breakdown row %+v, want %d validated sessions with a validation time and no parse", row, smokeSessions*rounds)
	}
	if report.Skip.MedianValidateMs <= 0 || report.Baseline.MedianValidateMs != 0 {
		t.Fatalf("median validate ms skip %v baseline %v, want a skip-arm time and none in the baseline", report.Skip.MedianValidateMs, report.Baseline.MedianValidateMs)
	}
}

// samplesPerRound is one sample per arm.
const samplesPerRound = 2

// TestSampleFlagsRoundTripThroughTheFlagSet checks that the flags the parent
// renders for each sample kind are exactly the expected list, and that the
// flag set the harness binds parses them back to the same sample.
func TestSampleFlagsRoundTripThroughTheFlagSet(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name  string `yaml:"name"`
			Flags struct {
				Kind  string `yaml:"kind"`
				Dir   string `yaml:"dir"`
				Base  string `yaml:"base"`
				Case  string `yaml:"case"`
				Arm   string `yaml:"arm"`
				Round int    `yaml:"round"`
			} `yaml:"flags"`
			WantArgs []string `yaml:"wantArgs"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(sampleFlagsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("sample_flags.yaml", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			want := sampleFlags(c.Flags)
			if got := want.args(); !slices.Equal(got, c.WantArgs) {
				t.Fatalf("args() = %q, want %q", got, c.WantArgs)
			}
			fs := flag.NewFlagSet("skip-budget", flag.ContinueOnError)
			got := bindSampleFlags(fs)
			if err := fs.Parse(c.WantArgs); err != nil {
				t.Fatal(err)
			}
			if *got != want || fs.NArg() != 0 {
				t.Fatalf("parsed %+v with %d left over, want %+v", *got, fs.NArg(), want)
			}
			if _, err := got.validate(); err != nil {
				t.Fatalf("validate() = %v, want the parsed flags accepted", err)
			}
		})
	}
}
