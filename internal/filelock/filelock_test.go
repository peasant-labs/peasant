package filelock

import (
	"errors"
	"os"
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
