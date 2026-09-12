package ingest_test

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// pipelineFixtureStore keeps persistence, eligibility and proof in the real
// test-owned Store. Existing doubles observe successful writes or inject errors;
// their maps never authorize a parser run or an input proof.
type pipelineFixtureStore struct {
	*store.Store
	t        *testing.T
	mu       sync.Mutex
	sessions *testutil.StubSessionStore
	metrics  *testutil.StubMetricsStore
}

func newPipelineFixtureStore(t *testing.T, sessions *testutil.StubSessionStore, metrics *testutil.StubMetricsStore) *pipelineFixtureStore {
	t.Helper()
	return &pipelineFixtureStore{Store: storetest.Open(t), t: t, sessions: sessions, metrics: metrics}
}

func (s *pipelineFixtureStore) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	if s.sessions != nil {
		if err := errors.Join(s.sessions.InsertErr, s.sessions.UpsertCommitsErr); err != nil {
			results := make([]ingest.ArtifactMirrorResult, len(requests))
			for i, request := range requests {
				results[i] = ingest.ArtifactMirrorResult{SessionID: request.Artifact.Metadata.SessionID, Err: err}
			}
			return results
		}
	}
	results := s.Store.MirrorArtifacts(ctx, requests)
	if s.sessions != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, result := range results {
			if result.Err == nil && result.Mirrored {
				s.sessions.MirrorArtifacts(ctx, requests[i:i+1])
			}
		}
	}
	return results
}

func (s *pipelineFixtureStore) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	if s.metrics != nil {
		if err := errors.Join(s.metrics.IndexErr, s.metrics.UpdateIndexErr); err != nil {
			results := make([]ingest.SessionEntryWriteResult, len(writes))
			for i, write := range writes {
				results[i] = ingest.SessionEntryWriteResult{SessionID: write.SessionID, Err: err}
			}
			return results
		}
	}
	results := s.Store.IndexSessionEntryBatch(ctx, writes)
	if s.metrics != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, result := range results {
			if result.Err != nil || !result.Written {
				continue
			}
			entries, err := s.Store.ListEntries(context.WithoutCancel(ctx), result.SessionID)
			if err != nil {
				s.t.Errorf("observe committed entries for %s: %v", result.SessionID, err)
				continue
			}
			state, err := s.Store.ReadIndexState(context.WithoutCancel(ctx), result.SessionID)
			if err != nil {
				s.t.Errorf("observe committed index state for %s: %v", result.SessionID, err)
				continue
			}
			s.metrics.IndexedEntries[result.SessionID] = entries
			s.metrics.IndexStates[result.SessionID] = state.IndexerVersion
		}
	}
	return results
}

func (s *pipelineFixtureStore) SaveMetrics(ctx context.Context, metrics *ingest.SessionMetrics) error {
	if s.metrics != nil && s.metrics.SaveErr != nil {
		return s.metrics.SaveErr
	}
	if err := s.Store.SaveMetrics(ctx, metrics); err != nil {
		return err
	}
	if s.metrics != nil {
		s.mu.Lock()
		s.metrics.SavedMetrics[metrics.SessionID] = metrics
		s.mu.Unlock()
	}
	return nil
}

// The production engine writes metrics through the input-proof path, so a
// fixture that only mirrors SaveMetrics sees none of them and a test that asks
// whether metrics were computed measures the mirror instead of the run.
func (s *pipelineFixtureStore) SaveMetricsForInput(ctx context.Context, expected *ingest.MetricInput, metrics *ingest.SessionMetrics) error {
	if s.metrics != nil && s.metrics.SaveErr != nil {
		return s.metrics.SaveErr
	}
	if err := s.Store.SaveMetricsForInput(ctx, expected, metrics); err != nil {
		return err
	}
	if s.metrics != nil && metrics != nil {
		s.mu.Lock()
		s.metrics.SavedMetrics[metrics.SessionID] = metrics
		s.mu.Unlock()
	}
	return nil
}

