package ingest

import (
	_ "embed"
	"testing"
)

//go:embed testdata/record_kinds_aggregate.yaml
var recordKindsAggregateYAML []byte

//go:embed testdata/record_kinds_tracked.yaml
var recordKindsTrackedYAML []byte

type recordKindsAggregateFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name     string `yaml:"name"`
		Refusals []struct {
			Harness string `yaml:"harness"`
			Kind    string `yaml:"kind"`
		} `yaml:"refusals"`
		Want []struct {
			Harness string `yaml:"harness"`
			Kind    string `yaml:"kind"`
			Count   int    `yaml:"count"`
		} `yaml:"want"`
	} `yaml:"cases"`
}

type recordKindsTrackedFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name       string            `yaml:"name"`
		Harness    string            `yaml:"harness"`
		Kind       string            `yaml:"kind"`
		Context    RecordKindContext `yaml:"context"`
		Namespace  string            `yaml:"namespace"`
		WantListed bool              `yaml:"want_listed"`
	} `yaml:"cases"`
}

func loadRecordKindsAggregateFixtures(t *testing.T) recordKindsAggregateFixture {
	t.Helper()
	var fixture recordKindsAggregateFixture
	decodeRegistryFixture(t, recordKindsAggregateYAML, &fixture)
	seen := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid record-kind aggregate fixture %q", row.Name)
		}
		seen[row.Name] = true
		if len(row.Refusals) == 0 || len(row.Want) == 0 {
			t.Fatalf("record-kind aggregate fixture %q is incomplete", row.Name)
		}
	}
	checkRegistryFixtureNames(t, seen, fixture.RequiredNames)
	return fixture
}

func loadRecordKindsTrackedFixtures(t *testing.T) recordKindsTrackedFixture {
	t.Helper()
	var fixture recordKindsTrackedFixture
	decodeRegistryFixture(t, recordKindsTrackedYAML, &fixture)
	seen := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid record-kind tracked fixture %q", row.Name)
		}
		seen[row.Name] = true
		if row.Harness == "" || row.Kind == "" {
			t.Fatalf("record-kind tracked fixture %q is incomplete", row.Name)
		}
	}
	checkRegistryFixtureNames(t, seen, fixture.RequiredNames)
	return fixture
}

func TestAggregateRecordKindRefusals(t *testing.T) {
	if out := AggregateRecordKindRefusals(nil); len(out) != 0 {
		t.Fatalf("empty refusals aggregate to %d rows, want 0", len(out))
	}
	for _, row := range loadRecordKindsAggregateFixtures(t).Cases {
		t.Run(row.Name, func(t *testing.T) {
			var refusals []RecordKindRefusal
			for _, refusal := range row.Refusals {
				harness := Harness(refusal.Harness)
				if !harness.IsKnown() {
					t.Fatalf("unknown harness %q", refusal.Harness)
				}
				refusals = append(refusals, RecordKindRefusal{Harness: harness, Kind: refusal.Kind})
			}
			got := AggregateRecordKindRefusals(refusals)
			if len(got) != len(row.Want) {
				t.Fatalf("aggregate has %d rows, want %d: %+v", len(got), len(row.Want), got)
			}
			for i, want := range row.Want {
				if string(got[i].Harness) != want.Harness || got[i].Kind != want.Kind || got[i].Count != want.Count {
					t.Errorf("row %d is %+v, want %s/%s x%d", i, got[i], want.Harness, want.Kind, want.Count)
				}
			}
		})
	}
}

func TestRecordKindsTrackedNotVisualized(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	tracked := registry.TrackedNotVisualized()
	if len(tracked) == 0 {
		t.Fatal("tracked-not-visualized list is empty; the registry stores kinds no renderer shows")
	}
	seen := make(map[RecordKindTracked]bool, len(tracked))
	for _, row := range tracked {
		if seen[row] {
			t.Errorf("tracked-not-visualized lists %s/%s twice", string(row.Harness), row.Kind)
		}
		seen[row] = true
		section, ok := registry.Harnesses[row.Harness]
		if !ok {
			t.Errorf("tracked row names unknown harness %q", string(row.Harness))
			continue
		}
		entry, ok := section.KindsByKey()[RecordKindKey{row.Context, row.Namespace, row.Kind, row.Match}]
		if !ok {
			t.Errorf("tracked row names unknown kind %q for harness %q", row.Kind, string(row.Harness))
			continue
		}
		stored := entry.Status == RecordKindRepresented || entry.Status == RecordKindTrackedOnly || entry.Status == RecordKindRetainedUnknown
		unshown := entry.Visualized == RecordKindHidden || entry.Visualized == RecordKindPlanned
		if !stored || !unshown {
			t.Errorf("tracked row %s/%s is not stored-but-unshown (status %q, visualized %q)",
				string(row.Harness), row.Kind, string(entry.Status), string(entry.Visualized))
		}
	}
	for harness, section := range registry.Harnesses {
		for _, kind := range section.Kinds {
			stored := kind.Status == RecordKindRepresented || kind.Status == RecordKindTrackedOnly || kind.Status == RecordKindRetainedUnknown
			unshown := kind.Visualized == RecordKindHidden || kind.Visualized == RecordKindPlanned
			if seen[RecordKindTracked{Harness: harness, Kind: kind.Kind, Context: kind.Context, Namespace: kind.Namespace, Match: kind.Match}] != (stored && unshown) {
				t.Errorf("exact report membership differs for %+v", kind.Key())
			}
		}
	}
	for _, row := range loadRecordKindsTrackedFixtures(t).Cases {
		t.Run(row.Name, func(t *testing.T) {
			harness := Harness(row.Harness)
			if !harness.IsKnown() {
				t.Fatalf("unknown harness %q", row.Harness)
			}
			member := seen[RecordKindTracked{Harness: harness, Kind: row.Kind, Context: row.Context, Namespace: row.Namespace, Match: RecordKindLiteral}]
			if member != row.WantListed {
				t.Errorf("tracked-not-visualized lists %s/%s = %v, want listed = %v",
					row.Harness, row.Kind, member, row.WantListed)
			}
		})
	}
}
