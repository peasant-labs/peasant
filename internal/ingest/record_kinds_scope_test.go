package ingest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	_ "embed"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

//go:embed testdata/record_kinds_scope.yaml
var recordKindsScopeYAML []byte

// recordKindConsumerScopeTokens are the naming fragments the registry and the
// harvest report must never carry. Rendering belongs to internal/transcript and
// to Fairtrade; the registry describes parser behavior and the report counts it.
// The bare "render" stem is included so a future RenderGraph/Render/Display-style
// carrier field is caught even when no longer stem matches.
var recordKindConsumerScopeTokens = []string{"renderer", "rendered", "render", "visual", "viewer"}

// recordKindScopeSurfaces are the production report/session types whose whole
// reachable graph the guard walks. Adding a type here puts its nested fields
// under the same boundary; keeping the list deliberately closed avoids a
// reflection walk that would flag unrelated schema, time, or indexformat types.
func recordKindScopeSurfaces() []any {
	return []any{
		RecordKindRegistry{},
		RecordKindHarness{},
		RecordKindInventory{},
		RecordKindSource{},
		RecordKindVersions{},
		RecordKind{},
		RecordKindRefusalCount{},
		RetainedUnknownKindCount{},
		PipelineSummary{},
		PipelineResult{},
	}
}

// recordKindScopeFieldNames yields the names one field exposes: its Go field
// name and its whole serialized tag. The whole tag is scanned rather than one
// key at a time, so a serialized name is caught whichever tag namespace carries
// it.
func recordKindScopeFieldNames(field reflect.StructField) []string {
	return []string{field.Name, string(field.Tag)}
}

// recordKindDescendable reports whether the walk expands this named type's own
// fields. Only types owned by this package are expanded. Schema-source types
// (github.com/peasant-labs/schema) and other module types (time, indexformat)
// are reached as leaf values but never expanded: their fields are the wire
// contract's and the libraries' concern, and walking them would flag unrelated
// shapes such as time.Time internals.
func recordKindDescendable(t reflect.Type) bool {
	return strings.HasSuffix(t.PkgPath(), "/internal/ingest")
}