func (s *pipelineFixtureStore) ListStaleIndexSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions) ([]ingest.SessionID, error) {
	if s.metrics != nil {
		s.mu.Lock()
		s.metrics.ListStaleCalledWithTargets = maps.Clone(targets)
		s.mu.Unlock()
		if s.metrics.StaleIndexErr != nil {
			return nil, s.metrics.StaleIndexErr
		}
	}
	return s.Store.ListStaleIndexSessions(ctx, targets)
}

// durabilityStore wraps a real *store.Store opened at a persistent path so that
// a crash between two write commits is modeled as a fault on one write and a
// restart as a fresh Store + Pipeline over the same database file. It injects
// the two seams the design's crash table names: a per-session MIRROR failure
// (the row is never recorded and the installed files are left on disk) and an
// ENTRY-BATCH failure (the row is recorded but its entries are not committed).
// Everything not faulted goes to the real store, so a child whose parent's
// mirror was faulted is refused by the store's own "parent not stored" rule in
// a later transaction rather than by the decorator.
type durabilityStore struct {
	*store.Store
	mu          sync.Mutex
	failMirror  map[ingest.SessionID]error
	failEntries error
	mirrorCalls int
	maxPage     int
	closeOnce   sync.Once
}

// newDurabilityStore opens a real store at dbPath (already migrated by a
// storetest golden copy) and registers Close. A restart opens a second one on
// the same path after the first is shut down.
func newDurabilityStore(t *testing.T, dbPath string) *durabilityStore {
	t.Helper()
	s, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open durability store at %s: %v", dbPath, err)
	}
	ds := &durabilityStore{Store: s, failMirror: make(map[ingest.SessionID]error)}
	t.Cleanup(ds.Shutdown)
	return ds
}

// Shutdown closes the store once, so a restart can close run 1 explicitly and
// the registered cleanup is a no-op.
func (s *durabilityStore) Shutdown() {
	s.closeOnce.Do(func() { _ = s.Store.Close() })
}

// FailMirrorFor forces the MirrorArtifacts result for one session to fail; its
// row is never recorded. Other sessions in the same page still reach the store.
func (s *durabilityStore) FailMirrorFor(sid ingest.SessionID, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMirror[sid] = err
}

// FailEntries forces every IndexSessionEntryBatch write to fail, modeling a
// crash after the row is mirrored but before its entries commit.
func (s *durabilityStore) FailEntries(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failEntries = err
}

// MirrorCalls returns how many MirrorArtifacts invocations carried a non-empty
// page; with pages of at most MirrorPageSize this is the store-level mirror
// transaction count.
func (s *durabilityStore) MirrorCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mirrorCalls
}

// MaxPage returns the largest page any MirrorArtifacts invocation carried, so a
// test can prove no transaction ever exceeded MirrorPageSize.
func (s *durabilityStore) MaxPage() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxPage
}

func (s *durabilityStore) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) []ingest.ArtifactMirrorResult {
	s.mu.Lock()
	if len(requests) > 0 {
		s.mirrorCalls++
	}
	if len(requests) > s.maxPage {
		s.maxPage = len(requests)
	}
	forward := make([]ingest.ArtifactMirrorRequest, 0, len(requests))
	forced := make(map[ingest.SessionID]error)
	for _, request := range requests {
		sid := request.Artifact.Metadata.SessionID
		if err, ok := s.failMirror[sid]; ok {
			forced[sid] = err
			continue
		}
		forward = append(forward, request)
	}
	s.mu.Unlock()

	var results []ingest.ArtifactMirrorResult
	if len(forward) > 0 {
		results = s.Store.MirrorArtifacts(ctx, forward)
	}
	for sid, err := range forced {
		results = append(results, ingest.ArtifactMirrorResult{SessionID: sid, Err: err})
	}
	return results
}

func (s *durabilityStore) IndexSessionEntryBatch(ctx context.Context, writes []ingest.SessionEntryWrite) []ingest.SessionEntryWriteResult {
	s.mu.Lock()
	failEntries := s.failEntries
	s.mu.Unlock()
	if failEntries != nil {
		results := make([]ingest.SessionEntryWriteResult, len(writes))
		for i, write := range writes {
			results[i] = ingest.SessionEntryWriteResult{SessionID: write.SessionID, Err: failEntries}
		}
		return results
	}
	return s.Store.IndexSessionEntryBatch(ctx, writes)
}
