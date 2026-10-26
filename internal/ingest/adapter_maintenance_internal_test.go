package ingest

import (
	_ "embed"
	"errors"
	"testing"
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

// TestForcedRepairWithAnUnnamedHarnessIsStillCounted holds the summary side of
// the tolerant repair scope: a repair the store could not name a harness for is
// still this run's failed session when a forced run cannot route it. The scope
// cases themselves live in testdata/run_scope.yaml; this test only checks that
// the verdict reaches the summary once.
func TestForcedRepairWithAnUnnamedHarnessIsStillCounted(t *testing.T) {
	harness := HarnessClaudeCode
	sid := SessionID("11111111-1111-4111-8111-111111111111")
	const source = "/synthetic/missing.jsonl"

	pipeline := &Pipeline{config: PipelineConfig{Force: true, Harness: &harness}}
	inScope := pipeline.pairRepairInScope(DiscoveredSession{SessionID: sid}, 0)
	pipeline.noteUnroutedForcedRepair(sid, "", source, inScope)
	failures := pipeline.appendUnroutedRepairFailures(nil)
	if len(failures) != 1 || failures[0].SessionID != sid || failures[0].Error == nil {
		t.Fatalf("the forced repair was not counted once: %+v", failures)
	}
}

// TestRoutePairRepairCountsAFailureOnlyInsideTheRunFilters pins where the
// run's filters are decided for an unroutable repair on the reindex path: the
// caller passes its own verdict from the shared scope test, and this function
// does not assume one. A repair outside the run's filters is still reported,
// but it is not counted as this run's failed session; one inside them is
// counted exactly once.
func TestRoutePairRepairCountsAFailureOnlyInsideTheRunFilters(t *testing.T) {
	sid := SessionID("11111111-1111-4111-8111-111111111111")
	target := reindexTarget{session: DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode}}

	pipeline := &Pipeline{config: PipelineConfig{Force: true}}
	routed := pipeline.routePairRepair(t.Context(), target, nil, map[SessionID]DiffEntry{}, map[SessionID]bool{}, false)
	if routed {
		t.Fatal("a repair with no reachable source was routed to native re-ingestion")
	}
	if failures := pipeline.appendUnroutedRepairFailures(nil); len(failures) != 0 {
		t.Fatalf("a repair outside the run's filters was counted as its failed session: %+v", failures)
	}

	pipeline = &Pipeline{config: PipelineConfig{Force: true}}
	if pipeline.routePairRepair(t.Context(), target, nil, map[SessionID]DiffEntry{}, map[SessionID]bool{}, true) {
		t.Fatal("a repair with no reachable source was routed to native re-ingestion")
	}
	failures := pipeline.appendUnroutedRepairFailures(nil)
	if len(failures) != 1 || failures[0].SessionID != sid || failures[0].Error == nil {
		t.Fatalf("a repair inside the run's filters was not counted once: %+v", failures)
	}
}
