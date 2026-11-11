package onboarding_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/docdiff.yaml
var docDiffData []byte

type docDiffLine struct {
	Type  string `yaml:"type"`
	OldNo int    `yaml:"oldNo"`
	NewNo int    `yaml:"newNo"`
	Text  string `yaml:"text"`
}

type docDiffHunk struct {
	Header string        `yaml:"header"`
	Lines  []docDiffLine `yaml:"lines"`
}

type docDiffLimits struct {
	MaxHunks *int `yaml:"maxHunks"`
	MaxLines *int `yaml:"maxLines"`
	MaxBytes *int `yaml:"maxBytes"`
}

type docDiffWant struct {
	Hunks          []docDiffHunk `yaml:"hunks"`
	HunkHeaders    []string      `yaml:"hunkHeaders"`
	LineCount      int           `yaml:"lineCount"`
	Truncated      bool          `yaml:"truncated"`
	Error          string        `yaml:"error"`
	StructuredOnly bool          `yaml:"structuredOnly"`
}

// docDiffGenerate asks for two documents of Lines lines that share no line. The
// fixture states the size; the test builds the text.
type docDiffGenerate struct {
	Lines int `yaml:"lines"`
}

type docDiffScenario struct {
	Note     string          `yaml:"note"`
	Before   string          `yaml:"before"`
	After    string          `yaml:"after"`
	Generate docDiffGenerate `yaml:"generate"`
	Limits   docDiffLimits   `yaml:"limits"`
	Want     docDiffWant     `yaml:"want"`
}

type docDiffCase struct {
	Name     string            `yaml:"name"`
	Before   string            `yaml:"before"`
	After    string            `yaml:"after"`
	Limits   docDiffLimits     `yaml:"limits"`
	Want     docDiffWant       `yaml:"want"`
	Variants []docDiffScenario `yaml:"variants"`
}

type docDiffDocument struct {
	RequiredNames []string      `yaml:"requiredNames"`
	Cases         []docDiffCase `yaml:"cases"`
}

func TestDiffDocuments(t *testing.T) {
	t.Parallel()
	var document docDiffDocument
	decodeFixture(t, "docdiff.yaml", docDiffData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			runDocDiffScenario(t, "base", docDiffScenario{Before: row.Before, After: row.After, Limits: row.Limits, Want: row.Want})
			for _, variant := range row.Variants {
				if variant.Generate.Lines > 0 {
					var before, after strings.Builder
					for i := 0; i < variant.Generate.Lines; i++ {
						fmt.Fprintf(&before, "old-%d\n", i)
						fmt.Fprintf(&after, "new-%d\n", i)
					}
					variant.Before, variant.After = before.String(), after.String()
				}
				if variant.Before == "" && variant.After == "" {
					// A variant that names no documents re-runs the row's own
					// under other bounds.
					variant.Before, variant.After = row.Before, row.After
				}
				runDocDiffScenario(t, variant.Note, variant)
			}
		})
	}
}

func runDocDiffScenario(t *testing.T, note string, scenario docDiffScenario) {
	t.Helper()
	limits := onboarding.DefaultDiffLimits()
	if scenario.Limits.MaxHunks != nil {
		limits.MaxHunks = *scenario.Limits.MaxHunks
	}
	if scenario.Limits.MaxLines != nil {
		limits.MaxLines = *scenario.Limits.MaxLines
	}
	if scenario.Limits.MaxBytes != nil {
		limits.MaxBytes = *scenario.Limits.MaxBytes
	}
	hunks, truncated, err := onboarding.DiffDocuments([]byte(scenario.Before), []byte(scenario.After), limits)
	want := scenario.Want
	if want.Error != "" {
		if err == nil || !strings.Contains(err.Error(), want.Error) {
			t.Fatalf("%s: error = %v, want it to contain %q", note, err, want.Error)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: DiffDocuments: %v", note, err)
	}
	if hunks == nil {
		t.Fatalf("%s: hunks is nil, want a non-nil list the contract can carry", note)
	}
	if truncated != want.Truncated {
		t.Errorf("%s: truncated = %t, want %t", note, truncated, want.Truncated)
	}
	for _, hunk := range hunks {
		if err := hunk.Validate(); err != nil {
			t.Errorf("%s: hunk %q is not a valid contract hunk: %v", note, hunk.Header, err)
		}
	}
	if want.HunkHeaders == nil {
		if got := toDocDiffHunks(hunks); !reflect.DeepEqual(got, nonNilHunks(want.Hunks)) {
			t.Errorf("%s: hunks differ\n got: %s\nwant: %s", note, mustJSON(t, got), mustJSON(t, want.Hunks))
		}
	}
	if want.HunkHeaders != nil {
		headers := make([]string, len(hunks))
		lines := 0
		for i, hunk := range hunks {
			headers[i] = hunk.Header
			lines += len(hunk.Lines)
		}
		if !reflect.DeepEqual(headers, want.HunkHeaders) {
			t.Errorf("%s: headers = %v, want %v", note, headers, want.HunkHeaders)
		}
		if lines != want.LineCount {
			t.Errorf("%s: shown lines = %d, want %d", note, lines, want.LineCount)
		}
	}
	if want.StructuredOnly {
		assertStructuredHunks(t, note, hunks)
	}
}

func toDocDiffHunks(hunks []schema.LocalKickstartHunk) []docDiffHunk {
	out := make([]docDiffHunk, 0, len(hunks))
	for _, hunk := range hunks {
		row := docDiffHunk{Header: hunk.Header, Lines: make([]docDiffLine, 0, len(hunk.Lines))}
		for _, line := range hunk.Lines {
			row.Lines = append(row.Lines, docDiffLine{Type: line.Type.String(), OldNo: line.OldNo, NewNo: line.NewNo, Text: line.Text})
		}
		out = append(out, row)
	}
	return out
}

func nonNilHunks(hunks []docDiffHunk) []docDiffHunk {
	if hunks == nil {
		return []docDiffHunk{}
	}
	return hunks
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// assertStructuredHunks pins the wire shape: the result marshals to objects with
// only the contract's keys, no line carries a newline or a diff marker the
// client would have to strip, and no text field holds a rendered hunk.
func assertStructuredHunks(t *testing.T, note string, hunks []schema.LocalKickstartHunk) {
	t.Helper()
	encoded, err := json.Marshal(hunks)
	if err != nil {
		t.Fatal(err)
	}
	var generic []map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("%s: hunks do not marshal to a list of objects: %v", note, err)
	}
	allowedHunk := map[string]bool{"header": true, "lines": true}
	allowedLine := map[string]bool{"type": true, "oldNo": true, "newNo": true, "text": true}
	for _, hunk := range generic {
		for key := range hunk {
			if !allowedHunk[key] {
				t.Errorf("%s: hunk carries key %q the contract does not define", note, key)
			}
		}
		for _, line := range hunk["lines"].([]any) {
			for key := range line.(map[string]any) {
				if !allowedLine[key] {
					t.Errorf("%s: line carries key %q the contract does not define", note, key)
				}
			}
		}
	}
	for _, hunk := range hunks {
		if strings.Contains(hunk.Header, "@@") || strings.Contains(hunk.Header, "\n") {
			t.Errorf("%s: header %q is rendered text", note, hunk.Header)
		}
		for _, line := range hunk.Lines {
			if strings.ContainsAny(line.Text, "\n\r") || strings.HasPrefix(line.Text, "@@") {
				t.Errorf("%s: line text %q is rendered hunk text", note, line.Text)
			}
		}
	}
}
