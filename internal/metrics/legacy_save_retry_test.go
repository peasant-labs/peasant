package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

type retryLegacyMetricsStore struct {
	ingest.MetricsStore
	fail bool
}

var _ ingest.MetricsStore = (*retryLegacyMetricsStore)(nil)

func (s *retryLegacyMetricsStore) SaveMetrics(ctx context.Context, value *ingest.SessionMetrics) error {
	if s.fail {
		return errors.New("synthetic save failure")
	}
	return s.MetricsStore.SaveMetrics(ctx, value)
}

func TestLegacyComputeSaveFailureRemainsRetryable(t *testing.T) {
	db := storetest.OpenWith(t)
	sid := mustSessionID(t, testutil.TestSessionUUID)
	seedSession(t, t.Context(), db, string(sid))
	entry := schema.SessionEntry{SessionID: sid, Harness: ingest.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser}
	if err := db.IndexSessionEntries(t.Context(), sid, []schema.SessionEntry{entry}); err != nil {
		t.Fatal(err)
	}
	backing := &retryLegacyMetricsStore{MetricsStore: db, fail: true}
	engine := metrics.NewEngine(backing)
	count, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid})
	if err != nil || count != 0 {
		t.Fatalf("save failure aborted legacy compute: count=%d err=%v", count, err)
	}
	backing.fail = false
	count, err = engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid})
	if err != nil || count != 1 {
		t.Fatalf("legacy compute did not recover: count=%d err=%v", count, err)
	}
}
