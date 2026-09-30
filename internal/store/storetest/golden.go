// Package storetest provides test helpers that use a pre-migrated "golden"
// SQLite database so a parallel test pays only a file copy and a connection
// open instead of re-running the migration-state check. A private fallback
// build is removed when its last user finishes; an adopted cached stamp stays
// in `.testcache/` until it is swept or the cache directory is deleted. This
// package is the sanctioned source of migrated templates: the
// no-migrating-store-open-in-tests ast-grep rule forbids a skip-less
// store.Open in _test.go outside this package and the migration suite, and
// sanctions an inline CopyGolden* followed by a store.WithSkipMigrations open.
//
// The template is cached per checkout under `.testcache/golden/` (gitignored,
// keyed by schema fingerprint) so focused runs reuse it across processes; the
// per-test copies stay private. The template is published read-only and each
// process reads it once into memory, so copies never touch the cache file's
// lifetime. Copies default to `t.TempDir()`; set `PEASANT_STORETEST_TMPDIR`
// to route them through a managed root (e.g. a RAM disk) with dead-owner
// sweeping. Delete the cache with `rm -rf
// .testcache/`.
package storetest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/filelock"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// cacheScheme versions the template recipe (migrated schema plus the one
// _install_salt row, no seed rows). Bump it when the recipe changes — seeds,
// pragmas, build steps, or a dependency bump that could alter the written
// file — so the new recipe builds under a new stamp instead of reusing a
// stale template.
const cacheScheme = "v1"

// cacheLockWait bounds the cross-process wait for a concurrent template
// build: generous versus the observed ~20-60 s race-mode migration pass, so a
// cold multi-package suite can serialize a few builds without ever waiting
// forever. On deadline the waiter builds privately instead of failing.
const cacheLockWait = 3 * time.Minute

// buildDirMaxAge bounds orphaned build-* debris: a killed run's worst trace
// is a build-* dir and a tiny lock file, reaped here on the next successful
// build.
const buildDirMaxAge = time.Hour

// staleStampMaxAge bounds retired-template debris: a non-current stamp older
// than this is swept on the next successful build. The age grace (runs last
// minutes, not days) ensures an in-flight run of an older build is never
// yanked; the current stamp is never swept.
const staleStampMaxAge = 7 * 24 * time.Hour

// cacheDirForTest injects the cache directory in unit tests (stamp mismatch,
// corrupt-file rebuild, cache-failure fallback) without an env knob. Empty
// means "discover from the checkout". Tests in this package set and restore
// it; it is never set outside tests.
var cacheDirForTest string

var (
	goldenMu         sync.Mutex
	goldenPath       string // path to the fully-migrated template DB
	goldenRefs       int
	goldenCached     bool   // goldenPath is the shared cached file: never delete it
	goldenPrivateDir string // temp dir owning a private (uncached) build: removed at refcount 0
	goldenBuf        []byte // the template read once per process; copies write from it
	goldenBufFor     string // template path goldenBuf was read from
)

// ensureGolden creates or reuses the shared golden DB for the current test.
func ensureGolden(t *testing.T) string {
	t.Helper()
	goldenMu.Lock()
	defer goldenMu.Unlock()
	if goldenPath != "" {
		goldenRefs++
		t.Cleanup(func() { releaseGolden(t) })
		return goldenPath
	}
	if path, ok := adoptCachedGolden(t); ok {
		goldenPath = path
		goldenCached = true
		goldenPrivateDir = ""
		goldenRefs++
		t.Cleanup(func() { releaseGolden(t) })
		return goldenPath
	}
	// Cache unavailable, contended past the deadline, or any cache-side
	// error: today's private temp build. Cache trouble never fails a test —
	// only a failure of this final build path is fatal.
	path, dir := buildPrivateGolden(t)
	goldenPath = path
	goldenCached = false
	goldenPrivateDir = dir
	goldenRefs++
	t.Cleanup(func() { releaseGolden(t) })
	return goldenPath
}

