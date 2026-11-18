package upgrade

import (
	"fmt"
	"math"
	"strings"
)

// BudgetProvenance identifies the measurement from which a skip budget came.
type BudgetProvenance struct {
	Head    string `yaml:"head" json:"head"`
	Base    string `yaml:"base" json:"base"`
	Command string `yaml:"command" json:"command"`
	Date    string `yaml:"date" json:"date"`
	Samples int    `yaml:"samples" json:"samples"`
}

// SkipBudget records median costs in the same unit, or nil until measured.
type SkipBudget struct {
	Action     StepAction       `yaml:"action" json:"action"`
	Proof      *float64         `yaml:"proof" json:"proof"`
	Settle     *float64         `yaml:"settle" json:"settle"`
	Avoided    *float64         `yaml:"avoided" json:"avoided"`
	Provenance BudgetProvenance `yaml:"provenance" json:"provenance"`
}

// ValidateMeasured rejects incomplete evidence and skips that cost at least as
// much as the work they avoid. Unmeasured rows are not evidence of a win.
func (b SkipBudget) ValidateMeasured() error {
	p := b.Provenance
	if strings.TrimSpace(p.Head) == "" || strings.TrimSpace(p.Base) == "" ||
		strings.TrimSpace(p.Command) == "" || strings.TrimSpace(p.Date) == "" ||
		p.Samples < 1 {
		return fmt.Errorf("skip budget %q: incomplete measurement provenance; record head, base, command, date and sample count", b.Action)
	}
	if b.Proof == nil || b.Settle == nil || b.Avoided == nil {
		return fmt.Errorf("skip budget %q: costs are unmeasured; measure proof, settle and avoided work before accepting the budget", b.Action)
	}
	proof, settle, avoided := *b.Proof, *b.Settle, *b.Avoided
	if math.IsNaN(proof) || math.IsNaN(settle) || math.IsNaN(avoided) ||
		math.IsInf(proof, 0) || math.IsInf(settle, 0) || math.IsInf(avoided, 0) ||
		proof < 0 || settle < 0 || avoided <= 0 {
		return fmt.Errorf("skip budget %q: invalid measured costs; record finite nonnegative costs and a positive avoided cost", b.Action)
	}
	if (proof+settle)/avoided >= 1 {
		return fmt.Errorf("skip budget %q: proof and settlement cost at least as much as avoided work; do not ship this skip", b.Action)
	}
	return nil
}
