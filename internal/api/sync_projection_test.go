package api

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func TestSyncSessionListNeverLoadsTranscriptBundles(t *testing.T) {
	db := &testutil.StubPushStore{
		AllSessions:       []ingest.PushSessionRow{{SessionID: testutil.TestSessionUUID}},
		ListEntriesErr:    errors.New("list must not read transcript entries"),
		PublicationInputs: map[ingest.SessionID]ingest.PublicationInputBundle{testutil.TestSessionUUID: {Metadata: schema.UnifiedMetadata{Model: testutil.TestModel}, Readiness: ingest.PublicationReady}},
	}
	response := httptest.NewRecorder()
	serveSyncSessions(response, httptest.NewRequest("GET", "/api/v1/sync/sessions", nil), db)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"syncStatus":"new"`) || db.PublicationMetadataCalls != 1 || db.PublicationInputCalls != 0 || db.ListEntriesCalls != 0 {
		t.Fatalf("list projection: %d %s calls=%d/%d/%d", response.Code, response.Body.String(), db.PublicationMetadataCalls, db.PublicationInputCalls, db.ListEntriesCalls)
	}
	db.PublicationInputErr = errors.New("database unavailable")
	response = httptest.NewRecorder()
	serveSyncSessions(response, httptest.NewRequest("GET", "/api/v1/sync/sessions", nil), db)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"syncStatus":"held"`) {
		t.Fatalf("database failure not held: %d %s", response.Code, response.Body.String())
	}
}
