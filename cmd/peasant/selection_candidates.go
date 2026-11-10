package main

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionfilter"
)

// selectionCandidateInput is raw operation-cohort evidence. ProjectPath stays a
// string until prepareSelectionCandidates resolves it through the injected path
// identity boundary. Push and prune keep this shape and delegate the resolution.
type selectionCandidateInput struct {
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	ProjectHash string
	ProjectPath string
	Branch      string
	SessionID   ingest.SessionID
}

// prepareSelectionCandidates resolves a complete operation cohort before any
// matcher call. The shared selection filter owns the resolution rules.
func prepareSelectionCandidates(
	ctx context.Context,
	inputs []selectionCandidateInput,
	resolver ingest.PathIdentityResolver,
) ([]ingest.DiscoveryCandidate, error) {
	converted := make([]selectionfilter.CandidateInput, len(inputs))
	for index, input := range inputs {
		converted[index] = selectionfilter.CandidateInput{
			Harness:     input.Harness,
			GitRemote:   input.GitRemote,
			ProjectName: input.ProjectName,
			ProjectHash: input.ProjectHash,
			ProjectPath: input.ProjectPath,
			Branch:      input.Branch,
			SessionID:   input.SessionID,
		}
	}
	return selectionfilter.PrepareCandidates(ctx, converted, resolver)
}
