package main

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionfilter"
)

type selectionCandidateInput = selectionfilter.CandidateInput

// prepareSelectionCandidates resolves a complete operation cohort before any
// matcher call. The shared selection filter owns the resolution rules.
func prepareSelectionCandidates(
	ctx context.Context,
	inputs []selectionCandidateInput,
	resolver ingest.PathIdentityResolver,
) ([]ingest.DiscoveryCandidate, error) {
	return selectionfilter.PrepareCandidates(ctx, inputs, resolver)
}
