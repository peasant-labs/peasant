package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/jsonl_record_reader.yaml
var jsonlRecordReaderFixtureData []byte

const jsonlRecordReaderFixturePath = "internal/ingest/testdata/jsonl_record_reader.yaml"

type jsonlRecordReaderFixtures struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cases         []jsonlRecordReaderFixture `yaml:"cases"`
}

type jsonlRecordReaderFixture struct {
	Name          string                       `yaml:"name"`
	LimitBytes    int                          `yaml:"limitBytes"`
	Records       []jsonlRecordReaderRecord    `yaml:"records"`
	FinalNewline  bool                         `yaml:"finalNewline"`
	WantYielded   []int                        `yaml:"wantYielded"`
	WantOversized []jsonlRecordReaderOversized `yaml:"wantOversized"`
}

type jsonlRecordReaderRecord struct {
	Size int `yaml:"size"`
}

type jsonlRecordReaderOversized struct {
	Line int `yaml:"line"`
	Size int `yaml:"size"`
}

// recordBytes rebuilds a fixture record exactly: the physical line number,
// a colon, then 'x' padding. The test compares the yielded bytes against
// this, so a reader that truncated or merged records fails.
func (r jsonlRecordReaderRecord) recordBytes(line int) []byte {
	if r.Size == 0 {
		return []byte{}
	}
	head := fmt.Sprintf("%d:", line)
	if len(head) >= r.Size {
		return []byte(head[:r.Size])
	}
	return []byte(head + strings.Repeat("x", r.Size-len(head)))
}

func loadJSONLRecordReaderFixtures(t *testing.T) jsonlRecordReaderFixtures {
	t.Helper()
	var fixtures jsonlRecordReaderFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(jsonlRecordReaderFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", jsonlRecordReaderFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", jsonlRecordReaderFixturePath, err)
	}
	names := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("missing or duplicate case name %q in %s", fixture.Name, jsonlRecordReaderFixturePath)
		}
		names[fixture.Name] = true
	}
	for _, required := range fixtures.RequiredNames {
		if !names[required] {
			t.Fatalf("required case %q is missing from %s; a case named in requiredNames must not be deleted", required, jsonlRecordReaderFixturePath)
		}
	}
	return fixtures
}

func TestForEachJSONLRecord(t *testing.T) {
	fixtures := loadJSONLRecordReaderFixtures(t)

	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var input bytes.Buffer
			wantBytes := make(map[int][]byte, len(fixture.Records))
			for i, record := range fixture.Records {
				line := i + 1
				raw := record.recordBytes(line)
				wantBytes[line] = raw
				input.Write(raw)
				if i < len(fixture.Records)-1 || fixture.FinalNewline {
					input.WriteByte('\n')
				}
			}

			var gotYielded []int
			var gotOversized []jsonlRecordReaderOversized
			err := forEachJSONLRecord(
				t.Context(),
				bytes.NewReader(input.Bytes()),
				fixture.LimitBytes,
				func(line int, raw []byte) error {
					gotYielded = append(gotYielded, line)
					want, known := wantBytes[line]
					if !known {
						t.Errorf("reader yielded line %d, which the input does not contain", line)
						return nil
					}
					if !bytes.Equal(raw, want) {
						t.Errorf("line %d yielded %d bytes, want the whole %d-byte record; the reader truncated or merged it", line, len(raw), len(want))
					}
					return nil
				},
				func(line int, size int) {
					gotOversized = append(gotOversized, jsonlRecordReaderOversized{Line: line, Size: size})
				},
			)
			if err != nil {
				t.Fatalf("forEachJSONLRecord returned %v; no record size may fail a read", err)
			}

			if !equalIntSlices(gotYielded, fixture.WantYielded) {
				t.Errorf("yielded lines = %v, want %v", gotYielded, fixture.WantYielded)
			}
			if len(gotOversized) != len(fixture.WantOversized) {
				t.Fatalf("oversized reports = %v, want %v", gotOversized, fixture.WantOversized)
			}
			for i, want := range fixture.WantOversized {
				if gotOversized[i] != want {
					t.Errorf("oversized report %d = %+v, want %+v; the reported size must be the record's true byte length", i, gotOversized[i], want)
				}
			}
		})
	}
}

// TestForEachJSONLRecordRefusesUnusableLimit pins the actionable refusal for a
// limit that would omit every record.
func TestForEachJSONLRecordRefusesUnusableLimit(t *testing.T) {
	err := forEachJSONLRecord(t.Context(), strings.NewReader("{}\n"), 0, func(int, []byte) error { return nil }, nil)
	if err == nil {
		t.Fatal("a zero record limit was accepted; every record would be reported omitted")
	}
	for _, want := range []string{"newJSONLRecordScanner", "jsonl_records.go", "defaults.MaxJSONLRecordBytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q; the caller cannot tell where to fix it", err, want)
		}
	}
}

// TestForEachJSONLRecordStopsOnCallbackError proves a caller's own parse error
// still stops the read, so the reader's size tolerance does not swallow real
// failures.
func TestForEachJSONLRecordStopsOnCallbackError(t *testing.T) {
	sentinel := fmt.Errorf("caller refused the record")
	seen := 0
	err := forEachJSONLRecord(t.Context(), strings.NewReader("a\nb\nc\n"), 1024, func(int, []byte) error {
		seen++
		return sentinel
	}, nil)
	if err != sentinel {
		t.Fatalf("err = %v, want the caller's own error", err)
	}
	if seen != 1 {
		t.Fatalf("callback ran %d times, want 1; the reader kept going after the caller refused", seen)
	}
}

func equalIntSlices(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
