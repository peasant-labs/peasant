package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// TestSearchServesAtMostTheDocumentedCap pins the flat route's contract cap: the
// published schema documents this number, so a request above it is served at the
// cap rather than refused or widened. The grouped view shares the same handler
// line and therefore the same number.
func TestSearchServesAtMostTheDocumentedCap(t *testing.T) {
	db := storetest.Open(t)
	for i := 0; i < codemap.SearchHTTPMaxLimit+10; i++ {
		seedHelperGroupSession(t, db, helperGroupListingSession{
			ID:      fmt.Sprintf("cap-%03d", i),
			StartMs: int64(1000 + i),
			Purpose: "interaction",
			Origin:  "user",
			Text:    "pineapple",
		}, int64(i))
	}
	base := startHelperGroupServer(t, ServerConfig{
		Store:      db,
		Config:     config.BaseConfig(),
		Provider:   NewStoreDataProvider(db, sessionvisibility.Browsing()),
		ConfigHome: t.TempDir(),
	})

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
