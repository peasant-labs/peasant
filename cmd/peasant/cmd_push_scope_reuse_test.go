package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
)

// TestScopedPushResolvesTheRepositoryOnce pins the reuse the push's cost fix
// rests on: the scope resolution takes the root and remote the push resolved
// ahead of the budget clock and does not run git for them again — while it still
// resolves them itself when it was handed none, which is what a push whose
// pre-clock resolution failed relies on.
//
// The proof is the path: the root handed in below is NOT a repository, so any
// attempt to resolve it would fail. A successful call therefore means the values
// were used as given, and removing the reuse turns this test red.
func TestScopedPushResolvesTheRepositoryOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	notARepository := t.TempDir()
	if _, err := resolveRepositoryScope(ctx, db, notARepository, notARepository, "github.com/acme/widgets"); err != nil {
		t.Fatalf("the scope resolution went to git despite being handed the repository: %v", err)
	}

	// With nothing to reuse it resolves, and a directory that is no repository
	// fails — the fallback, still intact.
	if _, err := resolveRepositoryScope(ctx, db, notARepository, "", ""); err == nil {
		t.Fatal("a directory that is no repository resolved with no pre-resolved values: the fallback must still resolve, and fail")
	}
}
