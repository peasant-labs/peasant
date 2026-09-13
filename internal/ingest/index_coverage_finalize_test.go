package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// coverageFinalizeStore is a SessionStore double that answers entries
// membership from a fixed map, or fails it on demand.
type coverageFinalizeStore struct {
	without map[SessionID]bool
	err     error
}

func (s *coverageFinalizeStore) InsertSessions(context.Context, []StoreEntry) error {
	return nil
}
func (s *coverageFinalizeStore) LookupSessionLocation(context.Context, SessionID) (string, string, error) {
	return "", "", nil
}
func (s *coverageFinalizeStore) BulkLookupSessionLocations(context.Context, []SessionID) (map[SessionID]SessionLocation, error) {
	return map[SessionID]SessionLocation{}, nil
}
func (s *coverageFinalizeStore) UpsertSessionCommits(context.Context, SessionID, []CommitInfo) error {
	return nil
}
func (s *coverageFinalizeStore) CleanupOrphanProjects(context.Context) error { return nil }
func (s *coverageFinalizeStore) Close() error                                { return nil }
func (s *coverageFinalizeStore) SessionsWithoutEntries(_ context.Context, ids []SessionID) (map[SessionID]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[SessionID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.without[id]
	}
	return out, nil
}

// coverageFinalizeLog builds an index log over three fixed sessions.
func coverageFinalizeLog(outcomes ...IndexOutcome) []IndexLogEntry {
	ids := []string{
		"11111111-1111-4111-8111-000000000101",
		"11111111-1111-4111-8111-000000000102",
		"11111111-1111-4111-8111-000000000103",
	}
	log := make([]IndexLogEntry, 0, len(outcomes))
	for i, outcome := range outcomes {
		id, err := NewSessionID(ids[i%len(ids)])
		if err != nil {
			panic(err)
		}
		log = append(log, IndexLogEntry{SessionID: id, Harness: HarnessClaudeCode, Outcome: outcome})
	}
	return log
}

func coverageFinalizeDiagnostics(p *Pipeline) []DiagnosticEntry {
	return p.snapshotDiagnostics()
}

// coverageFinalizePipeline builds the smallest pipeline that can run the
// production finalize: the real OS filesystem over an empty temp dir (cleanup
// finds no orphans there) plus the given store handle.
func coverageFinalizePipeline(t *testing.T, s SessionStore) *Pipeline {
	t.Helper()
	return &Pipeline{
		fs:     &OSFileSystem{},
		config: PipelineConfig{OutputDir: ResolvedPath(t.TempDir())},
		store:  s,
	}
}

func assertCoverageDiagnostic(t *testing.T, diagnostics []DiagnosticEntry, logPrefix string) {
	t.Helper()
	var found []DiagnosticEntry
	for _, diagnostic := range diagnostics {
		if diagnostic.ErrorType == "index_coverage_unavailable" {
			found = append(found, diagnostic)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one index_coverage_unavailable diagnostic, got %d: %+v", len(found), diagnostics)
	}
	if !strings.Contains(found[0].Location, logPrefix) {
		t.Errorf("diagnostic location = %q, want it to name the %q finalize step", found[0].Location, logPrefix)
	}
	if strings.TrimSpace(found[0].Message) == "" || strings.TrimSpace(found[0].Remediation) == "" {
		t.Errorf("diagnostic must carry an actionable message and remediation, got %+v", found[0])
	}
}

// TestIndexComputeAndFinalize_ComputesCoverage drives the production finalize
// with a capability-carrying store and pins the counts on the result.
func TestIndexComputeAndFinalize_ComputesCoverage(t *testing.T) {
	emptyID, _ := NewSessionID("11111111-1111-4111-8111-000000000101")
	p := coverageFinalizePipeline(t, &coverageFinalizeStore{without: map[SessionID]bool{emptyID: true}})
	log := coverageFinalizeLog(IndexOutcomeError, IndexOutcomeError, IndexOutcomeIndexed)

	result, err := p.indexComputeAndFinalize(context.Background(), nil, nil, nil, nil, time.Now(), log, IndexOutcomeIndexed, "pipeline", nil)
	if err != nil {
		t.Fatalf("indexComputeAndFinalize: %v", err)
	}
	coverage := result.IndexCoverage
	if coverage == nil {
		t.Fatal("finalize left IndexCoverage nil for a store that answers membership")
	}
	if coverage.FailedAttempts != 2 || coverage.Empty != 1 || coverage.FailedRetained != 1 {
		t.Errorf("IndexCoverage = %+v, want {FailedAttempts:2 Empty:1 FailedRetained:1}", coverage)
	}
	if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
		t.Errorf("FailedAttempts = %d, want Empty + FailedRetained = %d", coverage.FailedAttempts, coverage.Empty+coverage.FailedRetained)
	}
	for _, diagnostic := range coverageFinalizeDiagnostics(p) {
		if diagnostic.ErrorType == "index_coverage_unavailable" {
			t.Errorf("successful coverage reported an unavailable diagnostic: %+v", diagnostic)
		}
	}
}

