package store

import (
	"context"
	"fmt"
)

func validateActivationBlobSupplies(a GenerationActivation) error {
	emitted := emittedRefs(a.Generation.Generation)
	for _, record := range a.Generation.Generation.Content {
		if _, inline := emitted[record.Ref]; inline {
			continue
		}
		if _, supplied := a.Blobs[record.Ref]; !supplied {
			return fmt.Errorf("store: generation %s for session %s has no supplied bytes for non-emitted ref %s; supply the content even when it is empty; nothing was staged", a.Generation.Generation.ID, a.Generation.Generation.Metadata.SessionID, record.Ref)
		}
	}
	return nil
}

// StageGenerationBatch validates candidates independently before staging their
// objects in shared bounded transactions. Returned handles never authorize reads.
func (s *Store) StageGenerationBatch(ctx context.Context, activations []GenerationActivation) ([]*PreparedGeneration, []error) {
	handles := make([]*PreparedGeneration, len(activations))
	errs := make([]error, len(activations))
	var candidates []*preparedHarmonized
	for i, a := range activations {
		err := s.requireGenerationSupport()
		if err == nil {
			err = validateActivationBlobSupplies(a)
		}
		if err == nil {
			err = validateGenerationID(a.Generation.Generation.ID)
		}
		var p *preparedHarmonized
		if err == nil {
			p, err = prepareHarmonizedCandidate(a.Generation.Generation.Metadata.SessionID, a.Generation.Generation, a.Blobs)
		}
		if err == nil {
			err = validateEntriesForStorage(p.sessionID, allGenerationEntries(p.generation))
		}
		if err == nil {
			err = reportHarmonizedWriterSeam(harmonizedSeamAfterPrepare)
		}
		if err != nil {
			errs[i] = err
			continue
		}
		// Skip/repair/retry candidates must not stage redundant objects.
		// Activation rechecks these predicates under its ordinary locks.
		if err := s.checkImmutableIdentity(ctx, p.sessionID, p.generation.ID, p.binding); err != nil {
			continue
		}
		repair, err := s.needsRepair(ctx, p.sessionID)
		if err != nil {
			errs[i] = err
			continue
		}
		active, err := s.readActiveView(ctx, p.sessionID)
		if err != nil {
			errs[i] = err
			continue
		}
		if repair || (active.found && refreshEqualsActive(p, active.view)) {
			continue
		}
		handles[i] = &PreparedGeneration{sessionID: p.sessionID, generationID: p.generation.ID, prepared: p}
		candidates = append(candidates, p)
	}
	if err := s.stageHarmonizedBatch(ctx, candidates); err != nil {
		for i, h := range handles {
			if h != nil {
				handles[i], errs[i] = nil, err
			}
		}
	}
	return handles, errs
}
