package harnesslayout_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
)

const exampleTool harnesslayout.Tool = "example-tool"

var exampleTranscript = harnesslayout.Artifact{
	Name:        "transcript",
	Pattern:     "sessions/*.jsonl",
	Format:      harnesslayout.FormatJSONL,
	Role:        harnesslayout.RoleTranscript,
	Description: "one JSONL file per session",
}

type exampleProbe struct{}

var _ harnesslayout.Probe = exampleProbe{}

func (exampleProbe) Discover(_ context.Context, src harnesslayout.Source) ([]harnesslayout.SessionRef, error) {
	matches, err := fs.Glob(src.FS, exampleTranscript.Pattern)
	if err != nil {
		return nil, err
	}
	refs := make([]harnesslayout.SessionRef, 0, len(matches))
	for _, match := range matches {
		refs = append(refs, harnesslayout.SessionRef{
			ID:    strings.TrimSuffix(path.Base(match), ".jsonl"),
			Paths: []string{match},
		})
	}
	return refs, nil
}

func (exampleProbe) Capture(_ context.Context, src harnesslayout.Source, ref harnesslayout.SessionRef) (harnesslayout.Capture, error) {
	file, err := src.FS.Open(ref.Paths[0])
	if err != nil {
		return harnesslayout.Capture{}, err
	}
	defer file.Close()
	rec := harnesslayout.NewShapeRecorder(exampleTranscript, ref.Paths[0])
	meta := harnesslayout.Metadata{SessionID: ref.ID}
	err = harnesslayout.ShapeJSONL(file, rec, harnesslayout.KindField("type"), func(record any) {
		meta.Observe(harnesslayout.TimeField(record, "ts"), harnesslayout.StringField(record, "model"))
		switch harnesslayout.StringField(record, "type") {
		case "user":
			meta.UserTurns++
		case "assistant":
			meta.AssistantMsgs++
		}
		if title := harnesslayout.StringField(record, "title"); title != "" {
			meta.Title = title
		}
	})
	if err != nil {
		return harnesslayout.Capture{}, err
	}
	return harnesslayout.Capture{Metadata: meta, Shapes: []harnesslayout.ArtifactShape{rec.Shape()}}, nil
}

var exampleLayout = harnesslayout.Layout{
	Tool:        exampleTool,
	DisplayName: "Example Tool",
	Sessions:    "One JSONL file per session.",
	Roots: []harnesslayout.Root{
		{OS: []harnesslayout.OS{harnesslayout.OSLinux}, Path: "{config}/example"},
		{OS: []harnesslayout.OS{harnesslayout.OSDarwin}, Path: "{home}/Library/example"},
		{Path: "{home}/.example"},
	},
	Artifacts: []harnesslayout.Artifact{exampleTranscript},
	Sources:   []string{"synthetic test layout"},
	Probe:     exampleProbe{},
}

func init() {
	harnesslayout.Register(exampleLayout)
}

func TestNewToolValidatesAgainstRegisteredLayouts(t *testing.T) {
	tool, err := harnesslayout.NewTool("example-tool")
	if err != nil || tool != exampleTool {
		t.Fatalf("NewTool(example-tool) = %q, %v", tool, err)
	}
	if _, err := harnesslayout.NewTool("nope"); err == nil || !strings.Contains(err.Error(), "example-tool") {
		t.Fatalf("NewTool(nope) error = %v, want an error naming the known tools", err)
	}
}

func TestRegisterRejectsDuplicateAndIncompleteLayouts(t *testing.T) {
	assertPanics(t, "duplicate", func() { harnesslayout.Register(exampleLayout) })
	incomplete := exampleLayout
	incomplete.Tool = "incomplete-tool"
	incomplete.Probe = nil
	assertPanics(t, "no probe", func() { harnesslayout.Register(incomplete) })
	badGlob := exampleLayout
	badGlob.Tool = "bad-glob-tool"
	badGlob.Artifacts = []harnesslayout.Artifact{{Name: "x", Pattern: "[", Format: harnesslayout.FormatJSON, Role: harnesslayout.RoleIndex}}
	assertPanics(t, "bad glob", func() { harnesslayout.Register(badGlob) })
}