// TestIndexComputeAndFinalize_MembershipErrorLeavesNilWithOneDiagnostic pins
// the unavailable path: nil coverage, exactly one actionable diagnostic, and
// no partial count anywhere on the result.
func TestIndexComputeAndFinalize_MembershipErrorLeavesNilWithOneDiagnostic(t *testing.T) {
	p := coverageFinalizePipeline(t, &coverageFinalizeStore{err: errors.New("synthetic membership failure")})
	log := coverageFinalizeLog(IndexOutcomeError)

	result, err := p.indexComputeAndFinalize(context.Background(), nil, nil, nil, nil, time.Now(), log, IndexOutcomeReindexed, "reindex", nil)
	if err != nil {
		t.Fatalf("indexComputeAndFinalize: %v", err)
	}
	if result.IndexCoverage != nil {
		t.Errorf("IndexCoverage = %+v, want nil: a failed chunk must leave no partial count", result.IndexCoverage)
	}
	assertCoverageDiagnostic(t, coverageFinalizeDiagnostics(p), "reindex")
}

// TestIndexComputeAndFinalize_MissingCapabilityLeavesNilWithOneDiagnostic pins
// the capability-absent path through the same production finalize.
func TestIndexComputeAndFinalize_MissingCapabilityLeavesNilWithOneDiagnostic(t *testing.T) {
	p := coverageFinalizePipeline(t, nil)
	log := coverageFinalizeLog(IndexOutcomeError)

	result, err := p.indexComputeAndFinalize(context.Background(), nil, nil, nil, nil, time.Now(), log, IndexOutcomeIndexed, "pipeline", nil)
	if err != nil {
		t.Fatalf("indexComputeAndFinalize: %v", err)
	}
	if result.IndexCoverage != nil {
		t.Errorf("IndexCoverage = %+v, want nil: without a membership capability the answer is unavailable, never zero", result.IndexCoverage)
	}
	assertCoverageDiagnostic(t, coverageFinalizeDiagnostics(p), "pipeline")
}

// TestIndexComputeAndFinalize_CleanRunMeasuresZeroWithoutTheStore pins that a
// run with no failed attempts reports a measured zero and stays silent: no
// store question was asked, so no diagnostic is owed.
func TestIndexComputeAndFinalize_CleanRunMeasuresZeroWithoutTheStore(t *testing.T) {
	p := coverageFinalizePipeline(t, &coverageFinalizeStore{err: errors.New("must not be consulted")})
	log := coverageFinalizeLog(IndexOutcomeIndexed)

	result, err := p.indexComputeAndFinalize(context.Background(), nil, nil, nil, nil, time.Now(), log, IndexOutcomeIndexed, "pipeline", nil)
	if err != nil {
		t.Fatalf("indexComputeAndFinalize: %v", err)
	}
	if result.IndexCoverage == nil {
		t.Fatal("finalize left IndexCoverage nil for a run with no failures; nil means unavailable, and zero failures is a measured zero")
	}
	if *result.IndexCoverage != (IndexCoverage{}) {
		t.Errorf("IndexCoverage = %+v, want all zeros", result.IndexCoverage)
	}
	if diagnostics := coverageFinalizeDiagnostics(p); len(diagnostics) != 0 {
		t.Errorf("clean run reported diagnostics %+v, want none", diagnostics)
	}
}
