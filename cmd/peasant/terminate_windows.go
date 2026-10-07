//go:build windows

package main

import "os"

// terminateActionName names the stop mechanism in operator-facing output, so
// the message matches what the platform actually did.
const terminateActionName = "termination request"

// terminateProcess asks the backgrounded server to stop. Windows delivers no
// POSIX signal to another process — os.Process.Signal accepts only Kill there,
// and anything else returns "not supported by windows" — so this terminates the
// process outright.
//
// The server therefore does not run its graceful shutdown on this path. That is
// acceptable because the path is only a fallback: `web stop` first asks the
// server to shut itself down over HTTP, and reaches here only when the server
// no longer answers, in which case it had no graceful shutdown left to run.
func terminateProcess(proc *os.Process) error {
	return proc.Kill()
}