func releaseGolden(t *testing.T) {
	t.Helper()
	goldenMu.Lock()
	defer goldenMu.Unlock()
	if goldenRefs > 0 {
		goldenRefs--
	}
	if goldenRefs != 0 || goldenPath == "" {
		return
	}
	dir, cached := goldenPrivateDir, goldenCached
	goldenPath = ""
	goldenPrivateDir = ""
	goldenCached = false
	if cached || dir == "" {
		// The shared cached file outlives every process that used it; the
		// per-process read buffer stays for the next adoption. Only the
		// refcount is reset.
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("storetest: remove golden DB temp dir %q: %v", dir, err)
	}
}

// adoptCachedGolden returns the path of the shared cached template, building
// and publishing it under an exclusive lock when absent or corrupt. ok is
// false when caching is unavailable or fails at any step, in which case the
// caller falls back to a private build. The fast path (a present, valid
// stamped file) never takes the lock.
func adoptCachedGolden(t *testing.T) (path string, ok bool) {
	t.Helper()
	dir, ok := discoverCacheDir()
	if !ok {
		return "", false
	}
	final := filepath.Join(dir, goldenStamp())
	if cachedTemplateValid(dir, final) {
		return final, true
	}
	return buildCachedGolden(t, dir, final)
}

// discoverCacheDir resolves the per-checkout cache directory, creating it
// with owner-only permissions. ok is false on any failure — unknown checkout
// layout, uncreatable directory, or too little free space — and the caller
// falls back to a private build.
func discoverCacheDir() (string, bool) {
	if cacheDirForTest != "" {
		if err := os.MkdirAll(cacheDirForTest, 0o700); err != nil {
			return "", false
		}
		return cacheDirForTest, true
	}
	root, ok := repoRootForCache()
	if !ok {
		return "", false
	}
	dir := filepath.Join(root, ".testcache", "golden")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false
	}
	if !cacheSpaceOK(dir) {
		return "", false
	}
	return dir, true
}

// repoRootForCache walks up from the test's working directory to the nearest
// go.mod, the repo's established checkout-location pattern. It reports false
// when no enclosing module is found.
func repoRootForCache() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// goldenStamp is the cache filename: scheme + schema version + migration
// fingerprint. A stamp mismatch is a different filename (built on demand; the
// old file is only age-swept, never deleted from under a running process),
// while a same-named file with a wrong version is corrupt (unlinked under the
// lock and rebuilt).
func goldenStamp() string {
	return "golden-" + cacheScheme + "-" + strconv.Itoa(store.CurrentSchemaVersion()) + "-" + store.SchemaFingerprint() + ".db"
}

// cachedTemplateValid stats the stamped file and validates it with the
// read-only, non-migrating store.SchemaVersionAt check. The check runs
// against a private scratch copy inside the cache dir, never the cached file
// itself: a read-only open of a WAL-mode database creates -shm/-wal sidecars
// beside the file (verified by probe), and the shared template must stay
// sidecar-free. The scratch carries a build-* prefix so the under-lock
// reclaim and the age sweep cover a copy orphaned by a SIGKILLed validator.
// Any error means "absent or corrupt" — the caller takes the lock path,
// which re-checks under the lock.
func cachedTemplateValid(dir, final string) bool {
	if _, err := os.Stat(final); err != nil {
		return false
	}
	scratch, err := os.CreateTemp(dir, "build-validate-*")
	if err != nil {
		return false
	}
	scratchName := scratch.Name()
	_ = scratch.Close()
	// A read-only open of a WAL-mode database creates -shm/-wal sidecars
	// beside the scratch (the same probe result that keeps validation off
	// the shared file), so all three paths are removed, not just the copy.
	defer os.Remove(scratchName)
	defer os.Remove(scratchName + "-shm")
	defer os.Remove(scratchName + "-wal")
	data, err := os.ReadFile(final)
	if err != nil {
		return false
	}
	if err := os.WriteFile(scratchName, data, 0o600); err != nil {
		return false
	}
	version, err := store.SchemaVersionAt(scratchName)
	if err != nil {
		return false
	}
	return version == store.CurrentSchemaVersion()
}

