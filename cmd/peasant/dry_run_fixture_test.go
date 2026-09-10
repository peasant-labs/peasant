package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// seedClosedStore prepares the ONE database state a dry run may inspect: an
// existing, migrated, checkpointed file with no live sidecars, under the data
// directory the command is given.
//
// A dry run does not create or migrate state and does not touch a journal, so a
// test that expects a successful forecast has to arrange that state itself. The
// tests that instead prove the refusals — a missing database, or one with a live
// write-ahead log — must NOT call this.
//
// It asserts the sidecars are gone after Close, because a dry run that found one
// would refuse, and the refusal would then look like a product defect rather than
// a fixture that never checkpointed.
func seedClosedStore(t testing.TB, dir string) string {
	t.Helper()
	return seedClosedStoreAt(t, string(defaults.ResolveDBFilePathWith(dir)))
}

// seedClosedStoreAt is seedClosedStore for a test that lets the environment
// resolve the data directory instead of passing --data-dir.
func seedClosedStoreAt(t testing.TB, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("prepare the data directory for a dry-run fixture: %v", err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the database a dry run will inspect: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the database a dry run will inspect: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			t.Fatalf("the dry-run fixture left %s behind; a dry run refuses a database that is not checkpointed, so this fixture would measure the refusal instead of the forecast", filepath.Base(path+suffix))
		} else if !os.IsNotExist(err) {
			t.Fatalf("check the dry-run fixture's sidecars: %v", err)
		}
	}
	return path
}

// databaseDigest fingerprints the database file and its sidecars, so a test can
// prove a dry run changed nothing rather than only that it created nothing.
//
// The sidecars are part of the fingerprint because a write-ahead log that appears
// during an inspection is a mutation even when the main file is byte-identical.
func databaseDigest(t testing.TB, path string) string {
	t.Helper()
	digest := sha256.New()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		switch {
		case err == nil:
			digest.Write([]byte(filepath.Base(path + suffix)))
			digest.Write(data)
		case os.IsNotExist(err):
			// An absent sidecar is part of the state being fingerprinted.
		default:
			t.Fatalf("fingerprint %s: %v", path+suffix, err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// assertDatabaseUnchanged reports a dry run that wrote to the database it was
// asked only to read, including one that merely opened a journal.
func assertDatabaseUnchanged(t testing.TB, path, before string) {
	t.Helper()
	if after := databaseDigest(t, path); after != before {
		t.Fatalf("the forecast changed the database it was asked to inspect: %s is no longer byte-identical, or a journal appeared beside it", filepath.Base(path))
	}
}

// seedClosedStoreForForecast prepares the state a --dry-run in these arguments
// will inspect, and does nothing when the test already arranged its own database
// or is not running a forecast at all.
//
// It lives in the command harness because nearly every CLI forecast test needs
// it, and a forecast that has nothing to inspect measures the prerequisite
// refusal rather than the behaviour under test. The refusals themselves are
// proven where they belong: the missing-database refusal in
// TestHarvestCmd_DryRun_DoesNotCreateDB and TestPushCmd_DryRunRefusesAMissingDatabase,
// and the live-write-ahead-log refusal in TestDryRunCommandsPreserveExistingFiles.
func seedClosedStoreForForecast(t testing.TB, dir string, args []string) {
	t.Helper()
	if !slices.Contains(args, "--dry-run") {
		return
	}
	if _, err := os.Stat(string(defaults.ResolveDBFilePathWith(dir))); err == nil {
		return // the test arranged its own database
	}
	seedClosedStore(t, dir)
}

// fakeAvailableContent serves the available-content read the share preview
// consumes, so the preview can be driven over the CONTRACT rather than over
// whatever the store currently implements.
type fakeAvailableContent struct {
	snapshot *store.SessionContentSnapshot
	err      error
	asked    []string
}

var _ availableContentReader = (*fakeAvailableContent)(nil)

func (f *fakeAvailableContent) ReadSessionAvailable(_ context.Context, sessionID ingest.SessionID) (*store.SessionContentSnapshot, error) {
	f.asked = append(f.asked, sessionID.String())
	return f.snapshot, f.err
}

// TestStoredSessionEntriesShowsAvailableContent pins what the share preview asks
// of the store and what it does with the answer.
//
// The preview's whole purpose is to show a session a user may still have to
// repair, so it reads the AVAILABLE content: a bounded projection is a valid
// answer and must reach the pane, the session is asked for by its own validated
// identifier, and an unreadable projection is still an error because available
// content may be partial and never invented.
func TestStoredSessionEntriesShowsAvailableContent(t *testing.T) {
	t.Parallel()
	const sessionID = "cccc3333-cccc-4ccc-8ccc-cccccccccccc"
	preview := "a bounded projection of a session that cannot publish yet"

	t.Run("a-bounded-projection-reaches-the-pane", func(t *testing.T) {
		t.Parallel()
		reader := &fakeAvailableContent{snapshot: &store.SessionContentSnapshot{Entries: []schema.SessionEntry{{
			SessionID: schema.SessionID(sessionID), EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
			Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &preview,
		}}}}
		entries, err := storedSessionEntries(t.Context(), reader)(sessionID)
		if err != nil {
			t.Fatalf("the preview refused available content: %v", err)
		}
		if len(entries) != 1 || entries[0].ContentPreview == nil || *entries[0].ContentPreview != preview {
			t.Fatalf("the preview lost the available content: %+v", entries)
		}
		if !slices.Equal(reader.asked, []string{sessionID}) {
			t.Fatalf("the preview asked for %v, want exactly the session it was given", reader.asked)
		}
	})

	t.Run("an-unreadable-projection-is-reported", func(t *testing.T) {
		t.Parallel()
		reader := &fakeAvailableContent{err: errors.New("stored projection is an unknown format")}
		if _, err := storedSessionEntries(t.Context(), reader)(sessionID); err == nil {
			t.Fatal("an unreadable projection was reported as an empty transcript")
		}
	})

	t.Run("an-unusable-identifier-never-reaches-the-store", func(t *testing.T) {
		t.Parallel()
		reader := &fakeAvailableContent{}
		if _, err := storedSessionEntries(t.Context(), reader)("not-a-session-id"); err == nil {
			t.Fatal("an unusable identifier was accepted")
		}
		if len(reader.asked) != 0 {
			t.Fatalf("the store was asked about an unusable identifier: %v", reader.asked)
		}
	})
}
