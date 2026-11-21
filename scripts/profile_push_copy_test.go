package scripts_test

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_push_copy/*.yaml
var profileScriptFixtures embed.FS

const profileCasesPath = "testdata/profile_push_copy/cases.yaml"
const profileManifestPath = "testdata/profile_push_copy/manifest.yaml"

type profileScriptCase struct {
	Name string `yaml:"name"`
	// Setup is performed by the test below the case root before the script runs.
	Setup []profileStep `yaml:"setup"`
	Args  []string      `yaml:"args"`
	// Child is what the profiled command does; when it is empty no command is
	// passed after --.
	Child      []profileStep     `yaml:"child"`
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
	cases, err := decodeProfileScriptFixtures()
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func decodeProfileScriptFixtures() ([]profileScriptCase, error) {
	data, err := profileScriptFixtures.ReadFile(profileCasesPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", profileCasesPath, err)
	}
	var cases []profileScriptCase
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cases); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("fixture must have exactly one document: %v", err)
	}
	data, err = profileScriptFixtures.ReadFile(profileManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", profileManifestPath, err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "profile script")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.Name)
		if err := validateProfileSteps(c.Setup, "profile script case "+c.Name+" setup"); err != nil {
			return nil, err
		}
		if err := validateProfileSteps(c.Child, "profile script case "+c.Name+" child"); err != nil {
			return nil, err
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "profile script"); err != nil {
		return nil, err
	}
	return cases, nil
}

// profileChildCaseEnv names the fixture case whose child steps this test
// binary performs when the production script runs it as the profiled command.
const profileChildCaseEnv = "PEASANT_PROFILE_CHILD_CASE"

// profileChildStepFailed is the status the profiled command ends with when one
// of its expect steps fails, so the case's status assertion reports it.
const profileChildStepFailed = 86

// TestMain is this package's one TestMain, so it also carries the git
// isolation every package that starts a process needs (TESTING.md). The
// isolation wraps the profiled command too: the child inherits the isolated
// environment from its parent test either way.
func TestMain(m *testing.M) { os.Exit(gitconfig.RunWithIsolatedGitConfig(profileChildOr{m})) }

// profileChildOr runs the profiled command when the test binary is started as
// one, and the tests otherwise.
type profileChildOr struct{ m *testing.M }

func (r profileChildOr) Run() int {
	if name, ok := os.LookupEnv(profileChildCaseEnv); ok {
		return runProfileChild(name, os.Args[1:])
	}
	return r.m.Run()
}

// runProfileChild is the profiled command: it performs the named case's child
// steps in the environment the production script gives it and returns the
// status to exit with.
func runProfileChild(name string, args []string) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "profiled command for fixture case %q in scripts/profile_push_copy_test.go failed: %v\n", name, err)
		return profileChildStepFailed
	}
	cases, err := decodeProfileScriptFixtures()
	if err != nil {
		return fail(err)
	}
	index := slices.IndexFunc(cases, func(c profileScriptCase) bool { return c.Name == name })
	if index < 0 {
		return fail(fmt.Errorf("no fixture case has this name; %s must name a case in %s", profileChildCaseEnv, profileCasesPath))
	}
	root := os.Getenv("ROOT")
	if !filepath.IsAbs(root) {
		return fail(fmt.Errorf("ROOT is %q, not the absolute case root the test exports", root))
	}
	err = profileStepRunner{root: root, lookup: os.LookupEnv, args: args}.run(cases[index].Child)
	var exit errProfileExit
	switch {
	case errors.As(err, &exit):
		return exit.status
	case err != nil:
		return fail(err)
	}
	return 0
}

func TestProfilePushCopy(t *testing.T) {
	if err := prepareProfileScriptBase("/tmp/opencode"); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("profile-push-copy.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The profiled command is this test binary: TestMain runs the case's child
	// steps when it sees profileChildCaseEnv.
	self, err := os.Executable()
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
			if err := (profileStepRunner{root: root}).run(c.Setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			args := []string{script}
			for _, arg := range c.Args {
				args = append(args, strings.ReplaceAll(arg, "{root}", root))
			}
			if len(c.Child) > 0 {
				args = append(args, "--", self, "argument with spaces")
			}
			cmd := exec.CommandContext(ctx, "bash", args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "ROOT="+root, profileChildCaseEnv+"="+c.Name)
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
