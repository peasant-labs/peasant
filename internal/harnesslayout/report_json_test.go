package harnesslayout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const reportSecret = "do-not-publish-this-sentence"

func TestSavedReportFixtures(t *testing.T) {
	for _, tc := range savedReports(t) {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.report, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			if strings.Contains(string(got), reportSecret) {
				t.Fatalf("saved report contains transcript text:\n%s", got)
			}
			if tc.name == "private_success" {
				if !strings.Contains(string(got), "Private Title") || !strings.Contains(string(got), "/present") {
					t.Fatalf("private report dropped metadata:\n%s", got)
				}
			} else {
				for _, private := range []string{"Private Title", "/present", "secret-discovery-path", "secret-session"} {
					if strings.Contains(string(got), private) {
						t.Fatalf("report %s contains %q:\n%s", tc.name, private, got)
					}
				}
			}
			path := filepath.Join("testdata", "reports", tc.name+".json")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("saved report %s changed:\n%s", tc.name, got)
			}
		})
	}
}

func savedReports(t *testing.T) []struct {
	name   string
	report harnesslayout.Report
} {
	t.Helper()
	success := captureExample(t, true)
	return []struct {
		name   string
		report harnesslayout.Report
	}{
		{name: "public_success", report: success.ShapeOnly()},
		{name: "private_success", report: success},
		{name: "missing_root", report: missingRoot(t).ShapeOnly()},
		{name: "failures", report: failedReport(t).ShapeOnly()},
	}
}

func captureExample(t *testing.T, present bool) harnesslayout.Report {
	t.Helper()
	record := `{"type":"user","ts":"2026-01-01T00:00:00Z","text":"` + reportSecret + `","title":"Private Title","model":"m-1"}` + "\n"
	root := fstest.MapFS{"sessions/a.jsonl": {Data: []byte(record)}}
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: root} }
	paths := []string{"/present"}
	if !present {
		paths = []string{"/missing"}
	}
	report, err := harnesslayout.Run(context.Background(), exampleLayout, paths, open, 0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func missingRoot(t *testing.T) harnesslayout.Report {
	t.Helper()
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: missingFS{}} }
	report, err := harnesslayout.Run(context.Background(), exampleLayout, []string{"/missing"}, open, 0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func failedReport(t *testing.T) harnesslayout.Report {
	t.Helper()
	dir := t.TempDir()
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: os.DirFS(dir)} }
	layout := harnesslayout.Layout{
		Tool: "fixture-tool",
		Probe: scriptedProbe{
			discoverErr: fmt.Errorf("read secret-discovery-path: %w", fs.ErrPermission),
		},
	}
	discovered, err := harnesslayout.Run(context.Background(), layout, []string{"/secret/root"}, open, 0)
	if err != nil {
		t.Fatal(err)
	}
	capturing := harnesslayout.Layout{
		Tool:  "fixture-tool",
		Probe: scriptedProbe{sessions: []string{"sess"}, fail: map[string]bool{"sess": true}},
	}
	captured, err := harnesslayout.Run(context.Background(), capturing, []string{"/secret/root"}, open, 0)
	if err != nil {
		t.Fatal(err)
	}
	// One saved report carries both a discovery failure and a capture failure.
	discovered.Roots = append(discovered.Roots, captured.Roots...)
	discovered.Failures = captured.Failures
	if discovered.Roots[0].Error == "" || len(discovered.Failures) != 1 || discovered.Failures[0].Error == "" {
		t.Fatalf("pre-strip report = %+v, want error text on the root and the failure", discovered)
	}
	return discovered
}
