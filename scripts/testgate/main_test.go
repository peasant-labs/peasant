package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// These tests freeze the CLI and environment contract of the gate (IP-A):
// subcommands, flags, exit codes, budget/env precedence, and the
// INCONCLUSIVE-under-load verdict. Expected values live in testdata/*.yaml; the
// mutation cases prove each checker is not vacuous.

//go:embed testdata/cli_contract.yaml
var cliContractYAML []byte

//go:embed testdata/env_contract.yaml
var envContractYAML []byte

type cliCheck struct {
	Name     string `yaml:"name"`
	Contains string `yaml:"contains"`
}

type cliMutation struct {
	Name     string `yaml:"name"`
	Check    string `yaml:"check"`
	Contains string `yaml:"contains"`
}

type cliContractFile struct {
	RequiredNames []string      `yaml:"required_names"`
	Checks        []cliCheck    `yaml:"checks"`
	Mutations     []cliMutation `yaml:"mutations"`
}

type budgetCase struct {
	Name        string `yaml:"name"`
	FileSeconds int    `yaml:"file_seconds"`
	EnvSeconds  int    `yaml:"env_seconds"`
	WantPresent bool   `yaml:"want_present"`
	WantSeconds int    `yaml:"want_seconds"`
	WantBasis   string `yaml:"want_basis"`
}

type checkStartCase struct {
	Name        string `yaml:"name"`
	Value       string `yaml:"value"`
	WantPresent bool   `yaml:"want_present"`
}

type budgetVerdictCase struct {
	Name     string  `yaml:"name"`
	Present  bool    `yaml:"present"`
	CalL     float64 `yaml:"cal_l"`
	WallMS   int64   `yaml:"wall_ms"`
	Seconds  int     `yaml:"seconds"`
	WantFail bool    `yaml:"want_fail"`
}

type envContractFile struct {
	RequiredNames      []string            `yaml:"required_names"`
	BudgetCases        []budgetCase        `yaml:"budget_cases"`
	CheckStartCases    []checkStartCase    `yaml:"check_start_cases"`
	BudgetVerdictCases []budgetVerdictCase `yaml:"budget_verdict_cases"`
}

func decodeContractFixture(data []byte, target any, source string) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", source, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one YAML document, got %v", source, err)
	}
	return nil
}

func loadCLIContract(t *testing.T) cliContractFile {
	t.Helper()
	var file cliContractFile
	if err := decodeContractFixture(cliContractYAML, &file, "CLI contract fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("CLI contract fixture declares no required_names manifest")
	}
	present := map[string]bool{}
	for _, c := range file.Checks {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("CLI contract fixture has an empty or duplicate check %q", c.Name)
		}
		present[c.Name] = true
	}
	for _, m := range file.Mutations {
		if m.Name == "" || present[m.Name] {
			t.Fatalf("CLI contract fixture has an empty or duplicate name %q", m.Name)
		}
		present[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !present[want] {
			t.Fatalf("required CLI contract %q is missing from the fixture", want)
		}
	}
	return file
}

func loadEnvContract(t *testing.T) envContractFile {
	t.Helper()
	var file envContractFile
	if err := decodeContractFixture(envContractYAML, &file, "env contract fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("env contract fixture declares no required_names manifest")
	}
	names := map[string]bool{}
	collect := func(name string) {
		if name == "" || names[name] {
			t.Fatalf("env contract fixture has an empty or duplicate case %q", name)
		}
		names[name] = true
	}
	for _, c := range file.BudgetCases {
		collect(c.Name)
	}
	for _, c := range file.CheckStartCases {
		collect(c.Name)
	}
	for _, c := range file.BudgetVerdictCases {
		collect(c.Name)
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required env contract %q is missing from the fixture", want)
		}
	}
	return file
}

// captureUsage runs the real usage() and returns what it wrote to stderr.
func captureUsage(t *testing.T) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	usage()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return string(data)
}

func checkUsage(usage string, checks []cliCheck) []string {
	var problems []string
	for _, c := range checks {
		if !strings.Contains(usage, c.Contains) {
			problems = append(problems, fmt.Sprintf("%s: usage does not contain %q", c.Name, c.Contains))
		}
	}
	return problems
}

