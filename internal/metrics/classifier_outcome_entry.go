package metrics

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// Compile-time guard: classifyResolutionEntries must satisfy EntryClassifierFunc.
var _ EntryClassifierFunc = classifyResolutionEntries

// Resolution evidence phrases. An assistant entry containing any of these phrases
// is considered evidence that a task was resolved at that point in the conversation.
var resolutionEvidencePhrases = []string{
	"successfully",
	"completed",
	"fixed",
	"resolved",
	"done",
	"all tests pass",
	"implemented",
}

// classifyResolutionEntries is an EntryClassifierFunc that identifies individual
// assistant entries containing resolution evidence. Each matching entry produces
// a ClassifierResult with Target pointing to that specific entry index.
//
// Strategy: scan assistant entries' ContentPreview for resolution evidence phrases
// (case-insensitive). Returns one "present" result per matching entry.
//
// Returns nil (empty slice) if no entries contain resolution evidence.
func classifyResolutionEntries(
	_ context.Context,
	_ ingest.SessionID,
	entries []schema.SessionEntry,
	_ *ingest.SessionMetrics,
) []*ClassifierResult {
	if len(entries) == 0 {
		return nil
	}

	phrasesCSV := strings.Join(resolutionEvidencePhrases, ",")

	var results []*ClassifierResult
	for i, entry := range entries {
		// Only scan assistant entries for resolution evidence.
		if entry.Role != ingest.RoleAssistant {
			continue
		}

		for _, field := range contentFields(entry) {
			lc := strings.ToLower(field)
			for _, phrase := range resolutionEvidencePhrases {
				if strings.Contains(lc, phrase) {
					results = append(results, &ClassifierResult{
						TypeID:     typeIDResolutionEvidence,
						Value:      "present",
						Confidence: 0.7,
						Reason:     fmt.Sprintf("resolution evidence phrase %q found at entry index %d", phrase, entry.EntryIndex),
						Provenance: &schema.Provenance{
							Method:  "heuristic",
							Version: "1",
							Details: map[string]string{"phrases": phrasesCSV},
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
