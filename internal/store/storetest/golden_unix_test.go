//go:build unix

package storetest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDeadOwnerSweepCadence pins the conservative reap rule: a dead owner's
// shelf is removed only past the age floor (PID-reuse protection), a fresh
// dead shelf is kept, a live owner's old shelf is never touched, and
// non-matching entries are never touched. The dead PID comes from a reaped
// child process, so it is provably dead at sweep time. The sweep runs inside
// the per-user scheme subdirectory, mirroring the production override path.
//
// This test is unix-only: it observes the liveness probe (Kill(pid, 0)), and
// the !unix probe reports every process as alive by design, so the "live
// owner is never touched" property has no age-only counterpart to assert
// there.
func TestDeadOwnerSweepCadence(t *testing.T) {
	resetGoldenStateForTest(t)
	root := filepath.Join(t.TempDir(), managedRootDirName())
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	deadPid := deadChildPID(t)
	old := time.Now().Add(-(ownerAgeFloor + time.Minute))

	deadOld := filepath.Join(root, fmt.Sprintf("pid-%d", deadPid))
	// A neighbouring PID that was never assigned reads dead (ESRCH) exactly
	// like a reaped one; with a fresh mtime it must still be kept.
	deadFresh := filepath.Join(root, fmt.Sprintf("pid-%d", deadPid+1000000))
	liveOld := filepath.Join(root, fmt.Sprintf("pid-%d", os.Getpid()))
	other := filepath.Join(root, "not-ours")
	for _, dir := range []string{deadOld, deadFresh, liveOld, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the reaped candidate and the live-owner shelf; the fresh dead
	// shelf keeps a current mtime.
	if err := os.Chtimes(deadOld, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(liveOld, old, old); err != nil {
		t.Fatal(err)
	}

	sweepDeadOwners(root)

	if _, err := os.Stat(deadOld); !os.IsNotExist(err) {
		t.Fatalf("dead owner past the age floor was not reaped: %v", err)
	}
	for _, keep := range []string{deadFresh, liveOld, other} {
		if _, err := os.Stat(filepath.Join(keep, "marker")); err != nil {
			t.Fatalf("sweep removed %s, which must be kept: %v", keep, err)
		}
	}
}

// deadChildPID returns the PID of a child process that has exited and been
// reaped, hence provably dead for the sweep test.
func deadChildPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a trivial child process: %v", err)
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		t.Fatal("child process has no PID")
	}
	return cmd.Process.Pid
}
