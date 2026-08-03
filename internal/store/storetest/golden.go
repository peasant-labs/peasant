// Package storetest provides test helpers that use a pre-migrated "golden"
// SQLite database to avoid paying the full migration cost (~371ms) in every
// parallel test. The golden DB is created once per test binary via sync.Once.
package storetest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

var (
	goldenOnce sync.Once
	goldenPath string // path to the fully-migrated template DB
	goldenErr  error
)

// ensureGolden creates the golden DB exactly once per test binary.
func ensureGolden(t *testing.T) string {
	t.Helper()
	goldenOnce.Do(func() {
		// Use os.MkdirTemp so the golden DB outlives any single test's TempDir.
		dir, err := os.MkdirTemp("", "storetest-golden-*")
		if err != nil {
			goldenErr = err
			return
		}
		goldenPath = filepath.Join(dir, "golden.db")

		// Open (runs all migrations), then close immediately.
		s, err := store.Open(goldenPath)
		if err != nil {
			goldenErr = err
			return
		}
		goldenErr = s.Close()
	})
	if goldenErr != nil {
		t.Fatalf("storetest: create golden DB: %v", goldenErr)
	}
	return goldenPath
}

// copyFile performs a simple file copy using io.Copy.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// Open returns a *store.Store backed by a fresh copy of the golden DB.
// Cleanup (Close) is registered via t.Cleanup.
func Open(t *testing.T) *store.Store {
	t.Helper()
	golden := ensureGolden(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := copyFile(dbPath, golden); err != nil {
		t.Fatalf("storetest.Open: copy golden DB: %v", err)
	}
	// The copy is byte-identical to the freshly-migrated golden DB, so skip the
	// per-Open migration re-check (pure waste here). Pool size is left at the
	// default: some store paths take a second connection while holding the first
	// (the internal/store concurrency tests deadlock on a 1-connection pool);
	// single-threaded callers (the cmd/peasant CLI tests) opt into a small pool
	// via the EnvPoolSize override in their TestMain.
	s, err := store.Open(dbPath, store.WithSkipMigrations())
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

// CopyGoldenDB copies the golden DB to t.TempDir() and returns the path.
// The caller is responsible for opening and closing the store.
func CopyGoldenDB(t *testing.T) string {
	t.Helper()
	golden := ensureGolden(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := copyFile(dbPath, golden); err != nil {
		t.Fatalf("storetest.CopyGoldenDB: copy golden DB: %v", err)
	}
	return dbPath
}

// CopyGoldenTo copies the golden DB to the specified destination path.
// The caller is responsible for creating parent directories and managing
// the resulting file. This is useful for cmd/peasant tests that need the
// DB at a specific XDG-resolved path.
func CopyGoldenTo(t *testing.T, destPath string) {
	t.Helper()
	golden := ensureGolden(t)
	if err := copyFile(destPath, golden); err != nil {
		t.Fatalf("storetest.CopyGoldenTo(%q): %v", destPath, err)
	}
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
