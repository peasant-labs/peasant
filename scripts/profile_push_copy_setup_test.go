package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

// The shared base is a prerequisite, not a test-owned temporary directory.
// Never remove it or change an existing directory's permissions or contents.
func prepareProfileScriptBase(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create profiling test directory %s: %w; ensure its parent is writable", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect profiling test directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("profiling test directory %s is not a physical directory; choose a directory instead of a symlink or file", path)
	}
	return nil
}

type profileScriptBaseCase struct {
	Name      string            `yaml:"name"`
	Setup     string            `yaml:"setup"`
	WantError string            `yaml:"wantError"`
	Files     map[string]string `yaml:"files"`
}

func loadProfileScriptBaseFixtures(t *testing.T) []profileScriptBaseCase {
	t.Helper()
	data, err := profileScriptFixtures.ReadFile("testdata/profile_push_copy/base_cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cases []profileScriptBaseCase
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must have exactly one document: %v", err)
	}
	data, err = profileScriptFixtures.ReadFile("testdata/profile_push_copy/base_manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "profile script base")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "profile script base"); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestPrepareProfileScriptBase(t *testing.T) {
	for _, c := range loadProfileScriptBaseFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			if c.Setup != "" {
				cmd := exec.Command("bash", "-eu", "-c", c.Setup)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "ROOT="+root)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("setup: %v\n%s", err, out)
				}
			}
			path := filepath.Join(root, "base")
			before, err := os.Lstat(path)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			err = prepareProfileScriptBase(path)
			if c.WantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("got %v, want error containing %q", err, c.WantError)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if before != nil {
				if !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatal("pre-existing object or permissions changed")
				}
			} else if !after.IsDir() || after.Mode().Perm() != 0700 {
				t.Fatalf("new base must be a private directory: %v", after.Mode())
			}
			for name, want := range c.Files {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || string(got) != want {
					t.Errorf("%s: got %q, err=%v; want exactly %q", name, got, err, want)
				}
			}
		})
	}
}
