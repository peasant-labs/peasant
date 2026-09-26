package codexstate_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codexstate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// materializeNativeStateDB writes a real temporary native Codex state database
// with the production threads columns, so the read-only reader is exercised
// against real SQLite storage and never a prepared DTO.
func materializeNativeStateDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "state_5.sqlite")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open native state database: %v", err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, history_mode TEXT NOT NULL DEFAULT 'legacy')", nil); err != nil {
		t.Fatalf("create native threads table: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, "INSERT INTO threads (id, rollout_path, history_mode) VALUES (?, ?, ?)", &sqlitex.ExecOptions{
		Args: []any{"aaaaaaaa-1111-4111-8111-111111111111", filepath.Join(dir, "sessions", "rollout-current.jsonl"), "paginated"},
	}); err != nil {
		t.Fatalf("insert native thread row: %v", err)
	}
	return path
}

func TestPointerStoreReadsCurrentRollout(t *testing.T) {
	dir := t.TempDir()
	path := materializeNativeStateDB(t, dir)
	store := codexstate.NewPointerStore(path)

	record, found, err := store.CurrentRollout(context.Background(), "aaaaaaaa-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("CurrentRollout: %v", err)
	}
	if !found {
		t.Fatal("CurrentRollout found = false, want the native current-rollout row")
	}
	if record.Pointer != filepath.Join(dir, "sessions", "rollout-current.jsonl") {
		t.Fatalf("pointer = %q, want the native current rollout path", record.Pointer)
	}
	if record.HistoryMode != "paginated" {
		t.Fatalf("history mode = %q, want paginated", record.HistoryMode)
	}
}

func TestPointerStoreUnknownThreadIsNotAnAuthority(t *testing.T) {
	dir := t.TempDir()
	path := materializeNativeStateDB(t, dir)
	store := codexstate.NewPointerStore(path)

	_, found, err := store.CurrentRollout(context.Background(), "bbbbbbbb-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatalf("CurrentRollout: %v", err)
	}
	if found {
		t.Fatal("CurrentRollout found = true for a thread the native store does not hold; detached fallback must be allowed")
	}
}

func TestPointerStoreSanitizesMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	store := codexstate.NewPointerStore(filepath.Join(dir, "state_9.sqlite"))

	_, _, err := store.CurrentRollout(context.Background(), "aaaaaaaa-1111-4111-8111-111111111111")
	if err == nil {
		t.Fatal("CurrentRollout error = nil for a missing native state database")
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("pointer-store error leaks the raw private path: %v", err)
	}
	if !strings.Contains(err.Error(), "detached-file authority") {
		t.Fatalf("pointer-store error = %v, want the caller-effect recovery", err)
	}
}

func TestPointerStoreIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := materializeNativeStateDB(t, dir)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := codexstate.NewPointerStore(path)
	record, found, err := store.CurrentRollout(context.Background(), "aaaaaaaa-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("CurrentRollout: %v", err)
	}
	if !found || record.Pointer == "" {
		t.Fatal("read-only capture did not resolve the native pointer")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the read-only native pointer lookup mutated the native state database bytes")
	}
}
