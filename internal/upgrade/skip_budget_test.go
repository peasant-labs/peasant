package upgrade

import (
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/skip_budget.yaml
var skipBudgetFixture []byte

// TestValidateMeasuredRatios exercises the measured-budget validator on
// ratios and malformed costs: a missing cost, a zero avoided cost, a negative
// cost, NaN and infinity are all refused.
func TestValidateMeasuredRatios(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name                   string `yaml:"name"`
			Proof, Settle, Avoided *float64
			Valid                  bool `yaml:"valid"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(skipBudgetFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			b := SkipBudget{Proof: c.Proof, Settle: c.Settle, Avoided: c.Avoided, Provenance: BudgetProvenance{Head: "head", Base: "base", Command: "command", Date: "date", Samples: 5}}
			if got := b.ValidateMeasured() == nil; got != c.Valid {
				t.Fatalf("valid = %v; want %v", got, c.Valid)
			}
		})
	}
}

func TestSkipBudgetRequiresProvenance(t *testing.T) {
	proof, settle, avoided := 1.0, 1.0, 3.0
	var fixture struct {
		Missing []string `yaml:"missingProvenance"`
	}
	if err := yaml.Unmarshal(skipBudgetFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, field := range fixture.Missing {
		t.Run(field, func(t *testing.T) {
			p := BudgetProvenance{Head: "head", Base: "base", Command: "command", Date: "date", Samples: 5}
			switch field {
			case "head":
				p.Head = ""
			case "base":
				p.Base = ""
			case "command":
				p.Command = ""
			case "recorded-date":
				p.Date = ""
			case "samples":
				p.Samples = 0
			default:
				t.Fatalf("unknown provenance field %q", field)
			}
			b := SkipBudget{Proof: &proof, Settle: &settle, Avoided: &avoided, Provenance: p}
			if err := b.ValidateMeasured(); err == nil {
				t.Fatal("accepted incomplete provenance")
			}
		})
	}
}

func TestEveryPlannedSkipHasBudget(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Budgets       []struct {
			Name       string `yaml:"name"`
			SkipBudget `yaml:",inline"`
		} `yaml:"budgets"`
	}
	if err := yaml.Unmarshal(skipBudgetFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	paths, err := PlannedSkipPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[SkipPath]bool)
	for _, path := range paths {
		want[path] = true
	}
	names := make(map[string]bool)
	for _, row := range fixture.Budgets {
		if !want[row.Path] {
			t.Errorf("duplicate or non-skip path: %+v", row.Path)
		}
		delete(want, row.Path)
		if row.Name == "" || names[row.Name] {
			t.Errorf("duplicate or empty name: %q", row.Name)
		}
		names[row.Name] = true
	}
	if len(want) != 0 {
		t.Errorf("missing paths: %v", want)
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Errorf("missing required budget %q", name)
		}
	}
}

func TestStoredProfileHasAvailableStageShape(t *testing.T) {
	var proof, index bool
	for _, stage := range ingest.IndexProfileStageOrder {
		proof = proof || stage == ingest.StageRefreshProof
		index = index || stage == ingest.StageIndex
	}
	// INDEX includes settlement but does not separately persist its duration:
	// per-session write duration is in index_log, not ingest_run_stages.
	if !proof || !index {
		t.Fatal("stored profile must expose proof and INDEX stages")
	}
	// A settle stage would change the budget's shape: the settle cost would
	// then be read from it rather than from INDEX. Adding one must revisit
	// the budget rows, so this test goes red.
	for _, stage := range ingest.IndexProfileStageOrder {
		if strings.Contains(strings.ToLower(stage.String()), "settle") {
			t.Fatalf("stored profile gained a settle stage %q; read settle cost from it in the budget rows and update this test", stage)
		}
	}
}

// TestSkipBudgetRowsNameTheirMeasurement keeps every row traceable to the
// harness command and the cases that measure it. While measurementRequired is
// false a row carries no numbers; once it is true (the default) every row must
// pass the validator and the rows must not all share one ratio, which would
// mean a value was copied rather than measured.
func TestSkipBudgetRowsNameTheirMeasurement(t *testing.T) {
	var fixture struct {
		MeasurementRequired *bool `yaml:"measurementRequired"`
		Budgets             []struct {
			Name       string `yaml:"name"`
			SkipBudget `yaml:",inline"`
		} `yaml:"budgets"`
	}
	if err := yaml.Unmarshal(skipBudgetFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	required := fixture.MeasurementRequired == nil || *fixture.MeasurementRequired
	ratios := map[float64]bool{}
	for _, row := range fixture.Budgets {
		t.Run(row.Name, func(t *testing.T) {
			command := row.Provenance.Command
			if !strings.Contains(command, "./cmd/skip-budget") || len(row.Cases) == 0 {
				t.Fatalf("row names no harness command or case: %+v", row.SkipBudget)
			}
			fields := strings.Fields(command)
			flag := slices.Index(fields, "-case")
			if flag < 0 || flag+1 >= len(fields) {
				t.Fatalf("command %q selects no case", command)
			}
			selected := strings.Split(fields[flag+1], ",")
			for _, c := range row.Cases {
				if _, err := NewMeasurementCase(string(c)); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(selected, string(c)) {
					t.Errorf("command %q does not run case %s", command, c)
				}
			}
			if required {
				if err := row.ValidateMeasured(); err != nil {
					t.Fatal(err)
				}
				ratios[(*row.Proof+*row.Settle) / *row.Avoided] = true
				return
			}
			numbers := row.Proof != nil ||
				row.Settle != nil ||
				row.Avoided != nil ||
				row.Provenance.Samples != 0
			if numbers || row.Provenance.Head != "" || row.Provenance.Date != "" {
				t.Fatalf("row carries measurement while measurementRequired is false: %+v", row.SkipBudget)
			}
		})
	}
	if required && len(fixture.Budgets) > 1 && len(ratios) == 1 {
		t.Fatalf("every budget row has the same ratio %v; record each row from its own case report", ratios)
	}
}