// walkRecordKindSurface reflects one surface type and everything reachable from
// its exported fields: nested structs through pointers, slices, arrays, maps,
// and map keys. Each named type is expanded at most once per surface, so shared
// types are scanned once and a type cycle cannot loop.
func walkRecordKindSurface(t reflect.Type, visited map[reflect.Type]bool, fail func(typeName, name string)) {
	// Container kinds are walked through their element types (and the key type
	// of a map): the reachable struct shapes sit behind them.
	switch t.Kind() {
	case reflect.Pointer:
		walkRecordKindSurface(t.Elem(), visited, fail)
		return
	case reflect.Slice, reflect.Array:
		walkRecordKindSurface(t.Elem(), visited, fail)
		return
	case reflect.Map:
		walkRecordKindSurface(t.Key(), visited, fail)
		walkRecordKindSurface(t.Elem(), visited, fail)
		return
	case reflect.Chan, reflect.Func, reflect.Interface:
		return // dynamic or non-data kinds: nothing declarative to scan
	}
	if t == reflect.TypeOf(time.Time{}) || t == reflect.TypeOf(time.Duration(0)) {
		return
	}
	if t == reflect.TypeOf(indexformat.Outcome(0)) {
		return
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if visited[t] {
		return
	}
	visited[t] = true
	if !recordKindDescendable(t) {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		for _, name := range recordKindScopeFieldNames(field) {
			lowered := strings.ToLower(name)
			for _, token := range recordKindConsumerScopeTokens {
				if strings.Contains(lowered, token) {
					// One finding per name: the most specific stem in list
					// order reports, so a renderer row is never counted twice.
					fail(t.Name()+"."+field.Name, name)
					break
				}
			}
		}
		if field.PkgPath != "" {
			continue // unexported field: not part of the serialized surface
		}
		walkRecordKindSurface(field.Type, visited, fail)
	}
}

// TestRecordKindSurfaceCarriesNoVisualizationState is the standing guard for the
// consumer boundary. The registry and the harvest report are parser-facing, so
// a renderer name, visualization flag, or viewer-coverage field added to either
// surface — at the top level, or reachable through any nested struct, pointer,
// slice, or map field — would leak Fairtrade's presentation state into a
// reporting artifact. The walk is deliberately bounded: it expands only types
// owned by this package, stopping at schema, time, and indexformat types whose
// shapes other repositories and libraries own.
func TestRecordKindSurfaceCarriesNoVisualizationState(t *testing.T) {
	for _, surface := range recordKindScopeSurfaces() {
		value := reflect.ValueOf(surface)
		if value.Kind() != reflect.Struct {
			t.Fatalf("%T is not a struct", surface)
		}
		visited := make(map[reflect.Type]bool)
		walkRecordKindSurface(value.Type(), visited, func(typeName, name string) {
			t.Errorf("%s carries %q: renderer and viewer state stay out of the registry and the harvest report", typeName, name)
		})
	}
}

// recordKindScopeFixtureDoc holds the scope-walk mutation manifest. See
// testdata/record_kinds_scope.yaml: the required-name manifest names every case.
type recordKindScopeFixtureDoc struct {
	RequiredNames []string `yaml:"required_names"`
}

// recordKindScopeWalker states one mutation witness: a build function
// reproducing the contaminated shape in miniature, and the exact field path and
// name the walk must flag, so a fixture states the expectation and the walker
// supplies the detection.
type recordKindScopeWalker interface {
	build(t *testing.T) any
	want() (typePath string, name string)
}

// recordKindScopeMutationWitnesses are the shapes
// TestRecordKindSurfaceMutationWitnesses feeds through the same walker. Each
// witness embeds the real production component the boundary guards
// (SessionResult, IndexLogEntry, IndexCoverage), reaches it through the same
// composite shape the report surface uses — a slice element, a slice element of
// the log, and a pointer — and appends the one visualization field a radioactive
// edit would add. The exact-one assertion also proves nothing inside the
// embedded production component is flagged.
var recordKindScopeMutationWitnesses = map[string]recordKindScopeWalker{
	"nested session result carries render graph":   recordKindScopeSessionWitness{},
	"nested index log entry carries viewer field":  recordKindScopeIndexLogWitness{},
	"nested index coverage carries rendered field": recordKindScopeCoverageWitness{},
}

// recordKindScopeSessionWitness mirrors the report's session rows:
// PipelineResult.Sessions is a slice whose elements are SessionResult values,
// and the appended RenderGraph-style field sits two hops below the root.
type recordKindScopeSessionWitness struct{}

func (recordKindScopeSessionWitness) build(*testing.T) any {
	return recordKindScopeProbeSessionResult{}
}

func (recordKindScopeSessionWitness) want() (string, string) {
	return "recordKindScopeProbeSession.RenderGraph", "RenderGraph"
}

type recordKindScopeProbeSessionResult struct {
	Sessions []recordKindScopeProbeSession
}

type recordKindScopeProbeSession struct {
	SessionResult // the real production row; its own fields must stay clean
	RenderGraph   string
}

// recordKindScopeIndexLogWitness mirrors the report's index log: elements of
// PipelineResult.IndexLog embed IndexLogEntry, and the appended viewer field
// hangs off that row.
type recordKindScopeIndexLogWitness struct{}

func (recordKindScopeIndexLogWitness) build(*testing.T) any {
	return recordKindScopeProbeIndexLogEntry{}
}

func (recordKindScopeIndexLogWitness) want() (string, string) {
	return "recordKindScopeProbeLogEntry.ViewerCount", "ViewerCount"
}

type recordKindScopeProbeIndexLogEntry struct {
	IndexLog []recordKindScopeProbeLogEntry
}

type recordKindScopeProbeLogEntry struct {
	IndexLogEntry // the real production row; its own fields must stay clean
	ViewerCount   int
}

// recordKindScopeCoverageWitness mirrors the report's coverage pointer:
// PipelineResult.IndexCoverage is a pointer whose target embeds IndexCoverage,
// and the appended rendered field hangs off that row.
type recordKindScopeCoverageWitness struct{}

func (recordKindScopeCoverageWitness) build(*testing.T) any {
	return recordKindScopeProbeCoverageHolder{}
}

func (recordKindScopeCoverageWitness) want() (string, string) {
	return "recordKindScopeProbeCoverage.RenderedState", "RenderedState"
}

type recordKindScopeProbeCoverageHolder struct {
	Coverage *recordKindScopeProbeCoverage
}

type recordKindScopeProbeCoverage struct {
	IndexCoverage // the real production row; its own fields must stay clean
	RenderedState string
}

// TestRecordKindSurfaceMutationWitnesses proves the recursive guard catches the
// contaminated shapes. Each YAML-named witness is fed through the same walker
// the standing guard uses; the detection must name exactly the contaminated
// field and nothing else, so a walk that silently stopped recursing, skipped
// pointers, or lost the render stem turns red here.
func TestRecordKindSurfaceMutationWitnesses(t *testing.T) {
	fixture := recordKindScopeFixtureDoc{}
	decodeRegistryFixture(t, recordKindsScopeYAML, &fixture)
	names := make(map[string]bool, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" || names[name] {
			t.Fatalf("empty or duplicate required scope name %q", name)
		}
		names[name] = true
	}
	checkRegistryFixtureNames(t, names, fixture.RequiredNames)
	for witnessName, walker := range recordKindScopeMutationWitnesses {
		if !names[witnessName] {
			t.Errorf("witness %q absent from the required-name manifest", witnessName)
			continue
		}
		t.Run(witnessName, func(t *testing.T) {
			shape := walker.build(t)
			visited := make(map[reflect.Type]bool)
			var findings []string
			walkRecordKindSurface(reflect.TypeOf(shape), visited, func(typeName, found string) {
				findings = append(findings, typeName+" carries "+found)
			})
			wantType, wantName := walker.want()
			want := wantType + " carries " + wantName
			if len(findings) != 1 || findings[0] != want {
				t.Errorf("findings %v, want exactly [%s]", findings, want)
			}
		})
	}
}
