package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

var _ ingest.ManagedGenerationValidator = (*Store)(nil)

// ValidateManagedGenerationUnchanged proves, read-only, that a captured input
// whose cheap proof failed only on a context term would reproduce the active
// generation's stored context. It needs the builder's metadata candidate:
// without one it answers unsupported and touches nothing.
//
// It runs on one pooled reader connection inside one read transaction, never
// on the write lane, while holding one refresh read slot. It evaluates every
// non-context term of the cheap proof in the cheap proof's order, then
// compares the candidate's catalog row and context rows with the stored ones.
// It reads no mapping row, body, blob or stat. The request's Budget is
// ignored: the read-slot cap bounds the concurrency.
func (s *Store) ValidateManagedGenerationUnchanged(ctx context.Context, r ingest.ManagedGenerationValidationRequest) (result ingest.ManagedGenerationValidationResult, err error) {
	if r.Candidate == nil {
		return ingest.ManagedGenerationValidationResult{
			Outcome: ingest.ValidationUnsupported,
			Detail:  "no metadata candidate was built for this session; the session parses",
		}, nil
	}
	release, err := s.acquireRefreshReadSlot(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: take a reader connection to validate the context of session %s: %w; nothing was read; the session parses, and the next harvest retries", r.SessionID, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	proof, err := evaluateUnchangedTermsOnConn(conn, ManagedGenerationUnchangedProof{
		SessionID: r.SessionID, GenerationID: r.GenerationID, Metadata: r.Metadata,
		InputDigest: r.InputDigest, IndexerTarget: r.IndexerTarget, FormatTarget: r.FormatTarget,
		Expected:  r.Expected,
		Validated: &ingest.ValidatedContext{GenerationID: r.GenerationID, Candidate: *r.Candidate},
	}, termModeValidateContext)
	if err != nil {
		return result, err
	}
	if proof.Proven {
		return ingest.ManagedGenerationValidationResult{Outcome: ingest.ValidationProven}, nil
	}
	return ingest.ManagedGenerationValidationResult{
		Outcome: ingest.ValidationNotProven, Mismatch: proof.Mismatch,
		Detail: fmt.Sprintf("the %s term does not hold for the active generation; the session parses", proof.Mismatch),
	}, nil
}

// refreshReadSlotStage names the point a caller of acquireRefreshReadSlot has
// reached, for refreshReadSlotSeam.
type refreshReadSlotStage int

const (
	// refreshReadSlotWaiting: the caller is about to wait for a free slot.
	refreshReadSlotWaiting refreshReadSlotStage = iota
	// refreshReadSlotHeld: the caller holds a slot.
	refreshReadSlotHeld
)

// refreshReadSlotCap is the number of read slots a store with poolSize
// connections allows: half the pool, at least one, at most
// defaults.ManagedRefreshReadSlots. The rest of the pool stays free for the
// readers the application serves and for the write lane.
func refreshReadSlotCap(poolSize int) int {
	if poolSize <= 0 {
		poolSize = DefaultPoolSize
	}
	return min(defaults.ManagedRefreshReadSlots, max(1, poolSize/2))
}

// acquireRefreshReadSlot takes one read slot, waiting while every slot is
// held. A cancelled ctx ends the wait with ctx.Err() and takes nothing. The
// returned release gives the slot back; call it exactly once.
func (s *Store) acquireRefreshReadSlot(ctx context.Context) (release func(), err error) {
	s.refreshReadSlotsOnce.Do(func() {
		s.refreshReadSlots = make(chan struct{}, refreshReadSlotCap(s.poolSize))
	})
	if seam := s.refreshReadSlotSeam; seam != nil {
		seam(ctx, refreshReadSlotWaiting)
	}
	select {
	case s.refreshReadSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release = func() { <-s.refreshReadSlots }
	if seam := s.refreshReadSlotSeam; seam != nil {
		seam(ctx, refreshReadSlotHeld)
	}
	return release, nil
}
