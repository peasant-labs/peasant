//go:build unix

package proc

import "os/exec"

// HideConsoleWindow is a no-op on unix, where starting a process never opens a
// window. It exists so that a call site can be written once for every platform.
func HideConsoleWindow(cmd *exec.Cmd) {}
