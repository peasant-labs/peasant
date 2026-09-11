package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_stand_in_offsets.yaml
var piStandInOffsetFixtureData []byte

const piStandInOffsetFixturePath = "internal/ingest/testdata/pi_stand_in_offsets.yaml"

// piStandInOffsetLimit is the injected per-record limit the source is filtered
// with, so an over-limit record costs nothing to build. The Pi read itself then
// runs with the production limit, exactly as a reindex does.
const piStandInOffsetLimit = 4096

type piStandInOffsetFixtures struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []piStandInOffsetCase `yaml:"cases"`
}

type piStandInOffsetCase struct {
	Name                   string                 `yaml:"name"`
	Lines                  []piStandInOffsetEntry `yaml:"lines"`
	NoFinalNewline         bool                   `yaml:"noFinalNewline,omitempty"`
	Refused                bool                   `yaml:"refused,omitempty"`
	RefusedMessageContains string                 `yaml:"refusedMessageContains,omitempty"`
	ConsumesWholeDocument  bool                   `yaml:"consumesWholeDocument,omitempty"`
	IncompleteTail         bool                   `yaml:"incompleteTail,omitempty"`
}

// piStandInOffsetEntry is one physical line of the native recording: literal
// text, or a record over the injected limit that the filter replaces with its
// one-line stand-in.
type piStandInOffsetEntry struct {
	Text      string `yaml:"text,omitempty"`
	Oversized bool   `yaml:"oversized,omitempty"`
}

func loadPiStandInOffsetFixtures(t *testing.T) piStandInOffsetFixtures {
	t.Helper()
	var fixtures piStandInOffsetFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(piStandInOffsetFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", piStandInOffsetFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", piStandInOffsetFixturePath, err)
	}
	names := make(map[string]bool)
	for _, fixtureCase := range fixtures.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] {
			t.Fatalf("missing or duplicate Pi stand-in fixture name %q", fixtureCase.Name)
		}
		if len(fixtureCase.Lines) == 0 {
			t.Fatalf("Pi stand-in fixture %q has no lines", fixtureCase.Name)
		}
		if fixtureCase.Refused && fixtureCase.RefusedMessageContains == "" {
			t.Fatalf("Pi stand-in fixture %q expects a refusal without saying what it must name", fixtureCase.Name)
		}
		if !fixtureCase.Refused && !fixtureCase.ConsumesWholeDocument && !fixtureCase.IncompleteTail {
			t.Fatalf("Pi stand-in fixture %q asserts nothing about the read", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required Pi stand-in fixture %q missing from %s", name, piStandInOffsetFixturePath)
		}
	}
	return fixtures
}

// piStandInOffsetArtifact builds the native recording and filters it, which is
// what produces the stand-in a reindex reads.
func piStandInOffsetArtifact(t *testing.T, fixtureCase piStandInOffsetCase) []byte {
	t.Helper()
	var source []byte
	sawOversized := false
	for index, entry := range fixtureCase.Lines {
		switch {
		case entry.Oversized:
			source = append(source, bytes.Repeat([]byte{'x'}, piStandInOffsetLimit+1)...)
			sawOversized = true
		case entry.Text != "":
			source = append(source, entry.Text...)
		default:
			t.Fatalf("Pi stand-in fixture %q has a line that is neither text nor an over-limit record", fixtureCase.Name)
		}
		last := index == len(fixtureCase.Lines)-1
		if !last || !fixtureCase.NoFinalNewline {
			source = append(source, '\n')
		}
	}
	if !sawOversized {
		t.Fatalf("Pi stand-in fixture %q has no over-limit record, so it holds no stand-in", fixtureCase.Name)
	}
	filtered, diagnostics, err := filterOversizedJSONLRecords(
		t.Context(), source, fixtureCase.Name+".jsonl", piStandInOffsetLimit,
	)
	if err != nil {
		t.Fatalf("the filter refused the source: %v", err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("the filter reported %d omissions; the case builds exactly one over-limit record", len(diagnostics))
	}
	if !bytes.Contains(filtered, []byte(omittedRecordSentinelKey)) {
		t.Fatal("the filtered artifact holds no stand-in line")
	}
	return filtered
}

// TestPiReadPlacesRecordsAfterAStandInByTheBytesItRead holds the Pi read's byte
// accounting to the bytes the reader actually passed.
//
// A stand-in line is about a hundred and fifty bytes; the record it replaces may
// be hundreds of megabytes. Advancing by the omitted record's recorded size puts
// the reader's offset past the end of the document, so every later record looks
// like the document's last one. A later record that fails raw validation with a
// truncation-shaped error is then accepted as an incomplete final line and the
// session imports a truncated prefix, where it must be refused.
func TestPiReadPlacesRecordsAfterAStandInByTheBytesItRead(t *testing.T) {
	for _, fixtureCase := range loadPiStandInOffsetFixtures(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			data := piStandInOffsetArtifact(t, fixtureCase)
			doc, err := parsePiDocumentWithLimit(t.Context(), data, defaults.MaxJSONLRecordBytes)

			if fixtureCase.Refused {
				if err == nil {
					t.Fatalf("the read accepted a malformed record in the middle of the document; warnings: %+v, consumed %d of %d bytes",
						doc.warnings, doc.consumedBytes, len(data))
				}
				if !strings.Contains(err.Error(), fixtureCase.RefusedMessageContains) {
					t.Errorf("the refusal %q does not name %q", err.Error(), fixtureCase.RefusedMessageContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("the read refused a valid filtered artifact: %v", err)
			}

			tails := 0
			for _, warning := range doc.warnings {
				if warning.ErrorType == "incomplete_tail" {
					tails++
				}
			}
			if fixtureCase.IncompleteTail {
				if tails != 1 {
					t.Fatalf("the read reported %d incomplete-tail warnings, want exactly one; warnings: %+v", tails, doc.warnings)
				}
				lastLine := data[bytes.LastIndexByte(bytes.TrimRight(data, "\n"), '\n')+1:]
				wantConsumed := len(data) - len(lastLine)
				if doc.consumedBytes != wantConsumed {
					t.Errorf("the read consumed %d bytes, want the %d bytes before the incomplete final line", doc.consumedBytes, wantConsumed)
				}
				return
			}
			if tails != 0 {
				t.Fatalf("the read warned of an incomplete tail on a document whose records are all whole; warnings: %+v", doc.warnings)
			}
			if fixtureCase.ConsumesWholeDocument && doc.consumedBytes != len(data) {
				t.Errorf("the read consumed %d bytes of a %d-byte document whose records are all whole", doc.consumedBytes, len(data))
			}
		})
	}
}
