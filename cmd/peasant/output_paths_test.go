package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/spf13/cobra"
)

//go:embed testdata/output_paths.yaml
var outputPathsYAML []byte

type outputPathCase struct {
	Name     string `yaml:"name"`
	Loader   string `yaml:"loader"`
	Override bool   `yaml:"override"`
	Custom   bool   `yaml:"custom"`
	Expected string `yaml:"expected"`
}
type outputPathsFixture struct {
	RequiredNames []string         `yaml:"requiredNames"`
	SourceFixture string           `yaml:"sourceFixture"`
	SessionID     string           `yaml:"sessionID"`
	Cases         []outputPathCase `yaml:"cases"`
}

func loadOutputPathsFixture(t *testing.T) outputPathsFixture {
	t.Helper()
	var fixture outputPathsFixture
	if err := testutil.DecodeFixtureYAML(outputPathsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("invalid output-path fixture %q", c.Name)
		}
		seen[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("output paths", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestRuntimeOutputPaths(t *testing.T) {
	t.Parallel()
	for _, c := range loadOutputPathsFixture(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			custom := filepath.Join(dir, "custom-output")
			root := newTestRoot()
			sub := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }}
			root.AddCommand(sub)
			args := []string{"probe"}
			if c.Override {
				args = append([]string{"--data-dir", dir}, args...)
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			body := "version: 1\n"
			if c.Custom {
				body += "output:\n  basePath: " + custom + "\n"
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			var cfg *config.Config
			var err error
			switch c.Loader {
			case "runtime":
				cfg, err = loadCommandConfig(sub, path)
			case "harvest":
				cfg, err = loadRunConfig(sub, path, false)
			case "dry-run":
				cfg, err = loadRunConfig(sub, path, true)
			case "editing":
				cfg, err = loadConfig(path)
			default:
				t.Fatalf("unknown loader %q", c.Loader)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := defaults.ResolveOutputBasePath().String()
			switch c.Expected {
			case "override":
				want = filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), defaults.OutputSyncSubdir)
			case "custom":
				want = custom
			case "default":
			default:
				t.Fatalf("unknown expected root %q", c.Expected)
			}
			if cfg.Output.BasePath != want {
				t.Errorf("output root=%q, want %q", cfg.Output.BasePath, want)
			}
			stored, err := os.ReadFile(path)
			if err != nil || string(stored) != body {
				t.Fatalf("runtime loader mutated config: %v", err)
			}
		})
	}
}

func TestHarvestDataDirOwnsTranscriptCopies(t *testing.T) {
	fixture := loadOutputPathsFixture(t)
	xdg := t.TempDir()
	t.Setenv(defaults.EnvXDGDataHome.String(), xdg)
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "source")
	if err := os.CopyFS(src, os.DirFS(filepath.Join("..", "..", "internal", "e2e", "testdata", fixture.SourceFixture))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nsources:\n  claude-code: {enabled: false}\n  opencode: {enabled: false}\n  cursor: {enabled: false}\n  codex: {enabled: false}\n  strike: {enabled: false}\n  pi: {enabled: false}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := newTestRoot()
	root.AddCommand(BuildHarvestCommand())
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--data-dir", dir, "--config-dir", dir, "--config", path, "harvest", "--source-harness", "claude-code", "--source-path", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("harvest: %v\n%s", err, out.String())
	}
	want := filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), defaults.OutputSyncSubdir)
	if !strings.Contains(out.String(), want) {
		t.Errorf("harvest did not report output %q: %s", want, out.String())
	}
	found := false
	if err := filepath.WalkDir(want, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.Contains(path, fixture.SessionID) {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("harvest wrote no fixture transcript copy under data-dir")
	}
	if _, err := os.Stat(filepath.Join(xdg, "peasant", defaults.OutputSyncSubdir)); !os.IsNotExist(err) {
		t.Fatalf("harvest wrote to the default data home: %v", err)
	}
}
