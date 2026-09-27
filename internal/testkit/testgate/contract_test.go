package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file freezes the gate contract: the exported gate shapes a consumer
// compiles against. It has two halves.
//
//   - contract_compile_test.go pins names and types at COMPILE time: a rename,
//     removal, or retype breaks that file's build.
//   - this file pins the same shapes at RUNTIME against a committed fixture, so
//     struct tags and field order (which a compile-time literal cannot see) are
//     also frozen, and every mutation axis is proven to be detected.
//
// The expected shapes live in testdata/contract_shapes.yaml; the mutation cases
// in testdata/contract_mutations.yaml. Neither is an inline table.

//go:embed testdata/contract_shapes.yaml
var contractShapesYAML []byte

//go:embed testdata/contract_mutations.yaml
var contractMutationsYAML []byte

// TestStreamPackagePath is the frozen import path of the shared stream library.
const TestStreamPackagePath = "github.com/peasant-labs/peasant/internal/testkit/teststream"

type contractField struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Tag  string `yaml:"tag"`
}

type contractShape struct {
	Name   string          `yaml:"name"`
	Fields []contractField `yaml:"fields"`
}

type contractEnum struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

type contractShapeFile struct {
	RequiredNames []string        `yaml:"required_names"`
	Shapes        []contractShape `yaml:"shapes"`
	Enums         []contractEnum  `yaml:"enums"`
}

type contractMutation struct {
	Name   string `yaml:"name"`
	Target string `yaml:"target"`
	Op     string `yaml:"op"`
	Field  string `yaml:"field"`
	To     string `yaml:"to"`
	With   string `yaml:"with"`
	Value  string `yaml:"value"`
}

type contractMutationFile struct {
	RequiredNames []string           `yaml:"required_names"`
	Mutations     []contractMutation `yaml:"mutations"`
}

// frozenShapes maps every frozen shape name to the real type. A consumer that
// compiles against the contract also stands in this map.
func frozenShapes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"testgate.Record":          reflect.TypeOf(Record{}),
		"testgate.Invocation":      reflect.TypeOf(Invocation{}),
		"testgate.Runner":          reflect.TypeOf(Runner{}),
		"testgate.RunResult":       reflect.TypeOf(RunResult{}),
		"testgate.Report":          reflect.TypeOf(Report{}),
		"testgate.PassReport":      reflect.TypeOf(PassReport{}),
		"testgate.Calibration":     reflect.TypeOf(Calibration{}),
		"testgate.ReportRecord":    reflect.TypeOf(ReportRecord{}),
		"testgate.ReportTest":      reflect.TypeOf(ReportTest{}),
		"testgate.ReportFinding":   reflect.TypeOf(ReportFinding{}),
		"testgate.ClassRow":        reflect.TypeOf(ClassRow{}),
		"testgate.StepMeasurement": reflect.TypeOf(StepMeasurement{}),
		"testgate.PreTestCommand":  reflect.TypeOf(PreTestCommand{}),
		"testgate.Cost":            reflect.TypeOf(Cost{}),
		"testgate.Entry":           reflect.TypeOf(Entry{}),
		"testgate.Registry":        reflect.TypeOf(Registry{}),
		"testgate.Budget":          reflect.TypeOf(Budget{}),
		"testgate.Finding":         reflect.TypeOf(Finding{}),
	}
}

// frozenEnums maps every frozen closed set to its members in contract order.
func frozenEnums() map[string][]string {
	preTest := make([]string, 0, len(PreTestSteps))
	for _, s := range PreTestSteps {
		preTest = append(preTest, string(s))
	}
	return map[string][]string{
		"testgate.RegistryClasses": RegistryClassNames(),
		"testgate.PreTestSteps":    preTest,
		"testgate.PassModeValues":  {ModeRace.String(), ModeNoRace.String()},
	}
}

func decodeContractFixture(data []byte, target any, source string) error {
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

func loadContractShapes(t *testing.T) contractShapeFile {
	t.Helper()
	var file contractShapeFile
	if err := decodeContractFixture(contractShapesYAML, &file, "contract shapes fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("contract shapes fixture declares no required_names manifest")
	}
	names := map[string]bool{}
	for _, s := range file.Shapes {
		if s.Name == "" || names[s.Name] {
			t.Fatalf("contract shapes fixture has an empty or duplicate shape name %q", s.Name)
		}
		names[s.Name] = true
	}
	for _, e := range file.Enums {
		if e.Name == "" || names[e.Name] {
			t.Fatalf("contract shapes fixture has an empty or duplicate enum name %q", e.Name)
		}
		names[e.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required contract %q is missing from the fixture", want)
		}
	}
	return file
}

func loadContractMutations(t *testing.T) contractMutationFile {
	t.Helper()
	var file contractMutationFile
	if err := decodeContractFixture(contractMutationsYAML, &file, "contract mutations fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("contract mutations fixture declares no required_names manifest")
	}
	byName := map[string]bool{}
	for _, m := range file.Mutations {
		if m.Name == "" || byName[m.Name] {
			t.Fatalf("contract mutations fixture has an empty or duplicate case name %q", m.Name)
		}
		byName[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !byName[want] {
			t.Fatalf("required mutation case %q is missing from the fixture", want)
		}
	}
	return file
}

// reflectShape reads the ordered field set of a struct, including tags.
func reflectShape(typ reflect.Type) []contractField {
	out := make([]contractField, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		out = append(out, contractField{
			Name: f.Name,
			Type: f.Type.String(),
			Tag:  string(f.Tag),
		})
	}
	return out
}

// compareShape returns one problem string per divergent field, or nil when the
// shapes are identical in order, name, type, and tags.
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
			if w.Tag != g.Tag {
				problems = append(problems, fmt.Sprintf("%s.%s: tag is %q, want %q", name, g.Name, g.Tag, w.Tag))
			}
		}
	}
	return problems
}

