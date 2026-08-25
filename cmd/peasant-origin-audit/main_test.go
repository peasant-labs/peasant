//go:build origin_audit

package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/baseline/manifest.yaml
var baselineManifestBytes []byte

//go:embed testdata/baseline/cases.yaml
var baselineCasesBytes []byte

//go:embed all:testdata/baseline/claude-projects
var baselineFixtureTree embed.FS

const baselineFixtureTreeRoot = "testdata/baseline/claude-projects"

type baselineCaseFile struct {
	Cases []baselineCase `yaml:"cases"`
}

type baselineCase struct {
	Name         string `yaml:"name"`
	RelativePath string `yaml:"relative_path"`
	Origin       string `yaml:"origin"`
	Signal       string `yaml:"signal"`
	Unreadable   bool   `yaml:"unreadable"`
}

// loadBaselineCases decodes the baseline corpus, enforces the deletion guard
// through the SHARED internal/testutil.RequireFixtureNames helper, and refuses
// a case that declares an origin or signal outside the production closed sets.
func loadBaselineCases(t *testing.T) []baselineCase {
	t.Helper()

	// Decode the manifest to extract required case names.
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(baselineManifestBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode baseline manifest: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("baseline manifest must contain exactly one YAML document: %v", err)
	}

	var fixture baselineCaseFile
	decoder = yaml.NewDecoder(bytes.NewReader(baselineCasesBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode baseline cases: %v", err)
	}
	var caseTrailing any
	if err := decoder.Decode(&caseTrailing); !errors.Is(err, io.EOF) {
		t.Fatalf("baseline cases fixture must contain exactly one YAML document: %v", err)
	}

	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" {
			t.Fatalf("baseline cases fixture holds a case with no name")
		}
		if present[tc.Name] {
			t.Fatalf("baseline cases fixture repeats case name %q", tc.Name)
		}
		present[tc.Name] = true

		if _, err := sessionorigin.Parse(tc.Origin); err != nil {
			t.Fatalf("baseline case %q declares origin %q: %v", tc.Name, tc.Origin, err)
		}
		if !sessionorigin.Signal(tc.Signal).Valid() {
			t.Fatalf("baseline case %q declares signal %q, which is not a deciding signal", tc.Name, tc.Signal)
		}
		if tc.RelativePath == "" {
			t.Fatalf("baseline case %q has no relative_path", tc.Name)
		}
	}
	if err := testutil.RequireFixtureNames("origin-audit-baseline", "case", manifest.RequiredNames, present); err != nil {
		t.Fatalf("baseline cases fixture failed the deletion guard: %v", err)
	}

	return fixture.Cases
}

// writeBaselineFixtureTree materializes the checked-in fixture tree into a
// real, throwaway temporary directory. A real copy is required (rather than
// reading the embedded FS directly) because the read-failure case is proven
// by making one copied file genuinely unreadable with os.Chmod -- the
// checked-in fixture itself is left at ordinary permissions so it survives a
// git checkout on any OS unmodified.
func writeBaselineFixtureTree(t *testing.T) string {
	t.Helper()
	destRoot := filepath.Join(t.TempDir(), "claude-projects")

	err := iofs.WalkDir(baselineFixtureTree, baselineFixtureTreeRoot, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(baselineFixtureTreeRoot, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(destRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}
		data, err := baselineFixtureTree.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o640)
	})
	if err != nil {
		t.Fatalf("materialize baseline fixture tree: %v", err)
	}
	return destRoot
}

func TestRunAuditBaseline(t *testing.T) {
	cases := loadBaselineCases(t)
	sourceDir := writeBaselineFixtureTree(t)

	wantBySignal := make(map[sessionorigin.Signal]int, len(sessionorigin.AllSignals))
	wantByOrigin := make(map[sessionorigin.Origin]int, len(sessionorigin.All))
	for _, signal := range sessionorigin.AllSignals {
		wantBySignal[signal] = 0
	}
	for _, origin := range sessionorigin.All {
		wantByOrigin[origin] = 0
	}

	var wantUnreadable []string
	var wantRoot, wantSubagent int
	for _, tc := range cases {
		origin, err := sessionorigin.Parse(tc.Origin)
		if err != nil {
			t.Fatalf("case %q: %v", tc.Name, err)
		}
		signal := sessionorigin.Signal(tc.Signal)
		wantBySignal[signal]++
		wantByOrigin[origin]++

		absPath := filepath.Join(sourceDir, filepath.FromSlash(tc.RelativePath))
		if strings.Contains(tc.RelativePath, "/subagents/") {
			wantSubagent++
		} else {
			wantRoot++
		}

		if tc.Unreadable {
			if err := os.Chmod(absPath, 0o000); err != nil {
				t.Fatalf("case %q: make fixture unreadable: %v", tc.Name, err)
			}
			wantUnreadable = append(wantUnreadable, filepath.Clean(absPath))
		}
	}
	sort.Strings(wantUnreadable)

	sourcePath, err := ingest.NewResolvedPath(sourceDir)
	if err != nil {
		t.Fatalf("resolve fixture source path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := RunAudit(ctx, &ingest.OSFileSystem{}, sourcePath)
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}

	// This is the instrument's own baseline: every assertion below is an EXACT
	// value taken from the hand-counted cases, not a non-zero or
	// greater-than-zero check. A harness that silently reported all zeros --
	// the failure mode this baseline exists to catch -- fails every one of
	// these comparisons.
	if report.ExaminedFiles != len(cases) {
		t.Errorf("ExaminedFiles = %d, want %d", report.ExaminedFiles, len(cases))
	}
	if report.Sessions != len(cases) {
		t.Errorf("Sessions = %d, want %d (every fixture, including the unreadable one, fails OPEN to a classified session)", report.Sessions, len(cases))
	}
	if report.RootSessions != wantRoot {
		t.Errorf("RootSessions = %d, want %d", report.RootSessions, wantRoot)
	}
	if report.SubagentSessions != wantSubagent {
		t.Errorf("SubagentSessions = %d, want %d", report.SubagentSessions, wantSubagent)
	}
	for _, signal := range sessionorigin.AllSignals {
		if got, want := report.BySignal[signal], wantBySignal[signal]; got != want {
			t.Errorf("BySignal[%s] = %d, want %d", signal, got, want)
		}
	}
	for _, origin := range sessionorigin.All {
		if got, want := report.ByOrigin[origin], wantByOrigin[origin]; got != want {
			t.Errorf("ByOrigin[%s] = %d, want %d", origin, got, want)
		}
	}
	if got := fmt.Sprint(report.UnreadablePaths); got != fmt.Sprint(wantUnreadable) {
		t.Errorf("UnreadablePaths = %v, want %v", report.UnreadablePaths, wantUnreadable)
	}
	if len(report.UnaccountedPaths) != 0 {
		t.Errorf("UnaccountedPaths = %v, want none: every fixture file is either accounted for or flagged unreadable", report.UnaccountedPaths)
	}
}
