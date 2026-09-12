package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_diagnostic_lines.yaml
var metadataDiagnosticLineFixtureData []byte

const metadataDiagnosticLineFixturePath = "internal/ingest/testdata/metadata_diagnostic_lines.yaml"

// metadataDiagnosticLineLimit is the injected per-record limit these cases
// filter with. It is small so an over-limit record costs nothing to build, and
// it is passed through the filter's parameter, so no global changes.
const metadataDiagnosticLineLimit = 4096

type metadataDiagnosticLineFixtures struct {
	RequiredNames []string                     `yaml:"requiredNames"`
	Cases         []metadataDiagnosticLineCase `yaml:"cases"`
}

type metadataDiagnosticLineCase struct {
	Name                    string                        `yaml:"name"`
	Harness                 Harness                       `yaml:"harness"`
	Lines                   []metadataDiagnosticLineEntry `yaml:"lines"`
	ExpectedLocation        string                        `yaml:"expectedLocation"`
	ExpectedMessageContains string                        `yaml:"expectedMessageContains"`
}

// metadataDiagnosticLineEntry is one physical line of the native source: either
// literal text or a record over the injected limit, which the filter replaces
// with its one-line stand-in.
type metadataDiagnosticLineEntry struct {
	Text      string `yaml:"text,omitempty"`
	Oversized bool   `yaml:"oversized,omitempty"`
}

func loadMetadataDiagnosticLineFixtures(t *testing.T) metadataDiagnosticLineFixtures {
	t.Helper()
	var fixtures metadataDiagnosticLineFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(metadataDiagnosticLineFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", metadataDiagnosticLineFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", metadataDiagnosticLineFixturePath, err)
	}
	names := make(map[string]bool)
	for _, fixtureCase := range fixtures.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] {
			t.Fatalf("missing or duplicate diagnostic line fixture name %q", fixtureCase.Name)
		}
		if fixtureCase.ExpectedLocation == "" || fixtureCase.ExpectedMessageContains == "" || len(fixtureCase.Lines) == 0 {
			t.Fatalf("diagnostic line fixture %q is incomplete", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required diagnostic line fixture %q missing from %s", name, metadataDiagnosticLineFixturePath)
		}
	}
	return fixtures
}

// metadataDiagnosticLineSource builds the native source of a case and returns it
// with the physical line the case's stand-in will occupy.
func metadataDiagnosticLineSource(t *testing.T, fixtureCase metadataDiagnosticLineCase) []byte {
	t.Helper()
	var source []byte
	sawOversized := false
	for _, entry := range fixtureCase.Lines {
		switch {
		case entry.Oversized:
			source = append(source, bytes.Repeat([]byte{'x'}, metadataDiagnosticLineLimit+1)...)
			sawOversized = true
		case entry.Text != "":
			source = append(source, entry.Text...)
		default:
			t.Fatalf("diagnostic line fixture %q has a line that is neither text nor an over-limit record", fixtureCase.Name)
		}
		source = append(source, '\n')
	}
	if !sawOversized {
		t.Fatalf("diagnostic line fixture %q has no over-limit record, so it cannot show a line drift", fixtureCase.Name)
	}
	return source
}

// metadataDiagnosticLineExtract runs the harness's own metadata extractor over
// the filtered artifact and returns the warnings it recorded.
func metadataDiagnosticLineExtract(t *testing.T, harness Harness, filtered []byte) []DiagnosticEntry {
	t.Helper()
	meta := &UnifiedMetadata{SessionID: SessionID("11111111-2222-3333-4444-555555555555"), ModelHarness: harness}
	switch harness {
	case HarnessClaudeCode:
		if _, err := parseClaudeTranscriptMetadata(filtered, meta); err != nil {
			t.Fatalf("Claude metadata extraction refused the filtered artifact: %v", err)
		}
	case HarnessCursor:
		parseCursorTranscriptMetadata(filtered, meta)
	case HarnessCodex:
		if _, err := parseCodexTranscriptMetadata(t.Context(), filtered, meta); err != nil {
			t.Fatalf("Codex metadata extraction refused the filtered artifact: %v", err)
		}
	default:
		t.Fatalf("diagnostic line fixture names harness %q, which has no JSONL metadata extractor here", harness)
	}
	return meta.Diagnostics.Warnings
}

// TestMetadataDiagnosticNamesThePhysicalLineAfterAnOmittedRecord holds the
// metadata extractors to the physical line of the source.
//
// A record the reader passes over still occupies a line. A counter kept beside
// the reader counts only the records it hands back, so every diagnostic after an
// omission names an earlier line than the one at fault: the user opens the named
// line and finds a valid record there. The reader already reports the physical
// line, so it is the only line number these diagnostics may use.
func TestMetadataDiagnosticNamesThePhysicalLineAfterAnOmittedRecord(t *testing.T) {
	for _, fixtureCase := range loadMetadataDiagnosticLineFixtures(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			source := metadataDiagnosticLineSource(t, fixtureCase)
			filtered, diagnostics, err := filterOversizedJSONLRecords(
				t.Context(), source, fixtureCase.Name+".jsonl", metadataDiagnosticLineLimit,
			)
			if err != nil {
				t.Fatalf("the filter refused the source: %v", err)
			}
			if len(diagnostics) != 1 {
				t.Fatalf("the filter reported %d omissions; the case builds exactly one over-limit record", len(diagnostics))
			}
			if bytes.Count(filtered, []byte{'\n'}) != len(fixtureCase.Lines) {
				t.Fatalf("the filtered artifact holds %d lines, not the source's %d; the stand-in must keep the omitted record's own line",
					bytes.Count(filtered, []byte{'\n'}), len(fixtureCase.Lines))
			}

			warnings := metadataDiagnosticLineExtract(t, fixtureCase.Harness, filtered)
			var located []string
			for _, warning := range warnings {
				if !strings.Contains(warning.Message, fixtureCase.ExpectedMessageContains) {
					continue
				}
				located = append(located, warning.Location)
			}
			if len(located) != 1 {
				t.Fatalf("the extractor reported %d diagnostics saying %q, want exactly one; all warnings: %+v",
					len(located), fixtureCase.ExpectedMessageContains, warnings)
			}
			if located[0] != fixtureCase.ExpectedLocation {
				t.Errorf("the diagnostic names %q; the malformed record is at %s of the source, and the named line holds a valid record",
					located[0], fixtureCase.ExpectedLocation)
			}
		})
	}
}
