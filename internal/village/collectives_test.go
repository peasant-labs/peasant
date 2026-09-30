package village_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

const (
	testTranscript = schema.TranscriptID("99d59925-36bc-424c-a789-8be54d9702ba")
	testCollective = schema.VillageUUID("11111111-1111-4111-8111-111111111111")
)

// recordedRequest is one request the Village double received.
type recordedRequest struct {
	method, path, auth, body string
}

func collectiveVillage(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*village.VillageClient, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, recordedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
		mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return village.NewVillageClient(server.URL, "test-key", nil), &requests
}

// TestShareTranscriptOffersOneCollectiveAndNamesARefusal checks the share
// request the collective steps send, and that a 409 reaches the caller as a
// StatusError that carries Village's own reason.
func TestShareTranscriptOffersOneCollectiveAndNamesARefusal(t *testing.T) {
	t.Parallel()
	client, requests := collectiveVillage(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "This transcript is already submitted to 1 collective"})
	})
	err := client.ShareTranscript(t.Context(), testTranscript, testCollective)
	var refusal *village.StatusError
	if !errors.As(err, &refusal) || refusal.StatusCode != http.StatusConflict || refusal.Message != "This transcript is already submitted to 1 collective" {
		t.Fatalf("share error = %v; want a 409 StatusError with Village's reason", err)
	}
	got := (*requests)[0]
	if got.method != http.MethodPost || got.path != "/api/v1/transcripts/"+testTranscript.String()+"/share" || got.auth != "Bearer test-key" {
		t.Fatalf("share request = %+v", got)
	}
	var body schema.VillageShareTranscriptRequest
	if err := json.Unmarshal([]byte(got.body), &body); err != nil || len(body.GroupIDs) != 1 || body.GroupIDs[0] != testCollective {
		t.Fatalf("share body %q (%v); want exactly the one collective", got.body, err)
	}
}

// TestUnshareTranscriptTakesTheTranscriptBack checks the request that revokes a
// collective's access.
func TestUnshareTranscriptTakesTheTranscriptBack(t *testing.T) {
	t.Parallel()
	client, requests := collectiveVillage(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(schema.VillageStatusResponse{Status: "unshared"})
	})
	if err := client.UnshareTranscript(t.Context(), testTranscript, testCollective); err != nil {
		t.Fatal(err)
	}
	got := (*requests)[0]
	if got.method != http.MethodDelete || got.path != "/api/v1/transcripts/"+testTranscript.String()+"/share/"+testCollective.String() || got.auth != "Bearer test-key" {
		t.Fatalf("unshare request = %+v", got)
	}
}

// TestTranscriptSharesRefusesAnUnknownStatus checks that a share status outside
// the closed set fails the read instead of being reported as access.
func TestTranscriptSharesRefusesAnUnknownStatus(t *testing.T) {
	t.Parallel()
	client, _ := collectiveVillage(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enriched_shares":[{"transcript_id":"` + testTranscript.String() + `","group_id":"` + testCollective.String() + `","group_name":"platform","acceptance_mode":"open","status":"visible","shared_at":"2026-01-01T00:00:00Z"}]}`))
	})
	if shares, err := client.TranscriptShares(t.Context(), testTranscript); err == nil || !strings.Contains(err.Error(), "outside the closed set") {
		t.Fatalf("shares = %+v, err = %v; want the unknown status refused", shares, err)
	}
}

// TestListCollectivesReadsAnEmptyMembershipAsNone checks that a Village answer
// of null reads as no collective rather than a failure.
func TestListCollectivesReadsAnEmptyMembershipAsNone(t *testing.T) {
	t.Parallel()
	client, requests := collectiveVillage(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`null`))
	})
	collectives, err := client.ListCollectives(t.Context())
	if err != nil || collectives == nil || len(collectives) != 0 {
		t.Fatalf("collectives = %#v, err = %v; want an empty list", collectives, err)
	}
	if got := (*requests)[0]; got.method != http.MethodGet || got.path != "/api/v1/groups" || got.auth != "Bearer test-key" {
		t.Fatalf("list request = %+v", got)
	}
}
