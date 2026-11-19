//go:build astgrep

package astgreprules_test

import (
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

//go:embed testdata/executable_write.yaml
var executableWriteFixture []byte

func TestExecutableWriteRule(t *testing.T) {
	var fixture runtimeCallerCases
	if err := testutil.DecodeNamedFixtureYAML(executableWriteFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, c := range fixture.Cases {
		if !filepath.IsLocal(c.Path) {
			t.Fatalf("invalid fixture path %q", c.Path)
		}
		path := filepath.Join(root, c.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(c.Source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ast-grep", "scan", "--error=unused-suppression", "--config", filepath.Join(testutil.ModuleRoot(t), "sgconfig.yml"), "--json=compact", "--filter", "^no-unsafe-executable-write$", ".")
	cmd.Dir = root
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("scan: %v", err)
	}
	var matches []struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(out, &matches); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int)
	for _, match := range matches {
		got[filepath.ToSlash(match.File)]++
	}
	for _, c := range fixture.Cases {
		if got[c.Path] != c.Matches {
			t.Errorf("%s: got %d matches, want %d", c.Name, got[c.Path], c.Matches)
		}
		delete(got, c.Path)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected matched files: %v", got)
	}
}
