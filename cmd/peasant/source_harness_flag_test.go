package main

import (
	"bytes"
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_harness_flag.yaml
var sourceHarnessFlagYAML []byte

type sourceHarnessFlagFixture struct {
	Name            string   `yaml:"name"`
	Args            []string `yaml:"args"`
	ErrorContains   string   `yaml:"error_contains"`
	OutputContains  []string `yaml:"output_contains"`
	OutputExcludes  []string `yaml:"output_excludes"`
	StoredSession   string   `yaml:"stored_session"`
	OtherSession    string   `yaml:"other_session"`
	OtherTranscript string   `yaml:"other_transcript"`
	DatabaseAbsent  bool     `yaml:"database_absent"`
}

func LoadSourceHarnessFlagFixtures(t testing.TB) []sourceHarnessFlagFixture {
	t.Helper()
	var document struct {
		RequiredNames []string                   `yaml:"required_names"`
		Cases         []sourceHarnessFlagFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(sourceHarnessFlagYAML, &document); err != nil {
		t.Fatalf("decode source harness flag fixtures: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	for _, fixture := range document.Cases {
		if fixture.Name == "" || present[fixture.Name] || len(fixture.Args) == 0 {
			t.Fatalf("source harness flag fixture has an empty or duplicate name or missing args: %q", fixture.Name)
		}
		present[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("source harness flag fixtures", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestSourceHarnessFlagMounted(t *testing.T) {
	for _, fixture := range LoadSourceHarnessFlagFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			if err := os.CopyFS(source, os.DirFS(filepath.Join("testdata", "strike"))); err != nil {
				t.Fatalf("copy synthetic source fixture: %v", err)
			}
			output := filepath.Join(dir, "managed")
			configPath := writeTestConfigFile(t, dir)
			var otherPath string
			if fixture.OtherSession != "" {
				otherRoot := filepath.Join(dir, "other-source")
				otherPath = filepath.Join(otherRoot, "-fixture-project", fixture.OtherSession+".jsonl")
				if err := os.MkdirAll(filepath.Dir(otherPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(otherPath, []byte(fixture.OtherTranscript), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := loadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Sources.ClaudeCode.Enabled = true
				cfg.Sources.ClaudeCode.Paths = []string{otherRoot}
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			replace := strings.NewReplacer("{source}", source, "{output}", output)
			args := []string{"--config", configPath, "--data-dir", dir, "--config-dir", dir, "--state-dir", dir}
			for _, arg := range fixture.Args {
				args = append(args, replace.Replace(arg))
			}
			root := buildRootCommand()
			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(args)
			err := root.Execute()
			if fixture.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), fixture.ErrorContains) {
					t.Fatalf("command error = %v, want %q; output: %s", err, fixture.ErrorContains, &buf)
				}
				return
			}
			if err != nil {
				t.Fatalf("command failed: %v; output: %s", err, &buf)
			}
			if fixture.DatabaseAbsent {
				if _, err := os.Stat(defaults.ResolveDBFilePathWith(dir).String()); !os.IsNotExist(err) {
					t.Fatalf("file-only harvest unexpectedly created/opened an analytics database: %v", err)
				}
			}
			for _, want := range fixture.OutputContains {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("command output missing %q: %s", want, &buf)
				}
			}
			for _, unwanted := range fixture.OutputExcludes {
				if strings.Contains(buf.String(), unwanted) {
					t.Errorf("command output still contains %q: %s", unwanted, &buf)
				}
			}
			if otherPath != "" {
				if data, err := os.ReadFile(otherPath); err != nil || !bytes.Equal(data, []byte(fixture.OtherTranscript)) {
					t.Fatalf("unselected native source changed: %v", err)
				}
				if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if strings.Contains(entry.Name(), fixture.OtherSession) {
						t.Errorf("source override copied unselected session to %s", path)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.StoredSession != "" {
				db, err := store.Open(defaults.ResolveDBFilePathWith(dir).String())
				if err != nil {
					t.Fatalf("open harvested store: %v", err)
				}
				defer db.Close()
				session, err := db.SessionByID(context.Background(), fixture.StoredSession)
				if err != nil || session == nil {
					t.Fatalf("harvested session %q not persisted: %v", fixture.StoredSession, err)
				}
				if fixture.OtherSession != "" {
					other, err := db.SessionByID(t.Context(), fixture.OtherSession)
					if err != nil || other != nil {
						t.Fatalf("source override imported unrelated session: %+v, %v", other, err)
					}
				}
			}
		})
	}
}
