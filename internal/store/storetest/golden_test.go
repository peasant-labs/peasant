package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

// resetGoldenStateForTest makes cache/copy-root tests deterministic: it
// asserts quiescence, swaps in a clean slate, and restores the saved globals
// when the test ends. Callers are serial by construction (no t.Parallel) so
// the package-level injection seams never race.
func resetGoldenStateForTest(t *testing.T) {
	t.Helper()
	goldenMu.Lock()
	if goldenRefs != 0 {
		goldenMu.Unlock()
		t.Fatalf("reset golden state with %d outstanding refs: a previous test leaked its template hold", goldenRefs)
	}
	savedPath, savedRefs := goldenPath, goldenRefs
	savedCached, savedPrivateDir := goldenCached, goldenPrivateDir
	savedBuf, savedBufFor := goldenBuf, goldenBufFor
	savedCacheDir := cacheDirForTest
	savedProbe := probeTmpfsForTest
	copyRootMu.Lock()
	savedRoot, savedSwept := copyRoot, copyRootSwept
	copyRoot, copyRootSwept = "", false
	copyRootMu.Unlock()
	goldenPath, goldenRefs = "", 0
	goldenCached, goldenPrivateDir = false, ""
	goldenBuf, goldenBufFor = nil, ""
	cacheDirForTest = ""
	probeTmpfsForTest = nil
	goldenMu.Unlock()
	t.Cleanup(func() {
		goldenMu.Lock()
		defer goldenMu.Unlock()
		copyRootMu.Lock()
		copyRoot, copyRootSwept = savedRoot, savedSwept
		copyRootMu.Unlock()
		goldenPath, goldenRefs = savedPath, savedRefs
		goldenCached, goldenPrivateDir = savedCached, savedPrivateDir
		goldenBuf, goldenBufFor = savedBuf, savedBufFor
		cacheDirForTest = savedCacheDir
		probeTmpfsForTest = savedProbe
	})
}

// TestConcurrentGoldenCopiesAreIsolated protects the property that makes this
// package safe to use from several tests at once: every user gets its own
// private copy of the shared template in its own directory, so two concurrent
// opens can never share a database file. The template itself may be shared
// across processes (the per-checkout cache) and is published read-only, so a
// test can never write through its copy into another test's database.
func TestConcurrentGoldenCopiesAreIsolated(t *testing.T) {
	t.Parallel()

	const workers = 8
	var mu sync.Mutex
	copies := map[string]int{}
	for i := 0; i < workers; i++ {
		t.Run(fmt.Sprintf("worker-%d", i), func(t *testing.T) {
			t.Parallel()
			path := CopyGoldenDB(t)
			goldenMu.Lock()
			template := goldenPath
			cached := goldenCached
			goldenMu.Unlock()
			if template == "" {
				t.Fatal("the golden template vanished while a user still holds a reference")
			}
			if cached {
				info, err := os.Stat(template)
				if err != nil {
					t.Fatalf("stat the shared template: %v", err)
				}
				if info.Mode().Perm() != 0o444 {
					t.Fatalf("shared template %s has mode %o, want read-only 0444 so no test can write the shared file", template, info.Mode().Perm())
				}
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "isolation-marker"), []byte("private"), 0o600); err != nil {
				t.Fatalf("write a sibling of the copy: %v", err)
			}
			mu.Lock()
			copies[path]++
			mu.Unlock()
		})
	}

	t.Cleanup(func() {
		if len(copies) != workers {
			t.Fatalf("concurrent copies produced %d distinct paths, want one per worker (%d): %v", len(copies), workers, copies)
		}
		for path, count := range copies {
			if count != 1 {
				t.Fatalf("copy %s was handed out %d times", path, count)
			}
		}
	})
}

