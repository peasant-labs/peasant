package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sync_parent_projection.yaml
var syncParentProjectionYAML []byte

func TestSyncParentProjection(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name       string `yaml:"name"`
			Parent     string `yaml:"parent"`
			Unresolved bool   `yaml:"unresolved"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(syncParentProjectionYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			identity := store.ParentIdentityRow{SessionID: testutil.TestSessionUUID, ParentIdentity: tc.Parent, ParentUnresolved: tc.Unresolved}
			db := &testutil.StubPushStore{
				AllSessions:      []ingest.PushSessionRow{{SessionID: testutil.TestSessionUUID, ModelHarness: defaults.HarnessOpenCode.String(), ProjectHash: strings.Repeat("a", 64), SessionOrigin: "unknown"}},
				ParentIdentities: []store.ParentIdentityRow{identity},
			}
			response := httptest.NewRecorder()
			serveSyncSessions(response, httptest.NewRequest("GET", "/api/v1/sync/sessions", nil), db)
			if response.Code != 200 {
				t.Fatalf("flat projection: %d %s", response.Code, response.Body.String())
			}
			var payload struct {
				Sessions []map[string]json.RawMessage `json:"sessions"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Sessions) != 1 {
				t.Fatalf("expected one session: %s", response.Body.String())
			}
			var wantParent *string
			var wantUnresolved *bool
			if tc.Parent != "" {
				wantParent = &tc.Parent
			}
			if tc.Unresolved {
				wantUnresolved = &tc.Unresolved
			}
			flat := payload.Sessions[0]
			if _, present := flat["parentSessionId"]; present != (wantParent != nil) {
				t.Fatalf("parent id presence: %s", response.Body.String())
			}
			if _, present := flat["parentUnresolved"]; present != (wantUnresolved != nil) {
				t.Fatalf("unresolved presence: %s", response.Body.String())
			}
			var decoded syncSessionResponse
			rowJSON, err := json.Marshal(flat)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rowJSON, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.ParentSessionID, wantParent) || !reflect.DeepEqual(decoded.ParentUnresolved, wantUnresolved) {
				t.Fatalf("flat markers: %+v", decoded)
			}
			entries, err := loadSyncEntries(t.Context(), db)
			if err != nil {
				t.Fatal(err)
			}
			summary, syncSummary, err := buildSyncGroupedRow(entries[0], store.GroupingEvidenceRow{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(summary.ParentSessionID, wantParent) ||
				!reflect.DeepEqual(syncSummary.ParentSessionID, wantParent) ||
				!reflect.DeepEqual(summary.ParentUnresolved, wantUnresolved) ||
				!reflect.DeepEqual(syncSummary.ParentUnresolved, wantUnresolved) {
				t.Fatalf("grouped mirrors disagree: session=%+v sync=%+v", summary, syncSummary)
			}
		})
	}
}

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