func TestCLI_UsageMatchesFrozenContract(t *testing.T) {
	file := loadCLIContract(t)
	usage := captureUsage(t)
	if problems := checkUsage(usage, file.Checks); len(problems) > 0 {
		t.Fatalf("the gate CLI contract moved:\n  %s\n\nusage:\n%s", strings.Join(problems, "\n  "), usage)
	}
}

// TestCLI_UsageMutationIsDetected proves the substring checker fails when the
// contract text is absent, so a real usage change cannot pass silently.
func TestCLI_UsageMutationIsDetected(t *testing.T) {
	file := loadCLIContract(t)
	usage := captureUsage(t)
	byName := map[string]cliCheck{}
	for _, c := range file.Checks {
		byName[c.Name] = c
	}
	for _, m := range file.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			c, ok := byName[m.Check]
			if !ok {
				t.Fatalf("mutation targets unknown check %q", m.Check)
			}
			c.Contains = m.Contains
			if len(checkUsage(usage, []cliCheck{c})) == 0 {
				t.Fatalf("the %s mutation was NOT detected; the usage freeze is vacuous", m.Name)
			}
		})
	}
}

func TestBudget_ResolutionPrecedence(t *testing.T) {
	file := loadEnvContract(t)
	for _, tc := range file.BudgetCases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			if tc.FileSeconds > 0 {
				body := fmt.Sprintf("version: 1\nseconds: %d\nbasis: reference-machine wall\n", tc.FileSeconds)
				if err := os.WriteFile(filepath.Join(root, "budget.yaml"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TEST_BUDGET", strconv.Itoa(tc.EnvSeconds))
			seconds, basis, present, err := resolveBudget(root)
			if err != nil {
				t.Fatalf("resolveBudget: %v", err)
			}
			if present != tc.WantPresent {
				t.Fatalf("present = %v, want %v", present, tc.WantPresent)
			}
			if !tc.WantPresent {
				return
			}
			if seconds != tc.WantSeconds {
				t.Fatalf("seconds = %d, want %d", seconds, tc.WantSeconds)
			}
			if basis != tc.WantBasis {
				t.Fatalf("basis = %q, want %q", basis, tc.WantBasis)
			}
		})
	}
}

func TestCheckStartNS_Contract(t *testing.T) {
	file := loadEnvContract(t)
	for _, tc := range file.CheckStartCases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Setenv("CHECK_START_NS", tc.Value)
			start, present := checkStartNS()
			if present != tc.WantPresent {
				t.Fatalf("present = %v, want %v", present, tc.WantPresent)
			}
			if present && start.UnixNano() != mustParseInt(t, tc.Value) {
				t.Fatalf("start = %d, want %s", start.UnixNano(), tc.Value)
			}
		})
	}
}

func TestBudget_VerdictSemantics(t *testing.T) {
	file := loadEnvContract(t)
	for _, tc := range file.BudgetVerdictCases {
		t.Run(tc.Name, func(t *testing.T) {
			fail := printBudget(tc.CalL, time.Duration(tc.WallMS)*time.Millisecond, tc.Seconds, "fixture", tc.Present)
			if fail != tc.WantFail {
				t.Fatalf("printBudget fail = %v, want %v", fail, tc.WantFail)
			}
		})
	}
}

func mustParseInt(t *testing.T, value string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("fixture value %q is not an integer: %v", value, err)
	}
	return n
}

// TestCLI_ExitCodes exercises the real built binary on its cheap error paths:
// no subcommand, an unknown subcommand, and an invalid registry all exit 2. The
// success (0) and failure (1) codes require a full suite run and are pinned by
// the usage contract above.
func TestCLI_ExitCodes(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "testgate")
	build := exec.Command("go", "build", "-o", bin, "./scripts/testgate")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the gate binary: %v\n%s", err, out)
	}

	badRegistry := filepath.Join(t.TempDir(), "no-race-partition.yaml")
	if err := os.WriteFile(badRegistry, []byte("version: 2\npartition: []\nprotected: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "no-subcommand", args: nil, want: 2},
		{name: "unknown-subcommand", args: []string{"bogus"}, want: 2},
		{name: "invalid-registry", args: []string{"plan", "-registry", badRegistry}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			code := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("run %v: %v\n%s", tc.args, err, out)
			}
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d\n%s", code, tc.want, out)
			}
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
