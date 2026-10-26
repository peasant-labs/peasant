//go:build unix

package main

import (
	"os"
	"syscall"
)

// terminateActionName names the stop mechanism in operator-facing output, so
// the message matches what the platform actually did.
const terminateActionName = "SIGTERM"

// terminateProcess asks the backgrounded server to stop. Unix sends SIGTERM so
// the server can run its graceful shutdown; see terminate_windows.go for why
// Windows cannot.
func terminateProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
