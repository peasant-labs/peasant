package testgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
}
