package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// TestSyncGroupedViewIsOptInAndFlatRouteUnchanged pins the compatibility half of
// the sync chooser contract: omitting view keeps the exact flat sync envelope,
// only the published grouped value selects the grouped payload, and the member
// operation replays the sync route's exact predicate for the rendered group.
func TestSyncGroupedViewIsOptInAndFlatRouteUnchanged(t *testing.T) {
	db := storetest.Open(t)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-sync-a1", StartMs: 500, Purpose: "interaction", Origin: "user", Pushable: true,
	}, 0)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-sync-b1", StartMs: 490, Purpose: "helper_review", Origin: "agent",
		Owner: "agent-sync-a1", OwnerState: "target_known", Pushable: true,
	}, 1)
	seedHelperGroupSession(t, db, helperGroupListingSession{
		ID: "agent-sync-b2", StartMs: 480, Purpose: "helper_review", Origin: "agent",
		Owner: "agent-sync-a1", OwnerState: "target_known",
	}, 2)
	helperGroupDropMetrics(t, db, "agent-sync-b2")

	policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewStoreDataProvider(db, policy)
	base := startHelperGroupServer(t, ServerConfig{Port: 0, Store: db, Config: &config.Config{Selection: config.SelectionConfig{Mode: config.SelectionModeAll}}, Provider: provider})

	flat, status := helperGroupGET(t, base+"/api/v1/sync/sessions")
	if status != http.StatusOK {
		t.Fatalf("flat sync status = %d (body %s)", status, flat)
	}
	var flatEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(flat, &flatEnvelope); err != nil {
		t.Fatalf("decode flat sync: %v", err)
	}
	if _, ok := flatEnvelope["sessions"]; !ok {
		t.Fatalf("flat sync envelope = %v, want a sessions key", flatEnvelope)
	}
	if _, grouped := flatEnvelope["items"]; grouped {
		t.Fatal("flat sync response leaked the grouped items key")
	}

	grouped, status := helperGroupGET(t, base+"/api/v1/sync/sessions?view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped sync status = %d (body %s)", status, grouped)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(grouped, &payload); err != nil {
		t.Fatalf("decode grouped sync: %v", err)
	}
	if payload.Items == nil {
		t.Fatal("grouped sync items must be a non-null array")
	}
	group := helperGroupFirstGroup(t, payload)
	if group.Purpose != schema.SessionPurposeHelperReview {
		t.Fatalf("group purpose = %q, want helper_review", group.Purpose)
	}
	for _, item := range payload.Items {
		if item.Transcript == nil {
			continue
		}
		if item.Transcript.Sync == nil {
			t.Fatalf("grouped sync row %q carries no Sync mirror", item.Transcript.Session.ID)
		}
		if item.Transcript.Sync.ID != item.Transcript.Session.ID {
			t.Fatalf("sync mirror id = %q, want %q", item.Transcript.Sync.ID, item.Transcript.Session.ID)
		}
		// The sync chooser summary rides the same Z-only datetime contract as
		// the session lists: a local-zone offset fails client decoding.
		if item.Transcript.Session.StartTime.Location() != time.UTC {
			t.Errorf("grouped sync row %q startTime location = %v, want UTC", item.Transcript.Session.ID, item.Transcript.Session.StartTime.Location())
		}
	}

	members := helperGroupMembers(t, base, group.GroupID, group.MemberScope, 1, 20)
	if got := helperGroupMemberIDs(members); !reflect.DeepEqual(got, []string{"agent-sync-b1"}) {
		t.Fatalf("sync group members = %v, want exactly [agent-sync-b1]", got)
	}
	if members.Total != 1 {
		t.Fatalf("sync group member total = %d, want 1", members.Total)
	}

	body, status := helperGroupGET(t, base+"/api/v1/sync/sessions?view=everything")
	if status != http.StatusBadRequest {
		t.Fatalf("unknown sync view status = %d, want 400 (body %s)", status, body)
	}
	if code := helperGroupErrorCode(body); code != "grouped_view_unknown" {
		t.Fatalf("unknown sync view error code = %q, want grouped_view_unknown", code)
	}
}