func compareEnum(name string, want, got []string) []string {
	if reflect.DeepEqual(want, got) {
		return nil
	}
	return []string{fmt.Sprintf("%s: members are %v, want %v", name, got, want)}
}

func TestContract_FrozenShapesMatch(t *testing.T) {
	file := loadContractShapes(t)
	shapes := frozenShapes()
	for _, want := range file.Shapes {
		typ, ok := shapes[want.Name]
		if !ok {
			t.Fatalf("frozen shape %q has no reflect entry; add it to frozenShapes()", want.Name)
		}
		if problems := compareShape(want.Name, want.Fields, reflectShape(typ)); len(problems) > 0 {
			t.Fatalf("the frozen shape %s moved:\n  %s\nUpdate testdata/contract_shapes.yaml only when a contract change is deliberate and consumers are re-pinned.", want.Name, strings.Join(problems, "\n  "))
		}
	}
	enums := frozenEnums()
	for _, want := range file.Enums {
		got, ok := enums[want.Name]
		if !ok {
			t.Fatalf("frozen enum %q has no reflect entry; add it to frozenEnums()", want.Name)
		}
		if problems := compareEnum(want.Name, want.Members, got); len(problems) > 0 {
			t.Fatalf("the frozen closed set %s moved:\n  %s", want.Name, strings.Join(problems, "\n  "))
		}
	}
}

// TestContract_MutationsAreDetected proves the comparator is not vacuous: every
// mutation axis in the fixture must produce a mismatch against the real type.
func TestContract_MutationsAreDetected(t *testing.T) {
	file := loadContractShapes(t)
	mutations := loadContractMutations(t)
	shapes := frozenShapes()
	enums := frozenEnums()

	shapeFields := map[string][]contractField{}
	for _, s := range file.Shapes {
		shapeFields[s.Name] = append([]contractField(nil), s.Fields...)
	}
	enumMembers := map[string][]string{}
	for _, e := range file.Enums {
		enumMembers[e.Name] = append([]string(nil), e.Members...)
	}

	for _, m := range mutations.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			if want, ok := enumMembers[m.Target]; ok {
				got, ok := enums[m.Target]
				if !ok {
					t.Fatalf("mutation target %q is not a frozen enum", m.Target)
				}
				mutated, err := mutateEnum(want, m)
				if err != nil {
					t.Fatal(err)
				}
				if len(compareEnum(m.Target, mutated, got)) == 0 {
					t.Fatalf("the %s mutation of %s was NOT detected; the freeze is vacuous on that axis", m.Op, m.Target)
				}
				return
			}

			want, ok := shapeFields[m.Target]
			if !ok {
				t.Fatalf("mutation target %q is not a frozen shape", m.Target)
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
				t.Fatalf("the %s mutation of %s was NOT detected; the freeze is vacuous on that axis", m.Op, m.Target)
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
		out[idx].Tag = m.Value
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
	switch m.Op {
	case "drop":
		i := indexOf(out, m.Field)
		if i < 0 {
			return nil, fmt.Errorf("mutation %q names member %q not in the set", m.Name, m.Field)
		}
		return append(out[:i], out[i+1:]...), nil
	case "reorder":
		i, j := indexOf(out, m.Field), indexOf(out, m.With)
		if i < 0 || j < 0 {
			return nil, fmt.Errorf("mutation %q names members %q/%q not both in the set", m.Name, m.Field, m.With)
		}
		out[i], out[j] = out[j], out[i]
		return out, nil
	default:
		return nil, fmt.Errorf("mutation %q has unknown enum op %q", m.Name, m.Op)
	}
}

func indexOf(hay []string, needle string) int {
	for i, h := range hay {
		if h == needle {
			return i
		}
	}
	return -1
}

// TestContract_TestStreamPathIsFrozen pins the shared library's import path. A
// move of the package is a contract change: every consumer's import breaks.
func TestContract_TestStreamPathIsFrozen(t *testing.T) {
	if TestStreamPackagePath != "github.com/peasant-labs/peasant/internal/testkit/teststream" {
		t.Fatalf("the shared stream library moved to %q; every consumer must be re-pinned", TestStreamPackagePath)
	}
}
