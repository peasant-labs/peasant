package storetest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

// TestForkSeededCopiesTheDatabaseAndExistingSidecars pins the fork contract:
// both destinations are byte-identical to the seeded source, a sidecar that
// exists at the source is copied, and a sidecar that does not exist is neither
// invented nor left over from an earlier fork at the destination.
func TestForkSeededCopiesTheDatabaseAndExistingSidecars(t *testing.T) {
	t.Parallel()
	source := CopyGoldenDB(t)
	sourceBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the seeded source: %v", err)
	}

	dir := t.TempDir()
	first := filepath.Join(dir, "a", "store.db")
	second := filepath.Join(dir, "b", "store.db")
	// A stale sidecar at the destination must not survive a source that has
	// none: replaying another database's log against this one is not the same
	// store.
	if err := os.MkdirAll(filepath.Dir(second), 0o700); err != nil {
		t.Fatalf("prepare the second destination: %v", err)
	}
	if err := os.WriteFile(second+"-wal", []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant a stale sidecar: %v", err)
	}

	got := ForkSeeded(t, source, first, second)
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("ForkSeeded returned %v, want the two destinations in order", got)
	}
	for _, destination := range got {
		data, err := os.ReadFile(destination)
		if err != nil {
			t.Fatalf("read fork %s: %v", destination, err)
		}
		if string(data) != string(sourceBytes) {
			t.Fatalf("fork %s is not byte-identical to the seeded source (%d vs %d bytes)", destination, len(data), len(sourceBytes))
		}
		for _, suffix := range sqliteSidecars {
			if _, err := os.Stat(destination + suffix); !os.IsNotExist(err) {
				t.Fatalf("fork %s grew %s although the source has none (stat error: %v)", destination, suffix, err)
			}
		}
	}
}

// TestForkSeededCopiesWriteAheadLogSidecars proves the sidecar arm is not dead
// code: a source that does have a log and a shared-memory file forks both. A
// copy that dropped them could lose every transaction the log still holds.
func TestForkSeededCopiesWriteAheadLogSidecars(t *testing.T) {
	t.Parallel()
	source := CopyGoldenDB(t)
	logBytes := []byte("write-ahead-log")
	sharedBytes := []byte("shared-memory")
	if err := os.WriteFile(source+"-wal", logBytes, 0o600); err != nil {
		t.Fatalf("plant the source log: %v", err)
	}
	if err := os.WriteFile(source+"-shm", sharedBytes, 0o600); err != nil {
		t.Fatalf("plant the source shared-memory file: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "fork.db")
	ForkSeeded(t, source, destination)

	for suffix, want := range map[string][]byte{"-wal": logBytes, "-shm": sharedBytes} {
		data, err := os.ReadFile(destination + suffix)
		if err != nil {
			t.Fatalf("read the forked %s: %v", suffix, err)
		}
		if string(data) != string(want) {
			t.Fatalf("forked %s = %q, want %q", suffix, data, want)
		}
	}
}

// TestForkStoreClosesAndForksOneSeededStore is the use the parity runner
// needs: a store has written its seed rows, both forks must open and show the
// same stored population, and the source store must be closed by the fork.
func TestForkStoreClosesAndForksOneSeededStore(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "seeded.db")
	CopyGoldenTo(t, source)
	db, err := store.Open(source, store.WithSkipMigrations(), store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open the seeded store: %v", err)
	}

	first := filepath.Join(t.TempDir(), "a.db")
	second := filepath.Join(t.TempDir(), "b.db")
	if _, err := ForkStore(t, db, source, first, second); err != nil {
		t.Fatalf("ForkStore: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close after ForkStore: %v", err)
	}

	for _, path := range []string{first, second} {
		fork, err := store.Open(path, store.WithSkipMigrations(), store.WithPoolSize(1))
		if err != nil {
			t.Fatalf("open fork %s: %v", path, err)
		}
		rows, err := store.TableRowCount(fork, "sessions")
		if err != nil {
			_ = fork.Close()
			t.Fatalf("count sessions in fork %s: %v", path, err)
		}
		if err := fork.Close(); err != nil {
			t.Fatalf("close fork %s: %v", path, err)
		}
		if rows != 0 {
			t.Fatalf("fork %s has %d sessions, want the seeded store's 0", path, rows)
		}
	}
}

// TestForkSeededRefusesAnUnusableInput pins the loud refusals: no destination,
// an empty destination, the source as its own destination, and a repeated
// destination. Silently skipping any of these would leave a parity case
// comparing a store with itself.
func TestForkSeededRefusesAnUnusableInput(t *testing.T) {
	t.Parallel()
	source := CopyGoldenDB(t)
	destination := filepath.Join(t.TempDir(), "fork.db")

	cases := []struct {
		name     string
		dest     []string
		contains string
	}{
		{name: "no destination", dest: nil, contains: "no destination"},
		{name: "empty destination", dest: []string{""}, contains: "empty destination"},
		{name: "source as destination", dest: []string{source}, contains: "names the source"},
		{name: "repeated destination", dest: []string{destination, destination}, contains: "twice"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := forkSeeded(source, testCase.dest...)
			if err == nil {
				t.Fatalf("forkSeeded accepted %s, want a loud refusal", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("forkSeeded error = %v, want it to contain %q", err, testCase.contains)
			}
		})
	}
}
