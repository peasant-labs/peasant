package indexformat

import "fmt"

// CoordinateKind names the native coordinate space a context segment was
// captured from. It is a local closed enum: it never reaches the public wire,
// and every value is a native coordinate, never a UI turn index.
type CoordinateKind string

const (
	CoordinateKindCodexOrdinalRange     CoordinateKind = "codex_ordinal_range"
	CoordinateKindCodexReferenceRange   CoordinateKind = "codex_reference_range"
	CoordinateKindOpenCodeSequenceRange CoordinateKind = "opencode_sequence_range"
	CoordinateKindSnapshotOnly          CoordinateKind = "snapshot_only"
	CoordinateKindUnknown               CoordinateKind = "unknown"
)

// AllCoordinateKinds is the exact closed set of coordinate kinds.
var AllCoordinateKinds = []CoordinateKind{
	CoordinateKindCodexOrdinalRange,
	CoordinateKindCodexReferenceRange,
	CoordinateKindOpenCodeSequenceRange,
	CoordinateKindSnapshotOnly,
	CoordinateKindUnknown,
}

// IsValid reports whether the value is a member of AllCoordinateKinds.
func (v CoordinateKind) IsValid() bool {
	for _, known := range AllCoordinateKinds {
		if v == known {
			return true
		}
	}
	return false
}

// NewCoordinateKind validates a raw coordinate kind at an input boundary.
func NewCoordinateKind(raw string) (CoordinateKind, error) {
	v := CoordinateKind(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("indexformat.NewCoordinateKind: value %q is outside the closed coordinate-kind set %v; a segment coordinate cannot be interpreted; use a published coordinate kind or omit the segment", raw, AllCoordinateKinds)
	}
	return v, nil
}

// SegmentInclusion classifies how one captured context segment relates to the
// generation that owns it. It is a local closed enum, not a version axis.
type SegmentInclusion string

const (
	SegmentInclusionInherited               SegmentInclusion = "inherited"
	SegmentInclusionSameThreadSurvivingOwn  SegmentInclusion = "same_thread_surviving_own"
	SegmentInclusionUncertainEarlierHistory SegmentInclusion = "uncertain_earlier_history"
	SegmentInclusionExcludedReverted        SegmentInclusion = "excluded_reverted"
	SegmentInclusionInvalidIncomplete       SegmentInclusion = "invalid_incomplete"
)

// AllSegmentInclusions is the exact closed set of segment inclusions.
var AllSegmentInclusions = []SegmentInclusion{
	SegmentInclusionInherited,
	SegmentInclusionSameThreadSurvivingOwn,
	SegmentInclusionUncertainEarlierHistory,
	SegmentInclusionExcludedReverted,
	SegmentInclusionInvalidIncomplete,
}

// IsValid reports whether the value is a member of AllSegmentInclusions.
func (v SegmentInclusion) IsValid() bool {
	for _, known := range AllSegmentInclusions {
		if v == known {
			return true
		}
	}
	return false
}

// NewSegmentInclusion validates a raw segment inclusion at an input boundary.
func NewSegmentInclusion(raw string) (SegmentInclusion, error) {
	v := SegmentInclusion(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("indexformat.NewSegmentInclusion: value %q is outside the closed segment-inclusion set %v; captured history cannot be classified; use a published inclusion or omit the segment", raw, AllSegmentInclusions)
	}
	return v, nil
}

// GenerationCompleteness records whether a managed generation proves all of
// the native evidence it needs. It is a local closed enum, not another version
// axis: "complete" is the normal state, and "incomplete_new" is the one
// first-discovery exception that a complete last-good generation is never
// replaced with.
type GenerationCompleteness string

const (
	GenerationCompletenessComplete      GenerationCompleteness = "complete"
	GenerationCompletenessIncompleteNew GenerationCompleteness = "incomplete_new"
)

// AllGenerationCompletenesses is the exact closed set of completeness states.
var AllGenerationCompletenesses = []GenerationCompleteness{
	GenerationCompletenessComplete,
	GenerationCompletenessIncompleteNew,
}

// IsValid reports whether the value is a member of AllGenerationCompletenesses.
func (v GenerationCompleteness) IsValid() bool {
	for _, known := range AllGenerationCompletenesses {
		if v == known {
			return true
		}
	}
	return false
}

// NewGenerationCompleteness validates a raw completeness state at an input
// boundary.
func NewGenerationCompleteness(raw string) (GenerationCompleteness, error) {
	v := GenerationCompleteness(raw)
	if !v.IsValid() {
		return "", fmt.Errorf("indexformat.NewGenerationCompleteness: value %q is outside the closed completeness set %v; a reader cannot tell whether the generation is usable; use complete or incomplete_new", raw, AllGenerationCompletenesses)
	}
	return v, nil
}
