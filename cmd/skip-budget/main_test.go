package main

import (
	"os"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/gitconfig"
)

// runTestChild, when set by the tagged smoke tests, turns this binary into a
// sample child and exits before any test runs. A child is started only by an
// already isolated test process, so it inherits the isolated git environment.
// TestMain lives in an unconstrained file so the git isolation guard sees it.
var runTestChild func()

func TestMain(m *testing.M) {
	if runTestChild != nil {
		runTestChild()
	}
	os.Exit(gitconfig.RunWithIsolatedGitConfig(m))
}
