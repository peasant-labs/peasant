package store

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const migrateActivationStampGeneration = "gen_migrate_case"

// runMigrateActivationStampCase converts one file-backed session whose
// predecessor row carries the given activation literal ("NULL" or a value) and
// returns the migrated catalog row's installed and activation stamps.
func runMigrateActivationStampCase(t *testing.T, seedName, activatedLiteral string) (int64, *int64) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := migrateCaseSessionID(t, seedName, 0)
	seedGenerationSession(t, s, string(sid))
	seedMigrateProfile(t, s, root, sid, "clean", migrateActivationStampGeneration)
	execMigrateSQL(t, s, `UPDATE session_projection_generations SET installed_at_ms = 4242, activated_at_ms = `+activatedLiteral+` WHERE session_id = '`+string(sid)+`'`)

	outcome, err := s.MigrateSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("MigrateSession: %v", err)
	}
	if outcome != MigrateOutcomeConverted {
		t.Fatalf("outcome = %q, want converted", outcome)
	}

	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var installed int64
	var activated *int64
	found := false
	if err := sqlitex.Execute(conn, `SELECT installed_at_ms, activated_at_ms FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), migrateActivationStampGeneration},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			installed = stmt.ColumnInt64(0)
			if stmt.ColumnType(1) != sqlite.TypeNull {
				at := stmt.ColumnInt64(1)
				activated = &at
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read the migrated activation stamps: %v", err)
	}
	if !found {
		t.Fatalf("no migrated catalog row for generation %s", migrateActivationStampGeneration)
	}
	return installed, activated
}

// requireActivationStamp asserts the migrated stamps, naming a NULL activation
// explicitly rather than printing a pointer address.
func requireActivationStamp(t *testing.T, installed int64, activated *int64, wantInstalled, wantActivated int64) {
	t.Helper()
	if installed != wantInstalled {
		t.Fatalf("installed_at_ms = %d, want %d", installed, wantInstalled)
	}
	if activated == nil {
		t.Fatalf("activated_at_ms = NULL, want %d", wantActivated)
	}
	if *activated != wantActivated {
		t.Fatalf("activated_at_ms = %d, want %d", *activated, wantActivated)
	}
}

// TestMigrateActivationStamp pins both branches of the migration's activation
// stamping: the migration installs the ACTIVE generation, so a predecessor row
// that recorded an activation time carries it, and one that recorded none falls
// back to the install instant.
func TestMigrateActivationStamp(t *testing.T) {
	t.Run("null-predecessor-falls-back-to-installed", func(t *testing.T) {
		installed, activated := runMigrateActivationStampCase(t, "null-predecessor-falls-back-to-installed", "NULL")
		requireActivationStamp(t, installed, activated, 4242, 4242)
	})
	t.Run("non-null-predecessor-carries-source-stamp", func(t *testing.T) {
		installed, activated := runMigrateActivationStampCase(t, "non-null-predecessor-carries-source-stamp", "777")
		requireActivationStamp(t, installed, activated, 4242, 777)
	})
}
