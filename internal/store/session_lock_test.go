package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
)

const (
	sessionLockHelperEnv  = "PEASANT_SESSION_LOCK_HELPER"
	sessionLockRootEnv    = "PEASANT_SESSION_LOCK_ROOT"
	sessionLockIDEnv      = "PEASANT_SESSION_LOCK_ID"
	sessionLockReadyEnv   = "PEASANT_SESSION_LOCK_READY"
	sessionLockReleaseEnv = "PEASANT_SESSION_LOCK_RELEASE"
)

// TestSessionLockAcrossProcesses proves the per-session OS advisory lock
// serializes two REAL processes: a child holds the exclusive lock, the parent's
// exclusive acquisition is refused by context until the child releases, and the
// parent then acquires it. Cross-process safety is part of the reader protocol,
// not only a Go race test.
func TestSessionLockAcrossProcesses(t *testing.T) {
	if os.Getenv(sessionLockHelperEnv) == "1" {
		runSessionLockHelper(t)
		return
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	ready := filepath.Join(dir, "ready")
	releaseFile := filepath.Join(dir, "release")
	sid := "44444444-4444-4444-8444-444444444444"
	id, err := schema.NewSessionID(sid)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionLockAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		sessionLockHelperEnv+"=1",
		sessionLockRootEnv+"="+root,
		sessionLockIDEnv+"="+sid,
		sessionLockReadyEnv+"="+ready,
		sessionLockReleaseEnv+"="+releaseFile,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(releaseFile, []byte("release"), 0o600)
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child process did not acquire the session lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := locker.LockExclusive(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent acquired the lock the child held (err=%v); the OS lock is not cross-process", err)
	}

	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child process: %v", err)
	}
	enter, cancelEnter := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelEnter()
	unlock, err := locker.LockExclusive(enter, id)
	if err != nil {
		t.Fatalf("parent could not acquire the lock after the child released it: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func runSessionLockHelper(t *testing.T) {
	locker, err := NewFileSessionLocker(os.Getenv(sessionLockRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	id, err := schema.NewSessionID(os.Getenv(sessionLockIDEnv))
	if err != nil {
		t.Fatal(err)
	}
	release, err := locker.LockExclusive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if err := os.WriteFile(os.Getenv(sessionLockReadyEnv), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(sessionLockReleaseEnv)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child timed out waiting for release")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
