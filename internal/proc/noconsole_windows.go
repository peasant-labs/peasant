//go:build windows

package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsoleWindow stops cmd from opening a console window of its own.
//
// A console program inherits its parent's console when the parent has one. A
// process started with DETACHED_PROCESS has no console to lend, and `peasant web
// start` detaches exactly so the server outlives the shell that launched it, so
// every console child it spawns ALLOCATES ITS OWN console - and that console is a
// window the user watches appear and vanish. Serving one dashboard page resolves
// git state for many sessions, so the windows arrive in a burst rather than
// singly.
//
// CREATE_NO_WINDOW gives the child a console it owns but never shows. It is not
// DETACHED_PROCESS, which would leave the child with no console at all and can
// change how a program decides to behave; git writes to whichever pipes it is
// given either way.
//
// The flag is merged rather than assigned, so a call site that already sets
// SysProcAttr for its own reasons does not silently lose it.
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
