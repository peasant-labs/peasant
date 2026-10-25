package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

const (
	extrasSessionID  = "acce5500-0000-4000-8000-000000000001"
	plainSessionID   = "acce5500-0000-4000-8000-000000000002"
	unknownSessionID = "acce5500-0000-4000-8000-0000000000ff"
)

// newSessionExtrasStore opens a real migrated store and seeds the two stored
// sessions both routes read: one whose recorded work produced a commit and a
// pull request, and one with nothing to report.
func newSessionExtrasStore(t *testing.T) *store.Store {
	t.Helper()
	db := storetest.Open(t)
	storetest.SeedSession(t, db, extrasSessionID)
	storetest.SeedSession(t, db, plainSessionID)
	seedStoredEntries(t, db, ghPRCreateEntries(extrasSessionID))
	seedStoredEntries(t, db, plainTextEntries(plainSessionID))
	return db
}

// sessionExtrasProvider is the production store-backed data provider, so the
// pull-request read walks the same stored entries, turns, and detail
// projection the local transcript renders.
func sessionExtrasProvider(t *testing.T, db *store.Store) api.DataProvider {
	t.Helper()
	policy, err := sessionvisibility.New(config.SelectionConfig{Mode: config.SelectionModeAll})
	if err != nil {
		t.Fatalf("build selection policy: %v", err)
	}
	return api.NewStoreDataProvider(db, policy)
}

// newSessionExtrasTestServer starts one real server over a real migrated store
// so both routes read the production projections.
func newSessionExtrasTestServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	db := newSessionExtrasStore(t)
	return startSessionExtrasServer(t, db, sessionExtrasProvider(t, db)), db
}

// startSessionExtrasServer runs the production server over the supplied store
// and provider, either of which may be nil to exercise the unavailable-arm.
func startSessionExtrasServer(t *testing.T, db *store.Store, provider api.DataProvider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := api.NewServer(api.ServerConfig{Port: 0, Store: db, Provider: provider})
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

// stringPointer and intPointer build the nullable stored columns an entry
// carries.
func stringPointer(value string) *string { return &value }
func intPointer(value int) *int          { return &value }

// ghPRCreateEntries are the stored rows one assistant turn leaves when it runs
// `gh pr create`: the turn itself, its tool_use child, and the tool_result
// child that printed the created pull request's URL.
func ghPRCreateEntries(sessionID string) []schema.SessionEntry {
	const toolCallID = "call-1"
	harness := schema.HarnessClaudeCode
	return []schema.SessionEntry{
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     0,
			Harness:        harness,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleAssistant,
			ContentPreview: stringPointer("open the pull request for the export flow"),
			SourceEntryRef: "e_parent",
		},
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     1,
			Harness:        harness,
			EntryType:      schema.EntryTypeToolUse,
			Role:           schema.RoleAssistant,
			Depth:          1,
			ParentIndex:    intPointer(0),
			ToolCallID:     stringPointer(toolCallID),
			ToolNamesCSV:   stringPointer("Bash"),
			ToolInput:      stringPointer(`{"command":"cd /home/dev/storytold/photocraft && gh pr create --title \"Add the export flow\" --body-file body.md"}`),
			SourceEntryRef: "e_call",
		},
		{
			SessionID:      schema.SessionID(sessionID),
			EntryIndex:     2,
			Harness:        harness,
			EntryType:      schema.EntryTypeToolResult,
			Role:           schema.RoleUser,
			Depth:          1,
			ParentIndex:    intPointer(0),
			ToolCallID:     stringPointer(toolCallID),
			ToolOutput:     stringPointer("https://github.com/storytold/photocraft/pull/1204"),
			SourceEntryRef: "e_result",
		},
	}
}

// plainTextEntries are the stored rows of a session with nothing to report: no
// recorded commit and no gh-pr-create invocation.
func plainTextEntries(sessionID string) []schema.SessionEntry {
	return []schema.SessionEntry{{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     0,
		Harness:        schema.HarnessClaudeCode,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleUser,
		ContentPreview: stringPointer("summarize the parser"),
		SourceEntryRef: "e_text",
	}}
}

func seedStoredEntries(t *testing.T, db *store.Store, entries []schema.SessionEntry) {
	t.Helper()
	if err := db.IndexSessionEntries(context.Background(), entries[0].SessionID, entries); err != nil {
		t.Fatalf("index stored entries for session %s: %v", entries[0].SessionID, err)
	}
}

// seedExtrasCommits writes the commits the commits route must read, including
// one row whose attribution and times are unknown (stored as SQL NULL).
func seedExtrasCommits(t *testing.T, db *store.Store) []ingest.CommitInfo {
	t.Helper()
	commits := []ingest.CommitInfo{
		{
			Hash:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Message:     "second commit",
			AuthorName:  "Ada Lovelace",
			AuthorEmail: "ada@example.com",
			CommitTime:  1_700_000_020_000,
			AuthorTime:  1_700_000_020_000,
		},
		{
			Hash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Message:     "first commit",
			AuthorName:  "Ada Lovelace",
			AuthorEmail: "ada@example.com",
			CommitTime:  1_700_000_010_000,
			AuthorTime:  1_700_000_010_000,
		},
		{Hash: "cccccccccccccccccccccccccccccccccccccccc"},
	}
	if err := db.UpsertSessionCommits(context.Background(), ingest.SessionID(extrasSessionID), commits); err != nil {
		t.Fatalf("seed session commits: %v", err)
	}
	// The order the route must serve: known author times oldest first, then
	// the row with no author time, with the hash breaking ties.
	return []ingest.CommitInfo{commits[1], commits[0], commits[2]}
}

