package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

func TestCompareRequiresAffectedVersion(t *testing.T) {
	data, err := os.ReadFile("../../internal/ingest/testdata/strike_protocol.meta.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.ReplaceAll(data, []byte("canonical sidecar title"), []byte("changed title"))
	if bytes.Equal(data, changed) {
		t.Fatal("existing Strike fixture no longer exercises a title change")
	}
	name := "strike/AdapterVersion/strike_protocol"
	base := snapshot{Versions: map[string]versions{"strike": {AdapterVersion: 1, IndexerVersion: 16}}, Outputs: map[string]json.RawMessage{name: data}}
	if failures := compare(base, base); len(failures) != 0 {
		t.Fatalf("unchanged behavior failed: %v", failures)
	}
	candidate := snapshot{Versions: map[string]versions{"strike": {AdapterVersion: 1, IndexerVersion: 17}}, Outputs: map[string]json.RawMessage{name: changed}}
	if failures := compare(base, candidate); len(failures) != 1 {
		t.Fatalf("missing adapter bump was not refused: %v", failures)
	}
	candidate.Versions["strike"] = versions{AdapterVersion: 2, IndexerVersion: 17}
	if failures := compare(base, candidate); len(failures) != 0 {
		t.Fatalf("affected adapter bump failed: %v", failures)
	}
}

func TestNewHarnessNeedsNoInventedHistoricalOutput(t *testing.T) {
	base := snapshot{Versions: map[string]versions{}, Outputs: map[string]json.RawMessage{}}
	name := "pi/IndexerVersion/native/header-only"
	candidate := snapshot{
		Versions: map[string]versions{"pi": {AdapterVersion: 1, IndexerVersion: 16}},
		Outputs:  map[string]json.RawMessage{name: json.RawMessage(`{"Failed":false}`)},
	}
	if failures := compare(base, candidate); len(failures) != 0 {
		t.Fatalf("new harness requires invented historical observations: %v", failures)
	}
	if failures := compare(candidate, base); len(failures) == 0 {
		t.Fatal("removing an observed harness was silently accepted")
	}
}

func TestEmbeddedProbeIsValidGo(t *testing.T) {
	if _, err := parser.ParseFile(token.NewFileSet(), "harvester_guard_probe_test.go", probe, parser.AllErrors); err != nil {
		t.Fatalf("archived-revision probe has invalid Go syntax: %v", err)
	}
}
