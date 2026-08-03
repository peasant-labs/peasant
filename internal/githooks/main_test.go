package githooks_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// Environment variables this suite controls that are not Peasant's own. HOME
// and the two git switches belong to git, not to the defaults package, so they
// are named once here rather than repeated as literals.
const (
	envHome              = "HOME"
	envGitConfigNoSystem = "GIT_CONFIG_NOSYSTEM"
	envGitTerminalPrompt = "GIT_TERMINAL_PROMPT"
)

// originalEnvironment is the environment the test binary started with. It is
// captured before TestMain redirects everything, and is used by exactly one
// test: the guard that proves this suite never touched the original hooks
// directory or git configuration.
var originalEnvironment []string

// TestMain makes the whole test binary hermetic before a single hook is
// written. Git finds its "global" configuration through HOME and
// XDG_CONFIG_HOME, so redirecting both at a throwaway directory means a
// developer's own git settings - including any hooks directory they configured
// - can never reach a disposable repository and steer a write out of it.
// GIT_CONFIG_NOSYSTEM closes the last door, /etc/gitconfig.
//
// os.Setenv (not t.Setenv) is used deliberately: these settings are constant
// for the whole binary, so no test is marked non-parallelizable by them.
func TestMain(m *testing.M) {
	originalEnvironment = append([]string(nil), os.Environ()...)

	home, err := os.MkdirTemp("", "peasant-githooks-home-*")
	if err != nil {
		panic("githooks TestMain: create temp HOME: " + err.Error())
	}
	os.Setenv(envHome, home)
	os.Setenv(defaults.EnvXDGConfigHome.String(), filepath.Join(home, ".config"))
	os.Setenv(defaults.EnvXDGDataHome.String(), filepath.Join(home, ".local", "share"))
	os.Setenv(defaults.EnvXDGStateHome.String(), filepath.Join(home, ".local", "state"))
	os.Setenv(envGitConfigNoSystem, "1")
	os.Setenv(envGitTerminalPrompt, "0")

	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// disposableRepo creates a brand-new git repository under the test's own
// temporary directory and returns its resolved path. Nothing outside that
// directory is ever created, read for configuration, or written.
func disposableRepo(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}
	mustGit(t, repo, "init", "--quiet", "--initial-branch=main")
	mustGit(t, repo, "config", "user.email", "hooks-test@example.invalid")
	mustGit(t, repo, "config", "user.name", "Hooks Test")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

// mustGit runs one git command inside dir and fails the test on a non-zero exit.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, os.Environ(), args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return out
}

// runGit executes git in argument-list form with an explicit environment.
func runGit(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s", detail)
	}
	return strings.TrimSpace(string(out)), nil
}

// hookPath resolves the file git would run for event in repo, the same way the
// production resolver does, so tests can seed and assert on the real slot.
func hookPath(t *testing.T, repo, event string) string {
	t.Helper()
	path := mustGit(t, repo, "rev-parse", "--git-path", "hooks/"+event)
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	path = filepath.Clean(path)
	requireInsideRepo(t, repo, path)
	return path
}

// requireInsideRepo is the hard safety net of this suite. Every path a test is
// about to seed or assert on must live inside the disposable repository; if a
// stray configuration ever steered git elsewhere, the test fails loudly instead
// of writing into a real hooks directory.
func requireInsideRepo(t *testing.T, repo, path string) {
	t.Helper()
	rel, err := filepath.Rel(repo, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf(
			"refusing to touch %s: it is outside the disposable repository %s.\n"+
				"This means git resolved the hooks directory somewhere unexpected, and the test "+
				"stopped rather than write to a path it does not own.",
			path, repo)
	}
}

// fileFingerprint returns a mode-and-content fingerprint used to prove a file
// was left untouched.
func fileFingerprint(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("mode=%04o sha256=%s", info.Mode().Perm(), hex.EncodeToString(sum[:]))
}

// mustSlotFingerprint is slotFingerprint for a caller that cannot continue
// without an answer.
func mustSlotFingerprint(t *testing.T, path string) string {
	t.Helper()
	fingerprint, err := slotFingerprint(path)
	if err != nil {
		t.Fatalf("fingerprint the hook slot %s: %v", path, err)
	}
	return fingerprint
}

// slotFingerprint describes whatever occupies a hook slot, WITHOUT reading
// anything that is not an ordinary file.
//
// It exists because fileFingerprint cannot be used on an adversarial slot: it
// reads with os.ReadFile, which follows a symlink and then blocks forever on a
// FIFO with no writer. A test proving that Peasant refuses such a slot rather
// than hanging on it must not hang on it either - it would report the very
// failure it is checking for as a timeout of its own.
//
// A symlink is described by the link AND by its target, so a mutation reached
// THROUGH the link is caught as readily as one applied to the link itself:
// ownership is permission to rewrite and delete, and following a link to do
// either is the specific hole this distinction closes.
//
// It returns an error rather than taking a *testing.T because it is also called
// from inside a lifecycle call, on another goroutine, where t.Fatalf is not
// allowed.
func slotFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return shapeFingerprint(path, info)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return fmt.Sprintf("symlink=%s target=absent", target), nil
	}
	shape, err := shapeFingerprint(target, targetInfo)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("symlink=%s target=%s", target, shape), nil
}

// shapeFingerprint fingerprints a regular file by its bytes and mode, and
// anything else by its shape alone. A FIFO, socket, or device has no bytes worth
// comparing, and reading one is the hazard, not the measurement.
func shapeFingerprint(path string, info fs.FileInfo) (string, error) {
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("mode=%s", info.Mode()), nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("mode=%04o sha256=%s", info.Mode().Perm(), hex.EncodeToString(sum[:])), nil
}

// directoryFingerprint returns a stable fingerprint of every entry in dir. A
// missing directory fingerprints as "absent" rather than failing, so the guard
// works on machines whose layout differs.
func directoryFingerprint(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "absent:" + err.Error()
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Name()+" "+fileFingerprint(t, filepath.Join(dir, entry.Name())))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// environmentValue reads one variable out of a captured environment slice.
func environmentValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
}
