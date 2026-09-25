package ingest

import (
	_ "embed"
	"testing"
)

//go:embed testdata/record_kinds_aggregate.yaml
var recordKindsAggregateYAML []byte

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
