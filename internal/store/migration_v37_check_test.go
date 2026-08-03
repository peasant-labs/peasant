package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// licenseCheckLiterals extracts the accepted string literals from a migration
// script's `license_id IN ( ... )` CHECK constraint. It lets a test assert the
// SQLite mirror stays in EXACT (bijective) lockstep with schema.AllLicenses,
// catching not only a leaf menu that GREW beyond the CHECK but also a CHECK that
// still carries a license the leaf menu DROPPED.
func licenseCheckLiterals(t *testing.T, script string) map[string]bool {
	t.Helper()
	const marker = "license_id IN ("
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("migration script has no `license_id IN (...)` CHECK to parse:\n%s", script)
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("migration script `license_id IN (` is not closed with `)`:\n%s", script)
	}
	set := map[string]bool{}
	for _, part := range strings.Split(rest[:j], ",") {
		lit := strings.Trim(strings.TrimSpace(part), "'")
		if lit != "" {
			set[lit] = true
		}
	}
	if len(set) == 0 {
		t.Fatalf("migration script `license_id IN (...)` parsed no literals:\n%s", script)
	}
	return set
}

// assertLicenseCheckBijective asserts the accepted-literal set of a license_id
// CHECK equals schema.AllLicenses EXACTLY (both directions). The behavioural
// accept-loop in each test already proves AllLicenses ⊆ CHECK; this proves
// CHECK ⊆ AllLicenses too, so a stale mirror carrying a dropped license also
// fails. Widening/narrowing the menu therefore requires a table-rebuild
// migration (SQLite cannot ALTER a CHECK) before this goes green again.
func assertLicenseCheckBijective(t *testing.T, migrationScript string) {
	t.Helper()
	checkSet := licenseCheckLiterals(t, migrationScript)
	menuSet := map[string]bool{}
	for _, l := range schema.AllLicenses {
		menuSet[string(l)] = true
	}
	for lit := range checkSet {
		if !menuSet[lit] {
			t.Errorf("license_id CHECK accepts %q which is NOT in schema.AllLicenses — "+
				"the SQLite mirror carries a license the leaf menu dropped; rebuild the CHECK to match schema.AllLicenses", lit)
		}
	}
	for lit := range menuSet {
		if !checkSet[lit] {
			t.Errorf("schema.AllLicenses contains %q which the license_id CHECK does NOT accept — "+
				"widen the CHECK (table rebuild) to match schema.AllLicenses", lit)
		}
	}
}

// TestMigrationV37LicenseCheck verifies the license_id CHECK that migrationV37
// adds to sessions: the three menu ids and NULL (unset/legacy) are accepted, and
// any other value — including a near-miss case/whitespace variant or a software
// license like MIT — is rejected. This mirrors how source_format/model_harness
// guard their closed sets, and keeps the local mirror in lockstep with the
// village licenses.id menu (village migration 026) so drift is caught at write
// time rather than at the publish boundary.
//
// Standalone in-memory conn with FKs OFF (the base sessions DDL declares FKs to
// host_slugs/projects we don't create here) — the same approach as
// TestMigrationV33DataRename, isolating the CHECK from unrelated constraints.
func TestMigrationV37LicenseCheck(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer conn.Close()

	execT(t, conn, "PRAGMA foreign_keys = OFF")
	execT(t, conn, createSessions)
	// migrationV37 is a whitespace-wrapped script const; apply it via ExecuteScript
	// (matching TestMigrationV33DataRename) — execT is single-statement only.
	if err := sqlitex.ExecuteScript(conn, migrationV37, nil); err != nil {
		t.Fatalf("apply migrationV37: %v", err)
	}

	insertLicense := func(sessionID, licenseExpr string) error {
		return sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
			(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, license_id)
			VALUES ('`+sessionID+`','claude-code','m','h','p',1,2,3,'/x.jsonl','jsonl',`+licenseExpr+`)`, nil)
	}

	// The canonical menu (schema.AllLicenses) plus NULL (unset/legacy) are accepted.
	// Deriving the accept set from schema.AllLicenses (github.com/peasant-labs/schema)
	// binds this SQL mirror to the single
	// source of truth: add a license to schema.AllLicenses and this test exercises
	// it here, failing until a new migration widens the V37 CHECK to match. NULL
	// passes because `NULL IN (...)` evaluates to NULL, which SQLite treats as
	// satisfying a CHECK — so the column stays nullable without an explicit `IS NULL OR`.
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
	// software license (MIT), a lowercase near-miss (case-sensitive), and empty.
	reject := []string{`'MIT'`, `'GPL-3.0'`, `'cc0-1.0'`, `''`}
	for i, expr := range reject {
		if err := insertLicense(fmt.Sprintf("bad-%d", i), expr); err == nil {
			t.Errorf("expected license_id CHECK to reject %s, but insert succeeded", expr)
		}
	}

	// Bijective mirror: the CHECK's accepted-literal set must equal
	// schema.AllLicenses EXACTLY (not merely ⊇ it), so a leaf that DROPS a
	// license the CHECK still carries is caught, not silently accepted.
	assertLicenseCheckBijective(t, migrationV37)
}
