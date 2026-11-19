package gitconfig

import (
	"os"
	"testing"
)

var testsStarted bool

type recordingRun struct{ m *testing.M }

func (r recordingRun) Run() int {
	testsStarted = true
	return r.m.Run()
}

// TestMain uses the same exit path as other test packages. The test below
// proves that its runner was invoked; the helper test pins exit-code passthrough.
func TestMain(m *testing.M) { os.Exit(RunWithIsolatedGitConfig(recordingRun{m})) }

func TestMainRunsTests(t *testing.T) {
	if !testsStarted {
		t.Fatal("TestMain did not invoke the test runner")
	}
}
