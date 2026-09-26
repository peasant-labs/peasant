package fsdecorator

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

// This file freezes the decorator contract. contract_shapes.yaml is the expected
// shape, contract_mutations.yaml proves every mutation axis is detected, and
// classification_cases.yaml validates the classification schema. The drift
// guards assert that the mirrored FileSystem and the operation vocabulary match
// the production and existing declarations they must not diverge from.

//go:embed testdata/contract_shapes.yaml
var contractShapesYAML []byte

//go:embed testdata/contract_mutations.yaml
var contractMutationsYAML []byte

//go:embed testdata/classification_cases.yaml
var classificationCasesYAML []byte

// --- Compile-time conformance pins ----------------------------------------
//
// Each stub provides exactly the control methods its interface adds on top of
// FileSystem. Adding a control method to an interface breaks the build here; the
// runtime interface fixture catches a removal or a signature change.

type fileSystemStub struct{ FileSystem }

var _ FileSystem = fileSystemStub{}

// The internal/testutil owner's existing decorator already conforms to the
// shared base: it is an ingest.FileSystem, and the base is pinned equal to
// ingest.FileSystem below.
var _ FileSystem = (*testutil.CountingFS)(nil)

type gatedStub struct{ FileSystem }

func (gatedStub) Arm(Gate)                 {}
func (gatedStub) Reached() <-chan struct{} { return nil }
func (gatedStub) Release()                 {}

var _ GatedFS = gatedStub{}

type boundedStub struct{ FileSystem }

func (boundedStub) Limit(Bound) {}
func (boundedStub) ReadOnly()   {}

var _ BoundedFS = boundedStub{}

// --- Fixture types ---------------------------------------------------------

type contractField struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	JSON string `yaml:"json"`
	YAML string `yaml:"yaml"`
}

type contractShape struct {
	Name   string          `yaml:"name"`
	Fields []contractField `yaml:"fields"`
}

type contractEnum struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

type methodSpec struct {
	Name string `yaml:"name"`
	Sig  string `yaml:"sig"`
}

type contractInterface struct {
	Name    string       `yaml:"name"`
	Methods []methodSpec `yaml:"methods"`
}

type contractShapeFile struct {
	RequiredNames []string            `yaml:"required_names"`
	Shapes        []contractShape     `yaml:"shapes"`
	Enums         []contractEnum      `yaml:"enums"`
	Interfaces    []contractInterface `yaml:"interfaces"`
}

type contractMutation struct {
	Name   string `yaml:"name"`
	Target string `yaml:"target"`
	Op     string `yaml:"op"`
	Field  string `yaml:"field"`
	To     string `yaml:"to"`
	With   string `yaml:"with"`
	Tag    string `yaml:"tag"`
	Value  string `yaml:"value"`
}

type contractMutationFile struct {
	RequiredNames []string           `yaml:"required_names"`
	Mutations     []contractMutation `yaml:"mutations"`
}

type classificationCase struct {
	Name      string                `yaml:"name"`
	Version   int                   `yaml:"version"`
	WantError string                `yaml:"want_error"`
	Entries   []ClassificationEntry `yaml:"entries"`
}

type classificationCaseFile struct {
	RequiredNames []string             `yaml:"required_names"`
	Cases         []classificationCase `yaml:"cases"`
}

func decodeFixture(data []byte, target any, source string) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", source, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one YAML document, got %v", source, err)
	}
	return nil
}

func frozenShapes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"fsdecorator.Gate":                reflect.TypeOf(Gate{}),
		"fsdecorator.Bound":               reflect.TypeOf(Bound{}),
		"fsdecorator.ClassificationEntry": reflect.TypeOf(ClassificationEntry{}),
		"fsdecorator.Classification":      reflect.TypeOf(Classification{}),
	}
}

func frozenEnums() map[string][]string {
	ops := make([]string, 0, len(AllOps))
	for _, o := range AllOps {
		ops = append(ops, string(o))
	}
	caps := make([]string, 0, len(AllCapabilities))
	for _, c := range AllCapabilities {
		caps = append(caps, string(c))
	}
	return map[string][]string{
		"fsdecorator.AllOps":          ops,
		"fsdecorator.AllCapabilities": caps,
	}
}

func frozenInterfaces() map[string][]methodSpec {
	return map[string][]methodSpec{
		"fsdecorator.FileSystem": interfaceMethods(reflect.TypeOf((*FileSystem)(nil)).Elem()),
		"fsdecorator.GatedFS":    interfaceMethods(reflect.TypeOf((*GatedFS)(nil)).Elem()),
		"fsdecorator.BoundedFS":  interfaceMethods(reflect.TypeOf((*BoundedFS)(nil)).Elem()),
	}
}

