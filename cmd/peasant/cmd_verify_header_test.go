package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

// TestVerifyHeaderComesFromTheRealSchema guards #682/#699: the verify header
// named a frozen "v1-v20" schema and a hand-kept list of 35 tables against
// the 100+ a fresh database holds, so a table missing from that list (such as
// session_generations) could be dropped without verify noticing.
func TestVerifyHeaderComesFromTheRealSchema(t *testing.T) {
	t.Parallel()
	seed := seedVerifyHarvest(t)
	out, err := seed.run(t, "verify")
	if err != nil {
		t.Fatalf("verify on a fresh harvest: %v\n%s", err, out)
	}
	if want := fmt.Sprintf("Expected schema: v%d", store.CurrentSchemaVersion()); !strings.Contains(out, want) {
		t.Errorf("verify header does not name the current schema (%q):\n%s", want, out)
	}
	names, err := store.ExpectedTableNames()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("Expected tables: %d", len(names)); !strings.Contains(out, want) || len(names) <= len(store.AllTableNames) {
		t.Errorf("verify header should count the %d tables of the real schema (%q):\n%s", len(names), want, out)
	}
}

// TestVerifyNamesATableMissingFromTheOldList is the behavioural half of the
// header fix: session_generations was absent from the hand-kept table list,
// so dropping it passed verify. It uses only the CLI, so it builds on the
// previous head and fails there at runtime.
func TestVerifyNamesATableMissingFromTheOldList(t *testing.T) {
	t.Parallel()
	seed := seedVerifyHarvest(t)
	seed.exec(t, `PRAGMA foreign_keys=OFF; DROP TABLE session_generations;`)
	out, err := seed.run(t, "verify")
	if err == nil || !strings.Contains(out, "session_generations - MISSING") {
		t.Errorf("verify with session_generations dropped: err = %v, want it named as MISSING:\n%s", err, out)
	}
}
