package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// TestGroupedViewIsOptInAndLegacyFlatRoutesAreUnchanged pins the compatibility
// half of the grouped contract: omitting view keeps the exact flat response
// envelope, and only the published grouped value selects the grouped payload.
func TestGroupedViewIsOptInAndLegacyFlatRoutesAreUnchanged(t *testing.T) {
	db := storetest.Open(t)
	seedGroupedRouteBoundarySessions(t, db)
	policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewStoreDataProvider(db, policy)
	base := startHelperGroupServer(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{}, Provider: provider})

	legacySessions, status := helperGroupGET(t, base+"/api/v1/sessions")
	if status != http.StatusOK {
		t.Fatalf("legacy sessions status = %d", status)
	}
	var sessionsEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(legacySessions, &sessionsEnvelope); err != nil {
		t.Fatalf("decode legacy sessions: %v", err)
	}
	if _, ok := sessionsEnvelope["sessions"]; !ok {
		t.Fatalf("legacy sessions envelope = %v, want a sessions key", sessionsEnvelope)
	}
	if _, grouped := sessionsEnvelope["items"]; grouped {
		t.Fatal("legacy sessions response leaked the grouped items key")
	}

	legacySearch, status := helperGroupGET(t, base+"/api/v1/search?q=needle")
	if status != http.StatusOK {
		t.Fatalf("legacy search status = %d", status)
	}
	var searchEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(legacySearch, &searchEnvelope); err != nil {
		t.Fatalf("decode legacy search: %v", err)
	}
	if _, ok := searchEnvelope["results"]; !ok {
		t.Fatalf("legacy search envelope = %v, want a results key", searchEnvelope)
	}

	grouped, status := helperGroupGET(t, base+"/api/v1/sessions?view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped sessions status = %d (body %s)", status, grouped)
	}
	var groupedPayload schema.LocalSessionListPayload
	if err := json.Unmarshal(grouped, &groupedPayload); err != nil {
		t.Fatalf("decode grouped sessions: %v", err)
	}
	if groupedPayload.Items == nil {
		t.Fatal("grouped sessions items must be a non-null array")
	}

	for _, rawURL := range []string{
		base + "/api/v1/sessions?view=everything",
		base + "/api/v1/search?q=needle&view=everything",
	} {
		body, status := helperGroupGET(t, rawURL)
		if status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 (body %s)", rawURL, status, body)
		}
		if code := helperGroupErrorCode(body); code != "grouped_view_unknown" {
			t.Fatalf("%s error code = %q, want grouped_view_unknown", rawURL, code)
		}
	}
}

// TestMemberRouteRefusesFiltersAndStaleScopes pins the member trust boundary:
// the operation accepts only the opaque scope plus paging, resolves a live
// scope for its exact group, and refuses everything else without broadening.
func TestMemberRouteRefusesFiltersAndStaleScopes(t *testing.T) {
	db := storetest.Open(t)
	seedGroupedRouteBoundarySessions(t, db)
	policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewStoreDataProvider(db, policy)
	base := startHelperGroupServer(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{}, Provider: provider})

	body, status := helperGroupGET(t, base+"/api/v1/sessions?view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped list status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	group := helperGroupFirstGroup(t, payload)
	membersPath := base + "/api/v1/session-groups/" + group.GroupID + "/members"

	cases := []struct {
		name   string
		rawURL string
		status int
		code   string
	}{
		{"missing scope", membersPath, http.StatusBadRequest, "request_query_required"},
		{"extra filter", membersPath + "?scope=" + group.MemberScope + "&project=secret", http.StatusBadRequest, "request_query_unknown"},
		{"invalid page", membersPath + "?scope=" + group.MemberScope + "&page=0", http.StatusBadRequest, "grouped_page_invalid"},
		{"invalid limit", membersPath + "?scope=" + group.MemberScope + "&limit=zero", http.StatusBadRequest, "grouped_limit_invalid"},
		{"wrong group", base + "/api/v1/session-groups/hg_other/members?scope=" + group.MemberScope, http.StatusConflict, "group_scope_expired"},
		{"unknown scope", membersPath + "?scope=not-a-live-scope", http.StatusConflict, "group_scope_expired"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body, status := helperGroupGET(t, tc.rawURL)
			if status != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.status, body)
			}
			if code := helperGroupErrorCode(body); code != tc.code {
				t.Fatalf("error code = %q, want %q (body %s)", code, tc.code, body)
			}
		})
	}
}

