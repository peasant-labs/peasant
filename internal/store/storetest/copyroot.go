package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// EnvStoretestTmpDir names the override for the managed copy root that backs
// golden copies (OpenWith/CopyGoldenDB) and storetest's private cold/fallback
// template builds. It mirrors the repo's convention for package env overrides
// (store.EnvPoolSize, ingest.EnvArenaSizeBytes): a package-level constant so
// the literal appears once, read with os.Getenv. It is a Linux acceleration
// knob, not a requirement — the tmpfs probe below and the t.TempDir fallback
// keep every platform correct without it.
//
// Precedence: the override (validated: the path must be creatable and
// writable; an explicit configuration error fails loudly with an actionable
// message and no silent fallback) > the /dev/shm tmpfs probe (present,
// tmpfs, writable, >= 256 MiB free) > t.TempDir(). Empty means unset.
const EnvStoretestTmpDir = "PEASANT_STORETEST_TMPDIR"

// shmRoot is the RAM-backed candidate for the managed copy root on unix
// platforms that provide POSIX shared memory there. macOS has no /dev/shm
// (the probe reports "no") and small-container /dev/shm mounts fail the free
// check, so both fall back without error.
const shmRoot = "/dev/shm/peasant-storetest"

// ownerSweepAge is the age rule for the managed-root hygiene sweep: entries
// older than this are reaped even when liveness is unknown (non-unix), and
// dead-owner entries are reaped regardless of age on unix.
const ownerSweepAge = time.Hour

var (
	copyRootMu    sync.Mutex
	copyRoot      string // resolved managed root for this process ("" = t.TempDir fallback)
	copyRootSwept bool
	// probeTmpfsForTest overrides the /dev/shm probe in unit tests so the
	// precedence test is deterministic without touching the real /dev/shm.
	// nil means "probe the real filesystem".
	probeTmpfsForTest *bool
)

// resolveManagedRootForConfig is the precedence core: an explicit override
// wins, then the tmpfs probe, else no managed root. Pure over its inputs so
// unit tests can pin the precedence without touching the environment.
func resolveManagedRootForConfig(override string, tmpfs bool) (string, bool) {
	if override != "" {
		return override, true
	}
	if tmpfs {
		return shmRoot, true
	}
	return "", false
}

// managedRootCandidate applies the precedence chain to the live environment.
func managedRootCandidate() (string, bool) {
	return resolveManagedRootForConfig(os.Getenv(EnvStoretestTmpDir), tmpfsAvailable())
}

// resolveCopyRoot returns the managed copy root for this process, resolving
// once and sweeping dead-owner entries on first use. An explicitly set but
// unusable PEASANT_STORETEST_TMPDIR fails the calling test loudly (no silent
// fallback); a failed tmpfs probe falls back to t.TempDir.
func resolveCopyRoot(t *testing.T) (root string, useManaged bool) {
	t.Helper()
	if override := os.Getenv(EnvStoretestTmpDir); override != "" {
		// Validate outside the mutex: a loud failure must not wedge the lock.
		if err := validateCopyRootOverride(override); err != nil {
			t.Fatalf("storetest: %v", err)
		}
	}
	copyRootMu.Lock()
	defer copyRootMu.Unlock()
	if copyRootSwept {
		return copyRoot, copyRoot != ""
	}
	copyRootSwept = true
	root, ok := managedRootCandidate()
	if !ok {
		return "", false
	}
	if err := os.MkdirAll(ownerDir(root), 0o700); err != nil {
		if os.Getenv(EnvStoretestTmpDir) != "" {
			t.Fatalf("storetest: %s=%q: cannot create the owner shelf under the override copy root: %v; fix the permissions or unset %s to use the /dev/shm probe (else t.TempDir)", EnvStoretestTmpDir, root, err, EnvStoretestTmpDir)
		}
		return "", false
	}
	sweepDeadOwners(root)
	copyRoot = root
	return copyRoot, true
}

// copyDirForTest makes this test's private copy directory: a t-* subdir of
// the process's owner shelf under the managed root when available, else under
// t.TempDir. The removal cleanup is registered BEFORE the caller registers
// the store's Close, so t.Cleanup's LIFO order closes the store first and
// removes the directory after.
func copyDirForTest(t *testing.T) string {
	t.Helper()
	if root, ok := resolveCopyRoot(t); ok {
		dir, err := os.MkdirTemp(ownerDir(root), "t-*")
		if err == nil {
			t.Cleanup(func() {
				_ = os.RemoveAll(dir)
			})
			return dir
		}
		// A TOCTOU loss on the managed root falls back to t.TempDir rather
		// than failing the test; only an explicit override fails loudly, and
		// resolveCopyRoot already handled that.
	}
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}

// validateCopyRootOverride checks an explicitly set PEASANT_STORETEST_TMPDIR:
// the path must be creatable as a directory and writable. The error is
// actionable (what failed, why, where, how to fix) because an explicit
// configuration error must never be hidden behind a silent fallback.
func validateCopyRootOverride(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s=%q: cannot create the override copy root: %v; create the directory or unset %s to use the /dev/shm probe (else t.TempDir)", EnvStoretestTmpDir, dir, err, EnvStoretestTmpDir)
	}
	probe, err := os.CreateTemp(dir, ".storetest-writable-*")
	if err != nil {
		return fmt.Errorf("%s=%q: the override copy root is not writable: %v; fix the permissions or unset %s to use the /dev/shm probe (else t.TempDir)", EnvStoretestTmpDir, dir, err, EnvStoretestTmpDir)
	}
	_ = os.Remove(probe.Name())
	_ = probe.Close()
	return nil
}

// tmpfsAvailable reports whether /dev/shm exists, is a tmpfs, is writable,
// and has at least minFreeBytesForCache free.
func tmpfsAvailable() bool {
	if probeTmpfsForTest != nil {
		return *probeTmpfsForTest
	}
	info, err := os.Stat("/dev/shm")
	if err != nil || !info.IsDir() {
		return false
	}
	if !isTmpfsPath("/dev/shm") {
		return false
	}
	free, ok := freeBytesOnFilesystem("/dev/shm")
	if !ok || free < minFreeBytesForCache {
		return false
	}
	probe, err := os.CreateTemp("/dev/shm", ".storetest-writable-*")
	if err != nil {
		return false
	}
	_ = os.Remove(probe.Name())
	_ = probe.Close()
	return true
}

// ownerDir is this process's shelf under the managed root. Per-process shelves
// (pid-<pid>) let the first-use sweep reap a killed run's litter by owner
// liveness without ever touching a live process's copies.
func ownerDir(root string) string {
	return filepath.Join(root, fmt.Sprintf("pid-%d", os.Getpid()))
}

// sweepDeadOwners removes pid-* shelves whose owner is definitely dead, plus
// any pid-* shelf older than ownerSweepAge (the age rule that governs where
// liveness is unknown). It touches only entries matching the helper's own
// pid-* naming — never other files in a user-provided override root.
func sweepDeadOwners(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, ok := parseOwnerPID(entry.Name())
		if !ok {
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if processAlive(pid) && now.Sub(info.ModTime()) < ownerSweepAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}
