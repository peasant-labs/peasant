package store

// V45 CHECK-bijection tests: the two-table origin column addition.
//
// This file is package `store` (internal, matching migration_v37_check_test.go
// and migration_v33_test.go), because it executes the unexported
// `migrationV45` script directly against a hand-built table, and the parser
// below has to select its clause by TABLE NAME first — a naive substring
// search for the shorter marker `"origin IN ("` would match inside
// `"session_origin IN ("` first, silently reading the wrong table's clause
// while claiming to test the other one. The fixture-driven round trip and
// rejection tests live in migration_v45_test.go (package store_test), which
// needs testutil — importing testutil from THIS package would be a real
// import cycle (testutil imports store for its mocks).

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// originCheckLiteralsForTable extracts the accepted string literals from the
// `... IN (...)` CHECK that migrationV45 adds to the named table.
//
// It selects its statement by TABLE NAME FIRST: split the script on `;`, keep
// only the statement(s) that both alter the named table and declare an
// `IN (` clause, and require there to be exactly one. Only THEN does it parse
// the literal list within that one statement. This ordering is the whole
// point (review B, BLOCKER, carried into this slice): migrationV45 puts the
// sessions clause before the evidence clause, and the evidence table's own
// column is named plain `origin` — the shorter name that
// `"session_origin IN ("` also ends with. A parser that searched for
// `"origin IN ("` as a bare substring, the way licenseCheckLiterals
// (migration_v37_check_test.go) searches for `"license_id IN ("`, would match
// inside the sessions clause first and silently parse the WRONG table,
// whichever table it claimed to be testing.
//
// The empty-string literal `”` is kept as a real set member (key ""), never
// discarded as a parse artifact: the evidence table's CHECK legitimately
// accepts it, and dropping it would make the "both directions" bijection
// assertion pass even if that member went missing from the CHECK.
func originCheckLiteralsForTable(t *testing.T, script, table string) map[string]bool {
	t.Helper()
	tableMarker := "ALTER TABLE " + table
	var matches []string
	for _, stmt := range strings.Split(script, ";") {
		if strings.Contains(stmt, tableMarker) && strings.Contains(stmt, "IN (") {
			matches = append(matches, stmt)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %q statement carrying an IN (...) CHECK, found %d in:\n%s",
			tableMarker, len(matches), script)
	}
	stmt := matches[0]

	const marker = "IN ("
	i := strings.Index(stmt, marker)
	if i < 0 {
		t.Fatalf("statement for %q has no IN (...) clause to parse:\n%s", table, stmt)
	}
	rest := stmt[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("statement for %q has an unterminated IN (...) clause:\n%s", table, stmt)
	}

	set := map[string]bool{}
	for _, part := range strings.Split(rest[:j], ",") {
		lit := strings.Trim(strings.TrimSpace(part), "'")
		set[lit] = true
	}
	return set
}

// naiveSubstringOriginLiterals reproduces the REJECTED approach exactly:
// licenseCheckLiterals's own marker strategy (bare `strings.Index` for
// `"origin IN ("`), with no table selection at all. It exists only so the
// baseline test below can show it lands on the WRONG clause when asked to
// read the evidence table's CHECK — the concrete failure mode the
// table-name-first parser above is required to avoid.
func naiveSubstringOriginLiterals(t *testing.T, script string) map[string]bool {
	t.Helper()
	const marker = "origin IN ("
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("naive parser found no %q marker in:\n%s", marker, script)
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("naive parser found an unterminated IN (...) clause in:\n%s", script)
	}
	set := map[string]bool{}
	for _, part := range strings.Split(rest[:j], ",") {
		lit := strings.Trim(strings.TrimSpace(part), "'")
		set[lit] = true
	}
	return set
}

func sessionOriginMenuSet() map[string]bool {
	set := make(map[string]bool, len(sessionorigin.All))
	for _, origin := range sessionorigin.All {
		set[origin.String()] = true
	}
	return set
}

func evidenceOriginMenuSet() map[string]bool {
	set := sessionOriginMenuSet()
	set[""] = true
	return set
}

func assertOriginSetsEqual(t *testing.T, label string, got, want map[string]bool) {
	t.Helper()
	for lit := range got {
		if !want[lit] {
			t.Errorf("%s: CHECK accepts %q which is NOT in the expected menu — the mirror carries a value the menu dropped", label, lit)
		}
	}
	for lit := range want {
		if !got[lit] {
			t.Errorf("%s: expected menu contains %q which the CHECK does NOT accept — widen the CHECK to match", label, lit)
		}
	}
}

