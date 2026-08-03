package salt_test

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// dbCounter generates unique in-memory DB names to prevent cache sharing
// between parallel tests.
var dbCounter atomic.Int64

// openPool opens a fresh in-memory SQLite pool for testing.
// Each call gets a unique, isolated in-memory DB.
// The pool is closed when the test ends.
func openPool(t *testing.T) *sqlitex.Pool {
	t.Helper()
	id := dbCounter.Add(1)
	// Using a named in-memory DB prevents cross-test contamination.
	uri := fmt.Sprintf("file:salttest%d?mode=memory&cache=shared", id)
	pool, err := sqlitex.NewPool(uri, sqlitex.PoolOptions{
		PoolSize: 1,
	})
	if err != nil {
		t.Fatalf("openPool: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("pool.Close: %v", err)
		}
	})
	return pool
}

// readRawSalt reads the raw bytes from _install_salt for inspection.
// Returns nil if no row exists (table may not exist yet).
func readRawSalt(t *testing.T, pool *sqlitex.Pool) []byte {
	t.Helper()
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("readRawSalt: take conn: %v", err)
	}
	defer pool.Put(conn)

	var raw []byte
	err = sqlitex.ExecuteTransient(conn, `SELECT salt FROM _install_salt WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnLen(0)
			raw = make([]byte, n)
			stmt.ColumnBytes(0, raw)
			return nil
		},
	})
	if err != nil {
		// Table might not exist yet — treat as no row.
		return nil
	}
	return raw
}

// TestLoad_FreshDB verifies that Load generates a salt when the table is missing.
func TestLoad_FreshDB(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	s, wasGenerated, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("Load on fresh DB: %v", err)
	}
	if !wasGenerated {
		t.Error("expected wasGenerated=true on fresh DB, got false")
	}

	// The returned salt must be non-zero (random bytes are almost certainly non-zero).
	var zero salt.Salt
	if s == zero {
		t.Error("generated salt is all-zeros (crypto/rand failure?)")
	}

	// The value must have been persisted.
	raw := readRawSalt(t, pool)
	if len(raw) != 32 {
		t.Errorf("persisted salt length = %d, want 32", len(raw))
	}
	if !bytes.Equal(raw, s[:]) {
		t.Error("persisted salt does not match returned salt")
	}
}

// TestLoad_ExistingSalt verifies that Load returns the same salt on subsequent calls.
func TestLoad_ExistingSalt(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	// First call generates.
	s1, wasGenerated, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !wasGenerated {
		t.Error("first Load: expected wasGenerated=true")
	}

	// Second call must return the same salt without regenerating.
	s2, wasGenerated, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if wasGenerated {
		t.Error("second Load: expected wasGenerated=false (already exists)")
	}
	if s1 != s2 {
		t.Error("second Load returned different salt from first Load")
	}
}

// TestLoad_CorruptSalt verifies that Load regenerates when the stored blob has wrong length.
func TestLoad_CorruptSalt(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	// Manually insert a corrupt salt (wrong length).
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("take conn: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `CREATE TABLE IF NOT EXISTS _install_salt (
		id   INTEGER PRIMARY KEY CHECK(id = 1),
		salt BLOB    NOT NULL    CHECK(length(salt) = 32)
	)`, nil); err != nil {
		pool.Put(conn)
		t.Fatalf("create table: %v", err)
	}
	// Bypass the CHECK constraint by disabling enforcement temporarily.
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints = ON`, nil); err != nil {
		pool.Put(conn)
		t.Fatalf("disable check constraints: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO _install_salt (id, salt) VALUES (1, X'0102')`, nil); err != nil {
		pool.Put(conn)
		t.Fatalf("insert corrupt salt: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints = OFF`, nil); err != nil {
		pool.Put(conn)
		t.Fatalf("re-enable check constraints: %v", err)
	}
	pool.Put(conn)

	// Verify the corrupt row is actually there.
	raw := readRawSalt(t, pool)
	if len(raw) == 0 {
		t.Skip("corrupt row not inserted (SQLite build does not support ignore_check_constraints); skipping")
	}
	if len(raw) == 32 {
		t.Skip("corrupt row was silently rejected; skipping")
	}

	// Load must detect the corruption, delete, and regenerate.
	s, wasGenerated, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("Load with corrupt salt: %v", err)
	}
	if !wasGenerated {
		t.Error("Load with corrupt salt: expected wasGenerated=true")
	}
	var zero salt.Salt
	if s == zero {
		t.Error("regenerated salt is all-zeros")
	}

	// The persisted salt must now be valid.
	raw2 := readRawSalt(t, pool)
	if len(raw2) != 32 {
		t.Errorf("persisted salt after regeneration: length = %d, want 32", len(raw2))
	}
}

// TestLoad_IdempotentTableCreation verifies that calling Load twice on the same
// DB does not fail due to "table already exists".
func TestLoad_IdempotentTableCreation(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	for i := range 3 {
		_, _, err := salt.Load(pool)
		if err != nil {
			t.Fatalf("Load call %d: %v", i+1, err)
		}
	}
}

// TestHash_Deterministic verifies that the same salt+input always produces the
// same ProjectHash.
func TestHash_Deterministic(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	s, _, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	h1, err := s.Hash("github.com/user/repo")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h2, err := s.Hash("github.com/user/repo")
	if err != nil {
		t.Fatalf("Hash (second): %v", err)
	}

	if h1 != h2 {
		t.Errorf("Hash is non-deterministic: %q != %q", h1, h2)
	}
}

// TestHash_DifferentInputsDifferentHashes verifies that distinct inputs produce
// distinct hashes (no trivial collision).
func TestHash_DifferentInputsDifferentHashes(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	s, _, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	h1, err := s.Hash("github.com/user/repo-a")
	if err != nil {
		t.Fatalf("Hash a: %v", err)
	}
	h2, err := s.Hash("github.com/user/repo-b")
	if err != nil {
		t.Fatalf("Hash b: %v", err)
	}

	if h1 == h2 {
		t.Errorf("different inputs produced identical hash %q", h1)
	}
}

// TestHash_TypedProjectHash verifies that Hash returns a valid ProjectHash
// (64-char lowercase hex) that satisfies the schema.ProjectHash newtype contract.
func TestHash_TypedProjectHash(t *testing.T) {
	t.Parallel()
	pool := openPool(t)

	s, _, err := salt.Load(pool)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ph, err := s.Hash("/home/user/projects/myapp")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	str := ph.String()
	if len(str) != 64 {
		t.Errorf("ProjectHash length = %d, want 64", len(str))
	}
	for _, c := range str {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("ProjectHash contains non-hex character %q in %q", c, str)
			break
		}
	}
}

// TestHash_SaltIsolation verifies that two different salts produce different
// hashes for the same input (key isolation).
func TestHash_SaltIsolation(t *testing.T) {
	t.Parallel()

	pool1 := openPool(t)
	pool2 := openPool(t)

	s1, _, err := salt.Load(pool1)
	if err != nil {
		t.Fatalf("Load pool1: %v", err)
	}
	s2, _, err := salt.Load(pool2)
	if err != nil {
		t.Fatalf("Load pool2: %v", err)
	}

	if s1 == s2 {
		// Astronomically unlikely with crypto/rand; if it happens, skip.
		t.Skip("two independently generated salts are equal (astronomically unlikely); skipping")
	}

	input := "github.com/user/repo"
	h1, _ := s1.Hash(input)
	h2, _ := s2.Hash(input)

	if h1 == h2 {
		t.Errorf("different salts produced identical hash for the same input: %q", h1)
	}
}
