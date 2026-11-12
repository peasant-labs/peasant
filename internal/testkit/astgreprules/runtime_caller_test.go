//go:build astgrep

package astgreprules_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/runtime_caller.yaml
var runtimeCallerFixture []byte

type runtimeCallerCases struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []runtimeCallerCase `yaml:"cases"`
}

type runtimeCallerCase struct {
	Name    string `yaml:"name"`
	Path    string `yaml:"path"`
	Matches int    `yaml:"matches"`
	Source  string `yaml:"source"`
}

// The real scan runs over isolated files so its filename scope and suppressions
// are checked as well as the AST pattern. The astgrep tag keeps the CLI out of
// ordinary Go tests; make check runs this alongside the other ast-grep gate.
func TestRuntimeCallerRule(t *testing.T) {
	var fixture runtimeCallerCases
	if err := testutil.DecodeNamedFixtureYAML(runtimeCallerFixture, &fixture); err != nil {
		t.Fatalf("decode runtime caller rule fixture: %v", err)
	}

	tempRoot := t.TempDir()
	casesByPath := make(map[string]runtimeCallerCase, len(fixture.Cases))
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
	cmd := exec.CommandContext(ctx, "ast-grep", "scan", "--error=unused-suppression", "--config", config, "--json=compact", "--filter", "^no-runtime-caller-in-tests$", ".")
	cmd.Dir = tempRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("runtime caller rule scan timed out: %v; stderr: %s", ctx.Err(), stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("runtime caller rule scan must fail on error-level matches; got %v; stderr: %s", err, stderr.String())
	}
	var matches []struct {
		File     string `json:"file"`
		RuleID   string `json:"ruleId"`
		Severity string `json:"severity"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &matches); err != nil {
		t.Fatalf("decode runtime caller rule results: %v; stderr: %s", err, stderr.String())
	}
	got := make(map[string]int, len(fixture.Cases))
	for _, match := range matches {
		path := filepath.ToSlash(match.File)
		if _, ok := casesByPath[path]; !ok || match.RuleID != "no-runtime-caller-in-tests" || match.Severity != "error" {
			t.Fatalf("unexpected ast-grep match: %+v", match)
		}
		got[path]++
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if got[c.Path] != c.Matches {
				t.Errorf("runtime caller matches in %s = %d, want %d", c.Path, got[c.Path], c.Matches)
			}
		})
	}
}
