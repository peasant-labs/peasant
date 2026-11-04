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
	for i := 0; i < codemap.SearchHTTPMaxLimit+10; i++ {
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
	if len(payload.Results) != codemap.SearchHTTPMaxLimit {
		t.Fatalf("results = %d, want exactly %d (the documented cap)", len(payload.Results), codemap.SearchHTTPMaxLimit)
	}
}

// TestGroupedSearchServesAtMostTheDocumentedCap pins the same cap on the grouped
// view. The clamp runs before the view is dispatched, so this is the test that
// fails if it moves below the `view=grouped` branch: the grouped read would then
// hand the ranked reader an oversized window and answer with more matches than
// the contract documents. Each seeded session contributes one matching entry, so
// the number of returned items IS the number of matches kept.
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
	if len(payload.Items) != codemap.SearchHTTPMaxLimit {
		t.Fatalf("grouped items = %d, want exactly %d (the documented cap)", len(payload.Items), codemap.SearchHTTPMaxLimit)
	}
}
