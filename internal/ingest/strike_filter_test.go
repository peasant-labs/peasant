package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/strike_record_sizes.yaml
var strikeRecordSizeFixtureData []byte

const strikeRecordSizeFixturePath = "internal/ingest/testdata/strike_record_sizes.yaml"

type strikeRecordSizeFixtures struct {
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []strikeRecordSizeFixture `yaml:"cases"`
}

type strikeRecordSizeFixture struct {
	Name      string `yaml:"name"`
	SizeDelta int    `yaml:"sizeDelta"`
	Omitted   bool   `yaml:"omitted"`
}

func TestFilterStrikeOversizedRecordsBoundary(t *testing.T) {
	var fixtures strikeRecordSizeFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(strikeRecordSizeFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", strikeRecordSizeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", strikeRecordSizeFixturePath, err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("missing/duplicate boundary fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required boundary fixture %q missing", name)
		}
	}

	laterRecord := []byte("{\"type\":\"session.titled\",\"data\":{\"title\":\"retained\"}}\n")
	for _, fixture := range fixtures.Cases {
		recordSize := defaults.ScannerMaxLine + fixture.SizeDelta
		input := append(bytes.Repeat([]byte{'x'}, recordSize), '\n')
		input = append(input, laterRecord...)
		filtered, diagnostics := filterStrikeOversizedRecords(input, fixture.Name+".jsonl")
		if fixture.Omitted {
			if len(diagnostics) != 1 || diagnostics[0].ErrorType != "record_too_large" {
				t.Errorf("%s diagnostics = %+v, want one oversized-record warning", fixture.Name, diagnostics)
			}
			if !bytes.Equal(filtered, laterRecord) {
				t.Errorf("%s filtered bytes did not retain the later record", fixture.Name)
			}
			// Completion certifies the retained artifact, not the omitted native
			// event. Both its known control-only and wholly filtered forms are valid.
			indexer := NewStrikeIndexer(nil)
			session := DiscoveredSession{Harness: HarnessStrike}
			result, err := indexer.IndexTranscriptBytesResult(t.Context(), session, filtered)
			if err != nil {
				t.Fatalf("filtered known control refused: %v", err)
			}
			if len(result.(indexformat.V1).Entries) != 0 {
				t.Fatal("control unexpectedly emitted entries")
			}
			empty, emptyDiagnostics := filterStrikeOversizedRecords(input[:recordSize+1], fixture.Name+".jsonl")
			if len(empty) != 0 || len(emptyDiagnostics) != 1 {
				t.Fatal("fixture did not produce a filtered empty artifact")
			}
			result, err = indexer.IndexTranscriptBytesResult(t.Context(), session, empty)
			if err != nil {
				t.Fatalf("filtered empty artifact refused: %v", err)
			}
			if len(result.(indexformat.V1).Entries) != 0 {
				t.Fatal("filtered empty artifact emitted entries")
			}
		} else {
			if len(diagnostics) != 0 {
				t.Errorf("%s diagnostics = %+v, want none", fixture.Name, diagnostics)
			}
			if !bytes.Equal(filtered, input) {
				t.Errorf("%s safe input changed", fixture.Name)
			}
		}
	}
}