// buildCachedGolden publishes the stamped template under the exclusive lock,
// or reports false when locking fails (the caller builds privately). Under
// the lock it re-checks — another process may have just built — then builds
// into a build-* dir, publishes by atomic rename, and sweeps debris.
func buildCachedGolden(t *testing.T, dir, final string) (string, bool) {
	t.Helper()
	release, err := filelock.Acquire(final+".lock", time.Now().Add(cacheLockWait))
	if err != nil {
		return "", false
	}
	defer func() {
		_ = release()
	}()
	// Holding the exclusive lock proves no live builder exists (every build
	// happens under this lock), so any build-* entry is orphaned — a SIGKILLed
	// builder's worst trace — and is reclaimed at any age before rebuilding.
	reclaimBuildDirs(dir)
	if cachedTemplateValid(dir, final) {
		return final, true
	}
	buildDir, err := os.MkdirTemp(dir, "build-*")
	if err != nil {
		return "", false
	}
	// A failed build must not leave a build-* dir behind for the sweeper to
	// age out; remove it eagerly. Success removes it after the publish.
	buildOK := false
	defer func() {
		if !buildOK {
			_ = os.RemoveAll(buildDir)
		}
	}()
	built := filepath.Join(buildDir, "golden.db")
	s, err := store.Open(built)
	if err != nil {
		t.Fatalf("storetest: create golden DB: %v", err)
	}
	if err := s.Close(); err != nil {
		_ = os.RemoveAll(buildDir)
		t.Fatalf("storetest: close golden DB: %v", err)
	}
	published, ok := publishGolden(dir, built, final)
	if !ok {
		return "", false
	}
	_ = os.RemoveAll(buildDir)
	buildOK = true
	sweepCache(dir, filepath.Base(final))
	return published, true
}

// publishGolden makes the built template visible under its stamp. Readers see
// either nothing or the complete read-only file: the build is chmodded 0o444
// (a stray write-open fails loudly instead of leaking a WAL sidecar) and
// moved by atomic rename on the same filesystem. A lost rename race adopts
// the winner's file when it validates; any other rename failure falls back
// to a private build.
func publishGolden(dir, built, final string) (string, bool) {
	if err := os.Chmod(built, 0o444); err != nil {
		return "", false
	}
	if err := os.Rename(built, final); err != nil {
		// Another builder may have won the race: adopt its file when valid.
		if cachedTemplateValid(dir, final) {
			return final, true
		}
		// A corrupt or locked target blocks every future build; clear it and
		// retry once before giving up to the private path.
		removePublished(final)
		if err := os.Rename(built, final); err != nil {
			if cachedTemplateValid(dir, final) {
				return final, true
			}
			return "", false
		}
	}
	return final, true
}

// removePublished deletes a published template, clearing the read-only
// attribute first so the removal works where 0o444 maps to a read-only flag.
func removePublished(path string) {
	_ = os.Chmod(path, 0o666)
	_ = os.Remove(path)
}