// TestOriginCheckParserIsBaselinedAgainstAMismatchedScript feeds the parser a
// script whose two clauses DIFFER (migrationV45 itself: sessions accepts three
// literals, claude_transcript_evidence accepts those three plus the empty
// string) and asserts it returns the right, DIFFERENT set for each table
// before either set is trusted by the tests below.
//
// It also runs the naive, table-blind substring parser over the same script
// and shows it returns the SESSIONS clause even when asked to read the
// EVIDENCE table's CHECK — the concrete trap a bare `"origin IN ("` marker
// falls into, and the reason this parser selects by table name first.
func TestOriginCheckParserIsBaselinedAgainstAMismatchedScript(t *testing.T) {
	t.Parallel()

	sessionsSet := originCheckLiteralsForTable(t, migrationV45, "sessions")
	evidenceSet := originCheckLiteralsForTable(t, migrationV45, "claude_transcript_evidence")

	wantSessions := sessionOriginMenuSet()
	wantEvidence := evidenceOriginMenuSet()
	assertOriginSetsEqual(t, "sessions.session_origin", sessionsSet, wantSessions)
	assertOriginSetsEqual(t, "claude_transcript_evidence.origin", evidenceSet, wantEvidence)

	if len(sessionsSet) == len(evidenceSet) {
		t.Fatalf("baseline is vacuous: the two clauses must differ (evidence admits the empty marker, sessions does not), but both parsed to %d literals", len(sessionsSet))
	}
	if sessionsSet[""] {
		t.Fatal("sessions.session_origin's CHECK must not admit the empty string; the parser or the migration script is wrong")
	}
	if !evidenceSet[""] {
		t.Fatal("claude_transcript_evidence.origin's CHECK must admit the empty string; the parser or the migration script is wrong")
	}

	// The naive, table-blind parser lands on the SESSIONS clause (no empty
	// member) even though this call asks it to stand in for the EVIDENCE
	// table's CHECK. Its result therefore disagrees with the real evidence
	// set, which is exactly the silent-wrong-table failure this file's
	// parser must not repeat.
	naive := naiveSubstringOriginLiterals(t, migrationV45)
	if naive[""] {
		t.Fatal("expected the naive parser to demonstrate the trap by landing on the sessions clause (no empty member); it read the evidence clause instead, so this baseline no longer proves anything")
	}
	for lit := range naive {
		if !sessionsSet[lit] {
			t.Fatalf("expected the naive parser's result to equal the sessions clause (demonstrating it read the wrong table); got an unexpected literal %q", lit)
		}
	}
}

// TestMigrationV45SessionOriginCheckBijective proves sessions.session_origin's
// CHECK accepts exactly sessionorigin.All — no more (a value the Go menu
// dropped), no less (a value the Go menu grew to include).
func TestMigrationV45SessionOriginCheckBijective(t *testing.T) {
	t.Parallel()
	assertOriginSetsEqual(t, "sessions.session_origin",
		originCheckLiteralsForTable(t, migrationV45, "sessions"),
		sessionOriginMenuSet())
}

// TestMigrationV45EvidenceOriginCheckBijective proves
// claude_transcript_evidence.origin's CHECK accepts exactly sessionorigin.All
// plus the empty "record predates the field" marker — no more, no less.
func TestMigrationV45EvidenceOriginCheckBijective(t *testing.T) {
	t.Parallel()
	assertOriginSetsEqual(t, "claude_transcript_evidence.origin",
		originCheckLiteralsForTable(t, migrationV45, "claude_transcript_evidence"),
		evidenceOriginMenuSet())
}

func execV45(t *testing.T, conn *sqlite.Conn, sql string) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// newV45Conn builds a bare in-memory connection carrying only sessions and
// claude_transcript_evidence, with migrationV45 applied on top of their base
// (V1 / V44) shapes. Foreign keys are off, matching migration_v37_check_test.go:
// the base sessions DDL declares FKs to host_slugs/projects this test does not
// create.
func newV45Conn(t *testing.T) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close conn: %v", err)
		}
	})
	execV45(t, conn, "PRAGMA foreign_keys = OFF")
	execV45(t, conn, createSessions)
	if err := sqlitex.ExecuteScript(conn, migrationV44, nil); err != nil {
		t.Fatalf("apply migrationV44: %v", err)
	}
	if err := sqlitex.ExecuteScript(conn, migrationV45, nil); err != nil {
		t.Fatalf("apply migrationV45: %v", err)
	}
	return conn
}

// TestMigrationV45SessionOriginColumnAcceptsTheMenu is a direct-SQL sanity
// check that newV45Conn's sessions table actually carries the migrated
// column (belt-and-suspenders alongside the fixture-driven round trip in
// migration_v45_test.go, which exercises this same table through the real
// Store instead of a bare connection).
func TestMigrationV45SessionOriginColumnAcceptsTheMenu(t *testing.T) {
	t.Parallel()
	conn := newV45Conn(t)
	const insertSession = `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, session_origin)
		VALUES (?, 'claude-code', 'm', 'h', 'p', 1, 2, 3, '/x.jsonl', 'jsonl', ?)`
	for i, origin := range sessionorigin.All {
		if err := sqlitex.ExecuteTransient(conn, insertSession, &sqlitex.ExecOptions{
			Args: []any{"check-" + origin.String(), origin.String()},
		}); err != nil {
			t.Errorf("origin %d %q should be accepted, got: %v", i, origin, err)
		}
	}
}
