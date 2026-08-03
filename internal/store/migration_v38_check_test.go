package store

import (
	"fmt"
	"testing"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV38LicenseCheck verifies the license_id CHECK that migrationV38
// adds to pulled_transcripts: the menu ids and NULL (village sent no license)
// are accepted, and any other value is rejected — the same closed-set guard
// V37 gives the push-side sessions table (migration_v37_check_test.go). With
// V38 there are TWO local CHECK mirrors of the village licenses.id menu
// (sessions + pulled_transcripts); widening the menu means rebuilding BOTH
// tables in one future migration (SQLite cannot ALTER a CHECK).
//
// Standalone in-memory conn (pulled_transcripts has no FKs by design — V34);
// the accept set derives from schema.AllLicenses so a future menu addition
// fails this test until the widening migration lands.
func TestMigrationV38LicenseCheck(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer conn.Close()

	// migrationV34 (creates pulled_transcripts + pulled_annotations) and
	// migrationV38 are whitespace-wrapped script consts; apply via
	// ExecuteScript (matching TestMigrationV37LicenseCheck).
	if err := sqlitex.ExecuteScript(conn, migrationV34, nil); err != nil {
		t.Fatalf("apply migrationV34: %v", err)
	}
	if err := sqlitex.ExecuteScript(conn, migrationV38, nil); err != nil {
		t.Fatalf("apply migrationV38: %v", err)
	}

	insertLicense := func(transcriptID, licenseExpr string) error {
		return sqlitex.ExecuteTransient(conn, `INSERT INTO pulled_transcripts
			(village_host, transcript_id, owner_user_id, owner_username, content_hash, visibility, pull_dir, first_pulled_at, last_pulled_at, license_id)
			VALUES ('village.test','`+transcriptID+`','u1','@owner','hash','public','/pulls/x',1,1,`+licenseExpr+`)`, nil)
	}

	// The canonical menu (schema.AllLicenses) plus NULL are accepted. NULL
	// passes because `NULL IN (...)` evaluates to NULL, which SQLite treats as
	// satisfying a CHECK — the column stays nullable without an `IS NULL OR`.
	accept := []string{`NULL`}
	for _, l := range schema.AllLicenses {
		accept = append(accept, fmt.Sprintf("'%s'", l))
	}
	for i, expr := range accept {
		if err := insertLicense(fmt.Sprintf("ok-%d", i), expr); err != nil {
			t.Errorf("license_id %s should be accepted, got: %v", expr, err)
		}
	}

	// Anything outside the closed menu is rejected by the CHECK — including a
	// software license (MIT), a lowercase near-miss (case-sensitive), and the
	// empty string (the ""⇒NULL bind convention exists precisely because a
	// bound "" would violate this CHECK).
	reject := []string{`'MIT'`, `'GPL-3.0'`, `'cc0-1.0'`, `''`}
	for i, expr := range reject {
		if err := insertLicense(fmt.Sprintf("bad-%d", i), expr); err == nil {
			t.Errorf("expected license_id CHECK to reject %s, but insert succeeded", expr)
		}
	}

	// Bijective mirror: the pulled_transcripts CHECK's accepted-literal set must
	// equal schema.AllLicenses EXACTLY (not merely ⊇ it), so a leaf that DROPS a
	// license the CHECK still carries is caught, not silently accepted.
	assertLicenseCheckBijective(t, migrationV38)
}
