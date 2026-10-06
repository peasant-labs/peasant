package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvStoretestTmpDir names the opt-in override for the managed copy root
// that backs golden copies (OpenWith/CopyGoldenDB) and storetest's private
// cold/fallback template builds. It mirrors the repo's convention for package
// env overrides (store.EnvPoolSize, ingest.EnvArenaSizeBytes): a
// package-level constant so the literal appears once, read with os.Getenv.
//
// The default (unset or empty) is t.TempDir(): status-quo semantics, Go-owned
// cleanup, OS tmpfiles under SIGKILL. That default already honors TMPDIR:
// t.TempDir() and os.MkdirTemp("") both resolve through os.TempDir(), so
// setting TMPDIR alone (to a RAM disk, say) already routes every default copy.
// What this override adds on top of TMPDIR placement is cleanup scoped to
// storetest's own shelves: copies land under a per-user, scheme-versioned
// managed root, in per-process pid-* owner directories, and the first use in
// each process sweeps the shelves whose owner is provably dead. A focused-run
// micro-measurement showed no copy-speed difference between a tmpfs root and
// t.TempDir (100 copies of the 800 KiB template: 44.8 ms vs 36.6 ms — noise
// next to the ~25–30 s migration saving), so no RAM-backed default is worth
// its machinery. Set the override when that scoped, self-healing cleanup is
// wanted — e.g. a RAM disk whose copies should not outlive a killed run — and
// the dead-owner sweep below applies.
const EnvStoretestTmpDir = "PEASANT_STORETEST_TMPDIR"

// minFreeBytesForCache is the free-space floor for the template cache: below
// it the helper skips caching and builds privately, so a nearly-full
// filesystem never gains a new standing file from tests.
const minFreeBytesForCache = 256 << 20

// managedRootDirName scopes the managed root per user and carries a scheme
// version: an override root may live anywhere (any worktree, any build, any
// user), so storetest works inside its own subdirectory — users neither
// collide nor hit each other's permissions, the sweep never touches files
// outside it, and a future naming-scheme change does not make a new sweeper
// reap an old layout. (os.Getuid reports -1 on Windows; the name stays a
// deterministic per-machine directory there.)
func managedRootDirName() string {
	return "peasant-storetest-v1-u" + strconv.Itoa(os.Getuid())
}

// ownerAgeFloor is the conservative age floor for reaping a dead owner's
// shelf: an entry is removed only when its owner PID is provably dead AND the
// entry is past this age. The floor protects against PID reuse (a fresh
// shelf for a recycled PID is never old enough to reap) and against
// processes that are still starting. False-alive leaks (self-healing: reboot
// clears tmpfs, and the next sweep retries); false-dead deletes a live
// database, which is unacceptable — so the liveness check always wins ties.
const ownerAgeFloor = 10 * time.Minute

// ownerUnknownMaxAge is the age-only reap rule where liveness cannot be
// probed (!unix: processAlive always reports alive). With no dead/alive
// signal the bias is leak-not-reap, so the age is a full day: no test suite
// spans it, while killed-run litter is still eventually reclaimed.
const ownerUnknownMaxAge = 24 * time.Hour

var (
	copyRootMu    sync.Mutex
	copyRoot      string // resolved managed root for this process ("" = t.TempDir default)
	copyRootSwept bool
)

// resolveCopyRoot returns the managed copy root for this process when the
// override is set: validated loudly, resolved once, with dead-owner entries
// swept on first use. Without the override it reports no managed root and
// the caller uses t.TempDir. An explicitly set but unusable
// PEASANT_STORETEST_TMPDIR fails the calling test loudly (no silent
// fallback); there is no other fallback chain.
func resolveCopyRoot(t *testing.T) (root string, useManaged bool) {
	t.Helper()
	override := os.Getenv(EnvStoretestTmpDir)
	if override == "" {
		return "", false
	}
	// Validate outside the mutex: a loud failure must not wedge the lock.
	if err := validateCopyRootOverride(override); err != nil {
		t.Fatalf("storetest: %v", err)
	}
	root = filepath.Join(override, managedRootDirName())
	copyRootMu.Lock()
	defer copyRootMu.Unlock()
	if copyRootSwept {
		return copyRoot, copyRoot != ""
	}
	copyRootSwept = true
	if err := os.MkdirAll(ownerDir(root), 0o700); err != nil {
		t.Fatalf("storetest: %s=%q: cannot create the owner shelf under the managed root %q: %v; fix the permissions or unset %s to use t.TempDir", EnvStoretestTmpDir, override, root, err, EnvStoretestTmpDir)
	}
	sweepDeadOwners(root)
	copyRoot = root
	return copyRoot, true
}

// copyDirForTest makes this test's private copy directory: a t-* subdir of
// the process's owner shelf under the managed override root when set (the
// removal cleanup is registered BEFORE the caller registers the store's
// Close, so t.Cleanup's LIFO order closes the store first), else t.TempDir()
// with Go-owned cleanup.
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
	return t.TempDir()
}

// validateCopyRootOverride checks an explicitly set PEASANT_STORETEST_TMPDIR:
// the path must be creatable as a directory and writable. The error is
// actionable (what failed, why, where, how to fix) because an explicit
// configuration error must never be hidden behind a silent fallback to
// t.TempDir.
func validateCopyRootOverride(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s=%q: cannot create the override copy root: %v; create the directory or unset %s to use t.TempDir", EnvStoretestTmpDir, dir, err, EnvStoretestTmpDir)
	}
	probe, err := os.CreateTemp(dir, ".storetest-writable-*")
	if err != nil {
		return fmt.Errorf("%s=%q: the override copy root is not writable: %v; fix the permissions or unset %s to use t.TempDir", EnvStoretestTmpDir, dir, err, EnvStoretestTmpDir)
	}
	_ = os.Remove(probe.Name())
	_ = probe.Close()
	return nil
}

// ownerDir is this process's shelf under the managed root. Per-process shelves
// (pid-<pid>) let the first-use sweep reap a killed run's litter by owner
// liveness without ever touching a live process's copies.
func ownerDir(root string) string {
	return filepath.Join(root, fmt.Sprintf("pid-%d", os.Getpid()))
}

// sweepDeadOwners removes pid-* shelves whose owner is provably dead past
// the age floor (or past the unknown-liveness age where PID probing is
// unavailable). It touches only entries matching the helper's own pid-*
// naming inside the managed root — never other files in a user-provided
// override root. Every error (vanished entries, permission failures) is
// best-effort: the sweep never fails a test.
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
			// ENOENT race with a concurrently exiting owner: nothing to reap.
			continue
		}
		if !reapableOwner(pid, info.ModTime(), now) {
			continue
		}
		// Best-effort: an EPERM here (a shelf another user owns inside a
		// shared override root) leaks rather than fails.
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

// parseOwnerPID extracts the pid from a managed-root entry named pid-<pid>.
// It reports false for any other name so the sweep never touches entries the
// helper did not create.
func parseOwnerPID(name string) (int, bool) {
	pid, ok := strings.CutPrefix(name, "pid-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