func interfaceMethods(typ reflect.Type) []methodSpec {
	out := make([]methodSpec, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		out = append(out, methodSpec{Name: m.Name, Sig: m.Type.String()})
	}
	return out
}

func reflectShape(typ reflect.Type) []contractField {
	out := make([]contractField, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		out = append(out, contractField{Name: f.Name, Type: f.Type.String(), JSON: f.Tag.Get("json"), YAML: f.Tag.Get("yaml")})
	}
	return out
}

func compareShape(name string, want, got []contractField) []string {
	var problems []string
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			problems = append(problems, fmt.Sprintf("%s: field %d %q (type %s) is missing", name, i, want[i].Name, want[i].Type))
		case i >= len(want):
			problems = append(problems, fmt.Sprintf("%s: unexpected field %d %q (type %s)", name, i, got[i].Name, got[i].Type))
		default:
			w, g := want[i], got[i]
			if w.Name != g.Name {
				problems = append(problems, fmt.Sprintf("%s: field %d is %q, want %q", name, i, g.Name, w.Name))
			}
			if w.Type != g.Type {
				problems = append(problems, fmt.Sprintf("%s.%s: type is %s, want %s", name, g.Name, g.Type, w.Type))
			}
			if w.JSON != g.JSON {
				problems = append(problems, fmt.Sprintf("%s.%s: json tag is %q, want %q", name, g.Name, g.JSON, w.JSON))
			}
			if w.YAML != g.YAML {
				problems = append(problems, fmt.Sprintf("%s.%s: yaml tag is %q, want %q", name, g.Name, g.YAML, w.YAML))
			}
		}
	}
	return problems
}

func compareMethods(name string, want, got []methodSpec) []string {
	var problems []string
	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			problems = append(problems, fmt.Sprintf("%s: method %q is missing", name, want[i].Name))
		case i >= len(want):
			problems = append(problems, fmt.Sprintf("%s: unexpected method %q", name, got[i].Name))
		default:
			w, g := want[i], got[i]
			if w.Name != g.Name {
				problems = append(problems, fmt.Sprintf("%s: method %d is %q, want %q", name, i, g.Name, w.Name))
			}
			if w.Sig != g.Sig {
				problems = append(problems, fmt.Sprintf("%s.%s: signature is %q, want %q", name, g.Name, g.Sig, w.Sig))
			}
		}
	}
	return problems
}

func loadShapeFile(t *testing.T) contractShapeFile {
	t.Helper()
	var file contractShapeFile
	if err := decodeFixture(contractShapesYAML, &file, "decorator contract shapes fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("decorator contract shapes fixture declares no required_names manifest")
	}
	names := map[string]bool{}
	register := func(name string) {
		if name == "" || names[name] {
			t.Fatalf("decorator contract shapes fixture has an empty or duplicate name %q", name)
		}
		names[name] = true
	}
	for _, s := range file.Shapes {
		register(s.Name)
	}
	for _, e := range file.Enums {
		register(e.Name)
	}
	for _, i := range file.Interfaces {
		register(i.Name)
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required decorator contract %q is missing from the fixture", want)
		}
	}
	return file
}

