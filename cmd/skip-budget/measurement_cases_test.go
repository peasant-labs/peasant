//go:build skipbudget

package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/upgrade"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/measurement_cases.yaml
var measurementCasesYAML []byte

//go:embed testdata/measurement_cases.manifest.yaml
var measurementCasesManifest []byte

// skipBudgetFixture is the budget table the rows are read from. It lives with
// the budget contract in internal/upgrade, outside this package, so it cannot
// be embedded.
const skipBudgetFixture = "../../internal/upgrade/testdata/skip_budget.yaml"

type caseKind string

const (
	caseKindMeasured caseKind = "measured"
	caseKindSmoke    caseKind = "smoke"
	caseKindRetired  caseKind = "retired"
)

// TestMeasurementCasesAreOneClosedSet keeps the harness, the case vocabulary
// in internal/upgrade, and the budget rows naming the same ids. A case added
// to one place and not the others, or a retired id that comes back, fails here.
func TestMeasurementCasesAreOneClosedSet(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name string   `yaml:"name"`
			Kind caseKind `yaml:"kind"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(measurementCasesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(measurementCasesManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	byKind := map[caseKind][]upgrade.MeasurementCase{}
	for _, c := range fixture.Cases {
		if present[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		present[c.Name] = true
		switch c.Kind {
		case caseKindMeasured, caseKindSmoke, caseKindRetired:
		default:
			t.Fatalf("case %q has unknown kind %q", c.Name, c.Kind)
		}
		byKind[c.Kind] = append(byKind[c.Kind], upgrade.MeasurementCase(c.Name))
	}
	if err := testutil.RequireFixtureNames("measurement_cases.yaml", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}

	sorted := func(ids []upgrade.MeasurementCase) []upgrade.MeasurementCase {
		out := slices.Clone(ids)
		slices.Sort(out)
		return out
	}
	runnable := sorted(append(slices.Clone(byKind[caseKindMeasured]), byKind[caseKindSmoke]...))
	if got := sorted(upgrade.AllMeasurementCases); !slices.Equal(got, runnable) {
		t.Errorf("upgrade.AllMeasurementCases = %v, want the measured and smoke cases %v", got, runnable)
	}
	var matrix []upgrade.MeasurementCase
	for _, c := range matrixCases(t.TempDir()) {
		matrix = append(matrix, c.Name)
	}
	if got := sorted(matrix); !slices.Equal(got, runnable) {
		t.Errorf("harness matrix lists %v, want %v", got, runnable)
	}
	var specs []upgrade.MeasurementCase
	for id, spec := range caseSpecs() {
		if spec.id != id {
			t.Errorf("caseSpec keyed %s names id %s", id, spec.id)
		}
		specs = append(specs, id)
	}
	// A measured case changes its base before the arms run; only the
	// new-session smoke import leaves it unchanged, and the zero value is
	// never a mutation.
	for id, spec := range caseSpecs() {
		if !spec.mutation.defined() || spec.mutation == mutateNone {
			t.Errorf("measured case %s prepares with mutation %d, want a defined mutation other than none", id, spec.mutation)
		}
	}
	if mutation(0).defined() {
		t.Error("the zero mutation is defined; a case that forgets its mutation would measure an unchanged base")
	}
	if got, want := sorted(specs), sorted(byKind[caseKindMeasured]); !slices.Equal(got, want) {
		t.Errorf("harness caseSpecs cover %v, want the measured cases %v", got, want)
	}
	for _, id := range byKind[caseKindRetired] {
		if _, err := upgrade.NewMeasurementCase(string(id)); err == nil {
			t.Errorf("retired case %s is accepted again; give a new case a new id", id)
		}
	}

	// Every budget row reads reports of measured cases. A row's recorded
	// command may predate a retirement, so the row's own cases list is what
	// must stay current.
	data, err := os.ReadFile(filepath.FromSlash(skipBudgetFixture))
	if err != nil {
		t.Fatalf("read %s: %v", skipBudgetFixture, err)
	}
	var budget struct {
		Budgets []struct {
			Name  string   `yaml:"name"`
			Cases []string `yaml:"cases"`
		} `yaml:"budgets"`
	}
	if err := yaml.Unmarshal(data, &budget); err != nil {
		t.Fatal(err)
	}
	if len(budget.Budgets) == 0 {
		t.Fatalf("%s has no budget rows", skipBudgetFixture)
	}
	for _, row := range budget.Budgets {
		for _, raw := range row.Cases {
			id, err := upgrade.NewMeasurementCase(raw)
			if err != nil {
				t.Errorf("budget row %s: %v", row.Name, err)
				continue
			}
			if !slices.Contains(byKind[caseKindMeasured], id) {
				t.Errorf("budget row %s reads case %s, which is not a measured case", row.Name, id)
			}
		}
	}
}
