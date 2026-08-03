package village_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/village"
)

// TestVillageClient_Publish_TimingTrace verifies the production client.go seam:
// when the caller threads a perf.UploadTrace onto the request context (as the
// pipeline does under --timing), VillageClient.Publish attaches the httptrace
// hooks so the trace is populated with the connection/server split, and the
// connection is REUSED on a second upload over the same client. This drives the
// real client against an httptest server — no mock of the subject.
func TestVillageClient_Publish_TimingTrace(t *testing.T) {
	const serverDelay = 25 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay) // before first byte → server_ms
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer srv.Close()

	// Use the server's client so the httptest connection is trusted and pooled.
	client := village.NewVillageClient(srv.URL, testAPIKey, srv.Client())

	publishOnce := func(t *testing.T) perf.UploadSample {
		t.Helper()
		ctx, trace := perf.ContextWithUploadTrace(context.Background())
		_, status, err := client.Publish(ctx, []byte(`{"local_id":"s1"}`), strings.NewReader(`{"type":"say"}`), "s1--content.json")
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		return trace.Sample("s1")
	}

	first := publishOnce(t)
	if !first.Connected {
		t.Fatal("first upload: Connected=false, want true (trace not populated — httptrace not attached?)")
	}
	if first.Reused {
		t.Error("first upload: Reused=true, want false (fresh connection)")
	}
	if first.Server < serverDelay {
		t.Errorf("first upload: server_ms = %v, want >= %v", first.Server, serverDelay)
	}

	second := publishOnce(t)
	if !second.Reused {
		t.Error("second upload: Reused=false, want true (connection should be pooled + reused)")
	}
}

// TestVillageClient_Publish_NoTraceNoCost verifies that without an UploadTrace on
// the context (the default, timing off), Publish still works normally — the
// instrumentation is purely opt-in and never required.
func TestVillageClient_Publish_NoTraceNoCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, srv.Client())
	// Plain context — no perf.ContextWithUploadTrace.
	_, status, err := client.Publish(context.Background(), []byte(`{"local_id":"s1"}`), strings.NewReader(`{}`), "s1--content.json")
	if err != nil {
		t.Fatalf("publish without trace: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201", status)
	}
}
