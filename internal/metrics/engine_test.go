package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func mustSessionID(t *testing.T, raw string) ingest.SessionID {
	t.Helper()
	sid, err := ingest.NewSessionID(raw)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", raw, err)
	}
	return sid
}

func seedEntries(store *testutil.StubMetricsStore, sid ingest.SessionID, entries []schema.SessionEntry) {
	store.IndexedEntries[sid] = entries
}

func TestEngine_ComputeMetrics_Basic(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	ctx := context.Background()

	sid := mustSessionID(t, testutil.TestSessionUUID)
	seedEntries(store, sid, []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
		{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
	})

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 1 {
		t.Errorf("computed: expected 1, got %d", n)
	}

	m := store.SavedMetrics[sid]
	if m == nil {
		t.Fatal("expected saved metrics, got nil")
	}
	if m.ComputeVersion == nil || *m.ComputeVersion != metrics.CurrentComputeVersion {
		t.Errorf("compute_version: expected %d, got %v", metrics.CurrentComputeVersion, m.ComputeVersion)
	}
	if m.ComputedAt == nil {
		t.Error("computed_at: expected non-nil")
	}
}

func TestEngine_SkipsUpToDate(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	ctx := context.Background()

	sid := mustSessionID(t, testutil.TestSessionUUID)

	// Pre-seed entries and metrics at current version.
	seedEntries(store, sid, []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
	})
	cv := metrics.CurrentComputeVersion
	store.SavedMetrics[sid] = &ingest.SessionMetrics{
		SessionID:      sid,
		QualityMetrics: schema.QualityMetrics{ComputeVersion: &cv},
	}

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 0 {
		t.Errorf("computed: expected 0 (skipped), got %d", n)
	}
}

func TestEngine_ForceRecomputes(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	engine.SetForce(true)
	ctx := context.Background()

	sid := mustSessionID(t, testutil.TestSessionUUID)

	seedEntries(store, sid, []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
	})
	cv2 := metrics.CurrentComputeVersion
	store.SavedMetrics[sid] = &ingest.SessionMetrics{
		SessionID:      sid,
		QualityMetrics: schema.QualityMetrics{ComputeVersion: &cv2},
	}

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 1 {
		t.Errorf("computed: expected 1 (forced), got %d", n)
	}
}

func TestEngine_SkipsEmptyEntries(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	ctx := context.Background()

	sid := mustSessionID(t, testutil.TestSessionUUID)
	// No entries seeded — store returns empty slice.

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 0 {
		t.Errorf("computed: expected 0 (no entries), got %d", n)
	}
}

func TestEngine_ErrorIsolation(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	ctx := context.Background()

	good := mustSessionID(t, testutil.TestSessionUUID)
	bad := mustSessionID(t, testutil.TestSubagentID)

	seedEntries(store, good, []schema.SessionEntry{
		{SessionID: good, EntryIndex: 0, Role: ingest.RoleUser},
	})
	// bad has no entries, so ListEntries returns empty → skipped.
	// But let's inject an error via MetricsExistFn to test error isolation.
	store.MetricsExistFn = func(sid ingest.SessionID, _ int) (bool, error) {
		if sid == bad {
			return false, errors.New("simulated check error")
		}
		return false, nil
	}

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{bad, good})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	// bad is skipped due to MetricsExist error, good is computed.
	if n != 1 {
		t.Errorf("computed: expected 1, got %d", n)
	}
	if store.SavedMetrics[good] == nil {
		t.Error("good session should have saved metrics")
	}
}

func TestEngine_MultipleSessions(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)
	ctx := context.Background()

	sid1 := mustSessionID(t, testutil.TestSessionUUID)
	sid2 := mustSessionID(t, testutil.TestSubagentID)

	seedEntries(store, sid1, []schema.SessionEntry{
		{SessionID: sid1, EntryIndex: 0, Role: ingest.RoleUser},
	})
	seedEntries(store, sid2, []schema.SessionEntry{
		{SessionID: sid2, EntryIndex: 0, Role: ingest.RoleAssistant},
	})

	n, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid1, sid2})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if n != 2 {
		t.Errorf("computed: expected 2, got %d", n)
	}
}

func TestEngine_ContextCancellation(t *testing.T) {
	t.Parallel()
	store := testutil.NewStubMetricsStore()
	engine := metrics.NewEngine(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	sid := mustSessionID(t, testutil.TestSessionUUID)
	seedEntries(store, sid, []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser},
	})

	_, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}
