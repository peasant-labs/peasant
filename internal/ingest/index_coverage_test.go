package ingest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// stubCoverageReader answers entries membership from a fixed map, or fails.
type stubCoverageReader struct {
	without map[ingest.SessionID]bool
	err     error
	calls   int
}

func (s *stubCoverageReader) SessionsWithoutEntries(_ context.Context, ids []ingest.SessionID) (map[ingest.SessionID]bool, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[ingest.SessionID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.without[id]
	}
	return out, nil
}

func coverageFailedIDs(t *testing.T, raws ...string) []ingest.SessionID {
	t.Helper()
	ids := make([]ingest.SessionID, 0, len(raws))
	for _, raw := range raws {
		id, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("NewSessionID(%q): %v", raw, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func assertCoverageIdentity(t *testing.T, coverage *ingest.IndexCoverage) {
	t.Helper()
	if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
		t.Errorf("FailedAttempts = %d, but Empty + FailedRetained = %d + %d; the three must always agree",
			coverage.FailedAttempts, coverage.Empty, coverage.FailedRetained)
	}
}

func TestComputeIndexCoverage_EmptyFailedSetIsAMeasuredZero(t *testing.T) {
	t.Parallel()
	reader := &stubCoverageReader{}
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, nil)
	if err != nil {
		t.Fatalf("ComputeIndexCoverage: %v", err)
	}
	if coverage == nil {
		t.Fatal("ComputeIndexCoverage answered nil for an empty failed set; nil means unavailable, and there is nothing unavailable about zero failures")
	}
	if *coverage != (ingest.IndexCoverage{}) {
		t.Errorf("ComputeIndexCoverage = %+v, want all zeros", coverage)
	}
	if reader.calls != 0 {
		t.Errorf("ComputeIndexCoverage consulted the store %d time(s) for zero failed sessions", reader.calls)
	}
}

func TestComputeIndexCoverage_MixedFailuresSplitEmptyAndRetained(t *testing.T) {
	t.Parallel()
	failed := coverageFailedIDs(t,
		"11111111-1111-4111-8111-000000000001",
		"11111111-1111-4111-8111-000000000002",
		"11111111-1111-4111-8111-000000000003",
	)
	reader := &stubCoverageReader{without: map[ingest.SessionID]bool{failed[0]: true, failed[2]: true}}
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, failed)
	if err != nil {
		t.Fatalf("ComputeIndexCoverage: %v", err)
	}
	if coverage == nil {
		t.Fatal("ComputeIndexCoverage answered nil for a readable store")
	}
	assertCoverageIdentity(t, coverage)
	if coverage.FailedAttempts != 3 || coverage.Empty != 2 || coverage.FailedRetained != 1 {
		t.Errorf("ComputeIndexCoverage = %+v, want {FailedAttempts:3 Empty:2 FailedRetained:1}", coverage)
	}
}

func TestComputeIndexCoverage_AllRetained(t *testing.T) {
	t.Parallel()
	failed := coverageFailedIDs(t,
		"11111111-1111-4111-8111-000000000004",
		"11111111-1111-4111-8111-000000000005",
	)
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), &stubCoverageReader{}, failed)
	if err != nil {
		t.Fatalf("ComputeIndexCoverage: %v", err)
	}
	assertCoverageIdentity(t, coverage)
	if coverage.FailedAttempts != 2 || coverage.Empty != 0 || coverage.FailedRetained != 2 {
		t.Errorf("ComputeIndexCoverage = %+v, want {FailedAttempts:2 Empty:0 FailedRetained:2}", coverage)
	}
}

func TestComputeIndexCoverage_AllEmpty(t *testing.T) {
	t.Parallel()
	failed := coverageFailedIDs(t, "11111111-1111-4111-8111-000000000006")
	reader := &stubCoverageReader{without: map[ingest.SessionID]bool{failed[0]: true}}
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, failed)
	if err != nil {
		t.Fatalf("ComputeIndexCoverage: %v", err)
	}
	assertCoverageIdentity(t, coverage)
	if coverage.FailedAttempts != 1 || coverage.Empty != 1 || coverage.FailedRetained != 0 {
		t.Errorf("ComputeIndexCoverage = %+v, want {FailedAttempts:1 Empty:1 FailedRetained:0}", coverage)
	}
}

func TestComputeIndexCoverage_MissingCapabilityIsUnavailable(t *testing.T) {
	t.Parallel()
	failed := coverageFailedIDs(t, "11111111-1111-4111-8111-000000000007")
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), nil, failed)
	if err == nil {
		t.Fatal("ComputeIndexCoverage succeeded without a membership capability; a missing capability must leave coverage unavailable, not zero")
	}
	if coverage != nil {
		t.Errorf("ComputeIndexCoverage = %+v, want nil: unavailable is nil, never zero", coverage)
	}
}

func TestComputeIndexCoverage_QueryErrorLeavesNilWithNoPartialCounts(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("synthetic membership failure")
	failed := coverageFailedIDs(t,
		"11111111-1111-4111-8111-000000000008",
		"11111111-1111-4111-8111-000000000009",
	)
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), &stubCoverageReader{err: sentinel}, failed)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ComputeIndexCoverage error = %v, want it to wrap the store failure", err)
	}
	if coverage != nil {
		t.Errorf("ComputeIndexCoverage = %+v, want nil: a failed chunk must not leave a partial count behind", coverage)
	}
}

func TestComputeIndexCoverage_UnnamedSessionCountsAsRetained(t *testing.T) {
	t.Parallel()
	named := coverageFailedIDs(t, "11111111-1111-4111-8111-000000000010")[0]
	unnamed := coverageFailedIDs(t, "11111111-1111-4111-8111-000000000011")[0]
	unrequested := coverageFailedIDs(t, "11111111-1111-4111-8111-000000000012")[0]
	reader := &stubCoverageReader{without: map[ingest.SessionID]bool{named: true, unrequested: true}}
	coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, []ingest.SessionID{named, unnamed})
	if err != nil {
		t.Fatalf("ComputeIndexCoverage: %v", err)
	}
	assertCoverageIdentity(t, coverage)
	if coverage.Empty != 1 || coverage.FailedRetained != 1 {
		t.Errorf("ComputeIndexCoverage = %+v, want {Empty:1 FailedRetained:1}: the unnamed session must count as retained, and the unrequested answer must be ignored", coverage)
	}
}
