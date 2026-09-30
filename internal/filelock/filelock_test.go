package filelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAcquireExcludesConcurrentHolders proves the contract the golden-template
// cache depends on: two contenders for one lock file serialize (the second
// either waits for release or times out), and a released lock is acquirable
// again by the waiter.
func TestAcquireExcludesConcurrentHolders(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "test.lock")

	release, err := Acquire(lockPath, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatalf("acquire an uncontended lock: %v", err)
	}

	// A contender with an already-passed deadline must fail without blocking.
	if _, err := Acquire(lockPath, time.Now().Add(-time.Second)); err == nil {
		t.Fatal("a lock held by another description was acquired despite an expired deadline")
	} else if !errors.Is(err, errDeadline) {
		t.Fatalf("expired-deadline failure wraps %v, want the deadline error", err)
	}

	// A contender that arrives while the lock is held, then retries after the
	// release, must succeed — this is the cache's one-builds-one-waits shape.
	acquired := make(chan ReleaseFunc, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rel, err := Acquire(lockPath, time.Now().Add(10*time.Second))
		if err != nil {
			t.Errorf("acquire after the holder releases: %v", err)
			return
		}
		acquired <- rel
	}()
	// Give the contender a scheduling slice so it is provably waiting while
	// the lock is held; even if it has not reached Flock yet, the release
	// below still lets it proceed, which is all this test asserts.
	time.Sleep(100 * time.Millisecond)
	if err := release(); err != nil {
		t.Fatalf("release the held lock: %v", err)
	}
	wg.Wait()
	select {
	case rel := <-acquired:
		if err := rel(); err != nil {
			t.Fatalf("release the waiter's lock: %v", err)
		}
	default:
		t.Fatal("the waiter did not acquire the lock after the holder released it")
	}

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("the lock file should survive release for the next contender: %v", err)
	}
}

const (
	helperEnv        = "PEASANT_FILELOCK_HELPER"
	helperPathEnv    = "PEASANT_FILELOCK_PATH"
	helperReadyEnv   = "PEASANT_FILELOCK_READY"
	helperReleaseEnv = "PEASANT_FILELOCK_RELEASE"
)

// TestAcquireAcrossProcesses proves the exclusive lock serializes two REAL
// processes, which is the property the golden-template cache rests on: a child
// holds the lock, the parent's Acquire is refused at its deadline until the
// child releases, and the parent then acquires the lock. It re-executes the
// test binary as the child, the same idiom the session-lock test uses.
func TestAcquireAcrossProcesses(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		runAcquireHelper(t)
		return
	}

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "cross-process.lock")
	ready := filepath.Join(dir, "ready")
	releaseFile := filepath.Join(dir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		helperPathEnv+"="+lockPath,
		helperReadyEnv+"="+ready,
		helperReleaseEnv+"="+releaseFile,
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
			t.Fatal("child process did not acquire the file lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := Acquire(lockPath, time.Now().Add(250*time.Millisecond)); !errors.Is(err, errDeadline) {
		t.Fatalf("parent acquired the lock the child held (err=%v); the lock is not cross-process", err)
	}

	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child process: %v", err)
	}
	rel, err := Acquire(lockPath, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("parent could not acquire the lock after the child released it: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatal(err)
	}
}

// runAcquireHelper is the child half of TestAcquireAcrossProcesses: hold the
// lock, signal readiness, and wait for the release file.
func runAcquireHelper(t *testing.T) {
	t.Helper()
	release, err := Acquire(os.Getenv(helperPathEnv), time.Now().Add(15*time.Second))
	if err != nil {
		t.Fatalf("helper: acquire: %v", err)
	}
	defer func() { _ = release() }()
	if err := os.WriteFile(os.Getenv(helperReadyEnv), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv(helperReleaseEnv)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("helper timed out waiting for the release file")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
