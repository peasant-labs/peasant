package harnesslayout_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"github.com/peasant-labs/peasant/internal/testutil"
)

type shapeField struct {
	Path  string   `yaml:"path"`
	Type  string   `yaml:"type"`
	Types []string `yaml:"types"`
	Count int      `yaml:"count"`
}

type shapeCase struct {
	Name          string       `yaml:"name"`
	Raw           string       `yaml:"raw"`
	Document      string       `yaml:"document"`
	Fields        []shapeField `yaml:"fields"`
	WantError     bool         `yaml:"wantError"`
	WantRecords   int          `yaml:"wantRecords"`
	WantMalformed int          `yaml:"wantMalformed"`
	WantFields    []shapeField `yaml:"wantFields"`
	TimeValue     string       `yaml:"timeValue"`
	WantUnix      int64        `yaml:"wantUnix"`
	WantUnixMilli int64        `yaml:"wantUnixMilli"`
	WantZero      bool         `yaml:"wantZero"`
}

type shapeCaseFile struct {
	RequiredNames []string    `yaml:"requiredNames"`
	Cases         []shapeCase `yaml:"cases"`
}

func TestShapeCases(t *testing.T) {
	data, err := os.ReadFile("testdata/shape_cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file shapeCaseFile
	if err := testutil.DecodeNamedFixtureYAML(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			switch {
			case tc.Raw != "":
				runRawCase(t, tc)
			case tc.Document != "":
				runDocumentCase(t, tc)
			case len(tc.Fields) > 0:
				runAddFieldCase(t, tc)
			case tc.TimeValue != "":
				runTimeCase(t, tc)
			default:
				t.Fatal("case sets none of raw, document, fields, or timeValue")
			}
		})
	}
}

func TestShapeCasesRejectUnknownField(t *testing.T) {
	raw := []byte("requiredNames: [only]\ncases:\n  - name: only\n    unexpected: true\n")
	var file shapeCaseFile
	if err := testutil.DecodeNamedFixtureYAML(raw, &file); err == nil {
		t.Fatal("fixture loader accepted an unknown field")
	}
}

func runRawCase(t *testing.T, tc shapeCase) {
	t.Helper()
	rec := harnesslayout.NewShapeRecorder(exampleTranscript, "")
	err := rec.AddRaw("", []byte(tc.Raw))
	if tc.WantError {
		if err == nil {
			t.Fatal("AddRaw succeeded, want a decode error")
		}
	} else if err != nil {
		t.Fatal(err)
	}
	shape := rec.Shape()
	if shape.Records != tc.WantRecords || shape.Malformed != tc.WantMalformed {
		t.Fatalf("records=%d malformed=%d, want records=%d malformed=%d", shape.Records, shape.Malformed, tc.WantRecords, tc.WantMalformed)
	}
}

func runDocumentCase(t *testing.T, tc shapeCase) {
	t.Helper()
	rec := harnesslayout.NewShapeRecorder(exampleTranscript, "")
	if _, err := harnesslayout.ShapeJSON(strings.NewReader(tc.Document), rec, nil, nil); err != nil {
		t.Fatal(err)
	}
	shape := rec.Shape()
	if shape.Records != tc.WantRecords || shape.Malformed != tc.WantMalformed {
		t.Fatalf("records=%d malformed=%d, want records=%d malformed=%d", shape.Records, shape.Malformed, tc.WantRecords, tc.WantMalformed)
	}
	assertFields(t, shape.Fields, tc.WantFields)
}

func runAddFieldCase(t *testing.T, tc shapeCase) {
	t.Helper()
	rec := harnesslayout.NewShapeRecorder(exampleTranscript, "")
	for _, field := range tc.Fields {
		rec.AddField(field.Path, harnesslayout.JSONType(field.Type))
	}
	assertFields(t, rec.Shape().Fields, tc.WantFields)
}

func runTimeCase(t *testing.T, tc shapeCase) {
	t.Helper()
	for _, value := range []any{tc.TimeValue, json.Number(tc.TimeValue)} {
		got := harnesslayout.ParseTime(value)
		switch {
		case tc.WantZero:
			if !got.IsZero() {
				t.Errorf("ParseTime(%#v) = %v, want zero", value, got)
			}
		case tc.WantUnixMilli != 0:
			want := time.UnixMilli(tc.WantUnixMilli).UTC()
			if !got.Equal(want) {
				t.Errorf("ParseTime(%#v) = %v, want %v", value, got, want)
			}
		default:
			want := time.Unix(tc.WantUnix, 0).UTC()
			if !got.Equal(want) {
				t.Errorf("ParseTime(%#v) = %v, want %v", value, got, want)
			}
		}
	}
}

func assertFields(t *testing.T, got []harnesslayout.FieldShape, want []shapeField) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("fields = %s, want %d paths", fieldPaths(got), len(want))
	}
	for i, field := range got {
		types := make([]string, len(field.Types))
		for j, typ := range field.Types {
			types[j] = string(typ)
		}
		if field.Path != want[i].Path || field.Count != want[i].Count || !slices.Equal(types, want[i].Types) {
			t.Fatalf("field %d = path %q types %v count %d, want path %q types %v count %d", i, field.Path, types, field.Count, want[i].Path, want[i].Types, want[i].Count)
		}
	}
}

func fieldPaths(fields []harnesslayout.FieldShape) string {
	paths := make([]string, len(fields))
	for i, field := range fields {
		paths[i] = field.Path
	}
	return strings.Join(paths, " ")
}