// TestGoldenCacheStampMismatchBuildsFresh leaves a stale-stamp file in an
// injected cache dir and proves the lookup builds the current stamp on
// demand without deleting the old file out from under a running process.
func TestGoldenCacheStampMismatchBuildsFresh(t *testing.T) {
	resetGoldenStateForTest(t)
	dir := t.TempDir()
	cacheDirForTest = filepath.Join(dir, "golden")
	if err := os.MkdirAll(cacheDirForTest, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cacheDirForTest, "golden-v0-0-deadbeefcafe.db")
	if err := os.WriteFile(stale, []byte("retired template"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := CopyGoldenDB(t)
	if !strings.HasSuffix(path, "test.db") {
		t.Fatalf("copy landed at %s, want a per-test test.db", path)
	}
	goldenMu.Lock()
	template, cached := goldenPath, goldenCached
	goldenMu.Unlock()
	if !cached {
		t.Fatal("the injected cache dir was usable but the template was built privately")
	}
	if template == stale {
		t.Fatal("the stale stamp was adopted instead of building the current one")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the stale stamp was deleted instead of left for the age sweep: %v", err)
	}
	if version, err := store.SchemaVersionAt(template); err != nil || version != store.CurrentSchemaVersion() {
		t.Fatalf("built template version = %d, err = %v; want %d", version, err, store.CurrentSchemaVersion())
	}
}

// TestGoldenCacheCorruptFileRebuilds corrupts a previously built stamp and
// proves the next lookup unlinks it and rebuilds a valid template.
func TestGoldenCacheCorruptFileRebuilds(t *testing.T) {
	resetGoldenStateForTest(t)
	cacheDirForTest = filepath.Join(t.TempDir(), "golden")

	first := CopyGoldenDB(t)
	goldenMu.Lock()
	template := goldenPath
	goldenMu.Unlock()
	if _, err := os.Stat(template); err != nil {
		t.Fatalf("first build left no stamped file: %v", err)
	}
	if _, err := store.SchemaVersionAt(first); err != nil {
		t.Fatalf("first copy is not a valid database: %v", err)
	}

	// Drain the hold so the next lookup re-validates, then corrupt the stamp.
	// (Refcount cleanups run at test end; resetting the path here simulates a
	// fresh process while the test still owns the file — no other test runs
	// concurrently with this serial test.)
	goldenMu.Lock()
	goldenPath, goldenRefs = "", 0
	goldenMu.Unlock()
	if err := os.Chmod(template, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(template, []byte("torn template bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := CopyGoldenDB(t)
	goldenMu.Lock()
	rebuilt := goldenPath
	goldenMu.Unlock()
	if rebuilt != template {
		t.Fatalf("rebuild published under %s, want the same stamp %s", rebuilt, template)
	}
	if version, err := store.SchemaVersionAt(second); err != nil || version != store.CurrentSchemaVersion() {
		t.Fatalf("rebuilt copy version = %d, err = %v; want %d", version, err, store.CurrentSchemaVersion())
	}
}

// TestGoldenCacheFailureFallsBackPrivately points the injected cache dir at
// an unusable path (a regular file, so MkdirAll fails) and proves the lookup
// falls back to a private build instead of failing the test.
func TestGoldenCacheFailureFallsBackPrivately(t *testing.T) {
	resetGoldenStateForTest(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDirForTest = filepath.Join(blocker, "golden")

	path := CopyGoldenDB(t)
	goldenMu.Lock()
	cached := goldenCached
	goldenMu.Unlock()
	if cached {
		t.Fatal("an unusable cache dir was treated as a cache hit")
	}
	if _, err := store.SchemaVersionAt(path); err != nil {
		t.Fatalf("private-fallback copy is not a valid database: %v", err)
	}
}

// TestCopyRootPrecedence pins the managed-root precedence without touching
// the real /dev/shm: an explicit override wins, then the tmpfs probe, else
// no managed root. The pure core is tested directly; the probe seam covers
// the live branch.
func TestCopyRootPrecedence(t *testing.T) {
	resetGoldenStateForTest(t)

	if root, ok := resolveManagedRootForConfig("", false); ok || root != "" {
		t.Fatalf("no override and no tmpfs resolved to %q, want the t.TempDir fallback", root)
	}
	if root, ok := resolveManagedRootForConfig("", true); !ok || root != shmRoot {
		t.Fatalf("no override with tmpfs resolved to %q, want %q", root, shmRoot)
	}
	override := filepath.Join(t.TempDir(), "ramdisk")
	if root, ok := resolveManagedRootForConfig(override, true); !ok || root != override {
		t.Fatalf("explicit override with tmpfs present resolved to %q, want the override %q", root, override)
	}

	t.Setenv(EnvStoretestTmpDir, override)
	if err := validateCopyRootOverride(override); err != nil {
		t.Fatalf("a writable temp dir failed override validation: %v", err)
	}
	probeTmpfsForTest = boolPtr(false)
	root, ok := managedRootCandidate()
	if !ok || root != override {
		t.Fatalf("env override resolved to %q, want %q", root, override)
	}

	// An explicitly set but unusable override is an actionable error with no
	// silent fallback: a regular file is neither creatable as a dir nor
	// writable as one.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateCopyRootOverride(filepath.Join(blocker, "copies")); err == nil {
		t.Fatal("an unusable override validated cleanly; want the actionable error")
	} else if !strings.Contains(err.Error(), EnvStoretestTmpDir) {
		t.Fatalf("override error %q does not name the variable", err)
	}
}

// TestCopyRootTmpfsUnavailableFallsBack disables the tmpfs probe and proves
// copies still land in private per-test directories with no managed root.
func TestCopyRootTmpfsUnavailableFallsBack(t *testing.T) {
	resetGoldenStateForTest(t)
	probeTmpfsForTest = boolPtr(false)

	path := CopyGoldenDB(t)
	copyRootMu.Lock()
	managed := copyRoot
	copyRootMu.Unlock()
	if managed != "" {
		t.Fatalf("tmpfs unavailable but a managed root resolved: %q", managed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fallback copy missing: %v", err)
	}
}

// TestCopyRootOverrideDirectsCopies points the override at a writable dir and
// proves golden copies land under its owner shelf while CopyGoldenTo keeps
// writing to its caller-chosen destination.
func TestCopyRootOverrideDirectsCopies(t *testing.T) {
	resetGoldenStateForTest(t)
	override := filepath.Join(t.TempDir(), "ramdisk")
	t.Setenv(EnvStoretestTmpDir, override)
	probeTmpfsForTest = boolPtr(false)
	cacheDirForTest = filepath.Join(t.TempDir(), "golden")

	path := CopyGoldenDB(t)
	// The copy lives at <override>/pid-<pid>/t-*/test.db.
	if rel, err := filepath.Rel(override, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("override copy %s is not under %s", path, override)
	}

	callerChosen := filepath.Join(t.TempDir(), "xdg", "test.db")
	if err := os.MkdirAll(filepath.Dir(callerChosen), 0o700); err != nil {
		t.Fatal(err)
	}
	CopyGoldenTo(t, callerChosen)
	if _, err := os.Stat(callerChosen); err != nil {
		t.Fatalf("CopyGoldenTo did not write the caller-chosen path: %v", err)
	}
}

// TestCachedTemplateHasNoSidecars adopts the cached template and proves the
// lookup leaves no -shm/-wal litter beside the shared file: validation runs
// against a scratch copy, and copies write from the read-once buffer.
func TestCachedTemplateHasNoSidecars(t *testing.T) {
	resetGoldenStateForTest(t)
	cacheDirForTest = filepath.Join(t.TempDir(), "golden")

	CopyGoldenDB(t)
	goldenMu.Lock()
	template := goldenPath
	goldenMu.Unlock()
	dir := filepath.Dir(template)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == filepath.Base(template) || name == filepath.Base(template)+".lock" {
			continue
		}
		if strings.HasPrefix(name, filepath.Base(template)) {
			t.Fatalf("sidecar %s beside the cached template; validation must not open the shared file read-write", name)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
