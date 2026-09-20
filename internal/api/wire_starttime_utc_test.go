package api_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
)

// TestWireStartTimeIsCanonicalUTC pins the Local API wire startTime to a
// canonical UTC instant.
//
// time.UnixMilli returns the time in the MACHINE's local zone, and the
// published @peasant-labs/schema decoder validates date-times with zod v4's
// z.iso.datetime(), which rejects non-UTC offsets by default. A summary built
// without an explicit .UTC() therefore renders as "…-07:00" on any non-UTC
// machine and fails the grouped local list (and by-id) decode for every
// session. This test asserts the exact wire string through the real provider,
// so the construction cannot regress to a local-zone value on machines where
// local == UTC and the mistake would otherwise go unnoticed.
func TestWireStartTimeIsCanonicalUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		sessionID   = "aaaaaaaa-1111-4111-8111-111111111111"
		projectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		startMs     = 1_700_000_000_000
		// 1_700_000_000_000 ms since the epoch = 2023-11-14T22:13:20Z.
		wantUTC = "2023-11-14T22:13:20Z"
	)

	s := openTestStore(t)
	entry := makeStoreEntry(t, sessionID, projectHash, "utc-host",
		defaults.HarnessClaudeCode, startMs, 100, 200, "utc-project", 5, 2, 60_000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	provider := api.NewStoreDataProvider(s, sessionvisibility.All())

	summaries, err := provider.SessionSummariesByID(ctx, []string{sessionID})
	if err != nil {
		t.Fatalf("SessionSummariesByID: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("got %d summaries, want 1", len(summaries))
	}

	raw, err := json.Marshal(summaries[0])
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var wire struct {
		StartTime string `json:"startTime"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal summary wire: %v", err)
	}
	if wire.StartTime != wantUTC {
		t.Errorf("wire startTime = %q, want canonical UTC %q (a local-zone offset fails the schema decoder)", wire.StartTime, wantUTC)
	}
}