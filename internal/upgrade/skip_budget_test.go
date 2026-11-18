package upgrade

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/skip_budget.yaml
var skipBudgetFixture []byte

func TestSkipBudgetRatios(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name                   string `yaml:"name"`
			Proof, Settle, Avoided float64
			Valid                  bool `yaml:"valid"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(skipBudgetFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			b := SkipBudget{Proof: &c.Proof, Settle: &c.Settle, Avoided: &c.Avoided, Provenance: BudgetProvenance{Head: "head", Base: "base", Command: "command", Date: "date", Samples: 5}}
			if got := b.ValidateMeasured() == nil; got != c.Valid {
				t.Fatalf("valid = %v; want %v", got, c.Valid)
			}
		})
	}
}

func TestSkipBudgetRequiresProvenance(t *testing.T) {
	proof, settle, avoided := 1.0, 1.0, 3.0
	b := SkipBudget{Proof: &proof, Settle: &settle, Avoided: &avoided}
	if err := b.ValidateMeasured(); err == nil {
		t.Fatal("accepted measurement without provenance")
	}
}
