// Package salt provides per-installation HMAC salt stored in the SQLite database.
//
// The salt is a 32-byte random value stored in the _install_salt table. It is
// non-secret (read-accessible to any process that can open the DB), but prevents
// cross-user project hash correlation because each installation has a unique value.
//
// The salt is created once on first use (via crypto/rand) and reused for all
// subsequent calls. If the stored value is corrupt (wrong length), it is
// regenerated automatically.
package salt

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Salt is a 32-byte per-installation HMAC key.
type Salt [32]byte

// sqlCreateTable is the DDL for the _install_salt table.
// The CHECK constraint on id ensures at most one row can ever exist (singleton).
// The CHECK constraint on salt ensures the blob is always exactly 32 bytes.
const sqlCreateTable = `CREATE TABLE IF NOT EXISTS _install_salt (
  id   INTEGER PRIMARY KEY CHECK(id = 1),
  salt BLOB    NOT NULL    CHECK(length(salt) = 32)
)`

const sqlSelectSalt = `SELECT salt FROM _install_salt WHERE id = 1`
const sqlDeleteSalt = `DELETE FROM _install_salt WHERE id = 1`
const sqlInsertSalt = `INSERT OR IGNORE INTO _install_salt (id, salt) VALUES (1, ?)`

// Load reads the installation salt from the _install_salt table.
// If the table is empty it generates a fresh 32-byte salt via crypto/rand and
// inserts it. If the stored value is corrupt (wrong length) it deletes the row
// and regenerates.
//
// Returns (salt, wasGenerated, error).
//
//   - wasGenerated is true when a new salt was created in this call.
//   - error is non-nil only if the DB is unavailable or a fatal I/O failure
//     occurs; the caller should treat this as fatal.
func Load(pool *sqlitex.Pool) (Salt, bool, error) {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return Salt{}, false, fmt.Errorf(
			"salt.Load: take DB connection: %w — "+
				"ensure the SQLite pool is open before calling Load",
			err,
		)
	}
	defer pool.Put(conn)

	// Ensure the table exists.
	if err := sqlitex.ExecuteTransient(conn, sqlCreateTable, nil); err != nil {
		return Salt{}, false, fmt.Errorf(
			"salt.Load: create _install_salt table: %w — "+
				"check that the DB was opened with write permissions",
			err,
		)
	}

	// Try to read an existing salt.
	var rawBlob []byte
	err = sqlitex.ExecuteTransient(conn, sqlSelectSalt, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnLen(0)
			rawBlob = make([]byte, n)
			stmt.ColumnBytes(0, rawBlob)
			return nil
		},
	})
	if err != nil {
		return Salt{}, false, fmt.Errorf(
			"salt.Load: read _install_salt: %w — "+
				"DB may be locked or corrupt",
			err,
		)
	}

	if len(rawBlob) == 32 {
		// Happy path: valid salt already on disk.
		var s Salt
		copy(s[:], rawBlob)
		return s, false, nil
	}

	// Either no row exists (len == 0) or the stored value has wrong length
	// (corrupt). In both cases we delete any stale row and regenerate.
	if len(rawBlob) != 0 {
		// Corrupt: delete the offending row so we can insert a fresh one.
		if err := sqlitex.ExecuteTransient(conn, sqlDeleteSalt, nil); err != nil {
			return Salt{}, false, fmt.Errorf(
				"salt.Load: delete corrupt _install_salt row (got %d bytes, want 32): %w",
				len(rawBlob), err,
			)
		}
	}

	// Generate a fresh salt.
	newSalt, err := generate()
	if err != nil {
		return Salt{}, false, err
	}

	// INSERT OR IGNORE handles the concurrent-generation race: if two processes
	// simultaneously discover an empty table and both try to insert, the second
	// INSERT is silently dropped. Both will then agree on the first writer's value
	// (the concurrent reader will see it on its next Load call). For our use case
	// (single pipeline process) this is sufficient.
	if err := sqlitex.ExecuteTransient(conn, sqlInsertSalt, &sqlitex.ExecOptions{
		Args: []any{newSalt[:]},
	}); err != nil {
		return Salt{}, false, fmt.Errorf(
			"salt.Load: insert generated salt: %w — "+
				"check that _install_salt CHECK constraints are satisfied",
			err,
		)
	}

	return newSalt, true, nil
}

// Hash computes HMAC-SHA256(salt, input) and returns a typed ProjectHash.
// The result is a 64-character lowercase hex string (256-bit digest).
//
// Returns an error only if schema.NewProjectHash rejects the computed hex,
// which should never happen in practice (HMAC-SHA256 always produces 32 bytes).
func (s Salt) Hash(input string) (schema.ProjectHash, error) {
	mac := hmac.New(sha256.New, s[:])
	mac.Write([]byte(input))
	digest := mac.Sum(nil)
	hexStr := hex.EncodeToString(digest)

	ph, err := schema.NewProjectHash(hexStr)
	if err != nil {
		// This should be unreachable: HMAC-SHA256 always produces exactly 32 bytes →
		// 64 hex chars, which is exactly what NewProjectHash accepts.
		return "", fmt.Errorf(
			"salt.Hash: unexpected error constructing ProjectHash from HMAC-SHA256 digest %q: %w — "+
				"this is a bug; please report it",
			hexStr, err,
		)
	}
	return ph, nil
}

// generate creates a new cryptographically random Salt via crypto/rand.
func generate() (Salt, error) {
	var s Salt
	if _, err := rand.Read(s[:]); err != nil {
		return Salt{}, fmt.Errorf(
			"salt.generate: read 32 random bytes from crypto/rand: %w — "+
				"the OS random source may be unavailable (rare; retry or check /dev/urandom)",
			err,
		)
	}
	return s, nil
}