type runCase struct {
	Name               string   `yaml:"name"`
	Sessions           []string `yaml:"sessions"`
	Fail               []string `yaml:"fail"`
	Limit              int      `yaml:"limit"`
	Cancel             bool     `yaml:"cancel"`
	DiscoverError      string   `yaml:"discoverError"`
	WantError          string   `yaml:"wantError"`
	WantSessions       *int     `yaml:"wantSessions"`
	WantCaptures       []string `yaml:"wantCaptures"`
	WantFailures       []string `yaml:"wantFailures"`
	WantRootClass      string   `yaml:"wantRootClass"`
	WantFailureClass   string   `yaml:"wantFailureClass"`
	WantSecretStripped bool     `yaml:"wantSecretStripped"`
}

type runCaseFile struct {
	RequiredNames []string  `yaml:"requiredNames"`
	Cases         []runCase `yaml:"cases"`
}

func TestRunCases(t *testing.T) {
	data, err := os.ReadFile("testdata/run_cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file runCaseFile
	if err := testutil.DecodeNamedFixtureYAML(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			runScriptedCase(t, tc)
		})
	}
}

func runScriptedCase(t *testing.T, tc runCase) {
	t.Helper()
	probe := scriptedProbe{sessions: tc.Sessions, fail: map[string]bool{}}
	for _, id := range tc.Fail {
		probe.fail[id] = true
	}
	if tc.DiscoverError != "" {
		probe.discoverErr = discoveryError(tc.DiscoverError)
	}
	layout := harnesslayout.Layout{Tool: "scripted", Probe: probe}
	ctx := context.Background()
	if tc.Cancel {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	dir := t.TempDir()
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: os.DirFS(dir)} }
	report, err := harnesslayout.Run(ctx, layout, []string{"/present"}, open, tc.Limit)
	switch tc.WantError {
	case "":
		if err != nil {
			t.Fatal(err)
		}
	case "negative":
		if err == nil || !strings.Contains(err.Error(), "zero or greater") {
			t.Fatalf("error = %v, want a negative-limit rejection", err)
		}
		return
	case "canceled":
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	default:
		t.Fatalf("unknown wantError %q", tc.WantError)
	}
	if tc.WantSessions != nil {
		if len(report.Roots) != 1 || report.Roots[0].Sessions != *tc.WantSessions {
			t.Fatalf("roots = %+v, want sessions %d", report.Roots, *tc.WantSessions)
		}
	}
	if tc.WantCaptures != nil {
		got := captureIDs(report)
		if !slices.Equal(got, tc.WantCaptures) {
			t.Fatalf("captures = %v, want %v", got, tc.WantCaptures)
		}
	}
	if tc.WantFailures != nil {
		got := failureIDs(report)
		if !slices.Equal(got, tc.WantFailures) {
			t.Fatalf("failures = %v, want %v", got, tc.WantFailures)
		}
	}
	if tc.WantRootClass != "" {
		if len(report.Roots) != 1 || string(report.Roots[0].ErrorClass) != tc.WantRootClass || report.Roots[0].Error == "" {
			t.Fatalf("root = %+v, want class %s and error text", report.Roots, tc.WantRootClass)
		}
	}
	if tc.WantFailureClass != "" {
		if len(report.Failures) != 1 || string(report.Failures[0].ErrorClass) != tc.WantFailureClass {
			t.Fatalf("failures = %+v, want class %s", report.Failures, tc.WantFailureClass)
		}
	}
	if tc.WantSecretStripped {
		full, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(full), "secret-discovery-path") && !strings.Contains(string(full), "secret-session") {
			t.Fatalf("unstripped report never contained the private error text: %s", full)
		}
		stripped, err := json.Marshal(report.ShapeOnly())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-discovery-path", "secret-session"} {
			if strings.Contains(string(stripped), secret) {
				t.Fatalf("shape-only report contains %q: %s", secret, stripped)
			}
		}
	}
}

func captureIDs(report harnesslayout.Report) []string {
	ids := make([]string, len(report.Captures))
	for i, capture := range report.Captures {
		ids[i] = capture.Session
	}
	return ids
}

func failureIDs(report harnesslayout.Report) []string {
	ids := make([]string, len(report.Failures))
	for i, failure := range report.Failures {
		ids[i] = failure.Session
	}
	return ids
}

func discoveryError(class string) error {
	switch class {
	case "permission":
		return fmt.Errorf("read secret-discovery-path: %w", fs.ErrPermission)
	case "not_found":
		return fmt.Errorf("read secret-discovery-path: %w", fs.ErrNotExist)
	case "canceled":
		return fmt.Errorf("read secret-discovery-path: %w", context.Canceled)
	case "other":
		return fmt.Errorf("read secret-discovery-path: boom")
	default:
		return fmt.Errorf("unknown discover class %s", class)
	}
}

type scriptedProbe struct {
	sessions    []string
	fail        map[string]bool
	discoverErr error
}

func (p scriptedProbe) Discover(context.Context, harnesslayout.Source) ([]harnesslayout.SessionRef, error) {
	if p.discoverErr != nil {
		return nil, p.discoverErr
	}
	refs := make([]harnesslayout.SessionRef, len(p.sessions))
	for i, id := range p.sessions {
		refs[i] = harnesslayout.SessionRef{ID: id}
	}
	return refs, nil
}

func (p scriptedProbe) Capture(_ context.Context, _ harnesslayout.Source, ref harnesslayout.SessionRef) (harnesslayout.Capture, error) {
	if p.fail[ref.ID] {
		return harnesslayout.Capture{}, fmt.Errorf("capture secret-session %s: %w", ref.ID, fs.ErrPermission)
	}
	return harnesslayout.Capture{Session: ref.ID}, nil
}

var _ harnesslayout.Probe = scriptedProbe{}
