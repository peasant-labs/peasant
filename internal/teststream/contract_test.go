package teststream

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

// This file freezes the shared stream library's exported shape. The
// expected shapes live in testdata/contract_shapes.yaml; a deterministic
// mutation of each axis is proven to be detected via testdata/contract_mutations.yaml.
// The compile-time pins live in contract_compile_test.go.

//go:embed testdata/contract_shapes.yaml
var contractShapesYAML []byte

//go:embed testdata/contract_mutations.yaml
var contractMutationsYAML []byte

type contractField struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Tag  string `yaml:"tag"`
}

type contractShape struct {
	Name   string          `yaml:"name"`
	Fields []contractField `yaml:"fields"`
}

type contractShapeFile struct {
	RequiredNames []string        `yaml:"required_names"`
	Shapes        []contractShape `yaml:"shapes"`
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

func frozenShapes() map[string]reflect.Type {
	return map[string]reflect.Type{
		"teststream.Event":         reflect.TypeOf(Event{}),
		"teststream.Record":        reflect.TypeOf(Record{}),
		"teststream.ReportOptions": reflect.TypeOf(ReportOptions{}),
	}
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

func loadShapes(t *testing.T) contractShapeFile {
	t.Helper()
	var file contractShapeFile
	if err := decodeFixture(contractShapesYAML, &file, "stream contract shapes fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("stream contract shapes fixture declares no required_names manifest")
	}
	names := map[string]bool{}
	for _, s := range file.Shapes {
		if s.Name == "" || names[s.Name] {
			t.Fatalf("stream contract shapes fixture has an empty or duplicate shape name %q", s.Name)
		}
		names[s.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required stream contract %q is missing from the fixture", want)
		}
	}
	return file
}

func loadMutations(t *testing.T) contractMutationFile {
	t.Helper()
	var file contractMutationFile
	if err := decodeFixture(contractMutationsYAML, &file, "stream contract mutations fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("stream contract mutations fixture declares no required_names manifest")
	}
	seen := map[string]bool{}
	for _, m := range file.Mutations {
		if m.Name == "" || seen[m.Name] {
			t.Fatalf("stream contract mutations fixture has an empty or duplicate case name %q", m.Name)
		}
		seen[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !seen[want] {
			t.Fatalf("required stream mutation case %q is missing from the fixture", want)
		}
	}
	return file
}

func reflectShape(typ reflect.Type) []contractField {
	out := make([]contractField, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		out = append(out, contractField{Name: f.Name, Type: f.Type.String(), Tag: string(f.Tag)})
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
			if w.Tag != g.Tag {
				problems = append(problems, fmt.Sprintf("%s.%s: tag is %q, want %q", name, g.Name, g.Tag, w.Tag))
			}
		}
	}
	return problems
}

func TestContract_StreamShapesMatch(t *testing.T) {
	file := loadShapes(t)
	shapes := frozenShapes()
	for _, want := range file.Shapes {
		typ, ok := shapes[want.Name]
		if !ok {
			t.Fatalf("frozen stream shape %q has no reflect entry; add it to frozenShapes()", want.Name)
		}
		if problems := compareShape(want.Name, want.Fields, reflectShape(typ)); len(problems) > 0 {
			t.Fatalf("the frozen shape %s moved:\n  %s", want.Name, strings.Join(problems, "\n  "))
		}
	}
}

func TestContract_StreamMutationsAreDetected(t *testing.T) {
	file := loadShapes(t)
	mutations := loadMutations(t)
	shapes := frozenShapes()
	fields := map[string][]contractField{}
	for _, s := range file.Shapes {
		fields[s.Name] = append([]contractField(nil), s.Fields...)
	}
	for _, m := range mutations.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			want, ok := fields[m.Target]
			if !ok {
				t.Fatalf("mutation target %q is not a frozen stream shape", m.Target)
			}
			typ, ok := shapes[m.Target]
			if !ok {
				t.Fatalf("frozen stream shape %q has no reflect entry", m.Target)
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
