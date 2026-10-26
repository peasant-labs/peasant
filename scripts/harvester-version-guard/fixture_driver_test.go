package main

import (
	"bytes"
	_ "embed"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/fixture-driver-support.yaml
var fixtureDriverSupportYAML []byte

type fixtureDriverCase struct {
	Name            string            `yaml:"name"`
	Mode            string            `yaml:"mode"`
	CandidateFiles  map[string]string `yaml:"candidate_files"`
	HistoricalFiles map[string]string `yaml:"historical_files"`
	ExpectedFiles   map[string]string `yaml:"expected_files"`
	ExpectedError   string            `yaml:"expected_error"`
}

func loadFixtureDriverCases(t *testing.T) []fixtureDriverCase {
	t.Helper()
	var fixture struct {
		Cases []fixtureDriverCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(fixtureDriverSupportYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing fixture driver input: %v", err)
	}
	required := map[string]string{
		"absent historical driver receives only shared fixture runtime support": "absent",
		"identical historical driver remains untouched":                         "identical",
		"different historical runtime cannot be overwritten":                    "different",
		"additional historical runtime cannot be hidden":                        "additional",
		"original module based fixture builder needs no driver injection":       "legacy",
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if mode, ok := required[c.Name]; !ok || mode != c.Mode || seen[c.Name] {
			t.Fatalf("invalid named fixture driver obligation %q", c.Name)
		}
		switch c.Mode {
		case "absent":
			if c.CandidateFiles["sqlite.go"] == "" || c.CandidateFiles["sqlitex/exec.go"] == "" || c.CandidateFiles["LICENSE"] == "" || len(c.HistoricalFiles) != 0 || c.ExpectedError != "" {
				t.Fatalf("absent-support obligation is inert: %q", c.Name)
			}
		case "identical":
			if c.CandidateFiles["sqlite.go"] == "" || c.CandidateFiles["sqlite.go"] != c.HistoricalFiles["sqlite.go"] || c.HistoricalFiles["README.md"] == "" || c.ExpectedError != "" {
				t.Fatalf("existing-support preservation is inert: %q", c.Name)
			}
		case "different":
			if c.CandidateFiles["sqlite.go"] == c.HistoricalFiles["sqlite.go"] || c.ExpectedError != "cannot replace historical production driver" {
				t.Fatalf("different-driver refusal is inert: %q", c.Name)
			}
		case "additional":
			if c.HistoricalFiles["extra.go"] == "" || c.CandidateFiles["extra.go"] != "" || c.ExpectedError != "cannot replace historical production driver" {
				t.Fatalf("additional-driver refusal is inert: %q", c.Name)
			}
		case "legacy":
			if len(c.CandidateFiles) != 0 || len(c.HistoricalFiles) != 0 || len(c.ExpectedFiles) != 0 || c.ExpectedError != "" {
				t.Fatalf("legacy candidate obligation changed: %q", c.Name)
			}
		}
		seen[c.Name] = true
	}
	for name := range required {
		if !seen[name] {
			t.Fatalf("missing fixture driver obligation %q", name)
		}
	}
	return fixture.Cases
}

func TestSharedFixtureDriverSupport(t *testing.T) {
	for _, c := range loadFixtureDriverCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			candidate, historical := filepath.Join(t.TempDir(), "candidate"), filepath.Join(t.TempDir(), "historical")
			writeFixtureFiles(t, candidate, map[string]string{"go.mod": "module candidate\n"})
			writeFixtureFiles(t, historical, map[string]string{"go.mod": "module historical\n", "internal/ingest/reader.go": "original production imports\n"})
			writeFixtureFiles(t, filepath.Join(candidate, fixtureDriverPath), c.CandidateFiles)
			writeFixtureFiles(t, filepath.Join(historical, fixtureDriverPath), c.HistoricalFiles)
			err := supplyFixtureDriver(historical, candidate)
			if c.ExpectedError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.ExpectedError) {
				t.Fatalf("expected refusal %q, got %v", c.ExpectedError, err)
			}
			actual := make(map[string]string)
			target := filepath.Join(historical, fixtureDriverPath)
			if _, err := os.Stat(target); err == nil {
				if err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() {
						return nil
					}
					relative, err := filepath.Rel(target, path)
					if err != nil {
						return err
					}
					content, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					actual[relative] = string(content)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(actual, c.ExpectedFiles) {
				t.Fatalf("historical fixture support changed: got %#v, want %#v", actual, c.ExpectedFiles)
			}
			for name, expected := range map[string]string{"go.mod": "module historical\n", "internal/ingest/reader.go": "original production imports\n"} {
				content, err := os.ReadFile(filepath.Join(historical, name))
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != expected {
					t.Fatalf("historical production changed: %s", name)
				}
			}
		})
	}
}

func writeFixtureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if !fs.ValidPath(name) {
			t.Fatalf("invalid fixture path %q", name)
		}
		target := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