func helperGroupFirstGroup(t *testing.T, payload schema.LocalSessionListPayload) schema.HelperGroupSummary {
	t.Helper()
	for _, item := range payload.Items {
		if len(item.HelperGroups) > 0 {
			return item.HelperGroups[0]
		}
	}
	t.Fatal("grouped list rendered no helper group to expand")
	return schema.HelperGroupSummary{}
}

// TestMemberRouteRefusesChangedSelectionRevision proves a scope issued under one
// persisted selection is refused once the selection revision changes, so a
// changed selection can never be replayed against a different visible set.
func TestMemberRouteRefusesChangedSelectionRevision(t *testing.T) {
	db := storetest.Open(t)
	seedGroupedRouteBoundarySessions(t, db)
	policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewStoreDataProvider(db, policy)
	server, base := startHelperGroupServerHandle(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{}, Provider: provider})

	body, status := helperGroupGET(t, base+"/api/v1/sessions?view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped list status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	group := helperGroupFirstGroup(t, payload)

	server.groupedRevision = server.groupedRevision + "-changed"
	body, status = helperGroupGET(t, base+"/api/v1/session-groups/"+group.GroupID+"/members?scope="+group.MemberScope)
	if status != http.StatusConflict {
		t.Fatalf("changed-revision member status = %d, want 409 (body %s)", status, body)
	}
	if code := helperGroupErrorCode(body); code != "group_scope_expired" {
		t.Fatalf("changed-revision error code = %q, want group_scope_expired", code)
	}
}

func seedGroupedRouteBoundarySessions(t *testing.T, db *store.Store) {
	t.Helper()
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-c1", StartMs: 500, Purpose: "interaction", Origin: "user",
	}, 0)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-c2", StartMs: 490, Purpose: "helper_review", Origin: "agent",
		Owner: "agent-c1", OwnerState: "target_known",
	}, 1)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-c3", StartMs: 480, Purpose: "interaction", Origin: "user", Text: "needle",
	}, 2)
}

// TestStoreGroupingEvidenceReadsActiveGeneration pins the store read the grouped
// query depends on: purpose and the immediate started_by target come from the
// active generation, and a session with no evidence is reported with none.
func TestStoreGroupingEvidenceReadsActiveGeneration(t *testing.T) {
	db := storetest.Open(t)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-d1", StartMs: 100, Purpose: "helper_review", Origin: "agent",
		Owner: "agent-d0", OwnerState: "target_known",
	}, 0)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-d2", StartMs: 90, Purpose: "interaction", Origin: "user",
	}, 1)

	evidence, err := db.GroupingEvidenceForSessions(context.Background(), []string{"agent-d1", "agent-d2"})
	if err != nil {
		t.Fatalf("GroupingEvidenceForSessions: %v", err)
	}
	first := evidence["agent-d1"]
	if first.Purpose != schema.SessionPurposeHelperReview || first.OwnerState != schema.RelationshipTargetKnown {
		t.Fatalf("agent-d1 evidence = %+v, want helper_review/target_known", first)
	}
	if first.OwnerTargetLocalID == nil || *first.OwnerTargetLocalID != "agent-d0" {
		t.Fatalf("agent-d1 owner = %v, want agent-d0", first.OwnerTargetLocalID)
	}
	second := evidence["agent-d2"]
	if second.Purpose != schema.SessionPurposeInteraction || second.OwnerState != "" || second.OwnerTargetLocalID != nil {
		t.Fatalf("agent-d2 evidence = %+v, want interaction with no started_by evidence", second)
	}
}
