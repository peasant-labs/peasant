package main

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestKickstartIngestResultCarriesExcluded: the wizard's result keeps the
// sessions a selection excluded, so new + updated + unchanged + excluded +
// errors still equals the sessions the run saw.
func TestKickstartIngestResultCarriesExcluded(t *testing.T) {
	t.Parallel()
	got := kickstartIngestResult(&ingest.PipelineResult{Summary: ingest.PipelineSummary{New: 1, Unchanged: 2, ExcludedBySelection: 3, Errors: 1}})
	if got.ExcludedBySelection != 3 || got.New+got.Updated+got.Unchanged+got.ExcludedBySelection+got.Errors != 7 {
		t.Errorf("kickstart result = %+v, want 3 excluded and a total of 7", got)
	}
}
