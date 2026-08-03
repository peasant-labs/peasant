package village_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// pullWindowJSON is the schema-version response body advertising a pull window
// compatible with this CLI (current == cli, min == cli) — the happy preflight.
func pullWindowJSON() string {
	v := defaults.PullContractVersion.String()
	return `{"annotationSchemaVersion":"16","supportedTargetKinds":["session"],"supportedTypeIds":["t1"],` +
		`"pushContractVersion":"` + v + `","minPushContractVersion":"` + v + `",` +
		`"pullContractVersion":"` + v + `","minPullContractVersion":"` + v + `"}`
}

// noPullWindowJSON omits the pull window entirely (an older village that predates
// the pull surface) — the CLI must treat this as "village too old for pull".
const noPullWindowJSON = `{"annotationSchemaVersion":"16","supportedTargetKinds":["session"],` +
	`"supportedTypeIds":["t1"],"pushContractVersion":"0.1.0","minPushContractVersion":"0.1.0"}`

// pullTestServer wires an httptest server for the PURE pull GETs: a single pull
// route (method+path) is handled by `handler`. The four GETs no longer preflight
// the pull window (NegotiatePull is the caller's explicit stage), so the
// /api/v1/schema/version route is asserted NOT to be reached — a GET that issues
// a stray preflight fails loudly here. Any other unexpected path 500s too.
func pullTestServer(t *testing.T, route string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/schema/version" {
			t.Errorf("pull GET must NOT preflight /api/v1/schema/version (NegotiatePull is the explicit stage); got a request to it")
			http.Error(w, "unexpected preflight", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == route {
			handler(w, r)
			return
		}
		t.Errorf("unexpected request path %q", r.URL.Path)
		http.Error(w, "unexpected path", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// negotiateTestServer wires an httptest server whose /api/v1/schema/version route
// returns schemaVersionBody — used by the NegotiatePull tests, which drive the
// explicit NEGOTIATE stage directly (no pull GET involved).
func negotiateTestServer(t *testing.T, schemaVersionBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/schema/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, schemaVersionBody)
			return
		}
		t.Errorf("NegotiatePull must only hit /api/v1/schema/version; got %q", r.URL.Path)
		http.Error(w, "unexpected path", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustTranscriptID(t *testing.T) schema.TranscriptID {
	t.Helper()
	id, err := schema.NewTranscriptID(testutil.TestTranscriptUUID)
	if err != nil {
		t.Fatalf("NewTranscriptID: %v", err)
	}
	return id
}

// --- ListPullableTranscripts ---

func TestListPullableTranscripts_OK(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	srv := pullTestServer(t, "/api/v1/pull/transcripts", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.PullListResponse{
			Transcripts: []schema.PullTranscriptInfo{{
				TranscriptID: mustTranscriptID(t),
				OwnerUserID:  testutil.TestOwnerUserID,
			}},
			Page: 2, Limit: 10, Total: 1,
		})
	})

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	resp, err := client.ListPullableTranscripts(context.Background(), 2, 10)
	if err != nil {
		t.Fatalf("ListPullableTranscripts: %v", err)
	}
	if gotPath != "/api/v1/pull/transcripts" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("auth: got %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "page=2") || !strings.Contains(gotQuery, "limit=10") {
		t.Errorf("query: got %q, want page=2 & limit=10", gotQuery)
	}
	if resp.Total != 1 || len(resp.Transcripts) != 1 {
		t.Errorf("response: got total=%d len=%d", resp.Total, len(resp.Transcripts))
	}
}

func TestListPullableTranscripts_401(t *testing.T) {
	srv := pullTestServer(t, "/api/v1/pull/transcripts", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no creds", http.StatusUnauthorized)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	_, err := client.ListPullableTranscripts(context.Background(), 1, 50)
	if err == nil {
		t.Fatal("expected 401 error")
	}
	for _, want := range []string{"401", "peasant village login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("401 error missing %q; got: %v", want, err)
		}
	}
}

// --- GetPullTranscript ---

func TestGetPullTranscript_OK(t *testing.T) {
	id := mustTranscriptID(t)
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.PullTranscriptInfo{
			TranscriptID:  id,
			OwnerUserID:   testutil.TestOwnerUserID,
			OwnerUsername: testutil.TestOwnerUsername,
			ContentHash:   testutil.TestContentHash,
			Visibility:    schema.VisibilityGroup,
		})
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	info, err := client.GetPullTranscript(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPullTranscript: %v", err)
	}
	if info.TranscriptID != id || info.ContentHash != testutil.TestContentHash {
		t.Errorf("info: got id=%q hash=%q", info.TranscriptID, info.ContentHash)
	}
}

