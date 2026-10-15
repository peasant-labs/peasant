package store

// V66 CHECK-bijection test: sessions.thinking_level's CHECK must accept exactly
// schema.AllThinkingLevels. SQLite cannot ALTER a CHECK, so when the canonical
// set changes this test fails until a table-rebuild migration lands.

import (
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
)

// thinkingLevelCheckLiterals parses the literal list of the single statement in
// script that adds thinking_level with an IN (...) CHECK. It selects the
// statement by its column marker first so the raw column (no CHECK) can never be
// mistaken for it.
func thinkingLevelCheckLiterals(t *testing.T, script string) map[string]bool {
	t.Helper()
	var matches []string
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.Join(strings.Fields(stmt), " ")
		if strings.Contains(stmt, "ADD COLUMN thinking_level TEXT") && strings.Contains(stmt, "IN (") {
			matches = append(matches, stmt)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one thinking_level statement carrying an IN (...) CHECK, found %d in:\n%s", len(matches), script)
	}
	stmt := matches[0]
	rest := stmt[strings.Index(stmt, "IN (")+len("IN ("):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("thinking_level CHECK has an unterminated IN (...) clause:\n%s", stmt)
	}
	set := map[string]bool{}
	for _, part := range strings.Split(rest[:end], ",") {
		set[strings.Trim(strings.TrimSpace(part), "'")] = true
	}
	return set
}

func TestMigrationV66ThinkingLevelCheckBijective(t *testing.T) {
	t.Parallel()
	got := thinkingLevelCheckLiterals(t, migrationV66)
	want := make(map[string]bool, len(schema.AllThinkingLevels))
	for _, level := range schema.AllThinkingLevels {
		want[string(level)] = true
	}
	for lit := range got {
		if !want[lit] {
			t.Errorf("sessions.thinking_level CHECK accepts %q which is NOT in schema.AllThinkingLevels; add a rebuild migration that drops it", lit)
		}
	}
	for lit := range want {
		if !got[lit] {
			t.Errorf("schema.AllThinkingLevels contains %q which the sessions.thinking_level CHECK does NOT accept; add a rebuild migration that widens the CHECK", lit)
		}
	}
}

// TestMigrationV66RawColumnHasNoCheck guards that the raw spelling column stays
// unconstrained: its bound lives at ingest and in the wire contract.
func TestMigrationV66RawColumnHasNoCheck(t *testing.T) {
	t.Parallel()
	for _, stmt := range strings.Split(migrationV66, ";") {
		if strings.Contains(stmt, "thinking_level_raw") && strings.Contains(stmt, "CHECK") {
			t.Fatalf("sessions.thinking_level_raw must not carry a CHECK:\n%s", stmt)
		}
	}
}
