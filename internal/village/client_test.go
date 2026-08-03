package village_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

const testAPIKey = "test-api-key-abc123"

// parseMultipartBody reads and parses a multipart/form-data request body.
// Returns the raw parts as a map from field name to content.
func parseMultipartBody(t *testing.T, r *http.Request) map[string][]byte {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("expected multipart content-type, got %q", mediaType)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	parts := make(map[string][]byte)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		data, _ := io.ReadAll(p)
		parts[p.FormName()] = data
	}
	return parts
}

func TestPublishAuthoritativeRejectsMalformedSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	request, err := schema.DecodeAuthoritativePublishRequest([]byte(`{"model":{"harness":"claude-code","model":"claude-opus-4-6"},"contentHash":"a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a","visibilityIntent":"private"}`))
	if err != nil {
		t.Fatalf("fixture request: %v", err)
	}
	client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
	_, status, err := client.PublishAuthoritative(context.Background(), request, strings.NewReader(`{}`), "session.json")
	if status != http.StatusCreated || err == nil || !strings.Contains(err.Error(), "malformed 2xx receipt") {
		t.Fatalf("status=%d err=%v, want strict malformed-success rejection", status, err)
	}
}

func TestUpdateOwnerRejectsIncompleteSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"visibility":"public"}`))
	}))
	defer server.Close()
	id, err := schema.NewTranscriptID("99d59925-36bc-424c-a789-8be54d9702ba")
	if err != nil {
		t.Fatal(err)
	}
	visibility := schema.TranscriptUpdateVisibilityPublic
	client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
	_, status, err := client.UpdateOwner(context.Background(), id, schema.OwnerTranscriptUpdateRequest{Visibility: &visibility})
	if status != http.StatusOK || err == nil || !strings.Contains(err.Error(), "malformed 2xx response") {
		t.Fatalf("status=%d err=%v, want strict incomplete-success rejection", status, err)
	}
}

func TestVillageClient_Publish_201Created(t *testing.T) {
	const transcriptContent = `{"type":"say","text":"hello"}`
	const filename = "abc123--transcript.jsonl"
	metadataJSON := []byte(`{"local_id":"abc123","model_harness":"claude-code","visibility":"private"}`)
	var requestObserved atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestObserved.Load() {
			t.Error("request observer did not run before the HTTP request reached the server")
		}
		// Verify method and path.
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/transcripts/publish" {
			t.Errorf("path: got %q, want /api/v1/transcripts/publish", r.URL.Path)
		}

		// Verify auth header.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+testAPIKey {
			t.Errorf("Authorization: got %q, want %q", auth, "Bearer "+testAPIKey)
		}

		// Verify multipart body.
		parts := parseMultipartBody(t, r)
		if string(parts["metadata"]) != string(metadataJSON) {
			t.Errorf("metadata field: got %q, want %q", parts["metadata"], metadataJSON)
		}
		if string(parts["transcript_file"]) != transcriptContent {
			t.Errorf("transcript_file field: got %q, want %q", parts["transcript_file"], transcriptContent)
		}

		// Return 201 Created with a result body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ingest.PublishResult{TranscriptID: "tid-001"})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	client.SetRequestObserver(func() { requestObserved.Store(true) })
	result, statusCode, err := client.Publish(
		context.Background(),
		metadataJSON,
		strings.NewReader(transcriptContent),
		filename,
	)
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if statusCode != http.StatusCreated {
		t.Errorf("status code: got %d, want 201", statusCode)
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil")
	}
	if result.TranscriptID != "tid-001" {
		t.Errorf("TranscriptID: got %q, want %q", result.TranscriptID, "tid-001")
	}
}

func TestVillageClient_RequestObserverDoesNotMarkRequestConstructionFailure(t *testing.T) {
	t.Parallel()
	var requestObserved atomic.Bool
	client := village.NewVillageClient("http://[invalid", testAPIKey, &http.Client{})
	client.SetRequestObserver(func() { requestObserved.Store(true) })
	_, _, err := client.Publish(
		context.Background(),
		[]byte(`{"local_id":"x"}`),
		strings.NewReader("data"),
		"x--transcript.jsonl",
	)
	if err == nil {
		t.Fatal("Publish with an invalid endpoint succeeded")
	}
	if requestObserved.Load() {
		t.Fatal("request observer ran even though no HTTP request could be constructed")
	}
}

func TestVillageClient_Publish_200Updated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ingest.PublishResult{TranscriptID: "tid-002"})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	result, statusCode, err := client.Publish(
		context.Background(),
		[]byte(`{"local_id":"x"}`),
		strings.NewReader("data"),
		"x--transcript.jsonl",
	)
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if statusCode != http.StatusOK {
		t.Errorf("status code: got %d, want 200", statusCode)
	}
	if result == nil || result.TranscriptID != "tid-002" {
		t.Errorf("result: got %v, want TranscriptID=tid-002", result)
	}
}

func TestVillageClient_Publish_ErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"422 Unprocessable Entity", http.StatusUnprocessableEntity},
		{"401 Unauthorized", http.StatusUnauthorized},
		{"500 Internal Server Error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "error body", tt.statusCode)
			}))
			defer srv.Close()

			client := village.NewVillageClient(srv.URL, testAPIKey, nil)
			result, statusCode, err := client.Publish(
				context.Background(),
				[]byte(`{"local_id":"x"}`),
				strings.NewReader("data"),
				"x--transcript.jsonl",
			)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if result != nil {
				t.Errorf("result should be nil on error, got %v", result)
			}
			if statusCode != tt.statusCode {
				t.Errorf("status code: got %d, want %d", statusCode, tt.statusCode)
			}
		})
	}
}

func TestVillageClient_Publish_AuthHeader(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ingest.PublishResult{})
	}))
	defer srv.Close()

	const apiKey = "my-secret-key-xyz"
	client := village.NewVillageClient(srv.URL, apiKey, nil)
	_, _, err := client.Publish(
		context.Background(),
		[]byte(`{}`),
		bytes.NewReader(nil),
		"f.jsonl",
	)
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if capturedAuth != "Bearer "+apiKey {
		t.Errorf("Authorization header: got %q, want %q", capturedAuth, "Bearer "+apiKey)
	}
}

func TestVillageClient_Publish_MultipartFormStructure(t *testing.T) {
	var capturedParts map[string][]byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedParts = parseMultipartBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ingest.PublishResult{})
	}))
	defer srv.Close()

	metaJSON := []byte(`{"local_id":"sess1","visibility":"public"}`)
	transcriptData := []byte(`line1\nline2\nline3`)
	filename := "sess1--transcript.jsonl"

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	_, _, err := client.Publish(
		context.Background(),
		metaJSON,
		bytes.NewReader(transcriptData),
		filename,
	)
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}

	if string(capturedParts["metadata"]) != string(metaJSON) {
		t.Errorf("metadata part: got %q, want %q", capturedParts["metadata"], metaJSON)
	}
	if string(capturedParts["transcript_file"]) != string(transcriptData) {
		t.Errorf("transcript_file part: got %q, want %q", capturedParts["transcript_file"], transcriptData)
	}
}

// TestVillageClient_GetSchemaVersion_DecodesPushWindow verifies the (formerly
// dead) GetSchemaVersion preflight queries GET /api/v1/schema/version and
// decodes the push-contract accept window [Min, Current] plus the hit path and
// auth header. This is the transport the negotiation gate drives.
func TestVillageClient_GetSchemaVersion_DecodesPushWindow(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"annotationSchemaVersion": "16",
			"supportedTargetKinds": ["session"],
			"supportedTypeIds": ["t1"],
			"pushContractVersion": "0.3.0",
			"minPushContractVersion": "0.1.0"
		}`))
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	resp, status, err := client.GetSchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("GetSchemaVersion returned error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}
	if gotPath != "/api/v1/schema/version" {
		t.Errorf("path: got %q, want /api/v1/schema/version", gotPath)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("auth header: got %q", gotAuth)
	}
	if string(resp.PushContractVersion) != "0.3.0" {
		t.Errorf("pushContractVersion: got %q, want 0.3.0", resp.PushContractVersion)
	}
	if string(resp.MinPushContractVersion) != "0.1.0" {
		t.Errorf("minPushContractVersion: got %q, want 0.1.0", resp.MinPushContractVersion)
	}
}
