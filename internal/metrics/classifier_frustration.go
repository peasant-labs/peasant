package metrics

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// contentFields returns all non-nil text fields from a SessionEntry that may
// contain user-authored content: ContentPreview, ToolInput, ToolOutput.
func contentFields(entry schema.SessionEntry) []string {
	var fields []string
	if entry.ContentPreview != nil {
		fields = append(fields, *entry.ContentPreview)
	}
	if entry.ToolInput != nil {
		fields = append(fields, *entry.ToolInput)
	}
	if entry.ToolOutput != nil {
		fields = append(fields, *entry.ToolOutput)
	}
	return fields
}

// classifyFrustration is a ClassifierFunc that detects signs of user frustration
// and maps them to the typeIDUserFrustration annotation type.
//
// Strategy: scan ContentPreview, ToolInput, and ToolOutput for expletive patterns
// (case-insensitive string match). Returns "detected" on first match, "not_detected"
// if no entries match, nil if there are no entries (insufficient data).
func classifyFrustration(
	_ context.Context,
	_ ingest.SessionID,
	entries []schema.SessionEntry,
	_ *ingest.SessionMetrics,
) *ClassifierResult {
	if len(entries) == 0 {
		return nil // insufficient data
	}

	expletivePatterns := []string{"fuck"}
	patternsCSV := strings.Join(expletivePatterns, ",")

	for _, entry := range entries {
		for _, field := range contentFields(entry) {
			lc := strings.ToLower(field)
			for _, pattern := range expletivePatterns {
				if strings.Contains(lc, pattern) {
					return &ClassifierResult{
						TypeID:     typeIDUserFrustration,
						Value:      "detected",
						Confidence: 0.9,
						Reason:     fmt.Sprintf("expletive pattern %q found in session entries", pattern),
						Provenance: &schema.Provenance{
							Method:  "heuristic",
							Version: "1",
							Details: map[string]string{"patterns": patternsCSV},
						},
					}
				}
			}
		}
	}

	return &ClassifierResult{
		TypeID:     typeIDUserFrustration,
		Value:      "not_detected",
		Confidence: 0.95,
		Reason:     "no expletive patterns found",
		Provenance: &schema.Provenance{
			Method:  "heuristic",
			Version: "1",
			Details: map[string]string{"patterns": patternsCSV},
		},
	}
}
