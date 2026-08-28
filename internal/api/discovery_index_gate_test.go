package api_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
)

// TestSessionSummaries_HidesNotYetIndexedSession pins the discovery gate: a
// session that is stored but has not completed an index pass (indexed_at NULL,
// no entries, so opening it would show no turns) is withheld from the discovery
// list, yet still resolves through the by-id (deep-link) path. Once it is
// indexed, the list includes it. The two paths differ only in WHICH rows they
// return, so a deep link never fails because a session is merely not yet
// browsable.
func TestSessionSummaries_HidesNotYetIndexedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		sessionID   = "aaaaaaaa-1111-4111-8111-111111111111"
		projectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	s := openTestStore(t)
	entry := makeStoreEntry(t, sessionID, projectHash, "indexgate-host",
		defaults.HarnessClaudeCode, 1_700_000_000_000, 100, 200, "index-gate-project", 5, 2, 60_000)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	provider := api.NewStoreDataProvider(s, sessionvisibility.All())

	inList := func() bool {
		t.Helper()
		summaries, err := provider.SessionSummaries(ctx)
		if err != nil {
			t.Fatalf("SessionSummaries: %v", err)
		}
		for i := range summaries {
			if summaries[i].ID == sessionID {
				return true
			}
		}
		return false
	}

	// Not yet indexed: absent from the discovery list.
	if inList() {
		t.Fatal("a not-yet-indexed session appeared in the discovery list; the index gate did not hold")
	}

	// ...but a deep link still resolves it, so it is discovery scope, not access.
	byID, err := provider.SessionSummariesByID(ctx, []string{sessionID})
	if err != nil {
		t.Fatalf("SessionSummariesByID: %v", err)
	}
	if len(byID) != 1 || byID[0].ID != sessionID {
		t.Fatalf("by-id resolution of a not-yet-indexed session = %+v, want exactly %s", byID, sessionID)
	}

	// Once indexed, the discovery list includes it.
	api.MarkStoredSessionsIndexed(t, s)
	if !inList() {
		t.Fatal("an indexed session was missing from the discovery list")
	}
}