func TestEnvResolveFiltersRootsByOperatingSystem(t *testing.T) {
	linux := harnesslayout.Env{GOOS: harnesslayout.OSLinux, Home: "/h", ConfigDir: "/h/.config"}
	got := linux.Resolve(exampleLayout)
	want := []string{"/h/.config/example", "/h/.example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("linux roots = %v, want %v", got, want)
	}
	darwin := harnesslayout.Env{GOOS: harnesslayout.OSDarwin, Home: "/Users/u"}
	if got := darwin.Resolve(exampleLayout); len(got) != 2 || got[0] != "/Users/u/Library/example" {
		t.Fatalf("darwin roots = %v", got)
	}
}

func TestRunCapturesShapeAndMetadataWithoutContent(t *testing.T) {
	secret := "do not leak this sentence"
	root := fstest.MapFS{
		"sessions/b.jsonl": {Data: []byte(strings.Join([]string{
			`{"type":"user","ts":"2026-01-02T03:04:05Z","text":"` + secret + `"}`,
			`{"type":"assistant","ts":1767323100000,"model":"m-1","parts":[{"kind":"text","text":"hi"}],"title":"Session B"}`,
			`{"type":"assistant","ts":1767323200000,"model":"m-1","parts":[]}`,
			`{"type":"user","text":"partial`,
		}, "\n"))},
		"sessions/a.jsonl": {Data: []byte(`{"type":"user","ts":"2026-01-01T00:00:00Z","text":"x"}`)},
	}
	sources := map[string]harnesslayout.Source{"/present": {FS: root}, "/missing": {FS: missingFS{}}}
	open := func(p string) harnesslayout.Source { return sources[p] }

	report, err := harnesslayout.Run(context.Background(), exampleLayout, []string{"/present", "/missing"}, open, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Roots) != 2 || !report.Roots[0].Present || report.Roots[0].Sessions != 2 || report.Roots[1].Present {
		t.Fatalf("roots = %+v", report.Roots)
	}
	if len(report.Captures) != 2 || report.Captures[0].Session != "a" || report.Captures[1].Session != "b" {
		t.Fatalf("captures = %+v", report.Captures)
	}

	b := report.Captures[1]
	if b.Tool != exampleTool || b.Metadata.Title != "Session B" || b.Metadata.UserTurns != 1 || b.Metadata.AssistantMsgs != 2 {
		t.Fatalf("metadata = %+v", b.Metadata)
	}
	if !b.Metadata.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || b.Metadata.UpdatedAt.UnixMilli() != 1767323200000 {
		t.Fatalf("time range = %v..%v", b.Metadata.StartedAt, b.Metadata.UpdatedAt)
	}
	shape := b.Shapes[0]
	if shape.Records != 3 || shape.Malformed != 1 || shape.Kinds["user"] != 1 || shape.Kinds["assistant"] != 2 {
		t.Fatalf("shape counts = %+v", shape)
	}
	fields := map[string]harnesslayout.FieldShape{}
	for _, field := range shape.Fields {
		fields[field.Path] = field
	}
	if ts := fields["$.ts"]; len(ts.Types) != 2 || ts.Count != 3 {
		t.Fatalf("$.ts = %+v, want string and number over 3 records", ts)
	}
	if kind := fields["$.parts[].kind"]; kind.Count != 1 || kind.Types[0] != harnesslayout.JSONString {
		t.Fatalf("$.parts[].kind = %+v", kind)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("report contains transcript text")
	}
	shapeOnly, err := json.Marshal(report.ShapeOnly())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Session B", "/present", `"session":"b"`, "2026-01-02"} {
		if strings.Contains(string(shapeOnly), private) {
			t.Fatalf("shape-only report contains %q: %s", private, shapeOnly)
		}
	}
	if !strings.Contains(string(shapeOnly), "m-1") || !strings.Contains(string(shapeOnly), "$.parts[].kind") {
		t.Fatalf("shape-only report dropped models or shapes: %s", shapeOnly)
	}
}

