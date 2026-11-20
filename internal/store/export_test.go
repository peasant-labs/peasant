package store

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// PoolForTest exposes the connection pool for test assertions.
// This function is only available in test builds.
func (s *Store) PoolForTest() *sqlitex.Pool {
	return s.pool
}

// SetSessionMembershipChunk installs a per-store replacement for one chunk's
// membership read. It exists so a test can fail a LATER chunk after earlier
// chunks succeeded, which is how the all-or-nothing contract is exercised
// against the real chunk loop. Production never calls it.
// This function is only available in test builds.
func (s *Store) SetSessionMembershipChunk(fn func(conn *sqlite.Conn, chunk []ingest.SessionID, record func(ingest.SessionID)) error) {
	s.sessionMembershipChunk = fn
}

// SessionsWithoutEntriesChunkSizeForTest exposes the membership-read chunk size
// so a fixture can assert that its generated population spans more than one
// chunk instead of hard-coding the boundary.
// This function is only available in test builds.
func SessionsWithoutEntriesChunkSizeForTest() int {
	return sessionsWithoutEntriesChunkSize
}

// SettleCapturedGenerationUnchangedAtForTest is SettleCapturedGenerationUnchanged
// with the settle's stats stamp pinned, so two forks that settle the same
// session write the same bytes.
// This function is only available in test builds.
func (s *Store) SettleCapturedGenerationUnchangedAtForTest(ctx context.Context, request ingest.ManagedGenerationUnchangedProof, updatedAtMs int64) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	proof, found, err := s.capturedUnchangedRequest(ctx, request)
	if err != nil {
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, err
	}
	if !found {
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, &ingest.StaleIndexWorkError{SessionID: request.SessionID}
	}
	proof.UpdatedAtMs = updatedAtMs
	result, err := s.SettleManagedGenerationUnchanged(ctx, proof)
	if err != nil {
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, err
	}
	return ingest.ManagedGenerationUnchangedSettleOutcome{GenerationID: result.GenerationID}, nil
}

// SetValidatedSettleMutationForTest removes one named write from every
// validated settle for the rest of the test. The name is
// skipped-stats-write or skipped-mirror-write.
// This function is only available in test builds.
func SetValidatedSettleMutationForTest(t testing.TB, name string) {
	t.Helper()
	mutation := unchangedSettleMutation(name)
	if mutation != settleSkipsStatsWrite && mutation != settleSkipsMirrorWrite {
		t.Fatalf("unknown validated-settle mutation %q", name)
	}
	previous := settleValidatedMutationForTest
	settleValidatedMutationForTest = func() unchangedSettleMutation { return mutation }
	t.Cleanup(func() { settleValidatedMutationForTest = previous })
}
