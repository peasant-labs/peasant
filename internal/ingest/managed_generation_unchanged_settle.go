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
// the store's proof could not. It answers unsupported for any unavailable or
// invalid result; only a proven answer settles, and every other answer leaves
// the proof not proven so the session parses. A proven answer comes with the
// validated context the settle must carry. A not-proven answer comes with the
// candidate the validator compared, for the reference step.
func (p *Pipeline) validateRefresh(ctx context.Context, input *CapturedIndexInput, indexer TranscriptIndexer) (result ManagedGenerationValidationResult, validated *ValidatedContext, compared *GenerationMetadataCandidate) {
	unsupported := ManagedGenerationValidationResult{Outcome: ValidationUnsupported}
	start := time.Now()
	defer func() {
		done := 0
		if result.Outcome == ValidationProven {
			done = 1
		}
		p.config.IndexProfiler.RecordStage(StageRefreshValidate, time.Since(start), done, 1)
		if result.Outcome == ValidationUnsupported {
			p.config.IndexProfiler.RecordStage(StageRefreshValidateUnsupported, 0, 0, 1)
		}
	}()
	validator := p.config.Validator
	if validator == nil {
		validator, _ = p.metricsStore.(ManagedGenerationValidator)
	}
	builder, ok := indexer.(GenerationMetadataBuilder)
	if validator == nil || !ok || input == nil || input.expected == nil || input.metadata == nil {
		return unsupported, nil, nil
	}
	target := p.sessionVersionTarget(input.session)
	if target.IndexVersion == strictIndexFormat || input.expected.IndexVersion == nil ||
		*input.expected.IndexVersion != target.IndexVersion || input.expected.ActiveGenerationID == "" || input.metadata.ContentHash == "" {
		return unsupported, nil, nil
	}
	proof, err := p.unchangedProofRequest(input)
	if err != nil {
		return unsupported, nil, nil
	}
	candidate, err := builder.BuildGenerationMetadata(ctx, p.generationBuildInput(input))
	if err != nil {
		slog.Debug("ingest: refresh metadata unavailable; the session parses", "error", err)
		return unsupported, nil, nil
	}
	// The validator's frozen request extends the proof request with a generation
	// identity and budget; copy the common fields at this single boundary.
	request := ManagedGenerationValidationRequest{
		SessionID: proof.SessionID, GenerationID: proof.Expected.ActiveGenerationID,
		Metadata: proof.Metadata, InputDigest: proof.InputDigest,
		IndexerTarget: proof.IndexerTarget, FormatTarget: proof.FormatTarget, Expected: proof.Expected,
		Candidate: &candidate,
	}
	answer, err := validator.ValidateManagedGenerationUnchanged(ctx, request)
	if err != nil {
		slog.Debug("ingest: refresh validation failed; the session parses", "error", err)
		return unsupported, nil, nil
	}
	answer, err = NewManagedGenerationValidationResult(answer.Outcome, answer.Mismatch, answer.Detail)
	if err != nil {
		slog.Debug("ingest: invalid refresh validation; the session parses", "error", err)
		return unsupported, nil, nil
	}
	if answer.Outcome == ValidationProven {
		return answer, &ValidatedContext{GenerationID: proof.Expected.ActiveGenerationID, Candidate: candidate}, nil
	}
	if answer.Outcome == ValidationNotProven {
		return answer, nil, &candidate
	}
	return answer, nil, nil
}

func (p *Pipeline) settleManagedGenerationUnchanged(ctx context.Context, input *CapturedIndexInput, lane *storeWriteLane, validated *ValidatedContext) (ManagedGenerationUnchangedSettleOutcome, time.Duration, error) {
	prover, ok := p.metricsStore.(ManagedGenerationUnchangedProver)
	if !ok {
		return ManagedGenerationUnchangedSettleOutcome{}, 0, fmt.Errorf("ingest: store cannot settle an unchanged generation; use the ordinary parse")
	}
	request, err := p.unchangedProofRequest(input)
	if err != nil {
		return ManagedGenerationUnchangedSettleOutcome{}, 0, err
	}
	request.Validated = validated
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

// indexerSupportsMetadataHalf reports whether the harness's registered
// indexer implements GenerationMetadataBuilder.
func (p *Pipeline) indexerSupportsMetadataHalf(harness Harness) bool {
	if p.indexers == nil {
		return false
	}
	_, ok := p.indexers[harness].(GenerationMetadataBuilder)
	return ok
}

// supportsManagedGenerationRefresh reports whether an adapter-behind
// session for harness may enter the managed-generation proof/validation/
// reference path.
func (p *Pipeline) supportsManagedGenerationRefresh(harness Harness) bool {
	return p.adapterDeclaresChangeScope(harness) &&
		(p.adapterSupportsStoredExtraction(harness) || p.indexerSupportsMetadataHalf(harness))
}

// isManagedGenerationAdapterRefresh is the one structural predicate shared by
// managedGenerationRefreshLane and processNativeSessionWithMeta's timestamp
// preservation. It is deliberately independent of DisableRefreshSkips so
// skip and baseline arms produce byte-identical timestamps.
func (p *Pipeline) isManagedGenerationAdapterRefresh(session DiscoveredSession, decision RefreshDecision) bool {
	if p.config.Force || p.config.Reindex {
		return false
	}
	if !generationTargetApplicable(session) || p.sessionVersionTarget(session).IndexVersion == strictIndexFormat {
		return false
	}
	if decision.AdapterUndeclared || !p.adapterDeclaresChangeScope(session.Harness) {
		return false
	}
	switch decision.Reason {
	case RefreshReasonAdapterBehind, RefreshReasonAdapterStampAbsent:
		return p.adapterSupportsStoredExtraction(session.Harness)
	case RefreshReasonAdapterBehindNativeOnly:
		return !p.adapterSupportsStoredExtraction(session.Harness) && p.indexerSupportsMetadataHalf(session.Harness)
	default:
		return false
	}
}

// managedGenerationRefreshLane reports whether a refresh decision is a
// candidate for the unchanged proof and settle. It covers stored-metadata
// refreshes (adapter-behind / adapter-stamp-absent) and native-only adapter
// refreshes when the indexer implements GenerationMetadataBuilder. An indexer
// bump reads the stored transcript and belongs to the stored-index lane,
// --force or --reindex are deliberate full rebuilds, and a strict format-1
// target has no managed generation to settle. A measurement baseline
// (PipelineConfig.DisableRefreshSkips) declines the candidate, so the unchanged
// proof and its settle stay unused.
//
// The marker only says the settle is worth attempting. The index stage checks
// the captured proof and the store is the final authority; a failed proof or a
// refused settle sends the session to the ordinary parse path.
func (p *Pipeline) managedGenerationRefreshLane(session DiscoveredSession, decision RefreshDecision) bool {
	if p.config.DisableRefreshSkips {
		return false
	}
	if _, ok := p.metricsStore.(ManagedGenerationUnchangedProver); !ok {
		return false
	}
	return p.isManagedGenerationAdapterRefresh(session, decision)
}
