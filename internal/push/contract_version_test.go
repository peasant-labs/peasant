package push_test

import (
	"testing"

	"github.com/peasant-labs/schema"
)

// wantVillageAPIVersion is the single-update-point expectation for the Village
// API contract generation exposed by Peasant's schema module pin. Bump it here
// only after reviewing both the current and retained compatibility surfaces.
const wantVillageAPIVersion = "0.13.0"

// TestPinnedContractVersion_MatchesExpected fails if the schema module peasant
// depends on reports a different Village API contract version than expected.
//
// Why a consumer-side pin: schema.VillageAPIVersion is a version marker that
// legitimately changes on every contract bump, so the schema module's own
// breaking-change gate exempts it. Peasant currently emits the retained legacy
// PublishRequest and validates it with schema.ValidatePublishRequest; the current schema
// keeps that validator frozen at Village 0.10.0 while exposing the authoritative
// 0.13.0 successor alongside it. This assertion therefore acknowledges the whole
// imported contract generation without falsely claiming Peasant emits the
// authoritative request. Village explicitly retains the legacy route.
func TestPinnedContractVersion_MatchesExpected(t *testing.T) {
	if schema.VillageAPIVersion != wantVillageAPIVersion {
		t.Fatalf(
			"VillageAPIVersion mismatch.\n"+
				"  what: schema.VillageAPIVersion = %q, want %q\n"+
				"  why:  peasant is pinned (go.mod: github.com/peasant-labs/schema) to a module\n"+
				"        generation that differs from what this consumer explicitly reviewed.\n"+
				"  when: the schema module's breaking-change gate deliberately exempts this\n"+
				"        moving version marker, so the consumer owns detecting drift.\n"+
				"  means: the imported current and compatibility contract surfaces changed without\n"+
				"        Peasant acknowledging their production and validation consequences.\n"+
				"  fix:  confirm the bump is intended; review legacy publish acceptance and current\n"+
				"        successor changes; re-check the SQLite license mirrors (V37/V38) and\n"+
				"        downstream surfaces; then update this expectation with the module pin.",
			schema.VillageAPIVersion, wantVillageAPIVersion)
	}
}
