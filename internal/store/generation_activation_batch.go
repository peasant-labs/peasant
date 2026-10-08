package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// ActivationResult preserves request order and each session's disposition.
type ActivationResult struct {
	Outcome ingest.ActivationOutcome
	Err     error
}

// ActivateGenerationBatch stages independent candidates together and installs
// them in one transaction with a savepoint per session. Repair, identical
// refresh, and idempotent retries use the ordinary guarded single-session path.
// Session locks are acquired in ascending order and released before cleanup.
func (s *Store) ActivateGenerationBatch(ctx context.Context, activations []GenerationActivation) []ActivationResult {
	results := make([]ActivationResult, len(activations))
	var positions []int
	var prepared []*preparedHarmonized
	var unstaged []*preparedHarmonized
	seen := make(map[string]bool)
	for i, a := range activations {
		id := a.Generation.Generation.ID
		sid := a.Generation.Generation.Metadata.SessionID
		results[i].Outcome = ingest.ActivationOutcome{Disposition: ingest.ActivationNotCommitted, CandidateID: id}
		if seen[string(sid)] {
			results[i].Err = fmt.Errorf("store: activation batch repeats session %s; submit one candidate per session in a batch; the repeated candidate was not installed", sid)
			continue
		}
		seen[string(sid)] = true
		if err := s.requireGenerationSupport(); err != nil {
			results[i].Err = err
			continue
		}
		if err := validateGenerationID(id); err != nil {
			results[i].Err = err
			continue
		}
		if err := validateActivationBlobSupplies(a); err != nil {
			results[i].Err = err
			continue
		}
		p, err := prepareHarmonizedCandidate(sid, a.Generation.Generation, a.Blobs)
		if err == nil {
			err = validateEntriesForStorage(sid, allGenerationEntries(p.generation))
		}
		if err == nil {
			err = reportHarmonizedWriterSeam(harmonizedSeamAfterPrepare)
		}
		if err != nil {
			results[i].Err = err
			continue
		}
		err = s.checkImmutableIdentity(ctx, sid, id, p.binding)
		var already *alreadyCommittedError
		if err != nil && !errors.As(err, &already) {
			results[i].Err = err
			continue
		}
		repair, repairErr := s.needsRepair(ctx, sid)
		active, activeErr := s.readActiveView(ctx, sid)
		if repairErr != nil {
			results[i].Err = repairErr
			continue
		}
		if activeErr != nil {
			results[i].Err = activeErr
			continue
		}
		if already != nil || repair || (active.found && refreshEqualsActive(p, active.view)) {
			results[i].Outcome, results[i].Err = s.ActivateGeneration(ctx, a)
			continue
		}
		if a.Generation.Generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
			a.IndexerVersion, a.IndexedAtMs, a.IndexedInputHash = 0, 0, nil
		}
		if a.Prepared == nil || !a.Prepared.claim(sid, id) {
			unstaged = append(unstaged, p)
		}
		activations[i] = a
		positions = append(positions, i)
		prepared = append(prepared, p)
	}
	if len(positions) == 0 {
		return results
	}
	fail := func(err error) []ActivationResult {
		for _, i := range positions {
			results[i].Err = err
		}
		return results
	}
	if err := s.stageHarmonizedBatch(ctx, unstaged); err != nil {
		return fail(err)
	}
	if err := reportHarmonizedWriterSeam(harmonizedSeamBeforeCommit); err != nil {
		return fail(err)
	}
	order := append([]int(nil), positions...)
	sort.Slice(order, func(i, j int) bool {
		return activations[order[i]].Generation.Generation.Metadata.SessionID < activations[order[j]].Generation.Generation.Metadata.SessionID
	})
	var releases []func() error
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			_ = releases[i]()
		}
		releases = nil
	}
	defer releaseAll()
	for _, i := range order {
		release, err := s.sessionLocker.LockExclusive(ctx, activations[i].Generation.Generation.Metadata.SessionID)
		if err != nil {
			return fail(err)
		}
		releases = append(releases, release)
	}
	writes := make([]ingest.SessionEntryWrite, len(positions))
	for j, i := range positions {
		writes[j] = generationActivationWrite(activations[i])
	}
	commits := s.IndexSessionEntryBatch(ctx, writes)
	releaseAll()
	if len(commits) != len(positions) {
		return fail(fmt.Errorf("store: activation batch returned %d results for %d candidates; retry harvest to reconcile installed authority", len(commits), len(positions)))
	}
	for j, i := range positions {
		results[i].Err = commits[j].Err
		if commits[j].Err != nil {
			continue
		}
		results[i].Outcome.Disposition = ingest.ActivationCommittedNow
		if commits[j].Skipped {
			results[i].Outcome.Disposition = ingest.ActivationAlreadyCommitted
			continue
		}
		if err := reportHarmonizedWriterSeam(harmonizedSeamAfterCommit); err != nil {
			results[i].Err = err
			continue
		}
		results[i].Err = s.deleteConvertedMirrorRows(ctx, prepared[j].sessionID)
	}
	return results
}

func generationActivationWrite(a GenerationActivation) ingest.SessionEntryWrite {
	v2 := a.Generation
	v2.Generation.Content = fillContentRecords(v2.Generation.Content, a.Blobs)
	v2.PriorEvidence = a.PriorEvidence
	mode := ingest.SessionEntryWriteReplaceAll
	if a.ExplicitRebuild {
		mode = ingest.SessionEntryWriteExplicitRebuild
	}
	return ingest.SessionEntryWrite{
		SessionID: v2.Generation.Metadata.SessionID, Result: v2, IndexVersion: 2, Mode: mode,
		RequireFullContent: captureRequiresFullContent(a.ContentCapture), ContentCapture: a.ContentCapture,
		IndexerVersion: a.IndexerVersion, IndexedAtMs: a.IndexedAtMs, ExpectedState: a.ExpectedState,
		CaptureRevision: a.CaptureRevision, IndexedInputHash: a.IndexedInputHash,
		ArtifactIdentity: a.ArtifactIdentity, PublicationCapture: a.Capture,
	}
}
