//go:build unix

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const webServerStandInEnv = "PEASANT_TEST_WEB_SERVER_STAND_IN"

// The owned child enters before the testing flag parser so its argv is the
// production server argv, rather than a testing command that happens to carry it.
func init() {
	if os.Getenv(webServerStandInEnv) == "1" {
		time.Sleep(90 * time.Second)
		os.Exit(0)
	}
}

// freePort reserves and releases a loopback port, so nothing answers the
// HTTP shutdown request and `web stop` takes its PID-file fallback.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// startChild starts cmd and returns a channel closed when it exits.
func startChild(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	return exited
}

func writePIDFile(t *testing.T, port, pid int) string {
	t.Helper()
	path := pidFilePath(port)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// waitForCommandLine blocks until the process table shows pid running with
// the server's arguments for port: the wake source is the child's exec
// completing. It fails the test after a deadline instead of sleeping blind.
func waitForCommandLine(t *testing.T, pid, port int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Read the process table directly rather than through the code under
		// test, so this file also builds against develop and its tests fail
		// there at runtime instead of at compile time.
		if commandLineShows(pid, "web start --foreground --port "+strconv.Itoa(port)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID %d never showed the stand-in server's command line", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNotSignalled checks stopWeb took a no-signal branch for a process it
// must leave alone. stopWeb signals synchronously, so the error branch is the
// authoritative assertion; the liveness probe only catches a signal that
// already landed.
func assertNotSignalled(t *testing.T, what string, cmd *exec.Cmd, exited <-chan struct{}, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "nothing was signalled") {
		t.Errorf("stopWeb with a PID file naming %s = %v, want a nothing-was-signalled error", what, err)
	}
	select {
	case <-exited:
		t.Errorf("web stop signalled PID %d (%s), which is not a peasant web server", cmd.Process.Pid, what)
	default:
	}
}

// TestStopWebLeavesAnUnrelatedProcessAlone guards #671: a stale PID file that
// names some other process (here `sleep`) must not get it signalled. The file
// is removed and the error says it was stale. Ports are per test, so the
// tests run in parallel.
func TestStopWebLeavesAnUnrelatedProcessAlone(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	cmd := exec.Command("sleep", "300")
	exited := startChild(t, cmd)
	pidFile := writePIDFile(t, port, cmd.Process.Pid)

	err := stopWeb(port)
	assertNotSignalled(t, "`sleep`", cmd, exited, err)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("want a stale-PID-file error, got %v", err)
	}
	if _, statErr := os.Stat(pidFile); !os.IsNotExist(statErr) {
		t.Errorf("stale PID file %s was kept (stat err %v)", pidFile, statErr)
	}
}

// TestStopWebLeavesAnotherProgramWithServerArgsAlone is the #714 review repro:
// a different executable started with the server's own trailing arguments
// (`python3 -c … web start --foreground --port P`) must not be signalled.
func TestStopWebLeavesAnotherProgramWithServerArgsAlone(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	port := freePort(t)
	cmd := exec.Command(python, "-c", "import time; time.sleep(300)", "web", "start", "--foreground", "--port", strconv.Itoa(port))
	exited := startChild(t, cmd)
	writePIDFile(t, port, cmd.Process.Pid)
	deadline := time.Now().Add(10 * time.Second)
	for !commandLineShows(cmd.Process.Pid, "time.sleep") {
		if time.Now().After(deadline) {
			t.Fatal("python3 stand-in never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertNotSignalled(t, "python3 with the server's arguments", cmd, exited, stopWeb(port))
}

// commandLineShows reports whether ps shows want in pid's command line.
func commandLineShows(pid int, want string) bool {
	out, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.Contains(string(out), want)
}

// TestStopWebStopsTheServerItStarted keeps the fallback working for a real
// match: a child of this executable whose command line is the detached
// server's (`… web start --foreground --port N`) is stopped.
func TestStopWebStopsTheServerItStarted(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	cmd := exec.Command(os.Args[0], "web", "start", "--foreground", "--port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(), webServerStandInEnv+"=1")
	exited := startChild(t, cmd)
	writePIDFile(t, port, cmd.Process.Pid)
	waitForCommandLine(t, cmd.Process.Pid, port)

	if err := stopWeb(port); err != nil {
		t.Fatalf("stopWeb(%d) on the server's own PID: %v", port, err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("server stand-in PID %d still running after web stop", cmd.Process.Pid)
	}
}

// A copied binary can have the same name and exact server arguments. Its
// different executable path must still prevent a stale PID file signalling it.
func TestStopWebLeavesSameNamedExecutableAtAnotherPathAlone(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), filepath.Base(self))
	if err := os.WriteFile(other, data, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cmd := exec.Command(other, "web", "start", "--foreground", "--port", strconv.Itoa(port))
	cmd.Env = append(os.Environ(), webServerStandInEnv+"=1")
	exited := startChild(t, cmd)
	writePIDFile(t, port, cmd.Process.Pid)
	waitForCommandLine(t, cmd.Process.Pid, port)
	assertNotSignalled(t, "a same-named executable at another path", cmd, exited, stopWeb(port))
}
