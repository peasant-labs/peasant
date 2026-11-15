//go:build astgrep

package astgreprules_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/closed_field_comparison.yaml
var closedFieldComparisonFixture []byte

type astGrepRuleID string
type astGrepSeverity string

const (
	closedFieldComparisonRuleID astGrepRuleID   = "no-stringly-typed-field-comparison"
	astGrepWarning              astGrepSeverity = "warning"
)

type closedFieldComparisonCases struct {
	RequiredNames []string                    `yaml:"requiredNames"`
	Cases         []closedFieldComparisonCase `yaml:"cases"`
}

type closedFieldComparisonCase struct {
	Name    string `yaml:"name"`
	Path    string `yaml:"path"`
	Matches int    `yaml:"matches"`
	Source  string `yaml:"source"`
}

// TestClosedFieldComparisonRule runs the real project scanner against isolated
// files, so the AST rule, warning severity, and vendor exclusion are all tested.
func TestClosedFieldComparisonRule(t *testing.T) {
	var fixture closedFieldComparisonCases
	if err := testutil.DecodeNamedFixtureYAML(closedFieldComparisonFixture, &fixture); err != nil {
		t.Fatalf("decode closed-field comparison fixture: %v", err)
	}

	tempRoot := t.TempDir()
	casesByPath := make(map[string]closedFieldComparisonCase, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if !filepath.IsLocal(c.Path) || filepath.Ext(c.Path) != ".go" {
			t.Fatalf("case %q has invalid local Go path %q", c.Name, c.Path)
		}
		if _, exists := casesByPath[c.Path]; exists {
			t.Fatalf("duplicate fixture path %q", c.Path)
		}
		casesByPath[c.Path] = c
		path := filepath.Join(tempRoot, filepath.FromSlash(c.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory for %q: %v", c.Name, err)
		}
		if err := os.WriteFile(path, []byte(c.Source), 0o644); err != nil {
			t.Fatalf("write fixture for %q: %v", c.Name, err)
		}
	}

	config := filepath.Join(testutil.ModuleRoot(t), "sgconfig.yml")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ast-grep", "scan", "--config", config, "--json=compact", "--filter", "^no-stringly-typed-field-comparison$", ".")
	cmd.Dir = tempRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("closed-field comparison scan timed out: %v; stderr: %s", ctx.Err(), stderr.String())
	}
	if err != nil {
		t.Fatalf("warning-only closed-field comparison scan failed: %v; stderr: %s", err, stderr.String())
	}
	var matches []struct {
		File     string          `json:"file"`
		RuleID   astGrepRuleID   `json:"ruleId"`
		Severity astGrepSeverity `json:"severity"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &matches); err != nil {
		t.Fatalf("decode closed-field comparison results: %v; stderr: %s", err, stderr.String())
	}
	got := make(map[string]int, len(fixture.Cases))
	for _, match := range matches {
		path := filepath.ToSlash(match.File)
		if _, ok := casesByPath[path]; !ok || match.RuleID != closedFieldComparisonRuleID || match.Severity != astGrepWarning {
			t.Fatalf("unexpected ast-grep match: %+v", match)
		}
		got[path]++
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got[c.Path] != c.Matches {
				t.Errorf("closed-field comparison matches in %s = %d, want %d", c.Path, got[c.Path], c.Matches)
			}
		})
	}
}
