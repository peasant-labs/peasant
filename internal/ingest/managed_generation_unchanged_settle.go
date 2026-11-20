package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (p *Pipeline) unchangedProofRequest(input *CapturedIndexInput) (ManagedGenerationUnchangedProof, error) {
	if input == nil || input.metadata == nil || input.expected == nil {
		return ManagedGenerationUnchangedProof{}, fmt.Errorf("ingest: cannot prove unchanged generation: captured metadata or index state is missing; use the ordinary parse")
	}
	target := p.sessionVersionTarget(input.session)
	return ManagedGenerationUnchangedProof{SessionID: input.session.SessionID, Metadata: *input.metadata, InputDigest: input.inputHash, IndexerTarget: target.IndexerVersion, FormatTarget: target.IndexVersion, Expected: input.expected}, nil
}

func (p *Pipeline) proveManagedGenerationUnchanged(ctx context.Context, input *CapturedIndexInput) (UnchangedProof, error) {
	prover, ok := p.metricsStore.(ManagedGenerationUnchangedProver)
	if !ok {
		return UnchangedProof{}, fmt.Errorf("ingest: store cannot prove an unchanged generation; use the ordinary parse")
	}
	request, err := p.unchangedProofRequest(input)
	if err != nil {
		return UnchangedProof{}, err
	}
	return prover.ProveCapturedGenerationUnchanged(ctx, request)
}

// validateRefresh asks the optional validator to prove an unchanged generation
// the store's proof could not. It returns unsupported for any unavailable or
// invalid result; only a proven answer settles, and every other answer leaves
// the proof not proven so the session parses.
func (p *Pipeline) validateRefresh(ctx context.Context, input *CapturedIndexInput) ManagedGenerationValidationResult {
	fallback := ManagedGenerationValidationResult{Outcome: ValidationUnsupported}
	if p.config.Validator == nil || input == nil || input.expected == nil || input.metadata == nil {
		return fallback
	}
	target := p.sessionVersionTarget(input.session)
	if target.IndexVersion == strictIndexFormat || input.expected.IndexVersion == nil ||
		*input.expected.IndexVersion != target.IndexVersion || input.expected.ActiveGenerationID == "" || input.metadata.ContentHash == "" {
		return fallback
	}
	proof, err := p.unchangedProofRequest(input)
	if err != nil {
		return fallback
	}
	// The validator's frozen request extends the proof request with a generation
	// identity and budget; copy the common fields at this single boundary.
	request := ManagedGenerationValidationRequest{
		SessionID: proof.SessionID, GenerationID: proof.Expected.ActiveGenerationID,
		Metadata: proof.Metadata, InputDigest: proof.InputDigest,
		IndexerTarget: proof.IndexerTarget, FormatTarget: proof.FormatTarget, Expected: proof.Expected,
	}
	start := time.Now()
	result, err := p.config.Validator.ValidateManagedGenerationUnchanged(ctx, request)
	p.config.IndexProfiler.RecordStage(StageRefreshValidate, time.Since(start), 0, 1)
	if err != nil {
		slog.Debug("ingest: refresh validation failed; the session parses", "error", err)
		return fallback
	}
	result, err = NewManagedGenerationValidationResult(result.Outcome, result.Mismatch, result.Detail)
	if err != nil {
		slog.Debug("ingest: invalid refresh validation; the session parses", "error", err)
		return fallback
	}
	if result.Outcome == ValidationUnsupported {
		p.config.IndexProfiler.RecordStage(StageRefreshValidateUnsupported, 0, 0, 1)
	}
	return result
}

func (p *Pipeline) settleManagedGenerationUnchanged(ctx context.Context, input *CapturedIndexInput, lane *storeWriteLane) (ManagedGenerationUnchangedSettleOutcome, time.Duration, error) {
	prover, ok := p.metricsStore.(ManagedGenerationUnchangedProver)
	if !ok {
		return ManagedGenerationUnchangedSettleOutcome{}, 0, fmt.Errorf("ingest: store cannot settle an unchanged generation; use the ordinary parse")
	}
	request, err := p.unchangedProofRequest(input)
	if err != nil {
		return ManagedGenerationUnchangedSettleOutcome{}, 0, err
	}
	start := time.Now()
	var outcome ManagedGenerationUnchangedSettleOutcome
	p.runStoreWrite(lane, StageIndex, func() {
		outcome, err = prover.SettleCapturedGenerationUnchanged(ctx, request)
	})
	return outcome, time.Since(start), err
}

// ManagedGenerationUnchangedSettleOutcome is the pipeline-owned result of an unchanged
// settlement of a managed generation.
type ManagedGenerationUnchangedSettleOutcome struct {
	// GenerationID is the generation that is the active read authority
	// afterwards: the settled source generation.
	GenerationID string
}

// managedGenerationRefreshLane reports whether a stored-metadata result is a
// candidate for the unchanged proof and settle. It is the adapter-behind /
// adapter-stamp-absent lane ONLY: an indexer bump reads the stored transcript
// and belongs to the stored-index lane, and --force or --reindex are deliberate
// full rebuilds. A native-only adapter never reaches the stored-metadata lane
// because decideRefresh routes it native, and a strict format-1 target has no
// managed generation to settle. A measurement baseline
// (PipelineConfig.DisableRefreshSkips) declines the candidate, so the unchanged
// proof and its settle stay unused.
//
// The marker only says the settle is worth attempting. The index stage checks
// the captured proof and the store is the final authority; a failed proof or a
// refused settle sends the session to the ordinary parse path.
func (p *Pipeline) managedGenerationRefreshLane(session DiscoveredSession, decision RefreshDecision) bool {
	if decision.Reason != RefreshReasonAdapterBehind && decision.Reason != RefreshReasonAdapterStampAbsent {
		return false
	}
	if p.config.Force || p.config.Reindex || p.config.DisableRefreshSkips {
		return false
	}
	if _, ok := p.metricsStore.(ManagedGenerationUnchangedProver); !ok {
		return false
	}
	return p.sessionVersionTarget(session).IndexVersion != strictIndexFormat
}
