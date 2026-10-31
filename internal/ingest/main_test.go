package ingest_test

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain checks for leaked goroutines and gives every child git a hermetic
// configuration. The git resolver inherits the process environment, so a
// developer's ~/.gitconfig (for example a url.<base>.insteadof rewrite) would
// otherwise change what `git remote get-url` prints in the resolver tests.
// Tests that need a bounded staging arena or a deterministic OpenCode
// environment inject it through the WithArenaSizeBytes and
// WithOpenCodeEnvironment options; production keeps the environment default at
// the composition root.
func TestMain(m *testing.M) {
	if err := os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		panic(err)
	}
	// Config injected through the environment (git -c, GIT_CONFIG_COUNT/KEY/VALUE)
	// bypasses both files above.
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	goleak.VerifyTestMain(m)
}