func loadMutationFile(t *testing.T) contractMutationFile {
	t.Helper()
	var file contractMutationFile
	if err := decodeFixture(contractMutationsYAML, &file, "decorator contract mutations fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("decorator contract mutations fixture declares no required_names manifest")
	}
	seen := map[string]bool{}
	for _, m := range file.Mutations {
		if m.Name == "" || seen[m.Name] {
			t.Fatalf("decorator contract mutations fixture has an empty or duplicate case name %q", m.Name)
		}
		seen[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !seen[want] {
			t.Fatalf("required decorator mutation case %q is missing from the fixture", want)
		}
	}
	return file
}

func TestContract_DecoratorShapesAndInterfacesMatch(t *testing.T) {
	file := loadShapeFile(t)
	shapes := frozenShapes()
	for _, want := range file.Shapes {
		typ, ok := shapes[want.Name]
		if !ok {
			t.Fatalf("frozen shape %q has no reflect entry; add it to frozenShapes()", want.Name)
		}
		if problems := compareShape(want.Name, want.Fields, reflectShape(typ)); len(problems) > 0 {
			t.Fatalf("the frozen shape %s moved:\n  %s", want.Name, strings.Join(problems, "\n  "))
		}
	}
	enums := frozenEnums()
	for _, want := range file.Enums {
		got, ok := enums[want.Name]
		if !ok {
			t.Fatalf("frozen enum %q has no entry; add it to frozenEnums()", want.Name)
		}
		if !reflect.DeepEqual(want.Members, got) {
			t.Fatalf("the closed set %s moved:\n  got  %v\n  want %v", want.Name, got, want.Members)
		}
	}
	interfaces := frozenInterfaces()
	for _, want := range file.Interfaces {
		got, ok := interfaces[want.Name]
		if !ok {
			t.Fatalf("frozen interface %q has no reflect entry; add it to frozenInterfaces()", want.Name)
		}
		if problems := compareMethods(want.Name, want.Methods, got); len(problems) > 0 {
			t.Fatalf("the frozen interface %s moved:\n  %s", want.Name, strings.Join(problems, "\n  "))
		}
	}
}

// TestFileSystemMirrorsIngest pins the mirrored base to the production
// declaration. A change to ingest.FileSystem fails here until fsdecorator is
// reconciled, so the two method sets cannot drift silently.
func TestFileSystemMirrorsIngest(t *testing.T) {
	want := interfaceMethods(reflect.TypeOf((*ingest.FileSystem)(nil)).Elem())
	got := interfaceMethods(reflect.TypeOf((*FileSystem)(nil)).Elem())
	if problems := compareMethods("fsdecorator.FileSystem vs ingest.FileSystem", want, got); len(problems) > 0 {
		t.Fatalf("the decorator base drifted from ingest.FileSystem:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestOpVocabularyMatchesTestutil pins the canonical operation set to the one
// CountingFS already uses, so the later migration of FSOp onto Op cannot
// silently change the vocabulary.
func TestOpVocabularyMatchesTestutil(t *testing.T) {
	want := make([]string, 0, len(testutil.AllFSOps))
	for _, op := range testutil.AllFSOps {
		want = append(want, string(op))
	}
	got := make([]string, 0, len(AllOps))
	for _, op := range AllOps {
		got = append(got, string(op))
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the operation vocabulary drifted from testutil:\n  got  %v\n  want %v", got, want)
	}
}

func TestContract_DecoratorMutationsAreDetected(t *testing.T) {
	file := loadShapeFile(t)
	mutations := loadMutationFile(t)
	shapes := frozenShapes()
	enums := frozenEnums()
	interfaces := frozenInterfaces()

	shapeFields := map[string][]contractField{}
	for _, s := range file.Shapes {
		shapeFields[s.Name] = append([]contractField(nil), s.Fields...)
	}
	enumMembers := map[string][]string{}
	for _, e := range file.Enums {
		enumMembers[e.Name] = append([]string(nil), e.Members...)
	}
	ifaceMethods := map[string][]methodSpec{}
	for _, i := range file.Interfaces {
		ifaceMethods[i.Name] = append([]methodSpec(nil), i.Methods...)
	}

	for _, m := range mutations.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			switch {
			case enumMembers[m.Target] != nil:
				got := enums[m.Target]
				mutated, err := mutateEnum(enumMembers[m.Target], m)
				if err != nil {
					t.Fatal(err)
				}
				if reflect.DeepEqual(mutated, got) {
					t.Fatalf("the %s mutation of %s was NOT detected", m.Op, m.Target)
				}
			case ifaceMethods[m.Target] != nil:
				got := interfaces[m.Target]
				mutated, err := mutateMethods(ifaceMethods[m.Target], m)
				if err != nil {
					t.Fatal(err)
				}
				if len(compareMethods(m.Target, mutated, got)) == 0 {
					t.Fatalf("the %s mutation of %s was NOT detected", m.Op, m.Target)
				}
			default:
				want, ok := shapeFields[m.Target]
				if !ok {
					t.Fatalf("mutation target %q is not a frozen shape, enum, or interface", m.Target)
				}
				typ, ok := shapes[m.Target]
				if !ok {
					t.Fatalf("frozen shape %q has no reflect entry", m.Target)
				}
				mutated, err := mutateShape(want, m)
				if err != nil {
					t.Fatal(err)
				}
				if len(compareShape(m.Target, mutated, reflectShape(typ))) == 0 {
					t.Fatalf("the %s mutation of %s was NOT detected", m.Op, m.Target)
				}
			}
		})
	}
}

func mutateShape(fields []contractField, m contractMutation) ([]contractField, error) {
	out := append([]contractField(nil), fields...)
	idx := -1
	for i, f := range out {
		if f.Name == m.Field {
			idx = i
			break
		}
	}
	switch m.Op {
	case "drop":
		if idx < 0 {
			return nil, fmt.Errorf("mutation %q names field %q not in the shape", m.Name, m.Field)
		}
		return append(out[:idx], out[idx+1:]...), nil
	case "rename", "retype":
		if idx < 0 {
			return nil, fmt.Errorf("mutation %q names field %q not in the shape", m.Name, m.Field)
		}
		if m.Op == "rename" {
			out[idx].Name = m.To
		} else {
			out[idx].Type = m.To
		}
		return out, nil
	case "retag":
		if idx < 0 {
			return nil, fmt.Errorf("mutation %q names field %q not in the shape", m.Name, m.Field)
		}
		switch m.Tag {
		case "json":
			out[idx].JSON = m.Value
		case "yaml":
			out[idx].YAML = m.Value
		default:
			return nil, fmt.Errorf("mutation %q names unknown tag %q", m.Name, m.Tag)
		}
		return out, nil
	case "reorder":
		with := -1
		for i, f := range out {
			if f.Name == m.With {
				with = i
				break
			}
		}
		if idx < 0 || with < 0 {
			return nil, fmt.Errorf("mutation %q names fields %q/%q not both in the shape", m.Name, m.Field, m.With)
		}
		out[idx], out[with] = out[with], out[idx]
		return out, nil
	default:
		return nil, fmt.Errorf("mutation %q has unknown op %q", m.Name, m.Op)
	}
}

func mutateEnum(members []string, m contractMutation) ([]string, error) {
	out := append([]string(nil), members...)
	indexOf := func(needle string) int {
		for i, v := range out {
			if v == needle {
				return i
			}
		}
		return -1
	}
	switch m.Op {
	case "drop":
		i := indexOf(m.Field)
		if i < 0 {
			return nil, fmt.Errorf("mutation %q names member %q not in the set", m.Name, m.Field)
		}
		return append(out[:i], out[i+1:]...), nil
	case "reorder":
		i, j := indexOf(m.Field), indexOf(m.With)
		if i < 0 || j < 0 {
			return nil, fmt.Errorf("mutation %q names members %q/%q not both in the set", m.Name, m.Field, m.With)
		}
		out[i], out[j] = out[j], out[i]
		return out, nil
	default:
		return nil, fmt.Errorf("mutation %q has unknown enum op %q", m.Name, m.Op)
	}
}

func mutateMethods(methods []methodSpec, m contractMutation) ([]methodSpec, error) {
	out := append([]methodSpec(nil), methods...)
	idx := -1
	for i, s := range out {
		if s.Name == m.Field {
			idx = i
			break
		}
	}
	switch m.Op {
	case "drop-method":
		if idx < 0 {
			return nil, fmt.Errorf("mutation %q names method %q not in the interface", m.Name, m.Field)
		}
		return append(out[:idx], out[idx+1:]...), nil
	case "retype-method":
		if idx < 0 {
			return nil, fmt.Errorf("mutation %q names method %q not in the interface", m.Name, m.Field)
		}
		out[idx].Sig = m.To
		return out, nil
	default:
		return nil, fmt.Errorf("mutation %q has unknown interface op %q", m.Name, m.Op)
	}
}

func loadClassificationCases(t *testing.T) classificationCaseFile {
	t.Helper()
	var file classificationCaseFile
	if err := decodeFixture(classificationCasesYAML, &file, "classification cases fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("classification cases fixture declares no required_names manifest")
	}
	seen := map[string]bool{}
	for _, c := range file.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("classification cases fixture has an empty or duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !seen[want] {
			t.Fatalf("required classification case %q is missing from the fixture", want)
		}
	}
	return file
}

