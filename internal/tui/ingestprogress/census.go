package ingestprogress

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// CensusLines renders the pipeline's stored-outcome census, never a predicted
// refresh count. Its refresh outcomes include failed attempts, and indexed-byte
// totals may omit sessions which did not index.
func CensusLines(census *ingest.RefreshCensus) []string {
	if census == nil {
		return []string{"refresh census unavailable"}
	}
	lines := []string{fmt.Sprintf("refresh census: %d sessions (includes failed attempts)", census.Sessions())}
	for _, row := range census.Rows {
		lines = append(lines, fmt.Sprintf("%s%s / %s: %d", kit.Indent(2), row.Lane, row.Reason.Code(), row.Sessions))
		bytes := "indexed bytes: unknown"
		if row.SourceBytes > 0 {
			bytes = "indexed bytes measured: " + defaults.HumanByteSize(row.SourceBytes) + " (may be partial)"
		}
		lines = append(lines, kit.Indent(4)+bytes)
	}
	return lines
}