func getSessionExtras(t *testing.T, url string) (int, map[string]json.RawMessage) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("GET %s body is not a JSON object: %v (body %s)", url, err, body)
	}
	return resp.StatusCode, decoded
}

func TestSessionCommitsRoute(t *testing.T) {
	baseURL, db := newSessionExtrasTestServer(t)
	want := seedExtrasCommits(t, db)

	status, decoded := getSessionExtras(t, baseURL+"/api/v1/sessions/"+extrasSessionID+"/commits")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(decoded) != 1 {
		t.Fatalf("response keys = %v, want exactly commits", decoded)
	}
	raw, ok := decoded["commits"]
	if !ok {
		t.Fatalf("response keys = %v, want a commits key", decoded)
	}
	var got []schema.CommitInfo
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("commits do not decode: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("commits = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("commits[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestSessionPullRequestsRoute(t *testing.T) {
	baseURL, _ := newSessionExtrasTestServer(t)

	status, decoded := getSessionExtras(t, baseURL+"/api/v1/sessions/"+extrasSessionID+"/pull-requests")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(decoded) != 1 {
		t.Fatalf("response keys = %v, want exactly pullRequests", decoded)
	}
	raw, ok := decoded["pullRequests"]
	if !ok {
		t.Fatalf("response keys = %v, want a pullRequests key", decoded)
	}
	type pullRequestRow struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		Source string `json:"source"`
	}
	var got []pullRequestRow
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("pullRequests do not decode: %v", err)
	}
	want := []pullRequestRow{{
		Owner:  "storytold",
		Repo:   "photocraft",
		Number: 1204,
		URL:    "https://github.com/storytold/photocraft/pull/1204",
		Source: "gh_pr_create",
	}}
	if len(got) != len(want) {
		t.Fatalf("pullRequests = %+v, want %+v", got, want)
	}
	if got[0] != want[0] {
		t.Fatalf("pullRequests[0] = %+v, want %+v", got[0], want[0])
	}
}

// TestSessionExtrasEmptyResults pins the additive contract: a stored session
// with no commits and no detected pull requests serves empty arrays, never a
// null that would force the consumer to guess.
func TestSessionExtrasEmptyResults(t *testing.T) {
	baseURL, _ := newSessionExtrasTestServer(t)

	for _, route := range []struct {
		name string
		path string
		key  string
	}{
		{"commits", "/api/v1/sessions/" + plainSessionID + "/commits", "commits"},
		{"pull requests", "/api/v1/sessions/" + plainSessionID + "/pull-requests", "pullRequests"},
	} {
		t.Run(route.name, func(t *testing.T) {
			status, decoded := getSessionExtras(t, baseURL+route.path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			raw, ok := decoded[route.key]
			if !ok {
				t.Fatalf("response keys = %v, want a %s key", decoded, route.key)
			}
			if string(raw) != "[]" {
				t.Fatalf("%s = %s, want an empty array", route.key, raw)
			}
		})
	}
}

func TestSessionExtrasRejectMalformedID(t *testing.T) {
	baseURL, _ := newSessionExtrasTestServer(t)

	for _, path := range []string{
		"/api/v1/sessions/not-a-session-id/commits",
		"/api/v1/sessions/not-a-session-id/pull-requests",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestSessionExtrasUnknownSession(t *testing.T) {
	baseURL, _ := newSessionExtrasTestServer(t)

	for _, path := range []string{
		"/api/v1/sessions/" + unknownSessionID + "/commits",
		"/api/v1/sessions/" + unknownSessionID + "/pull-requests",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != `{"error":"session not found"}`+"\n" {
				t.Fatalf("body = %q, want the honest not-found error", body)
			}
		})
	}
}

// TestSessionExtrasUnavailableDependencies pins the refusal arms: each route
// names the dependency it needs, and the commits read never requires a data
// provider it does not use.
func TestSessionExtrasUnavailableDependencies(t *testing.T) {
	t.Run("store unavailable", func(t *testing.T) {
		baseURL := startSessionExtrasServer(t, nil, nil)

		for _, path := range []string{
			"/api/v1/sessions/" + extrasSessionID + "/commits",
			"/api/v1/sessions/" + extrasSessionID + "/pull-requests",
		} {
			resp, err := http.Get(baseURL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s status = %d, want 503", path, resp.StatusCode)
			}
		}
	})

	t.Run("provider unavailable", func(t *testing.T) {
		db := newSessionExtrasStore(t)
		baseURL := startSessionExtrasServer(t, db, nil)

		resp, err := http.Get(baseURL + "/api/v1/sessions/" + extrasSessionID + "/commits")
		if err != nil {
			t.Fatalf("GET commits: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("commits status = %d, want 200 without a data provider", resp.StatusCode)
		}

		resp, err = http.Get(baseURL + "/api/v1/sessions/" + extrasSessionID + "/pull-requests")
		if err != nil {
			t.Fatalf("GET pull requests: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("pull requests status = %d, want 503 without a data provider", resp.StatusCode)
		}
	})
}
