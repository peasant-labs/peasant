package scripts_test

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_push_copy/*.yaml
var profileScriptFixtures embed.FS

type profileScriptCase struct {
	Name       string            `yaml:"name"`
	Setup      string            `yaml:"setup"`
	Args       []string          `yaml:"args"`
	Command    string            `yaml:"command"`
	Status     int               `yaml:"status"`
	Output     string            `yaml:"output"`
	Files      map[string]string `yaml:"files"`
	ExactFiles map[string]string `yaml:"exactFiles"`
	Absent     []string          `yaml:"absent"`
	EmptyDirs  []string          `yaml:"emptyDirs"`
	PrivateDir string            `yaml:"privateDir"`
}

func loadProfileScriptFixtures(t *testing.T) []profileScriptCase {
	t.Helper()
	data, err := profileScriptFixtures.ReadFile("testdata/profile_push_copy/cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cases []profileScriptCase
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must have exactly one document: %v", err)
	}
	data, err = profileScriptFixtures.ReadFile("testdata/profile_push_copy/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "profile script")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "profile script"); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestProfilePushCopy(t *testing.T) {
	if err := prepareProfileScriptBase("/tmp/opencode"); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("profile-push-copy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loadProfileScriptFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp/opencode", "peasant-push-profile-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if c.Setup != "" {
				setup := exec.CommandContext(ctx, "bash", "-eu", "-c", c.Setup)
				setup.Dir = root
				setup.Env = append(os.Environ(), "ROOT="+root)
				if out, err := setup.CombinedOutput(); err != nil {
					t.Fatalf("setup: %v\n%s", err, out)
				}
			}
			args := []string{script}
			for _, arg := range c.Args {
				args = append(args, strings.ReplaceAll(arg, "{root}", root))
			}
			if c.Command != "" {
				args = append(args, "--", "bash", "-eu", "-c", c.Command, "profile-child", "argument with spaces")
			}
			cmd := exec.CommandContext(ctx, "bash", args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "ROOT="+root)
			out, runErr := cmd.CombinedOutput()
			status := 0
			if runErr != nil {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) {
					t.Fatalf("run production script: %v", runErr)
				}
				status = exit.ExitCode()
			}
			if status != c.Status || !strings.Contains(string(out), c.Output) {
				t.Errorf("status=%d want=%d; want output %q\n%s", status, c.Status, c.Output, out)
			}
			assertFile := func(name, want string, exact bool) {
				t.Helper()
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !strings.Contains(string(got), want) || (exact && string(got) != want) {
					t.Errorf("%s: got %q, err=%v; want %q (exact=%v)", name, got, err, want, exact)
				}
			}
			assertFile("sentinel", "untouched", true)
			for name, want := range c.Files {
				assertFile(name, want, false)
			}
			for name, want := range c.ExactFiles {
				assertFile(name, want, true)
			}
			for _, name := range c.Absent {
				if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Errorf("%s should be absent: %v", name, err)
				}
			}
			for _, name := range c.EmptyDirs {
				entries, err := os.ReadDir(filepath.Join(root, name))
				if err != nil || len(entries) != 0 {
					t.Errorf("%s should be an empty directory: %v, %v", name, entries, err)
				}
			}
			if c.PrivateDir != "" {
				info, err := os.Stat(filepath.Join(root, c.PrivateDir))
				if err != nil || info.Mode().Perm() != 0700 {
					t.Errorf("%s must have private mode 0700: %v, %v", c.PrivateDir, info, err)
				}
			}
		})
	}
}