func TestClassification_ValidationCases(t *testing.T) {
	file := loadClassificationCases(t)
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			err := ValidateClassification(Classification{Version: c.Version, Entries: c.Entries})
			if c.WantError == "" {
				if err != nil {
					t.Fatalf("valid classification was rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("case %q was accepted; want error containing %q", c.Name, c.WantError)
			}
			if !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("case %q failed for the wrong reason:\n  got: %v\n want: %q", c.Name, err, c.WantError)
			}
		})
	}
}

// TestClassification_OwnerRouting proves the white_box flag routes a decorator
// to the owner that can implement it without an import cycle.
func TestClassification_OwnerRouting(t *testing.T) {
	whiteBox := ClassificationEntry{WhiteBox: true}
	if got := whiteBox.Owner(); got != OwnerIngestWhiteBox {
		t.Fatalf("white-box entry routed to %q, want %q", got, OwnerIngestWhiteBox)
	}
	shared := ClassificationEntry{WhiteBox: false}
	if got := shared.Owner(); got != OwnerTestutil {
		t.Fatalf("shared entry routed to %q, want %q", got, OwnerTestutil)
	}
}

func TestClassification_RoundTripsThroughYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/classification.yaml"
	body := "version: 1\nentries:\n  - type: gatedPairFS\n    package: internal/ingest\n    white_box: false\n    capability: gated\n    where: internal/ingest/pair_concurrency_test.go:18\n    rationale: holds a writer mid-install\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cls, err := LoadClassification(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := ValidateClassification(cls); err != nil {
		t.Fatalf("the round-tripped classification must validate: %v", err)
	}
	if len(cls.Entries) != 1 || cls.Entries[0].Capability != CapabilityGated || cls.Entries[0].Owner() != OwnerTestutil {
		t.Fatalf("round-trip lost the entry: %+v", cls)
	}
}
