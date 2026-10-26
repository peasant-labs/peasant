package ingest

import (
	_ "embed"
	"errors"
	"testing"
	"time"
)

//go:embed testdata/unrouted_repair_failures.yaml
var unroutedRepairFailuresFixtureData []byte

// repairTestSession names one session in the append fixture. The labels keep the
// fixture readable; the function under test never validates an id.
func repairTestSession(label string) SessionID { return SessionID(label) }

// TestAppendUnroutedRepairFailuresKeepsOneRowPerSession pins the summary rule:
// a forced repair that never entered the process path is counted once, and a
// session the results already carry is left as the process path recorded it —
// whether that row failed or settled. The guards are exercised directly because
// no pipeline run reaches them twice for one session.
func TestAppendUnroutedRepairFailuresKeepsOneRowPerSession(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name    string   `yaml:"name"`
			Pending []string `yaml:"pending"`
			Results []struct {
				Session string `yaml:"session"`
				Failed  bool   `yaml:"failed"`
			} `yaml:"results"`
			WantAppended []string `yaml:"want_appended"`
		} `yaml:"cases"`
	}
	decodeOneFixture(t, unroutedRepairFailuresFixtureData, &fixtures, "unrouted repair failures fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate unrouted repair fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	requireNamedCases(t, "unrouted repair failures fixture", fixtures.Required, names)

	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			pipeline := &Pipeline{}
			for _, label := range fixture.Pending {
				sid := repairTestSession(label)
				pipeline.unroutedRepairs = append(pipeline.unroutedRepairs, SessionResult{
					SessionID: sid, Status: DiffUpdated, Error: unroutedRepairError(sid, "/synthetic/source.jsonl"),
				})
			}
			var results []SessionResult
			for _, row := range fixture.Results {
				result := SessionResult{SessionID: repairTestSession(row.Session), Status: DiffUnchanged}
				if row.Failed {
					result.Error = errors.New("the process path failed this session")
				}
				results = append(results, result)
			}
			existing := append([]SessionResult(nil), results...)

			got := pipeline.appendUnroutedRepairFailures(results)
			if len(got) < len(existing) {
				t.Fatalf("append dropped existing results: %d of %d left", len(got), len(existing))
			}
			for i, want := range existing {
				if got[i].SessionID != want.SessionID || (got[i].Error != nil) != (want.Error != nil) {
					t.Fatalf("append changed existing result %d: got %+v, want %+v", i, got[i], want)
				}
			}
			appended := got[len(existing):]
			if len(appended) != len(fixture.WantAppended) {
				t.Fatalf("appended %d rows %+v, want %d %v", len(appended), appended, len(fixture.WantAppended), fixture.WantAppended)
			}
			for i, want := range fixture.WantAppended {
				if appended[i].SessionID != repairTestSession(want) {
					t.Fatalf("appended row %d is %s, want %s", i, appended[i].SessionID, want)
				}
				if appended[i].Error == nil {
					t.Fatalf("appended row %d carries no failure: %+v", i, appended[i])
				}
			}
		})
	}
}

// TestRepairScopeKeepsARepairWhoseHarnessIsUnnamed holds the scope a forced
// repair is counted under. A repair that reached the selection is already the
// run's work, so a --harness run cannot drop it for a harness the store could
// not name; a named harness, an explicit session list and an age window still
// exclude what they name.
func TestRepairScopeKeepsARepairWhoseHarnessIsUnnamed(t *testing.T) {
	harness := HarnessClaudeCode
	sid := SessionID("11111111-1111-4111-8111-111111111111")
	const source = "/synthetic/missing.jsonl"

	t.Run("an unnamed harness stays in a harness run", func(t *testing.T) {
		pipeline := &Pipeline{config: PipelineConfig{Harness: &harness}}
		if !pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid}, 0) {
			t.Fatal("a repair the store could not name a harness for was dropped by a --harness run")
		}
	})

	t.Run("a named harness the run did not ask for is excluded", func(t *testing.T) {
		other := HarnessCodex
		pipeline := &Pipeline{config: PipelineConfig{Harness: &other}}
		if pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid, Harness: harness}, 0) {
			t.Fatal("a repair for another harness survived a --harness run")
		}
	})

	t.Run("a session filter excludes a repair outside it", func(t *testing.T) {
		pipeline := &Pipeline{config: PipelineConfig{AllowedSessionIDs: map[SessionID]bool{"another-session": true}}}
		if pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid, Harness: harness}, 0) {
			t.Fatal("a repair outside the explicit session list was counted")
		}
	})

	t.Run("an age filter excludes a repair with no start", func(t *testing.T) {
		since := time.UnixMilli(1748736000000)
		pipeline := &Pipeline{config: PipelineConfig{Since: &since}}
		if pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid, Harness: harness}, 0) {
			t.Fatal("a repair with no known start was counted inside an age window")
		}
	})

	t.Run("a forced repair with an unnamed harness is still counted", func(t *testing.T) {
		pipeline := &Pipeline{config: PipelineConfig{Force: true, Harness: &harness}}
		inScope := pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid}, 0)
		pipeline.noteUnroutedForcedRepair(sid, "", source, inScope)
		failures := pipeline.appendUnroutedRepairFailures(nil)
		if len(failures) != 1 || failures[0].SessionID != sid || failures[0].Error == nil {
			t.Fatalf("the forced repair was not counted once: %+v", failures)
		}
	})
}
