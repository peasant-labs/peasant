package storetest

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

func TestOpenPreparedPreparesMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "prepared.db")
	db, err := OpenPrepared(t, path)
	if err != nil {
		t.Fatalf("OpenPrepared on a missing path: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prepared path was not created: %v", err)
	}
	if version, err := store.SchemaVersionAt(path); err != nil || version != store.CurrentSchemaVersion() {
		t.Fatalf("prepared copy version = %d, err = %v; want %d", version, err, store.CurrentSchemaVersion())
	}
}

func TestCopyGoldenToIfAbsentKeepsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep.db")
	db, err := OpenPrepared(t, path)
	if err != nil {
		t.Fatalf("first OpenPrepared: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close prepared store: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prepared file: %v", err)
	}
	CopyGoldenToIfAbsent(t, path)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prepared file after the second call: %v", err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("an existing prepared path was overwritten")
	}
}
