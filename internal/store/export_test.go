package store

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
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
// validated settle of this store. The name is skipped-stats-write or
// skipped-mirror-write.
// This function is only available in test builds.
func (s *Store) SetValidatedSettleMutationForTest(t testing.TB, name string) {
	t.Helper()
	mutation := unchangedSettleMutation(name)
	if mutation != settleSkipsStatsWrite && mutation != settleSkipsMirrorWrite {
		t.Fatalf("unknown validated-settle mutation %q", name)
	}
	s.settleValidatedMutation = func() unchangedSettleMutation { return mutation }
}

// ActiveHarmonizedViewForTest is the exported form of the skip comparison's
// operand, the structured read of a session's active harmonized generation.
// This type is only available in test builds.
type ActiveHarmonizedViewForTest struct {
	Found        bool
	GenerationID string
	Record       GenerationRecord
	Children     GenerationChildren
	Mapping      []MappedBodyForTest
	Descriptors  map[schema.SourceEntryRef]string
	Aliases      map[string]schema.SourceEntryRef
}

// MappedBodyForTest is one entry-mapping row of the active view.
// This type is only available in test builds.
type MappedBodyForTest struct {
	Partition int
	Index     int
	Digest    string
}

// ReadActiveHarmonizedViewForTest runs the production readActiveHarmonizedView
// on a pooled connection inside one savepoint, so the reads see one snapshot.
// This function is only available in test builds.
func (s *Store) ReadActiveHarmonizedViewForTest(ctx context.Context, sessionID schema.SessionID) (_ ActiveHarmonizedViewForTest, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return ActiveHarmonizedViewForTest{}, err
	}
	defer s.pool.Put(conn)
	defer sqlitex.Save(conn)(&err)
	view, err := readActiveHarmonizedView(conn, sessionID)
	if err != nil {
		return ActiveHarmonizedViewForTest{}, err
	}
	out := ActiveHarmonizedViewForTest{Found: view.found, GenerationID: view.generationID, Record: view.record, Children: view.children, Descriptors: view.descriptors, Aliases: view.aliases}
	for _, mapped := range view.mapping {
		out.Mapping = append(out.Mapping, MappedBodyForTest{Partition: mapped.partition, Index: mapped.index, Digest: mapped.digest})
	}
	return out, nil
}
