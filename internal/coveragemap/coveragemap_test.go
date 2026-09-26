package coveragemap

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file freezes the coverage-map contract: the shapes and the destination
// closed set against contract_shapes.yaml, every mutation axis against
// contract_mutations.yaml, and the validators against validation_cases.yaml.

//go:embed testdata/contract_shapes.yaml
var contractShapesYAML []byte

//go:embed testdata/contract_mutations.yaml
var contractMutationsYAML []byte

//go:embed testdata/validation_cases.yaml
var validationCasesYAML []byte

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

type destinationCase struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	WantError string `yaml:"want_error"`
}

type inventoryCase struct {
	Name      string    `yaml:"name"`
	WantError string    `yaml:"want_error"`
	Inventory Inventory `yaml:"inventory"`
}

type mapCase struct {
	Name      string      `yaml:"name"`
	WantError string      `yaml:"want_error"`
	Inventory Inventory   `yaml:"inventory"`
	Map       CoverageMap `yaml:"map"`
}

type validationCaseFile struct {
	RequiredNames    []string          `yaml:"required_names"`
	DestinationCases []destinationCase `yaml:"destination_cases"`
	InventoryCases   []inventoryCase   `yaml:"inventory_cases"`
	MapCases         []mapCase         `yaml:"map_cases"`
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
		"coveragemap.Destination":   reflect.TypeOf(Destination{}),
		"coveragemap.InventoryName": reflect.TypeOf(InventoryName{}),
		"coveragemap.Inventory":     reflect.TypeOf(Inventory{}),
		"coveragemap.MapEntry":      reflect.TypeOf(MapEntry{}),
		"coveragemap.CoverageMap":   reflect.TypeOf(CoverageMap{}),
	}
}

