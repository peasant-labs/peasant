package ingest

import "testing"

func TestAggregateRecordKindRefusals(t *testing.T) {
	if out := AggregateRecordKindRefusals(nil); len(out) != 0 {
		t.Fatalf("empty refusals aggregate to %d rows, want 0", len(out))
	}
	out := AggregateRecordKindRefusals([]RecordKindRefusal{
		{Harness: HarnessStrike, Kind: "child.started"},
		{Harness: HarnessCodex, Kind: "world_state"},
		{Harness: HarnessCodex, Kind: "world_state"},
		{Harness: HarnessClaudeCode, Kind: "image"},
	})
	if len(out) != 3 {
		t.Fatalf("aggregate has %d rows, want 3", len(out))
	}
	first, second, third := out[0], out[1], out[2]
	if first.Harness != HarnessClaudeCode || first.Kind != "image" || first.Count != 1 {
		t.Errorf("first row is %+v, want claude-code/image x1", first)
	}
	if second.Harness != HarnessCodex || second.Kind != "world_state" || second.Count != 2 {
		t.Errorf("second row is %+v, want codex/world_state x2", second)
	}
	if third.Harness != HarnessStrike || third.Kind != "child.started" || third.Count != 1 {
		t.Errorf("third row is %+v, want strike/child.started x1", third)
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
	foundAttachment := false
	for _, row := range tracked {
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
		if entry.Status != RecordKindTrackedOnly || entry.Visualized == RecordKindRendered {
			t.Errorf("tracked row %q for harness %q is not stored-but-unshown", row.Kind, string(row.Harness))
		}
		if row.Harness == HarnessClaudeCode && row.Kind == "attachment" {
			foundAttachment = true
		}
	}
	if !foundAttachment {
		t.Error("tracked-not-visualized list omits claude-code/attachment")
	}
}
