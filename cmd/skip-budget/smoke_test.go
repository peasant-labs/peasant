//go:build skipbudget

package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/smoke_samples.yaml
var smokeSamplesYAML []byte

// The test binary doubles as the sample child: these variables select the
// child behaviour, so the runner is driven through real child processes.
const (
	childModeEnv     = "SKIP_BUDGET_TEST_CHILD"
	childDirEnv      = "SKIP_BUDGET_TEST_CHILD_DIR"
	childSessionsEnv = "SKIP_BUDGET_TEST_CHILD_SESSIONS"
)

type childMode string

const (
	childModeImport  childMode = "import"
	childModeGarbage childMode = "garbage"
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
			err = runSampleChild(context.Background(), string(sampleKindImport), os.Getenv(childDirEnv), sessions, smokeEntries, os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "%s=%q is not a child mode\n", childModeEnv, os.Getenv(childModeEnv))
		os.Exit(2)
	}
}

// TestSmokeSamplesRunInChildProcesses drives the new-session smoke runner
// with the test binary as its child: each sample must come from its own
// process and directory, and a child that imported the wrong shape or printed
// something other than a result fails the run.
func TestSmokeSamplesRunInChildProcesses(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string    `yaml:"name"`
			Child     childMode `yaml:"child"`
			Sessions  int       `yaml:"sessions"`
			WantError string    `yaml:"wantError"`
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
