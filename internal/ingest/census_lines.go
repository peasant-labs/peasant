package ingest

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// CensusLines renders stored outcomes, including failed attempts. Indexed-byte
// totals may omit sessions which did not index.
func CensusLines(census *RefreshCensus) []string {
	if census == nil {
		return []string{"refresh census unavailable"}
	}
	lines := []string{fmt.Sprintf("refresh census: %d %s (includes failed attempts)", census.Sessions(), defaults.SessionWord(census.Sessions()))}
	for _, row := range census.Rows {
		lines = append(lines, fmt.Sprintf("  %s / %s: %d", row.Lane, row.Reason.Code(), row.Sessions))
		bytes := "indexed bytes: unknown"
		if row.SourceBytes > 0 {
			bytes = "indexed bytes measured: " + defaults.HumanByteSize(row.SourceBytes) + " (may be partial)"
		}
		lines = append(lines, "    "+bytes)
	}
	return lines
}
