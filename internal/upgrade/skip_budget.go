package upgrade

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// SkipDisposition names the skip a lane takes. The stored-metadata lane skips
// the parse only through a proven settle; a proof that fails parses, so it has
// no skip of its own to budget.
type SkipDisposition string

const (
	SkipProven SkipDisposition = "proven"
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
			paths = append(paths, SkipPath{lane, SkipProven})
		case ingest.RefreshLaneStoredIndex:
			paths = append(paths, SkipPath{lane, SkipIndex})
		default:
			return nil, fmt.Errorf("skip budget: refresh lane %q has no budget classification; classify it before shipping", lane)
		}
	}
	return paths, nil
}

// MeasurementCase is the closed set of synthetic corpus cases the skip-budget
// harness runs. A budget row names the cases whose reports supply its costs.
type MeasurementCase string

const (
	// MeasurementCaseStampOnly is an adapter bump that changes no metadata:
	// the proof and its settle against the baseline parse.
	MeasurementCaseStampOnly MeasurementCase = "C1"
	// C2 (a context change) and C5 (a legacy row without the seal) are
	// retired. Both measured the metadata-only copy refresh after a failed
	// proof; a failed proof now parses, so neither has a skip to measure. Their
	// ids are not reused, so an old report cannot be read as a new case.

	// MeasurementCaseIndexerBump is an indexer bump: the stored-index lane
	// against the native capture and parse.
	MeasurementCaseIndexerBump MeasurementCase = "C3"
	// MeasurementCaseNewSession imports new sessions: the full step.
	MeasurementCaseNewSession MeasurementCase = "C4"
	// MeasurementCaseUndeclaredProducer is the stamp-only bump planned
	// against a table in which the adapter declares no change scope: the
	// full step the plan names.
	MeasurementCaseUndeclaredProducer MeasurementCase = "C6"
)

// AllMeasurementCases lists every case in matrix order.
var AllMeasurementCases = []MeasurementCase{
	MeasurementCaseStampOnly,
	MeasurementCaseIndexerBump,
	MeasurementCaseNewSession,
	MeasurementCaseUndeclaredProducer,
}

// NewMeasurementCase validates a raw case id at an input boundary.
func NewMeasurementCase(raw string) (MeasurementCase, error) {
	if c := MeasurementCase(raw); slices.Contains(AllMeasurementCases, c) {
		return c, nil
	}
	return "", fmt.Errorf("skip budget: read measurement case %q: the harness defines no such case; nothing was run; use one of %q", raw, AllMeasurementCases)
}

// BudgetProvenance identifies the measurement from which a skip budget came.
type BudgetProvenance struct {
	Head    string `yaml:"head" json:"head"`
	Base    string `yaml:"base" json:"base"`
	Command string `yaml:"command" json:"command"`
	Date    string `yaml:"date" json:"date"`
	Samples int    `yaml:"samples" json:"samples"`
}

// CostSpread is the min–max range of one cost over a report's samples, in the
// same unit as the median it brackets.
type CostSpread struct {
	Min float64 `yaml:"min" json:"min"`
	Max float64 `yaml:"max" json:"max"`
}

// brackets reports whether the spread is finite and 0 < min <= median <= max.
func (s CostSpread) brackets(median float64) bool {
	finite := !math.IsNaN(s.Min) && !math.IsInf(s.Min, 0) &&
		!math.IsNaN(s.Max) && !math.IsInf(s.Max, 0)
	positive := s.Min > 0
	ordered := s.Min <= median && median <= s.Max
	return finite && positive && ordered
}

// SkipBudget records median costs in the same unit, or nil until measured.
// Each cost may carry its min–max spread. A recorded spread must bracket its
// median, and the worst case the spreads imply must still beat the avoided
// work.
type SkipBudget struct {
	Path          SkipPath         `yaml:",inline" json:"path"`
	Proof         *float64         `yaml:"proof" json:"proof"`
	ProofSpread   *CostSpread      `yaml:"proofSpread,omitempty" json:"proofSpread,omitempty"`
	Settle        *float64         `yaml:"settle" json:"settle"`
	SettleSpread  *CostSpread      `yaml:"settleSpread,omitempty" json:"settleSpread,omitempty"`
	Avoided       *float64         `yaml:"avoided" json:"avoided"`
	AvoidedSpread *CostSpread      `yaml:"avoidedSpread,omitempty" json:"avoidedSpread,omitempty"`
	Provenance    BudgetProvenance `yaml:"provenance" json:"provenance"`
	// Cases names the measurement cases whose reports supply the costs.
	Cases []MeasurementCase `yaml:"cases" json:"cases"`
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
	return b.validateSpreads(proof, settle, avoided)
}

// validateSpreads checks each recorded spread against its median and refuses
// a skip whose worst case, (proof max + settle max) / avoided min, does not
// beat the avoided work, so a lane cannot pass on a lucky median. A cost
// without a spread contributes its median to the worst case.
func (b SkipBudget) validateSpreads(proof, settle, avoided float64) error {
	costs := []struct {
		name   string
		median float64
		spread *CostSpread
	}{
		{"proof", proof, b.ProofSpread},
		{"settle", settle, b.SettleSpread},
		{"avoided", avoided, b.AvoidedSpread},
	}
	for _, c := range costs {
		if c.spread == nil {
			continue
		}
		if !c.spread.brackets(c.median) {
			return fmt.Errorf("skip budget %v: %s spread %v–%v does not bracket its median %v; record finite spreads with 0 < min <= median <= max", b.Path, c.name, c.spread.Min, c.spread.Max, c.median)
		}
	}
	worstProof, worstSettle, leastAvoided := proof, settle, avoided
	if b.ProofSpread != nil {
		worstProof = b.ProofSpread.Max
	}
	if b.SettleSpread != nil {
		worstSettle = b.SettleSpread.Max
	}
	if b.AvoidedSpread != nil {
		leastAvoided = b.AvoidedSpread.Min
	}
	if (worstProof+worstSettle)/leastAvoided >= 1 {
		return fmt.Errorf("skip budget %v: worst-case proof and settlement cost at least as much as the least avoided work; the median win is not robust, so do not ship this skip", b.Path)
	}
	return nil
}
