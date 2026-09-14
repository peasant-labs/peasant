package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// detailReadStubProvider serves one canned flat read payload through the
// production provider contract. Embedding api.DataProvider keeps the remaining
// methods unimplemented: the detail route must not consult any of them.
type detailReadStubProvider struct {
	api.DataProvider
	payload *schema.SessionDetailReadPayload
	err     error
}

func (p detailReadStubProvider) DetailReadPayload(context.Context, string) (*schema.SessionDetailReadPayload, error) {
	return p.payload, p.err
}

func detailReadTestPayload(t *testing.T) *schema.SessionDetailReadPayload {
	t.Helper()
	inputCount := int64(1)
	target := schema.SessionID("11111111-1111-4111-8111-111111111111")
	return &schema.SessionDetailReadPayload{
		SessionDetailPayload: schema.SessionDetailPayload{
			ID:                   "45454545-4545-4545-4545-454545454548",
			Harness:              schema.HarnessCodex,
			TurnCount:            5,
			InputSubmissionCount: &inputCount,
			Purpose:              schema.SessionPurposeInteraction,
			Turns: []schema.TurnDetail{
				{Index: 0, Role: schema.RoleUser, Content: "fix parser"},
			},
		},
		RelationshipNavigation: []schema.SessionRelationshipNavigation{
			{Kind: schema.SessionRelationshipStartedBy, Status: schema.RelationshipNavigationResolved, LocalID: &target},
		},
	}
}

func startDetailReadServer(t *testing.T, provider api.DataProvider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Provider: provider})
	if err := srv.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return "http://" + srv.Addr().String()
}

// TestServer_SessionDetailReadRoute pins the flat additive read: durable detail
// fields stay at the JSON root under an anonymous embedding and authorized
// relationshipNavigation rides beside them, with no nested detail wrapper.
func TestServer_SessionDetailReadRoute(t *testing.T) {
	baseURL := startDetailReadServer(t, detailReadStubProvider{payload: detailReadTestPayload(t)})

	resp, err := http.Get(baseURL + "/api/v1/sessions/45454545-4545-4545-4545-454545454548")
	if err != nil {
		t.Fatalf("GET session detail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("detail route body is not a flat JSON object: %v", err)
	}
	if _, wrapped := decoded["detail"]; wrapped {
		t.Fatal("detail route must not wrap the durable payload under a detail key")
	}
	if _, ok := decoded["turnCount"]; !ok {
		t.Fatal("detail route must keep durable root fields at the JSON root")
	}
	var nav []schema.SessionRelationshipNavigation
	if raw, ok := decoded["relationshipNavigation"]; !ok {
		t.Fatal("detail route omitted relationshipNavigation for a linkable relationship")
	} else if err := json.Unmarshal(raw, &nav); err != nil {
		t.Fatalf("relationshipNavigation does not decode: %v", err)
	}
	if len(nav) != 1 || nav[0].Status != schema.RelationshipNavigationResolved || nav[0].LocalID == nil {
		t.Fatalf("relationshipNavigation = %+v, want one resolved local target", nav)
	}
}

func TestServer_SessionDetailReadRouteRejectsMalformedID(t *testing.T) {
	baseURL := startDetailReadServer(t, detailReadStubProvider{payload: detailReadTestPayload(t)})
	resp, err := http.Get(baseURL + "/api/v1/sessions/not-a-session-id")
	if err != nil {
		t.Fatalf("GET malformed session detail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id status = %d, want 400", resp.StatusCode)
	}
}

func TestServer_SessionDetailReadRouteHonestNotFound(t *testing.T) {
	baseURL := startDetailReadServer(t, detailReadStubProvider{err: api.ErrSessionNotFound})
	resp, err := http.Get(baseURL + "/api/v1/sessions/45454545-4545-4545-4545-454545454548")
	if err != nil {
		t.Fatalf("GET missing session detail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "45454545") {
		t.Fatal("not-found body must not echo the requested identifier")
	}
}

// TestSessionDetailReadForProviderRequiresResolverEvidence documents the
// fallback contract: a provider that cannot resolve stored targets keeps the
// durable conversion and emits no navigation rather than guessing links.
func TestSessionDetailReadForProviderRequiresResolverEvidence(t *testing.T) {
	session := &ingest.Session{
		ID:       schema.SessionID("45454545-4545-4545-4545-454545454548"),
		Harness:  defaults.HarnessClaudeCode,
		Turns:    []ingest.Turn{{Index: 0, Role: ingest.RoleUser, Content: "hello", EntryType: schema.EntryTypeText}},
		Metadata: ingest.SessionMetadata{TurnCount: 1},
	}
	detail, err := api.SessionDetailReadForProvider(context.Background(), stubLinkProvider{session: session}, string(session.ID))
	if err != nil {
		t.Fatalf("fallback read: %v", err)
	}
	if len(detail.RelationshipNavigation) != 0 {
		t.Fatalf("fallback navigation = %+v, want none", detail.RelationshipNavigation)
	}
	if detail.ID != string(session.ID) {
		t.Fatalf("fallback read lost the durable session identity: %q", detail.ID)
	}
}