// reclaimBuildDirs removes every build-* entry at any age. Callers must hold
// the template-build lock: exclusivity proves no live builder exists, so
// every leftover is a killed run's trace (a torn build dir or an orphaned
// validate scratch) and is safe to remove. Best-effort; failures surface as
// an age-swept leftover on the next successful build, never as a test
// failure.
func reclaimBuildDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !isBuildDir(entry.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// sweepCache reaps bounded debris after a successful build: build-* dirs
// older than buildDirMaxAge, and golden-*.db/lock files whose stamp is not
// the current one and whose mtime is older than staleStampMaxAge. The current
// stamp is never swept.
func sweepCache(dir, current string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		if name == current || name == current+".lock" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		switch {
		case isBuildDir(name):
			if now.Sub(info.ModTime()) < buildDirMaxAge {
				continue
			}
		case isGoldenFile(name):
			if now.Sub(info.ModTime()) < staleStampMaxAge {
				continue
			}
		default:
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
}

// buildPrivateGolden is today's per-process template build, used when the
// cache is unavailable or contended past the deadline. It routes under the
// managed override root when set so the dead-owner sweep covers it,
// otherwise under os.MkdirTemp exactly as before. Only a failure here is
// fatal.
func buildPrivateGolden(t *testing.T) (path, dir string) {
	t.Helper()
	if root, ok := resolveCopyRoot(t); ok {
		var err error
		dir, err = os.MkdirTemp(ownerDir(root), "storetest-golden-*")
		if err == nil {
			path = buildGoldenIn(t, dir)
			if path != "" {
				return path, dir
			}
			_ = os.RemoveAll(dir)
		}
	}
	var err error
	dir, err = os.MkdirTemp("", "storetest-golden-*")
	if err != nil {
		t.Fatalf("storetest: create golden DB temp dir: %v", err)
	}
	return buildGoldenIn(t, dir), dir
}

// buildGoldenIn opens (running all migrations once) and closes the template
// at dir/golden.db. It returns "" (without failing) only when dir is under
// the managed root and the build itself fails, so the caller can fall back
// to os.MkdirTemp; every other failure is fatal, exactly as before.
func buildGoldenIn(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "golden.db")
	// Open (runs all migrations), then close immediately.
	s, err := store.Open(path)
	if err != nil {
		// A managed-root build may fail where the classic temp build
		// succeeds (e.g. transient pressure on the override filesystem); let
		// the caller retry on the classic path instead of failing the test.
		if isManagedDir(dir) {
			return ""
		}
		_ = os.RemoveAll(dir)
		t.Fatalf("storetest: create golden DB: %v", err)
	}
	if err := s.Close(); err != nil {
		if isManagedDir(dir) {
			return ""
		}
		_ = os.RemoveAll(dir)
		t.Fatalf("storetest: close golden DB: %v", err)
	}
	return path
}

// templateBytes reads the template once per process into a buffer; every copy
// writes from that buffer instead of re-reading the cache file. The dominant
// saving is the migration pass the cache removes; the buffer turns each copy
// into one write and decouples copies from the cache file's lifetime.
func templateBytes(t *testing.T, path string) []byte {
	t.Helper()
	goldenMu.Lock()
	defer goldenMu.Unlock()
	if goldenBufFor != path || goldenBuf == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("storetest: read golden template %s: %v", path, err)
		}
		goldenBuf = data
		goldenBufFor = path
	}
	return goldenBuf
}

// writeCopy writes one private copy of the template to dst.
func writeCopy(t *testing.T, golden, dst string) {
	t.Helper()
	if err := os.WriteFile(dst, templateBytes(t, golden), 0o644); err != nil {
		t.Fatalf("storetest: copy golden DB to %s: %v", dst, err)
	}
}

// Open returns a *store.Store backed by a fresh copy of the golden DB.
// Cleanup (Close) is registered via t.Cleanup.
func Open(t *testing.T) *store.Store {
	t.Helper()
	return OpenWith(t)
}

// OpenWith returns a *store.Store backed by a fresh copy of the golden DB with
// additional open options, such as managed generation support. Cleanup (Close)
// is registered via t.Cleanup.
func OpenWith(t *testing.T, options ...store.OpenOption) *store.Store {
	t.Helper()
	golden := ensureGolden(t)
	// copyDirForTest registers the copy-dir removal BEFORE the store Close
	// below, so t.Cleanup's LIFO order closes the store first.
	dbPath := filepath.Join(copyDirForTest(t), "test.db")
	writeCopy(t, golden, dbPath)
	// The copy is byte-identical to the freshly-migrated golden DB, so skip the
	// per-Open migration re-check (pure waste here). Pool size is left at the
	// default: some store paths take a second connection while holding the first
	// (the internal/store concurrency tests deadlock on a 1-connection pool);
	// single-threaded callers (the cmd/peasant CLI tests) opt into a small pool
	// via the EnvPoolSize override in their TestMain.
	openOptions := append([]store.OpenOption{store.WithSkipMigrations()}, options...)
	s, err := store.Open(dbPath, openOptions...)
	if err != nil {
		t.Fatalf("storetest.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("storetest.Open: Close: %v", err)
		}
	})
	return s
}

