package upgrade

import (
	"fmt"
	"math"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// SkipDisposition distinguishes the outcomes of the stored-metadata lane.
// The refresh decision itself does not distinguish proof settlement from copy.
type SkipDisposition string

const (
	SkipProven SkipDisposition = "proven"
	SkipCopy   SkipDisposition = "copy"
	SkipIndex  SkipDisposition = "index"
)

// SkipPath identifies both the decision lane and, where necessary, its outcome.
type SkipPath struct {
	Lane        ingest.RefreshLane `yaml:"lane" json:"lane"`
	Disposition SkipDisposition    `yaml:"disposition" json:"disposition"`
}

// PlannedSkipPaths enumerates skip outcomes from the decision lane vocabulary.
func PlannedSkipPaths() ([]SkipPath, error) {
	var paths []SkipPath
	for _, lane := range ingest.AllRefreshLanes {
		switch lane {
		case ingest.RefreshLaneNone, ingest.RefreshLaneNative:
		case ingest.RefreshLaneStoredMetadata:
			paths = append(paths, SkipPath{lane, SkipProven}, SkipPath{lane, SkipCopy})
		case ingest.RefreshLaneStoredIndex:
			paths = append(paths, SkipPath{lane, SkipIndex})
		default:
			return nil, fmt.Errorf("skip budget: refresh lane %q has no budget classification; classify it before shipping", lane)
		}
	}
	return paths, nil
}

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
	Path       SkipPath         `yaml:",inline" json:"path"`
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
		return fmt.Errorf("skip budget %v: incomplete measurement provenance; record head, base, command, date and sample count", b.Path)
	}
	if b.Proof == nil || b.Settle == nil || b.Avoided == nil {
		return fmt.Errorf("skip budget %v: costs are unmeasured; measure proof, settle and avoided work before accepting the budget", b.Path)
	}
	proof, settle, avoided := *b.Proof, *b.Settle, *b.Avoided
	for _, value := range []float64{proof, settle, avoided} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("skip budget %v: invalid measured costs; record finite nonnegative costs and a positive avoided cost", b.Path)
		}
	}
	if avoided == 0 {
		return fmt.Errorf("skip budget %v: invalid measured costs; record finite nonnegative costs and a positive avoided cost", b.Path)
	}
	if (proof+settle)/avoided >= 1 {
		return fmt.Errorf("skip budget %v: proof and settlement cost at least as much as avoided work; do not ship this skip", b.Path)
	}
	return nil
}