func TestGetPullTranscript_404(t *testing.T) {
	id := mustTranscriptID(t)
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String(), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	_, err := client.GetPullTranscript(context.Background(), id)
	if err == nil {
		t.Fatal("expected 404 error")
	}
	// Typed classification as PullStatus "not-found" plus actionable text.
	if !errors.Is(err, village.ErrPullNotFound) {
		t.Errorf("404 must wrap ErrPullNotFound (errors.Is); got: %v", err)
	}
	// 404-not-403: actionable, no existence leak (does not say "private"/"403").
	for _, want := range []string{"404", "not pullable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("404 error missing %q; got: %v", want, err)
		}
	}
}

// --- GetPullTranscriptContent ---

func TestGetPullTranscriptContent_OK_StreamsAndETag(t *testing.T) {
	id := mustTranscriptID(t)
	const blob = `{"type":"say","text":"pulled"}`
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String()+"/content", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", testutil.TestContentHash)
		_, _ = io.WriteString(w, blob)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	rc, etag, err := client.GetPullTranscriptContent(context.Background(), id, "")
	if err != nil {
		t.Fatalf("GetPullTranscriptContent: %v", err)
	}
	defer rc.Close()
	if etag != testutil.TestContentHash {
		t.Errorf("etag: got %q, want %q", etag, testutil.TestContentHash)
	}
	body, _ := io.ReadAll(rc)
	if string(body) != blob {
		t.Errorf("body: got %q, want %q", body, blob)
	}
}

// 304 ⇒ ErrNotModified sentinel; the If-None-Match header carries the stored hash.
func TestGetPullTranscriptContent_304_ReturnsErrNotModified(t *testing.T) {
	id := mustTranscriptID(t)
	var gotINM string
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String()+"/content", func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", testutil.TestContentHash)
		w.WriteHeader(http.StatusNotModified)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	rc, etag, err := client.GetPullTranscriptContent(context.Background(), id, testutil.TestContentHash)
	if !errors.Is(err, village.ErrNotModified) {
		t.Fatalf("expected ErrNotModified, got err=%v rc=%v", err, rc)
	}
	if rc != nil {
		t.Error("reader must be nil on 304")
	}
	if gotINM != testutil.TestContentHash {
		t.Errorf("If-None-Match: got %q, want %q", gotINM, testutil.TestContentHash)
	}
	if etag != testutil.TestContentHash {
		t.Errorf("etag on 304: got %q", etag)
	}
}

func TestGetPullTranscriptContent_404(t *testing.T) {
	id := mustTranscriptID(t)
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String()+"/content", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	rc, _, err := client.GetPullTranscriptContent(context.Background(), id, "")
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if rc != nil {
		t.Error("reader must be nil on error")
	}
	// Typed classification maps this to PullStatus "not-found" — not a
	// brittle substring match. The actionable text is asserted separately above.
	if !errors.Is(err, village.ErrPullNotFound) {
		t.Errorf("404 must wrap ErrPullNotFound (errors.Is); got: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("missing 404; got: %v", err)
	}
}

// --- GetPullTranscriptAnnotations ---

func TestGetPullTranscriptAnnotations_OK(t *testing.T) {
	id := mustTranscriptID(t)
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String()+"/annotations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]schema.PullAnnotation{
			{AuthorUserID: testutil.TestAuthorUserID, AuthorUsername: testutil.TestAuthorUsername},
		})
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	anns, err := client.GetPullTranscriptAnnotations(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPullTranscriptAnnotations: %v", err)
	}
	if len(anns) != 1 || anns[0].AuthorUserID != testutil.TestAuthorUserID {
		t.Errorf("annotations: got %+v", anns)
	}
}

func TestGetPullTranscriptAnnotations_401(t *testing.T) {
	id := mustTranscriptID(t)
	srv := pullTestServer(t, "/api/v1/pull/transcripts/"+id.String()+"/annotations", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no creds", http.StatusUnauthorized)
	})
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	_, err := client.GetPullTranscriptAnnotations(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error; got: %v", err)
	}
}

