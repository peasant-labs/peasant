package metrics_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func TestMetricsRecomputesChangedInputAndReusesEqualProof(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "metrics.db"), store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	seedSession(t, t.Context(), db, string(sid))
	var entries []schema.SessionEntry
	for _, row := range loadTitleFixture(t).Cases {
		if row.Name != "nested_user_is_not_title" {
			continue
		}
		for index, entry := range row.Entries {
			preview, tokens := entry.Preview, index+1
			entries = append(entries, schema.SessionEntry{SessionID: sid, Harness: row.Harness,
				EntryIndex: index, EntryType: ingest.EntryTypeText, Role: entry.Role,
				Depth: entry.Depth, ContentPreview: &preview, TokensIn: &tokens})
		}
	}
	if len(entries) == 0 {
		t.Fatal("existing title fixture is missing")
	}
	if err := db.IndexSessionEntries(t.Context(), sid, entries); err != nil {
		t.Fatal(err)
	}
	engine := metrics.NewEngineWithModels(db, db)
	if count, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil || count != 1 {
		t.Fatalf("initial computation: count=%d error=%v", count, err)
	}
	first, err := db.GetMetrics(t.Context(), sid)
	if err != nil || first == nil || first.InputHash == nil || first.OutputHash == nil || first.M2TokenOutcomeRatio == nil {
		t.Fatalf("initial metrics lacked current-run output/proof: %+v %v", first, err)
	}
	if count, err := engine.RecomputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil || count != 0 {
		t.Fatalf("equal input was recomputed: count=%d error=%v", count, err)
	}
	unchanged, err := db.GetMetrics(t.Context(), sid)
	if err != nil || !reflect.DeepEqual(first, unchanged) {
		t.Fatal("equal input changed stored metrics or completion clock")
	}
	entries[len(entries)-1].IsError = true
	if err := db.IndexSessionEntries(t.Context(), sid, entries); err != nil {
		t.Fatal(err)
	}
	if count, err := engine.ComputeMetrics(t.Context(), []ingest.SessionID{sid}); err != nil || count != 1 {
		t.Fatalf("same-version changed input was skipped: count=%d error=%v", count, err)
	}
	changed, err := db.GetMetrics(t.Context(), sid)
	if err != nil || changed == nil || changed.InputHash == nil || changed.OutputHash == nil ||
		*changed.InputHash == *first.InputHash || *changed.OutputHash == *first.OutputHash ||
		changed.M2TokenOutcomeRatio == nil || *changed.M2TokenOutcomeRatio <= *first.M2TokenOutcomeRatio {
		t.Fatalf("changed input did not refresh current-run metrics/proof: %+v %v", changed, err)
	}
}
