//go:build skipbudget

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/upgrade"
)

//go:embed testdata/smoke_samples.yaml
var smokeSamplesYAML []byte

// The test binary doubles as the sample child: these variables select the
// child behaviour, so the runner is driven through real child processes.
const (
	childModeEnv     = "SKIP_BUDGET_TEST_CHILD"
	childDirEnv      = "SKIP_BUDGET_TEST_CHILD_DIR"
	childSessionsEnv = "SKIP_BUDGET_TEST_CHILD_SESSIONS"
	childFlagsEnv    = "SKIP_BUDGET_TEST_CHILD_FLAGS"
)

type childMode string

const (
	childModeImport  childMode = "import"
	childModeGarbage childMode = "garbage"
	// childModeRun runs the arm sample the flags name, as the harness child
	// does; childModeRunMiscount runs it and then reports one extra parse.
	childModeRun         childMode = "run"
	childModeRunMiscount childMode = "run-miscount"
)

func init() { runTestChild = runChildIfAsked }

// runChildIfAsked runs the child behaviour the environment selects and exits;
// with no child mode set it returns and the tests run.
func runChildIfAsked() {
	switch childMode(os.Getenv(childModeEnv)) {
	case "":
		return
	case childModeGarbage:
		fmt.Println("not a sample result")
		os.Exit(0)
	case childModeImport:
		sessions, err := strconv.Atoi(os.Getenv(childSessionsEnv))
		if err == nil {
			err = runSampleChild(context.Background(), sampleFlags{Kind: string(sampleKindImport), Dir: os.Getenv(childDirEnv)}, sessions, smokeEntries, os.Stdout)
		}
		exitChild(err)
	case childModeRun, childModeRunMiscount:
		exitChild(runArmChild(childMode(os.Getenv(childModeEnv)) == childModeRunMiscount))
	default:
		fmt.Fprintf(os.Stderr, "%s=%q is not a child mode\n", childModeEnv, os.Getenv(childModeEnv))
		os.Exit(2)
	}
}

func exitChild(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runArmChild runs the arm sample the flags in the environment name through
// the harness child; miscount makes it report one parse more than it ran.
func runArmChild(miscount bool) error {
	var f sampleFlags
	if err := json.Unmarshal([]byte(os.Getenv(childFlagsEnv)), &f); err != nil {
		return fmt.Errorf("%s: %w", childFlagsEnv, err)
	}
	if !miscount {
		return runSampleChild(context.Background(), f, 0, 0, os.Stdout)
	}
	var out bytes.Buffer
	if err := runSampleChild(context.Background(), f, 0, 0, &out); err != nil {
		return err
	}
	var s sample
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		return err
	}
	s.Parses++
	return json.NewEncoder(os.Stdout).Encode(s)
}

// TestSmokeSamplesRunInChildProcesses drives the new-session smoke runner
// and a corpus case's arm runner with the test binary as their child: each
// sample must come from its own process and directory, and a child that
// imported the wrong shape, reported counts the case does not expect, or
// printed something other than a result fails the run before any report.
func TestSmokeSamplesRunInChildProcesses(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string     `yaml:"name"`
			Sample    sampleKind `yaml:"sample"`
			Child     childMode  `yaml:"child"`
			Sessions  int        `yaml:"sessions"`
			WantError string     `yaml:"wantError"`
		} `yaml:"cases"`
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
	const samples = 2
	for _, c := range fixture.Cases {
		if _, err := newSampleKind(string(c.Sample)); err != nil {
			t.Fatalf("case %s: %v", c.Name, err)
		}
		if c.Sample == sampleKindRun {
			t.Run(c.Name, func(t *testing.T) { testRunSamples(t, c.Child, c.WantError) })
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			command := func(sampleDir string) *exec.Cmd {
				cmd := exec.Command(os.Args[0], "-test.run=^$")
				cmd.Env = append(os.Environ(), childModeEnv+"="+string(c.Child), childDirEnv+"="+sampleDir,
					childSessionsEnv+"="+strconv.Itoa(c.Sessions))
				return cmd
			}
			results, err := runSmoke(dir, harnessOptions{}, samples, command)
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
		})
	}
}

// testRunSamples runs one smoke round of the stamp-only case, both arms, with
// the test binary as each arm's child.
func testRunSamples(t *testing.T, child childMode, wantError string) {
	root := t.TempDir()
	command := func(f sampleFlags) *exec.Cmd {
		flags, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), childModeEnv+"="+string(child), childFlagsEnv+"="+string(flags))
		return cmd
	}
	opts := harnessOptions{root: root, sessions: smokeSessions, entries: smokeEntries, rounds: 1}
	path, err := runCase(context.Background(), caseSpecs()[upgrade.MeasurementCaseStampOnly], opts, command)
	reportPath := filepath.Join(root, string(upgrade.MeasurementCaseStampOnly), "report.json")
	if wantError != "" {
		if err == nil || !strings.Contains(err.Error(), wantError) {
			t.Fatalf("runCase error = %v, want one containing %q", err, wantError)
		}
		if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
			t.Fatalf("a failed case left a report at %s: %v", reportPath, statErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
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
	if len(all) != samplesPerRound {
		t.Fatalf("report holds %d samples, want both arms of one round", len(all))
	}
	pids := map[int]bool{os.Getpid(): true}
	for _, s := range all {
		if pids[s.PID] {
			t.Fatalf("sample %s round %d ran in pid %d, which the runner or another sample used", s.Arm, s.Round, s.PID)
		}
		pids[s.PID] = true
	}
}

// samplesPerRound is one sample per arm.
const samplesPerRound = 2
