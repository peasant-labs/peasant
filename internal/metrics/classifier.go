package metrics

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TypeID constants for annotation type registry entries. Used in ClassifierFuncs and
// typeAnnotatorNames to avoid bare string literals throughout the metrics package.
const (
	typeIDSessionOutcome  = "quality.session_outcome"
	typeIDUserFrustration = "quality.user_frustration"
	typeIDSessionScope    = "metadata.session_scope"

	// Entry-level annotation type IDs (V18 migration).
	typeIDFrustrationSignal  = "quality.frustration_signal"
	typeIDResolutionEvidence = "quality.resolution_evidence"
)

// ClassifierFunc computes an annotation classification from session entries and metrics.
// It mirrors the MetricFunc pattern (engine.go:24) but returns a ClassifierResult
// instead of a partial SessionMetrics update.
//
// ClassifierFuncs are composed into the ClassifierEngine (classifier_engine.go) which
// runs after the COMPUTE stage and persists results via AnnotationStore (internal/store).
type ClassifierFunc func(
	ctx context.Context,
	sessionID ingest.SessionID,
	entries []schema.SessionEntry,
	metrics *ingest.SessionMetrics,
) *ClassifierResult

// Compile-time guards: each concrete classifier must satisfy ClassifierFunc.
// A signature change in ClassifierFunc will immediately break the build here.
var _ ClassifierFunc = classifyOutcome
var _ ClassifierFunc = classifyFrustration
var _ ClassifierFunc = classifyScope

// ClassifierTarget identifies the entry-level span a ClassifierResult targets.
// When nil on a ClassifierResult, the result targets the session as a whole.
// When non-nil, the result targets a specific entry or entry range.
type ClassifierTarget struct {
	// EntryIndex is the 0-based start index of the targeted entry.
	EntryIndex int

	// EndIndex is the half-open end index. 0 means single-entry (EntryIndex + 1).
	EndIndex int
}

// ClassifierResult is the output of a ClassifierFunc or EntryClassifierFunc.
// TypeID identifies the annotation type in the registry; Value is the computed
// annotation value string (must satisfy the type's ValueDomain constraints).
// A nil result means the classifier has no opinion for this session.
type ClassifierResult struct {
	// TypeID is the dot-notation annotation type identifier (e.g., "quality.session_outcome").
	TypeID string

	// Value is the computed annotation value string.
	// Must be valid for the annotation type's ValueDomain before persisting.
	Value string

	// Confidence is the classifier's confidence in [0.0, 1.0].
	// 0.0 means "no confidence stated".
	Confidence float64

	// Reason is a human-readable explanation of the classification decision.
	Reason string

	// Provenance records the method and version used to derive this result.
	// Required for rule-based and agent annotators (EvalevalAI R7).
	Provenance *schema.Provenance

	// Target identifies the entry-level span this result targets.
	// nil means session-level annotation; non-nil means entry-level annotation.
	Target *ClassifierTarget
}

// EntryClassifierFunc computes entry-level annotation classifications from session
// entries and metrics. Unlike ClassifierFunc (which returns a single result per session),
// an EntryClassifierFunc returns zero or more results, each targeting a specific entry
// or entry range via ClassifierTarget.
type EntryClassifierFunc func(
	ctx context.Context,
	sessionID ingest.SessionID,
	entries []schema.SessionEntry,
	metrics *ingest.SessionMetrics,
) []*ClassifierResult
