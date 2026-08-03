package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/strike_record_sizes.yaml
var strikeRecordSizeFixtureData []byte

const strikeRecordSizeFixturePath = "internal/ingest/testdata/strike_record_sizes.yaml"

type strikeRecordSizeFixtures struct {
	Cases []strikeRecordSizeFixture `yaml:"cases"`
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
	if len(fixtures.Cases) != 3 {
		t.Fatalf("committed fixture %s must define three boundary cases, got %d", strikeRecordSizeFixturePath, len(fixtures.Cases))
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
