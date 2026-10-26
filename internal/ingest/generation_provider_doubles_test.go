package ingest_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// generationTestPriorSource is a test double for one attempt's evidence access.
// The production implementation applies the mode and capability gates; a case
// states the evidence it wants served, so the gates are covered separately by
// the white-box seam test.
type generationTestPriorSource struct {
	prior        *ingest.GenerationPrior
	priorErr     error
	authority    *ingest.StoredCaptureAuthority
	authorityErr error
}

func (s generationTestPriorSource) ReadGenerationPrior(context.Context) (*ingest.GenerationPrior, error) {
	return s.prior, s.priorErr
}

func (s generationTestPriorSource) ReadStoredCaptureAuthority(context.Context) (*ingest.StoredCaptureAuthority, error) {
	return s.authority, s.authorityErr
}

// generationTestOpenCodeSource is a test double for one attempt's
// recorded-source access. It performs no I/O of its own: the case owns the
// closure that snapshots the synthetic source.
type generationTestOpenCodeSource struct {
	snapshot func(ctx context.Context, locator string) (ingest.OpenCodeHistorySnapshot, error)
}

func (a generationTestOpenCodeSource) Snapshot(ctx context.Context, locator string) (ingest.OpenCodeHistorySnapshot, error) {
	return a.snapshot(ctx, locator)
}

// generationTestPriorFromOpenCode adapts a fixture-stated OpenCode prior into
// the attempt's evidence access, so a case keeps stating the last-good alias
// state, captured prefix and completeness exactly as activation persists them.
// A bridged stored-capture certificate is served only when no active generation
// exists, which is the gate the production access applies.
func generationTestPriorFromOpenCode(t *testing.T, prior ingest.OpenCodeProvenancePrior) ingest.GenerationPriorSource {
	t.Helper()
	source := generationTestPriorSource{authority: prior.CaptureAuthority}
	if prior.CaptureAuthority != nil {
		return source
	}
	encoded, err := ingest.EncodeOpenCodeProvenancePrior(prior)
	if err != nil {
		t.Fatalf("encode fixture prior evidence: %v", err)
	}
	source.prior = &ingest.GenerationPrior{
		GenerationID:          "gen-fixture-prior",
		Aliases:               prior.Aliases,
		HasCompleteGeneration: prior.HasCompleteGeneration,
		PriorEvidence:         encoded,
	}
	return source
}
