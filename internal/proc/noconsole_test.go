package proc

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHideConsoleWindowLeavesTheCommandRunnable is the portable half: whatever the
// platform does with the flags, the command must still run and still return its
// output. A helper that quietly broke every subprocess would otherwise only be
// caught on one platform.
func TestHideConsoleWindowLeavesTheCommandRunnable(t *testing.T) {
	t.Parallel()
	// The command is this test binary, which exists on every platform and
	// prints PASS when told to run no tests.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate this test binary: %v", err)
	}
	cmd := exec.Command(self, "-test.run=^$")
	HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s did not run after HideConsoleWindow: %v", self, err)
	}
	if got := string(out); !strings.Contains(got, "PASS") {
		t.Fatalf("%s printed %q after HideConsoleWindow, want its PASS line", self, got)
	}
}

// TestHideConsoleWindowIsIdempotent pins that calling it twice is harmless, since
// a caller cannot always tell whether a command it was handed has been through
// the helper already.
func TestHideConsoleWindowIsIdempotent(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	HideConsoleWindow(cmd)
	first := cmd.SysProcAttr
	HideConsoleWindow(cmd)
	if cmd.SysProcAttr != first {
		t.Fatal("the second call replaced SysProcAttr instead of reusing it")
	}
}
