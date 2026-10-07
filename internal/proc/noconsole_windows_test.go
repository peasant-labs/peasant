//go:build windows

package proc

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestHideConsoleWindowSetsTheFlag pins the behaviour the fix depends on. A
// detached parent has no console to lend, so without CREATE_NO_WINDOW a console
// child allocates its own console and that console is a visible window.
func TestHideConsoleWindowSetsTheFlag(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("cmd", "/c", "ver")
	HideConsoleWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil; the flag was never set")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW (%#x) set",
			cmd.SysProcAttr.CreationFlags, uint32(windows.CREATE_NO_WINDOW))
	}
}

// TestHideConsoleWindowPreservesExistingFlags pins that the flag is merged. A call
// site that sets its own process-group or detachment flags must keep them, or
// suppressing a window would silently change process lifetime.
func TestHideConsoleWindowPreservesExistingFlags(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("cmd", "/c", "ver")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	HideConsoleWindow(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("CREATE_NEW_PROCESS_GROUP was dropped")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("CREATE_NO_WINDOW was not added")
	}
}
