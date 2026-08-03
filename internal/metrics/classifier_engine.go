package metrics

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// ClassifierEngine runs registered ClassifierFuncs and EntryClassifierFuncs for each session.
// It mirrors the Engine pattern (engine.go) but produces ClassifierResults
// (annotation labels) instead of SessionMetrics updates.
//
// Session-level classifiers (ClassifierFunc) produce at most one result each.
// Entry-level classifiers (EntryClassifierFunc) produce zero or more results,
// each targeting a specific entry or entry range via ClassifierTarget.
type ClassifierEngine struct {
	classifiers      []namedClassifierFunc
	entryClassifiers []namedEntryClassifierFunc
}

type namedClassifierFunc struct {
	name string
	fn   ClassifierFunc
}

type namedEntryClassifierFunc struct {
	name string
	fn   EntryClassifierFunc
}

// Compile-time guard: ClassifierEngine must be usable as the annotation engine.
var _ interface {
	Run(context.Context, ingest.SessionID, []schema.SessionEntry, *ingest.SessionMetrics) []*ClassifierResult
} = (*ClassifierEngine)(nil)

// NewClassifierEngine creates a ClassifierEngine with the default set of classifiers.
func NewClassifierEngine() *ClassifierEngine {
	e := &ClassifierEngine{}
	e.classifiers = []namedClassifierFunc{
		{name: typeIDSessionOutcome, fn: classifyOutcome},
		{name: typeIDUserFrustration, fn: classifyFrustration},
		{name: typeIDSessionScope, fn: classifyScope},
	}
	e.entryClassifiers = []namedEntryClassifierFunc{
		{name: typeIDFrustrationSignal, fn: classifyFrustrationEntries},
		{name: typeIDResolutionEvidence, fn: classifyResolutionEntries},
	}
	return e
}

// RegisterEntryClassifier appends a named entry-level classifier to the engine.
// Used by tests or extensions to inject custom entry classifiers.
func (e *ClassifierEngine) RegisterEntryClassifier(name string, fn EntryClassifierFunc) {
	e.entryClassifiers = append(e.entryClassifiers, namedEntryClassifierFunc{name: name, fn: fn})
}

// Run runs all registered session-level and entry-level classifiers for the given session.
// Returns non-nil ClassifierResults only.
// Individual classifier errors are absorbed: a nil return means no opinion.
func (e *ClassifierEngine) Run(
	ctx context.Context,
	sessionID ingest.SessionID,
	entries []schema.SessionEntry,
	metrics *ingest.SessionMetrics,
) []*ClassifierResult {
	var results []*ClassifierResult

	// Session-level classifiers.
	for _, c := range e.classifiers {
		result := c.fn(ctx, sessionID, entries, metrics)
		if result != nil {
			results = append(results, result)
		}
	}

	// Entry-level classifiers.
	for _, ec := range e.entryClassifiers {
		entryResults := ec.fn(ctx, sessionID, entries, metrics)
		for _, r := range entryResults {
			if r != nil {
				results = append(results, r)
			}
		}
	}

	return results
}
