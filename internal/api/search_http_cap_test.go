package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// seedCapSessions stores more matching sessions than the documented cap, so a
// request above the cap has to be truncated rather than served whole. Every
// seeded session contributes exactly one matching entry.
func seedCapSessions(t *testing.T, db *store.Store) {
	t.Helper()
	for i := 0; i < codemap.SearchMaxLimit+10; i++ {
		seedHelperGroupSession(t, db, helperGroupListingSession{
			ID:      fmt.Sprintf("cap-%03d", i),
			StartMs: int64(1000 + i),
			Purpose: "interaction",
			Origin:  "user",
			Text:    "pineapple",
		}, int64(i))
	}
}

func startCapServer(t *testing.T, db *store.Store) string {
	t.Helper()
	return startHelperGroupServer(t, ServerConfig{
		Store:      db,
		Config:     config.BaseConfig(),
		Provider:   NewStoreDataProvider(db, sessionvisibility.Browsing()),
		ConfigHome: t.TempDir(),
	})
}

// TestSearchServesAtMostTheDocumentedCap pins the flat route's contract cap: the
// published schema documents this number, so a request above it is served at the
// cap rather than refused or widened.
func TestSearchServesAtMostTheDocumentedCap(t *testing.T) {
	db := storetest.Open(t)
	seedCapSessions(t, db)
	base := startCapServer(t, db)

	body, status := helperGroupGET(t, base+"/api/v1/search?q=pineapple&limit=501")
	if status != http.StatusOK {
		t.Fatalf("search status = %d (body %s)", status, body)
	}
	var payload schema.SearchPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode search payload: %v", err)
	}
	if len(payload.Results) != codemap.SearchMaxLimit {
		t.Fatalf("results = %d, want exactly %d (the documented cap)", len(payload.Results), codemap.SearchMaxLimit)
	}
}

// TestGroupedSearchServesAtMostTheDocumentedCap pins the same cap on the grouped
// view. The clamp runs before the view is dispatched, so this is the test that
// fails if it moves below the `view=grouped` branch: the grouped read then hands
// the ranked reader an oversized window, which it refuses, and the request fails
// instead of being served at the cap. Each seeded session contributes one
// matching entry, so the number of returned items IS the number of matches kept.
func TestGroupedSearchServesAtMostTheDocumentedCap(t *testing.T) {
	db := storetest.Open(t)
	seedCapSessions(t, db)
	base := startCapServer(t, db)

	body, status := helperGroupGET(t, base+"/api/v1/search?q=pineapple&limit=501&view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped search status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode grouped search payload: %v", err)
	}
	// The ranked reader keeps exactly the documented cap; the grouped payload
	// then pages that bounded set into perPage rows plus nextItems.
	if payload.Total != codemap.SearchMaxLimit {
		t.Fatalf("grouped total = %d, want exactly %d (the documented cap)", payload.Total, codemap.SearchMaxLimit)
	}
	if len(payload.Items) != int(payload.PerPage) || len(payload.NextItems) != int(payload.PerPage) {
		t.Fatalf("grouped page = items %d nextItems %d, want %d and %d", len(payload.Items), len(payload.NextItems), payload.PerPage, payload.PerPage)
	}
}
