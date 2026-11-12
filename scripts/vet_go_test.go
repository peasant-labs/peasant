package scripts_test

import (
	"context"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/vet-go/cases.yaml
var vetGoCasesYAML []byte

//go:embed testdata/vet-go/manifest.yaml
var vetGoManifestYAML []byte

type vetGoFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
	Tracked bool   `yaml:"tracked"`
}

type vetGoCase struct {
	Name         string      `yaml:"name"`
	Files        []vetGoFile `yaml:"files"`
	WantFailure  bool        `yaml:"wantFailure"`
	WantContains []string    `yaml:"wantContains"`
	WantAbsent   []string    `yaml:"wantAbsent"`
}

type vetGoFixture struct {
	CommonFiles []vetGoFile `yaml:"commonFiles"`
	Cases       []vetGoCase `yaml:"cases"`
}

// TestVetGoTaggedDiscovery invokes the same script as make lint in isolated
// miniature Git modules. It uses real go vet (which compiles but does not run
// the panic-bearing test), so discovery, package grouping, and exit status are
// checked together rather than modeled by a fake Go command.
func TestVetGoTaggedDiscovery(t *testing.T) {
	var fixture vetGoFixture
	if err := testutil.DecodeFixtureYAML(vetGoCasesYAML, &fixture); err != nil {
		t.Fatalf("decode vet-go cases: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(vetGoManifestYAML, "vet-go")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "vet-go"); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("vet-go.sh")
	if err != nil {
		t.Fatalf("resolve vet script: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			env := append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
			run := func(name string, args ...string) ([]byte, error) {
				cmd := exec.CommandContext(ctx, name, args...)
				cmd.Dir = dir
				cmd.Env = env
				// Do not wait indefinitely on pipes inherited by a child after cancellation.
				cmd.WaitDelay = 2 * time.Second
				return cmd.CombinedOutput()
			}
			if output, err := run("git", "init", "--quiet", "--template="); err != nil {
				t.Fatalf("initialize isolated Git repository: %v\n%s", err, output)
			}
			for _, file := range append(fixture.CommonFiles, c.Files...) {
				if file.Path == "" || filepath.IsAbs(file.Path) || strings.HasPrefix(filepath.Clean(file.Path), "..") {
					t.Fatalf("invalid fixture path %q", file.Path)
				}
				path := filepath.Join(dir, file.Path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(file.Content), 0o644); err != nil {
					t.Fatal(err)
				}
				if file.Tracked {
					if output, err := run("git", "add", "--", file.Path); err != nil {
						t.Fatalf("stage fixture %s: %v\n%s", file.Path, err, output)
					}
				}
			}
			output, err := run("bash", script)
			if ctx.Err() != nil {
				t.Fatalf("vet script exceeded its deadline: %v\n%s", ctx.Err(), output)
			}
			if (err != nil) != c.WantFailure {
				t.Fatalf("vet script error = %v, want failure %v\n%s", err, c.WantFailure, output)
			}
			for _, want := range c.WantContains {
				if !strings.Contains(string(output), want) {
					t.Errorf("output does not contain %q:\n%s", want, output)
				}
			}
			for _, absent := range c.WantAbsent {
				if strings.Contains(string(output), absent) {
					t.Errorf("output unexpectedly contains %q:\n%s", absent, output)
				}
			}
		})
	}
}
