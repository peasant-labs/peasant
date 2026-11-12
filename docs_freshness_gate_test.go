package peasant

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDocsFreshnessGate drives the gate that keeps the committed CLI reference
// honest. It covers both branches the gate exists for and that a Makefile-only
// implementation could not exercise: a regenerated page that was not committed,
// and a checkout where git cannot answer at all.
func TestDocsFreshnessGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the gate is a POSIX shell script; the Linux gate job covers it")
	}
	script, err := filepath.Abs(filepath.Join("scripts", "check-docs-fresh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("docs freshness gate script: %v", err)
	}

	t.Run("clean checkout passes", func(t *testing.T) {
		repo := initDocsRepo(t)
		out, code := runDocsGate(t, repo, script, nil)
		if code != 0 {
			t.Fatalf("gate exited %d on a clean checkout: %s", code, out)
		}
	})

	t.Run("modified page is stale", func(t *testing.T) {
		repo := initDocsRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "docs", "cli", "page.md"), []byte("regenerated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runDocsGate(t, repo, script, nil)
		if code == 0 {
			t.Fatalf("gate passed with an uncommitted page: %s", out)
		}
		if !strings.Contains(out, "is stale") {
			t.Fatalf("gate output = %q, want the stale-pages reason", out)
		}
	})

	t.Run("new page is stale", func(t *testing.T) {
		repo := initDocsRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "docs", "cli", "new-command.md"), []byte("new page\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runDocsGate(t, repo, script, nil)
		if code == 0 {
			t.Fatalf("gate passed with an untracked new page: %s", out)
		}
		if !strings.Contains(out, "new-command.md") {
			t.Fatalf("gate output = %q, want the untracked page named", out)
		}
	})

	t.Run("unreadable git fails closed", func(t *testing.T) {
		repo := initDocsRepo(t)
		out, code := runDocsGate(t, repo, script, []string{"GIT_BIN=/bin/false"})
		if code == 0 {
			t.Fatalf("gate passed when git could not answer: %s", out)
		}
		if !strings.Contains(out, "cannot verify") {
			t.Fatalf("gate output = %q, want the unreadable-checkout reason", out)
		}
	})
}

// initDocsRepo builds the smallest checkout the gate can judge: one committed
// page under docs/cli.
func initDocsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "cli", "page.md"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "gate@example.com"},
		{"config", "user.name", "gate"},
		{"add", "-A"},
		{"commit", "-qm", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		// Isolate the seed repository from the developer's git configuration:
		// a global commit.gpgsign blocks on a pinentry prompt the test cannot
		// answer.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	return repo
}

func runDocsGate(t *testing.T, repo, script string, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", script, "docs/cli")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run docs gate: %v", err)
	}
	return string(out), exit.ExitCode()
}
