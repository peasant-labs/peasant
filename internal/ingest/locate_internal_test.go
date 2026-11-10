package ingest

import (
	"slices"
	"sort"
	"testing"
)

// TestScopeAllowlistIsUpkeep pins the run gate to the report's upkeep set. A
// recorded member whose source could not be read again -- and a recorded id
// whose duplicate was refused -- is not processed, but its stored row still
// exists, so the store-side passes a full run performs for that row must still
// reach it. Shipping Processed() here instead leaves both out and fails.
func TestScopeAllowlistIsUpkeep(t *testing.T) {
	scope, err := NewSessionScope([]SessionID{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, RelatedInclude)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}
	p := &Pipeline{config: PipelineConfig{Scope: &scope}}
	report := ScopeReport{
		Members: []LocatedMember{
			{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Role: CohortSelected, Outcome: LocateStoredPath},
			{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Role: CohortSelected, Outcome: LocateStoredUnreadable, Unreadable: UnreadableSourceGone},
			{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Role: CohortChild, Outcome: LocateStoredPath},
			{ID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Role: CohortContext, Outcome: LocateStoredPath},
		},
		refusedUpkeep: []SessionID{"ffffffff-ffff-4fff-8fff-ffffffffffff"},
	}

	p.applyScopeReport(report)

	var allowed []SessionID
	for id := range p.config.AllowedSessionIDs {
		allowed = append(allowed, id)
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i] < allowed[j] })

	want := report.Upkeep()
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !slices.Equal(allowed, want) {
		t.Fatalf("AllowedSessionIDs = %v, want the upkeep set %v", allowed, want)
	}
	processed := report.Processed()
	if len(allowed) <= len(processed) {
		t.Fatalf("AllowedSessionIDs (%d) is not wider than Processed (%d); the upkeep-only members are missing", len(allowed), len(processed))
	}
	for _, id := range []SessionID{"cccccccc-cccc-4ccc-8ccc-cccccccccccc", "ffffffff-ffff-4fff-8fff-ffffffffffff"} {
		if !p.config.AllowedSessionIDs[id] {
			t.Errorf("upkeep-only id %s is missing from the run gate", id)
		}
	}
	if p.config.AllowedSessionIDs["eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"] {
		t.Errorf("a context member joined the run gate")
	}

	narrowed, ok := p.runStoreScope().Narrowed()
	if !ok {
		t.Fatalf("a scoped run's store scope is not narrowed")
	}
	sort.Slice(narrowed, func(i, j int) bool { return narrowed[i] < narrowed[j] })
	if !slices.Equal(narrowed, want) {
		t.Fatalf("runStoreScope() = %v, want the same upkeep set %v", narrowed, want)
	}
}