// --- Pull-window negotiation (the EXPLICIT NEGOTIATE stage; stricter than push) ---
//
// NegotiatePull is the pipeline's explicit NEGOTIATE stage. It is the
// ONLY method that hits /api/v1/schema/version; the four pull GETs are pure data
// calls. These tests drive NegotiatePull directly (no pull GET), mirroring how the
// pipeline calls it exactly once per command.

// A village that advertises a compatible pull window ⇒ NegotiatePull returns nil
// and the pull may proceed.
func TestNegotiatePull_CompatibleWindow_OK(t *testing.T) {
	srv := negotiateTestServer(t, pullWindowJSON())
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	if err := client.NegotiatePull(context.Background()); err != nil {
		t.Fatalf("compatible window must negotiate cleanly; got: %v", err)
	}
}

// A village that advertises NO pull window is too old: NegotiatePull aborts with
// an actionable "village too old" error that wraps ErrPullContractIncompatible
// and names WHICH village (host/URL) is too old. No CLI/pull request is issued.
func TestNegotiatePull_MissingWindow_AbortsActionably(t *testing.T) {
	srv := negotiateTestServer(t, noPullWindowJSON)
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)

	err := client.NegotiatePull(context.Background())
	if err == nil {
		t.Fatal("missing pull window must abort the pull")
	}
	// Typed classification as PullStatus "contract-error".
	if !errors.Is(err, village.ErrPullContractIncompatible) {
		t.Errorf("missing-window must wrap ErrPullContractIncompatible (errors.Is); got: %v", err)
	}
	for _, want := range []string{"does not advertise a pull contract", "predates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing-window error missing %q; got: %v", want, err)
		}
	}
	// C-actionable-errors: the 'where' must name the actual village (host/URL),
	// not a literal placeholder.
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("missing-window 'where' must name the village URL %q; got: %v", srv.URL, err)
	}
	if strings.Contains(err.Error(), "schema/version preflight)") {
		t.Errorf("'where' must not render the literal placeholder; got: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "upgrade the village") {
		t.Errorf("error must NEVER instruct upgrading the village; got: %v", err)
	}
}

// A village that advertises a pull window OUTSIDE this CLI's pull-contract version
// ⇒ a typed contract error naming the window (incompatible, not too-old).
func TestNegotiatePull_IncompatibleWindow_ContractError(t *testing.T) {
	// Window [9.9.0, 9.9.0] is far ahead of the CLI's 0.1.0 ⇒ incompatible.
	incompatible := `{"annotationSchemaVersion":"16","supportedTargetKinds":["session"],` +
		`"supportedTypeIds":["t1"],"pushContractVersion":"0.1.0","minPushContractVersion":"0.1.0",` +
		`"pullContractVersion":"9.9.0","minPullContractVersion":"9.9.0"}`
	srv := negotiateTestServer(t, incompatible)
	client := village.NewVillageClient(srv.URL, testAPIKey, nil)

	err := client.NegotiatePull(context.Background())
	if err == nil {
		t.Fatal("incompatible pull window must abort the pull")
	}
	if !errors.Is(err, village.ErrPullContractIncompatible) {
		t.Errorf("incompatible-window must wrap ErrPullContractIncompatible (errors.Is); got: %v", err)
	}
	for _, want := range []string{"incompatible", "9.9.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("incompatible-window error missing %q; got: %v", want, err)
		}
	}
}

// Compile-time guard: the production client provides the explicit NEGOTIATE stage
// (NegotiatePull) plus the four pure-data pull GETs (the pull pipeline
// consumes via a narrow interface). This pullReader mirrors that narrow interface
// so a signature drift breaks the build here.
type pullReader interface {
	NegotiatePull(ctx context.Context) error
	ListPullableTranscripts(ctx context.Context, page, limit int) (*schema.PullListResponse, error)
	GetPullTranscript(ctx context.Context, id schema.TranscriptID) (*schema.PullTranscriptInfo, error)
	GetPullTranscriptContent(ctx context.Context, id schema.TranscriptID, ifNoneMatch string) (io.ReadCloser, string, error)
	GetPullTranscriptAnnotations(ctx context.Context, id schema.TranscriptID) ([]schema.PullAnnotation, error)
}

var _ pullReader = (*village.VillageClient)(nil)
