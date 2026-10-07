package proc

import (
	"os/exec"
	"runtime"
	"testing"
)

// TestHideConsoleWindowLeavesTheCommandRunnable is the portable half: whatever the
// platform does with the flags, the command must still run and still return its
// output. A helper that quietly broke every subprocess would otherwise only be
// caught on one platform.
func TestHideConsoleWindowLeavesTheCommandRunnable(t *testing.T) {
	t.Parallel()
	name, args := "sh", []string{"-c", "printf hidden"}
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", "echo hidden"}
	}
	cmd := exec.Command(name, args...)
	HideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s did not run after HideConsoleWindow: %v", name, err)
	}
	if got := string(out); len(got) == 0 {
		t.Fatalf("%s produced no output after HideConsoleWindow", name)
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
