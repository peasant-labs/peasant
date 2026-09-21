package main

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generate.yaml
var generatorFixtures []byte

func TestGenerate(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"required_names"`
		Cases         []struct {
			Name  string `yaml:"name"`
			Mode  string `yaml:"mode"`
			Error string `yaml:"error"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(generatorFixtures))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing fixture: %v", err)
	}
	names := map[string]bool{}
	for _, row := range fixtures.Cases {
		if row.Name == "" || names[row.Name] {
			t.Fatal("empty/duplicate fixture", row.Name)
		}
		names[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.md")
			args := []string{path}
			switch row.Mode {
			case "no-args":
				args = nil
			case "extra-args":
				args = append(args, "extra")
			case "missing":
			case "directory":
				args = []string{dir}
			case "file":
				if err := os.WriteFile(path, []byte("obsolete manual policy"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unknown fixture mode", row.Mode)
			}
			var output bytes.Buffer
			err := generate(args, &output)
			if row.Error != "" {
				if err == nil || !strings.Contains(err.Error(), row.Error) {
					t.Fatalf("got %v, want %q", err, row.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			registry, err := ingest.LoadRecordKindRegistry()
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != registry.Document() {
				t.Fatal("generated output differs")
			}
			output.Reset()
			if err := generate(args, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "already current") {
				t.Fatal("regeneration not byte identical")
			}
		})
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Error("missing fixture", name)
		}
		delete(names, name)
	}
	for name := range names {
		t.Error("unlisted fixture", name)
	}
}
