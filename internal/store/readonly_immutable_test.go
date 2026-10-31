package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// TestImmutableReadOnlyRefusesPendingWAL: an immutable open would skip the
// frames a non-empty -wal still holds, so it must refuse instead of reading
// stale data, and open normally once the log is empty.
func TestImmutableReadOnlyRefusesPendingWAL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "peasant.db")
	storetest.CopyGoldenTo(t, path)
	if err := os.WriteFile(path+"-wal", []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if db, err := store.OpenReadOnlyWithOptions(path, store.WithImmutableReadOnly()); err == nil {
		db.Close()
		t.Fatal("immutable open succeeded with a non-empty -wal")
	} else if !strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("want a write-ahead log refusal, got: %v", err)
	}
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenReadOnlyWithOptions(path, store.WithImmutableReadOnly())
	if err != nil {
		t.Fatalf("immutable open without a -wal: %v", err)
	}
	db.Close()
}
