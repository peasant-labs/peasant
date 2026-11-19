package gitconfig

import (
	"fmt"
	"os"
)

// RunWithIsolatedGitConfig makes every git process the calling test binary
// starts read no configuration and no repository location from the developer's
// environment, then runs the tests and returns their exit code. Call it from
// TestMain in every package whose tests start a process:
//
//	func TestMain(m *testing.M) { os.Exit(gitconfig.RunWithIsolatedGitConfig(m)) }
//
// Without it a test inherits, for example, a global commit.gpgsign=true (which
// starts a signer that can block on a pinentry prompt) or a url.<base>.insteadOf
// rewrite, so it passes in CI and fails on a developer machine. Children built
// from os.Environ() inherit the isolation. A test that needs hostile values
// sets them with t.Setenv, which overrides this baseline for that test only.
// GIT_CONFIG_GLOBAL needs git 2.32 or newer.
func RunWithIsolatedGitConfig(m interface{ Run() int }) int {
	if err := isolateGitConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return m.Run()
}

// isolatedGitSet replaces the global (~/.gitconfig and
// $XDG_CONFIG_HOME/git/config) and system configuration files.
var isolatedGitSet = map[string]string{
	"GIT_CONFIG_GLOBAL":   os.DevNull,
	"GIT_CONFIG_NOSYSTEM": "1",
}

// isolatedGitUnset removes configuration injected through the environment
// (git -c, GIT_CONFIG_COUNT/KEY/VALUE), which bypasses both files, and the
// repository location an outer git (a hook, rebase -x) exports, which would
// point a test's git at the outer repository instead of its temp directory.
var isolatedGitUnset = []string{
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
}

func isolateGitConfig() error {
	for name, value := range isolatedGitSet {
		if err := os.Setenv(name, value); err != nil {
			return isolationError(name, err)
		}
	}
	for _, name := range isolatedGitUnset {
		if err := os.Unsetenv(name); err != nil {
			return isolationError(name, err)
		}
	}
	return nil
}

func isolationError(name string, err error) error {
	return fmt.Errorf("gitconfig.RunWithIsolatedGitConfig: cannot change %s in the test process environment: %w; "+
		"git children would read the developer's configuration, so no test was run", name, err)
}

// WithCleanup runs cleanup after the wrapped test runner returns.
func WithCleanup(m interface{ Run() int }, cleanup func()) interface{ Run() int } {
	return cleanupRunner{m: m, cleanup: cleanup}
}

type cleanupRunner struct {
	m       interface{ Run() int }
	cleanup func()
}

func (r cleanupRunner) Run() int {
	code := r.m.Run()
	r.cleanup()
	return code
}
