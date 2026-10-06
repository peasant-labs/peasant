package scripts_test

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// The rollup unit parser is pinned by driving the real script over a committed
// corpus rather than by calling a helper: the corpus carries the inputs and the
// hand-computed expectations (the arithmetic is in cases.yaml), and the run's
// stdout is compared byte-for-byte. A two-decimal mis-scale cannot hide behind
// a tolerant comparison, and the script's own output is never the oracle.

//go:embed testdata/rollup/cases.yaml
var rollupCasesYAML []byte

//go:embed testdata/rollup/manifest.yaml
var rollupManifestYAML []byte

type rollupCase struct {
	Name     string `yaml:"name"`
	Input    string `yaml:"input"`
	Expected string `yaml:"expected"`
	Status   int    `yaml:"status"`
	Stderr   string `yaml:"stderr"`
}

func loadRollupCases(t *testing.T) []rollupCase {
	t.Helper()
	var cases []rollupCase
	if err := testutil.DecodeFixtureYAML(rollupCasesYAML, &cases); err != nil {
		t.Fatalf("decode rollup cases fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("rollup cases fixture declares no cases")
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(rollupManifestYAML, "rollup")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		if c.Input == "" {
			t.Fatalf("rollup case %q names no input", c.Name)
		}
		if c.Status == 0 && c.Expected == "" {
			t.Fatalf("rollup case %q expects success but names no expected output; a positive case without a golden proves nothing", c.Name)
		}
		if c.Status != 0 && c.Stderr == "" {
			t.Fatalf("rollup case %q expects failure but names no stderr message", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "rollup"); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestRollupFamilyShares(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("perf", "rollup.sh"))
	if err != nil {
		t.Fatalf("resolve the production script: %v", err)
	}
	for _, c := range loadRollupCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			// The relative input path is passed through to the script so the
			// "== <path>" banner the script prints is deterministic.
			input := filepath.Join("testdata", "rollup", c.Input)
			cmd := exec.Command("bash", script, input)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()
			status := 0
			if runErr != nil {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) {
					t.Fatalf("run %s: %v", script, runErr)
				}
				status = exit.ExitCode()
			}
			if status != c.Status {
				t.Fatalf("bash %s %s exited %d, want %d\nstderr:\n%s", script, c.Input, status, c.Status, stderr.String())
			}
			if c.Stderr != "" && !strings.Contains(stderr.String(), c.Stderr) {
				t.Fatalf("bash %s %s stderr = %q, want it to contain %q", script, c.Input, stderr.String(), c.Stderr)
			}
			if c.Expected == "" {
				return
			}
			want, err := os.ReadFile(filepath.Join(dir, "testdata", "rollup", c.Expected))
			if err != nil {
				t.Fatalf("read expected output %s: %v", c.Expected, err)
			}
			if !bytes.Equal(stdout.Bytes(), want) {
				t.Errorf("rollup stdout does not match %s byte-for-byte:\n--- got ---\n%s--- want ---\n%s", c.Expected, stdout.String(), want)
			}
		})
	}
}
