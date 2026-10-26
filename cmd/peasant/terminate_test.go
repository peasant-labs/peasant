package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// terminateHelperEnv marks the re-executed child that TestTerminateProcess
// stops, following the same self-exec pattern the harvest-interrupt test uses.
const terminateHelperEnv = "PEASANT_TEST_TERMINATE_HELPER"

// TestTerminateProcessStopsAChild proves the `web stop` fallback can actually
// end the backgrounded server on the running platform. It matters most on
// Windows, where os.Process.Signal delivers no POSIX signal and a SIGTERM
// attempt returns "not supported by windows", so the fallback would report
// failure while leaving the server running.
func TestTerminateProcessStopsAChild(t *testing.T) {
	if os.Getenv(terminateHelperEnv) == "1" {
		// The child exists only to be stopped. The bound keeps a stray child
		// from outliving a broken run.
		time.Sleep(90 * time.Second)
		return
	}

	child := exec.Command(os.Args[0], "-test.run=^TestTerminateProcessStopsAChild$")
	child.Env = append(os.Environ(), terminateHelperEnv+"=1")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	t.Cleanup(func() { _ = child.Process.Kill() })

	if err := terminateProcess(child.Process); err != nil {
		t.Fatalf("terminateProcess on PID %d: %v", child.Process.Pid, err)
	}

	select {
	case <-exited:
		// Any exit status is acceptable: a terminated process does not exit 0.
	case <-time.After(30 * time.Second):
		t.Fatalf("PID %d still running 30s after terminateProcess", child.Process.Pid)
	}
}

// TestTerminateActionNameIsNotEmpty guards the operator-facing label, which is
// interpolated into the `web stop` output and so must say something on every
// platform.
func TestTerminateActionNameIsNotEmpty(t *testing.T) {
	if terminateActionName == "" {
		t.Error("terminateActionName is empty; `web stop` would print a blank mechanism")
	}
}
