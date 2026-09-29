package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/budget_cases.yaml
var budgetCasesYAML []byte

// budgetEnforcementCase is one LoadBudget enforcement case: either the budget
// loads with the expected seconds and enforcement, or loading fails naming
// the expected fragment.
type budgetEnforcementCase struct {
	Name            string `yaml:"name"`
	Body            string `yaml:"body"`
	WantFound       bool   `yaml:"want_found"`
	WantSeconds     int    `yaml:"want_seconds"`
	WantEnforcement string `yaml:"want_enforcement"`
	WantErrContains string `yaml:"want_err_contains"`
}

type budgetCasesFile struct {
	RequiredNames []string                `yaml:"required_names"`
	Cases         []budgetEnforcementCase `yaml:"cases"`
}

func loadBudgetCases(t *testing.T) budgetCasesFile {
	t.Helper()
	var file budgetCasesFile
	decoder := yaml.NewDecoder(bytes.NewReader(budgetCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode budget cases fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("budget cases fixture must contain exactly one YAML document, got %v", err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("budget cases fixture declares no required_names manifest")
	}
	present := map[string]bool{}
	for _, c := range file.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("budget cases fixture has an empty or duplicate case %q", c.Name)
		}
		present[c.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !present[want] {
			t.Fatalf("required budget case %q is missing from the fixture", want)
		}
	}
	return file
}

// TestLoadBudget_Enforcement proves the enforcement field is parsed when
// present, defaults to blocking when absent, and fails closed on any other
// value — while the seconds and version guards still fire underneath it.
func TestLoadBudget_Enforcement(t *testing.T) {
	file := loadBudgetCases(t)
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.yaml")
			if err := os.WriteFile(path, []byte(tc.Body), 0o644); err != nil {
				t.Fatal(err)
			}
			b, found, err := LoadBudget(path)
			if tc.WantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.WantErrContains) {
					t.Fatalf("load error = %v, want an error containing %q", err, tc.WantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if found != tc.WantFound {
				t.Fatalf("found = %v, want %v", found, tc.WantFound)
			}
			if b.Seconds != tc.WantSeconds {
				t.Fatalf("seconds = %d, want %d", b.Seconds, tc.WantSeconds)
			}
			if string(b.Enforcement) != tc.WantEnforcement {
				t.Fatalf("enforcement = %q, want %q", b.Enforcement, tc.WantEnforcement)
			}
		})
	}
}

func TestLoadBudget_Found(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nseconds: 120\nbasis: reference-machine wall\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, found, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !found || b.Seconds != 120 || b.Basis == "" {
		t.Fatalf("budget = %+v found=%v, want seconds=120 with a basis", b, found)
	}
}

func TestLoadBudget_AbsentIsNotFound(t *testing.T) {
	_, found, err := LoadBudget(filepath.Join(t.TempDir(), "budget.yaml"))
	if err != nil || found {
		t.Fatalf("absent budget: found=%v err=%v, want found=false no error", found, err)
	}
}

func TestLoadBudget_MalformedIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nseconds: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadBudget(path)
	if err == nil || !strings.Contains(err.Error(), "seconds") {
		t.Fatalf("zero budget must be a loud error, got %v", err)
	}
}

func TestWriteReport_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	in := Report{
		SchemaVersion: 1,
		Module:        "github.com/peasant-labs/peasant",
		Records:       []ReportRecord{{Unit: "pkg/TestX", Class: "subprocess", Pass: "no-race", WallMS: 12, UserMS: 8, SystemMS: 4}},
		Findings:      []ReportFinding{{Rule: "exactly-once", Severity: "FAIL"}},
	}
	if err := WriteReport(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"unit": "pkg/TestX"`, `"class": "subprocess"`, `"wall_ms": 12`, `"rule": "exactly-once"`} {
		if !strings.Contains(got, want) {
			t.Errorf("report JSON missing %s:\n%s", want, got)
		}
	}
	// Empty collections must serialize as [] rather than null.
	for _, want := range []string{`"failed_tests": []`, `"invocation_errors": []`} {
		if !strings.Contains(got, want) {
			t.Errorf("report JSON should normalize empty collections to [], missing %s:\n%s", want, got)
		}
	}
}