// CopyGoldenDB copies the golden DB to a private per-test directory and
// returns the path. The caller is responsible for opening and closing the store.
func CopyGoldenDB(t *testing.T) string {
	t.Helper()
	golden := ensureGolden(t)
	dbPath := filepath.Join(copyDirForTest(t), "test.db")
	writeCopy(t, golden, dbPath)
	return dbPath
}

// CopyGoldenTo copies the golden DB to the specified destination path.
// The caller is responsible for creating parent directories and managing
// the resulting file. This is useful for cmd/peasant tests that need the
// DB at a specific XDG-resolved path.
func CopyGoldenTo(t *testing.T, destPath string) {
	t.Helper()
	writeCopy(t, ensureGolden(t), destPath)
}

// isBuildDir reports whether name is a leftover template build directory.
func isBuildDir(name string) bool {
	return len(name) > len("build-") && name[:len("build-")] == "build-"
}

// isGoldenFile reports whether name is a stamped template or its lock file.
func isGoldenFile(name string) bool {
	if len(name) > len("golden-") && name[:len("golden-")] == "golden-" {
		return true
	}
	return false
}

// isManagedDir reports whether dir lives under a managed copy root, where a
// failed build may fall back to the classic temp path instead of failing.
func isManagedDir(dir string) bool {
	copyRootMu.Lock()
	defer copyRootMu.Unlock()
	if copyRoot == "" {
		return false
	}
	rel, err := filepath.Rel(copyRoot, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// SeedSession inserts a minimal session row (with supporting project and host_slug rows)
// into s so that annotation FK constraints on session_id are satisfied.
// Uses INSERT OR REPLACE semantics; calling with the same sessionID twice is safe.
func SeedSession(t *testing.T, s *store.Store, sessionID string) {
	SeedSessionInProject(t, s, sessionID, schema.ProjectHash("testprojhash0000000000000000000000000000000000000000000000000000"))
}

// SeedSessionInProject inserts a minimal session row under the supplied project.
func SeedSessionInProject(t *testing.T, s *store.Store, sessionID string, projectHash schema.ProjectHash) {
	t.Helper()
	ingestedMs := int64(3)
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SessionID:    schema.SessionID(sessionID),
			ModelHarness: ingest.HarnessClaudeCode,
			Model:        schema.ModelID("claude-opus-4-6"),
			HostSlug:     schema.HostSlug("testslug"),
			Project: schema.ProjectContext{
				Hash:     projectHash,
				Name:     "testproj",
				FilePath: "/testproj",
			},
			Timestamp: schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingestedMs},
			Source:    schema.SourceInfo{FilePath: "/f", Format: schema.SourceFormatJSONL},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("storetest.SeedSession(%q): %v", sessionID, err)
	}
}

// SeedSessionEntry inserts a minimal session_entries row so that annotation FK
// constraints on (session_id, entry_index) in annotation_target_entries are satisfied.
// The session row must already exist (call SeedSession first).
func SeedSessionEntry(t *testing.T, s *store.Store, sessionID string, entryIndex int) {
	t.Helper()
	entries := []schema.SessionEntry{
		{
			SessionID:  schema.SessionID(sessionID),
			EntryIndex: entryIndex,
			Harness:    ingest.HarnessClaudeCode,
			EntryType:  schema.EntryTypeText,
			Role:       schema.RoleAssistant,
		},
	}
	if err := s.IndexSessionEntries(context.Background(), schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("storetest.SeedSessionEntry(%q, %d): %v", sessionID, entryIndex, err)
	}
}
