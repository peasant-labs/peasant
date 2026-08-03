package metrics

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Compile-time guard: classifyFrustrationEntries must satisfy EntryClassifierFunc.
var _ EntryClassifierFunc = classifyFrustrationEntries

// classifyFrustrationEntries is an EntryClassifierFunc that identifies individual
// entries containing frustration signals (expletive patterns). Each matching entry
// produces a ClassifierResult with Target pointing to that specific entry index.
//
// Unlike the session-level classifyFrustration (which returns a single detected/not_detected
// result per session), this entry-level variant returns one "detected" result per
// matching entry, enabling fine-grained annotation of frustration signals.
//
// Returns nil (empty slice) if no entries contain frustration signals.
func classifyFrustrationEntries(
	_ context.Context,
	_ ingest.SessionID,
	entries []schema.SessionEntry,
	_ *ingest.SessionMetrics,
) []*ClassifierResult {
	if len(entries) == 0 {
		return nil
	}

	expletivePatterns := []string{"fuck"}
	patternsCSV := strings.Join(expletivePatterns, ",")

	var results []*ClassifierResult
	for i, entry := range entries {
		for _, field := range contentFields(entry) {
			lc := strings.ToLower(field)
			for _, pattern := range expletivePatterns {
				if strings.Contains(lc, pattern) {
					results = append(results, &ClassifierResult{
						TypeID:     typeIDFrustrationSignal,
						Value:      "detected",
						Confidence: 0.9,
						Reason:     fmt.Sprintf("expletive pattern %q found at entry index %d", pattern, entry.EntryIndex),
						Provenance: &schema.Provenance{
							Method:  "heuristic",
							Version: "1",
							Details: map[string]string{"patterns": patternsCSV},
						},
						Target: &ClassifierTarget{
							EntryIndex: i,
						},
					})
					goto nextEntry // one match per entry is sufficient
				}
			}
		}
	nextEntry:
	}
	return results
}