func frozenEnums() map[string][]string {
	kinds := make([]string, 0, len(AllDestinationKinds))
	for _, k := range AllDestinationKinds {
		kinds = append(kinds, string(k))
	}
	return map[string][]string{"coveragemap.AllDestinationKinds": kinds}
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

func loadShapeFile(t *testing.T) contractShapeFile {
	t.Helper()
	var file contractShapeFile
	if err := decodeFixture(contractShapesYAML, &file, "coverage contract shapes fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("coverage contract shapes fixture declares no required_names manifest")
	}
	names := map[string]bool{}
	for _, s := range file.Shapes {
		if s.Name == "" || names[s.Name] {
			t.Fatalf("coverage contract shapes fixture has an empty or duplicate name %q", s.Name)
		}
		names[s.Name] = true
	}
	for _, e := range file.Enums {
		if e.Name == "" || names[e.Name] {
			t.Fatalf("coverage contract shapes fixture has an empty or duplicate enum %q", e.Name)
		}
		names[e.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !names[want] {
			t.Fatalf("required coverage contract %q is missing from the fixture", want)
		}
	}
	return file
}

func loadMutationFile(t *testing.T) contractMutationFile {
	t.Helper()
	var file contractMutationFile
	if err := decodeFixture(contractMutationsYAML, &file, "coverage contract mutations fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("coverage contract mutations fixture declares no required_names manifest")
	}
	seen := map[string]bool{}
	for _, m := range file.Mutations {
		if m.Name == "" || seen[m.Name] {
			t.Fatalf("coverage contract mutations fixture has an empty or duplicate case %q", m.Name)
		}
		seen[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !seen[want] {
			t.Fatalf("required coverage mutation %q is missing from the fixture", want)
		}
	}
	return file
}

func TestContract_CoverageShapesMatch(t *testing.T) {
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
}

func TestContract_CoverageMutationsAreDetected(t *testing.T) {
	file := loadShapeFile(t)
	mutations := loadMutationFile(t)
	shapes := frozenShapes()
	enums := frozenEnums()
	fields := map[string][]contractField{}
	for _, s := range file.Shapes {
		fields[s.Name] = append([]contractField(nil), s.Fields...)
	}
	members := map[string][]string{}
	for _, e := range file.Enums {
		members[e.Name] = append([]string(nil), e.Members...)
	}
	for _, m := range mutations.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			if members[m.Target] != nil {
				got := enums[m.Target]
				mutated, err := mutateEnum(members[m.Target], m)
				if err != nil {
					t.Fatal(err)
				}
				if reflect.DeepEqual(mutated, got) {
					t.Fatalf("the %s mutation of %s was NOT detected", m.Op, m.Target)
				}
				return
			}
			want, ok := fields[m.Target]
			if !ok {
				t.Fatalf("mutation target %q is not a frozen shape or enum", m.Target)
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

func loadValidationCases(t *testing.T) validationCaseFile {
	t.Helper()
	var file validationCaseFile
	if err := decodeFixture(validationCasesYAML, &file, "validation cases fixture"); err != nil {
		t.Fatal(err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("validation cases fixture declares no required_names manifest")
	}
	seen := map[string]bool{}
	collect := func(name string) {
		if name == "" || seen[name] {
			t.Fatalf("validation cases fixture has an empty or duplicate case %q", name)
		}
		seen[name] = true
	}
	for _, c := range file.DestinationCases {
		collect(c.Name)
	}
	for _, c := range file.InventoryCases {
		collect(c.Name)
	}
	for _, c := range file.MapCases {
		collect(c.Name)
	}
	for _, want := range file.RequiredNames {
		if !seen[want] {
			t.Fatalf("required validation case %q is missing from the fixture", want)
		}
	}
	return file
}

func TestDestination_Parses(t *testing.T) {
	file := loadValidationCases(t)
	for _, c := range file.DestinationCases {
		t.Run(c.Name, func(t *testing.T) {
			d, err := ParseDestination(c.Value)
			if c.WantError == "" {
				if err != nil {
					t.Fatalf("valid destination %q was rejected: %v", c.Value, err)
				}
				if d.String() != strings.TrimSpace(c.Value) {
					t.Fatalf("round trip = %q, want %q", d.String(), c.Value)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("case %q: got %v, want error containing %q", c.Name, err, c.WantError)
			}
		})
	}
}

func TestInventory_ValidationCases(t *testing.T) {
	file := loadValidationCases(t)
	for _, c := range file.InventoryCases {
		t.Run(c.Name, func(t *testing.T) {
			err := ValidateInventory(c.Inventory)
			if c.WantError == "" {
				if err != nil {
					t.Fatalf("valid inventory rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("case %q: got %v, want error containing %q", c.Name, err, c.WantError)
			}
		})
	}
}

func TestCoverageMap_ValidationCases(t *testing.T) {
	root := repoRoot(t)
	file := loadValidationCases(t)
	for _, c := range file.MapCases {
		t.Run(c.Name, func(t *testing.T) {
			if err := ValidateInventory(c.Inventory); err != nil {
				t.Fatalf("case %q has an invalid inventory: %v", c.Name, err)
			}
			err := ValidateCoverageMap(root, c.Inventory, c.Map)
			if c.WantError == "" {
				if err != nil {
					t.Fatalf("valid coverage map rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("case %q: got %v, want error containing %q", c.Name, err, c.WantError)
			}
		})
	}
}

func TestDestination_RoundTripsThroughYAML(t *testing.T) {
	m := CoverageMap{
		Version:   1,
		Inventory: "testdata/pre-epoch-inventory.yaml",
		Entries: []MapEntry{
			{Name: "TestA", Destination: Destination{Kind: DestinationRetained}},
			{Name: "seedB", Destination: Destination{Kind: DestinationMoved, Ref: "internal/testutil/counting_fs.go"}},
			{Name: "deadC", Destination: Destination{Kind: DestinationDeleted, Ref: "rationale-abc123"}},
			{Name: "laterD", Destination: Destination{Kind: DestinationFollowup, Ref: "task-abc123"}},
		},
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := DecodeCoverageMap(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip changed the map:\n got  %+v\n want %+v", got, m)
	}
}

func TestCoverageMap_RejectsUnknownField(t *testing.T) {
	_, err := DecodeCoverageMap([]byte("version: 1\ninventory: x\nentries: []\nsurprise: true\n"))
	if err == nil {
		t.Fatal("an unknown field must be rejected")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