func TestRunHonorsLimitAndRecordsFailures(t *testing.T) {
	root := fstest.MapFS{
		"sessions/a.jsonl": {Data: []byte(`{"type":"user"}`)},
		"sessions/b.jsonl": {Data: []byte(`{"type":"user"}`)},
	}
	open := func(string) harnesslayout.Source { return harnesslayout.Source{FS: root} }
	report, err := harnesslayout.Run(context.Background(), exampleLayout, []string{"/r"}, open, 1)
	if err != nil || len(report.Captures) != 1 {
		t.Fatalf("limit 1: %d captures, %v", len(report.Captures), err)
	}

	failing := exampleLayout
	failing.Probe = failingProbe{exampleProbe{}}
	report, err = harnesslayout.Run(context.Background(), failing, []string{"/r"}, open, 0)
	if err != nil || len(report.Captures) != 0 || len(report.Failures) != 2 {
		t.Fatalf("failing probe: %+v, %v", report, err)
	}
}

func TestShapeRecorderBoundsFieldPaths(t *testing.T) {
	rec := harnesslayout.NewShapeRecorder(exampleTranscript, "")
	wide := map[string]any{}
	for i := 0; i < harnesslayout.MaxFieldPaths+10; i++ {
		wide["k"+strconv.Itoa(i)] = true
	}
	rec.AddRecord("", wide)
	shape := rec.Shape()
	if !shape.Truncated || len(shape.Fields) != harnesslayout.MaxFieldPaths {
		t.Fatalf("truncated=%v fields=%d", shape.Truncated, len(shape.Fields))
	}
}

func TestShapeJSONSelectsRecordsAndKeepsDocumentFields(t *testing.T) {
	artifact := harnesslayout.Artifact{Name: "history", Pattern: "*.json", Format: harnesslayout.FormatJSON, Role: harnesslayout.RoleTranscript}
	rec := harnesslayout.NewShapeRecorder(artifact, "h.json")
	doc := `{"id":"x","messages":[{"role":"user"},{"role":"assistant"},{"role":"assistant"}]}`
	_, err := harnesslayout.ShapeJSON(strings.NewReader(doc), rec, func(d any) []any {
		return harnesslayout.ArrayField(d, "messages")
	}, harnesslayout.KindField("role"))
	if err != nil {
		t.Fatal(err)
	}
	shape := rec.Shape()
	if shape.Records != 3 || shape.Kinds["assistant"] != 2 {
		t.Fatalf("shape = %+v", shape)
	}
	paths := make([]string, 0, len(shape.Fields))
	for _, field := range shape.Fields {
		paths = append(paths, field.Path)
	}
	if strings.Join(paths, " ") != "$ $.id $.messages $.messages[] $.messages[].role" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestParseTimeAcceptsSecondsMillisAndRFC3339(t *testing.T) {
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, value := range []any{"2026-01-01T00:00:00Z", json.Number("1767225600"), json.Number("1767225600000"), float64(1767225600000), "1767225600000"} {
		if got := harnesslayout.ParseTime(value); !got.Equal(want) {
			t.Errorf("ParseTime(%#v) = %v", value, got)
		}
	}
	if got := harnesslayout.ParseTime("not a time"); !got.IsZero() {
		t.Errorf("ParseTime(garbage) = %v", got)
	}
}

type missingFS struct{}

func (missingFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

type failingProbe struct{ exampleProbe }

func (failingProbe) Capture(context.Context, harnesslayout.Source, harnesslayout.SessionRef) (harnesslayout.Capture, error) {
	return harnesslayout.Capture{}, fs.ErrPermission
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}
