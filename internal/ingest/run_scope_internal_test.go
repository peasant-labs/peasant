package ingest

import (
	_ "embed"
	"testing"
	"time"
)

//go:embed testdata/run_scope.yaml
var runScopeFixtureData []byte

type runScopeFixture struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name           string   `yaml:"name"`
		Harness        string   `yaml:"harness"`
		SessionHarness string   `yaml:"session_harness"`
		StartMs        int64    `yaml:"start_ms"`
		CreatedAtMs    int64    `yaml:"created_at_ms"`
		ModTimeMs      int64    `yaml:"mod_time_ms"`
		SinceMs        int64    `yaml:"since_ms"`
		AllowedSession []string `yaml:"allowed_sessions"`
		WantInventory  bool     `yaml:"want_inventory"`
		WantRepair     bool     `yaml:"want_repair"`
	} `yaml:"cases"`
}

// TestRunScopeSeparatesTheInventoryFromTheRepair holds where the run's explicit
// filters apply and how far. The inventory is strict about the harness: a
// --harness run does not pick up a candidate whose harness is unnamed, which is
// exactly the widening that would make it read a session it did not ask for.
// The repair is tolerant about the same field only, because the database
// already selected that candidate as this run's work.
//
// The two share the session filter and the age window, and neither re-derives
// them: the run states the window once in withinRunAge. Only the repair resolves
// a start from the candidate it was handed, for a candidate with no metadata
// start, and the created time is preferred over the modification time.
func TestRunScopeSeparatesTheInventoryFromTheRepair(t *testing.T) {
	var fixtures runScopeFixture
	decodeOneFixture(t, runScopeFixtureData, &fixtures, "run scope fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate run scope fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
		if fixture.Harness != "" && !Harness(fixture.Harness).IsKnown() {
			t.Fatalf("fixture %s names unknown harness %q", fixture.Name, fixture.Harness)
		}
		if fixture.SessionHarness != "" && !Harness(fixture.SessionHarness).IsKnown() {
			t.Fatalf("fixture %s names unknown session harness %q", fixture.Name, fixture.SessionHarness)
		}
	}
	requireNamedCases(t, "run scope fixture", fixtures.Required, names)

	sid := SessionID("11111111-1111-4111-8111-111111111111")
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			config := PipelineConfig{}
			if fixture.Harness != "" {
				harness := Harness(fixture.Harness)
				config.Harness = &harness
			}
			if fixture.SinceMs != 0 {
				since := time.UnixMilli(fixture.SinceMs)
				config.Since = &since
			}
			if len(fixture.AllowedSession) > 0 {
				config.AllowedSessionIDs = make(map[SessionID]bool, len(fixture.AllowedSession))
				for _, allowed := range fixture.AllowedSession {
					config.AllowedSessionIDs[SessionID(allowed)] = true
				}
			}
			pipeline := &Pipeline{config: config}
			session := DiscoveredSession{SessionID: sid, Harness: Harness(fixture.SessionHarness)}
			if fixture.CreatedAtMs != 0 {
				session.CreatedAt = time.UnixMilli(fixture.CreatedAtMs)
			}
			if fixture.ModTimeMs != 0 {
				session.ModTime = time.UnixMilli(fixture.ModTimeMs)
			}
			target := reindexTarget{session: session, startMs: fixture.StartMs}
			if got := pipeline.includesIndexTarget(target); got != fixture.WantInventory {
				t.Fatalf("the index inventory includes the target = %v, want %v", got, fixture.WantInventory)
			}
			if got := pipeline.pairRepairInScope(session, fixture.StartMs); got != fixture.WantRepair {
				t.Fatalf("the pair repair is in scope = %v, want %v", got, fixture.WantRepair)
			}
		})
	}
}
