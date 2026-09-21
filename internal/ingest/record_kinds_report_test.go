package ingest

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
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
	if err := yaml.Unmarshal(recordKindsAggregateYAML, &fixture); err != nil {
		t.Fatal(err)
	}
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
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing record-kind aggregate fixture %q", name)
		}
	}
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
		entry, ok := section.KindsByName()[row.Kind]
		if !ok {
			t.Errorf("tracked row names unknown kind %q for harness %q", row.Kind, string(row.Harness))
			continue
		}
		stored := entry.Status == RecordKindRepresented || entry.Status == RecordKindTrackedOnly
		unshown := entry.Visualized == RecordKindHidden || entry.Visualized == RecordKindPlanned
		if !stored || !unshown {
			t.Errorf("tracked row %s/%s is not stored-but-unshown (status %q, visualized %q)",
				string(row.Harness), row.Kind, string(entry.Status), string(entry.Visualized))
		}
	}
	// The list must span both treatments: a tracked-only hidden kind and a
	// represented planned kind. Either half missing means the predicate
	// silently narrowed.
	if !seen[RecordKindTracked{Harness: HarnessClaudeCode, Kind: "attachment"}] {
		t.Error("tracked-not-visualized list omits claude-code/attachment")
	}
	if !seen[RecordKindTracked{Harness: HarnessClaudeCode, Kind: "compact_boundary"}] {
		t.Error("tracked-not-visualized list omits claude-code/compact_boundary")
	}
	// Rendered, ignored, and refused kinds must never appear: they are shown,
	// accounted without an entry, or refused.
	for _, absent := range []RecordKindTracked{
		{Harness: HarnessClaudeCode, Kind: "user"},
		{Harness: HarnessClaudeCode, Kind: "last-prompt"},
		{Harness: HarnessClaudeCode, Kind: "image"},
		{Harness: HarnessCodex, Kind: "event_msg"},
	} {
		if seen[absent] {
			t.Errorf("tracked-not-visualized lists %s/%s, which is shown or entryless",
				string(absent.Harness), absent.Kind)
		}
	}
}
