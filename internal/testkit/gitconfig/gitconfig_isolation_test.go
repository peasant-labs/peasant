package gitconfig

import (
	"fmt"
	"os"
	"testing"
)

// recordingRun records whether the helper under test ran this package's tests
// and what they returned.
type recordingRun struct {
	m    *testing.M
	ran  bool
	code int
}

func (r *recordingRun) Run() int {
	r.ran = true
	r.code = r.m.Run()
	return r.code
}

// TestMain isolates git like every other package that starts processes. This
// package tests the helper itself, so it exits with the tests' own result and
// fails when the helper skips them or changes their exit code: otherwise a
// helper that returned 0 would pass here and in every package that uses it.
func TestMain(m *testing.M) {
	run := &recordingRun{m: m}
	code := RunWithIsolatedGitConfig(run)
	if !run.ran || code != run.code {
		fmt.Fprintf(os.Stderr, "gitconfig TestMain: RunWithIsolatedGitConfig returned %d (tests ran: %t, their exit code %d); "+
			"it must run the tests and return their exit code\n", code, run.ran, run.code)
		os.Exit(1)
	}
	os.Exit(run.code)
}
