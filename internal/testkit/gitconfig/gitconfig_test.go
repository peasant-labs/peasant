package gitconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostileMarker is the prefix of every config key the hostile environment
// plants; a child git that lists one has read a source the isolation must
// remove.
const hostileMarker = "hostile"

// plantHostileGitEnvironment sets every git configuration source and
// repository-location variable to a hostile value for the rest of the test,
// and returns a fresh repository to run git in.
func plantHostileGitEnvironment(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	section := func(name string) string { return "[" + hostileMarker + "]\n\t" + name + " = true\n" }
	write(filepath.Join(home, ".gitconfig"), section("home"))
	write(filepath.Join(home, "xdg", "git", "config"), section("xdg"))
	poison := filepath.Join(home, "poison.gitconfig")
	write(poison, section("file"))

	// An outer repository whose location leaks in as from a hook or rebase -x.
	outer := filepath.Join(home, "outer")
	gitIn(t, home, "init", "-q", outer)
	gitIn(t, outer, "config", hostileMarker+".outer", "true")
	outerGit := filepath.Join(outer, ".git")

	repo := filepath.Join(home, "repo")
	gitIn(t, home, "init", "-q", repo)

	for name, value := range map[string]string{
		"HOME":                             home,
		"XDG_CONFIG_HOME":                  filepath.Join(home, "xdg"),
		"GIT_CONFIG_GLOBAL":                poison,
		"GIT_CONFIG_SYSTEM":                poison,
		"GIT_CONFIG_NOSYSTEM":              "0",
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 hostileMarker + ".count",
		"GIT_CONFIG_VALUE_0":               "true",
		"GIT_CONFIG_PARAMETERS":            "'" + hostileMarker + ".parameters'='true'",
		"GIT_DIR":                          outerGit,
		"GIT_WORK_TREE":                    outer,
		"GIT_INDEX_FILE":                   filepath.Join(outerGit, "index"),
		"GIT_COMMON_DIR":                   outerGit,
		"GIT_OBJECT_DIRECTORY":             filepath.Join(outerGit, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(outerGit, "objects"),
	} {
		t.Setenv(name, value)
	}
	return repo
}

// gitIn runs git in dir with the test process's current environment.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestIsolatedChildGitReadsNoHostileSource runs a real child git before and
// after the isolation. Before, it must see the planted keys (so the poison is
// real); after, it must see none of them and must resolve its own repository.
func TestIsolatedChildGitReadsNoHostileSource(t *testing.T) {
	repo := plantHostileGitEnvironment(t)

	before := gitIn(t, repo, "config", "--list", "--show-origin")
	for _, key := range []string{"file", "count", "parameters", "outer"} {
		if !strings.Contains(before, hostileMarker+"."+key) {
			t.Fatalf("control: the hostile %s source is not visible before isolation, so this test proves nothing:\n%s", key, before)
		}
	}

	if err := IsolateGitConfig(); err != nil {
		t.Fatal(err)
	}
	after := gitIn(t, repo, "config", "--list", "--show-origin")
	if strings.Contains(after, hostileMarker) {
		t.Errorf("isolated git still reads a hostile source:\n%s", after)
	}
	top := strings.TrimSpace(gitIn(t, repo, "rev-parse", "--show-toplevel"))
	if want, _ := filepath.EvalSymlinks(repo); top != want && top != repo {
		t.Errorf("isolated git resolved repository %q, want %q (an outer GIT_DIR or GIT_WORK_TREE leaked)", top, repo)
	}
}

type fakeTestRun struct {
	code   int
	ran    bool
	global string
}

func (f *fakeTestRun) Run() int {
	f.ran = true
	f.global = os.Getenv("GIT_CONFIG_GLOBAL")
	return f.code
}

// TestRunWithIsolatedGitConfigRunsTheTestsIsolated proves the helper runs the
// tests after isolating and passes their exit code through.
func TestRunWithIsolatedGitConfigRunsTheTestsIsolated(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "hostile.gitconfig"))
	fake := &fakeTestRun{code: 7}
	if got := RunWithIsolatedGitConfig(fake); got != fake.code {
		t.Errorf("exit code = %d, want the tests' own %d", got, fake.code)
	}
	if !fake.ran {
		t.Fatal("the tests were never run")
	}
	if fake.global != os.DevNull {
		t.Errorf("tests ran with GIT_CONFIG_GLOBAL=%q, want %q", fake.global, os.DevNull)
	}
}
