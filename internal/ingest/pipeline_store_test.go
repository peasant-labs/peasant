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
