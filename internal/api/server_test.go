package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestServer_DynamicPort_Health(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := api.NewServer(api.ServerConfig{Port: 0})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	addr := srv.Addr()
	if addr == nil {
		t.Fatal("Addr() returned nil after Listen")
	}
	baseURL := "http://" + addr.String()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	resp, err := http.Get(baseURL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestServer_DynamicPort_NeverCollides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv1 := api.NewServer(api.ServerConfig{Port: 0})
	srv2 := api.NewServer(api.ServerConfig{Port: 0})

	if err := srv1.Listen(ctx); err != nil {
		t.Fatalf("srv1.Listen: %v", err)
	}
	if err := srv2.Listen(ctx); err != nil {
		t.Fatalf("srv2.Listen: %v", err)
	}

	a1, a2 := srv1.Addr().String(), srv2.Addr().String()
	if a1 == a2 {
		t.Errorf("both servers bound to same address: %s", a1)
	}

	go srv1.Serve(ctx)
	go srv2.Serve(ctx)

	// Verify both respond.
	for _, base := range []string{"http://" + a1, "http://" + a2} {
		resp, err := http.Get(base + "/api/v1/health")
		if err != nil {
			t.Fatalf("GET %s/api/v1/health: %v", base, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want %d", base, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestServer_MockConfig_ReturnsConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := api.NewServer(api.ServerConfig{
		Port: 0,
		MockConfig: &api.MockConfigResponse{
			Enabled: true,
			Web:     []string{"dashboard", "sessions"},
			TUI:     []string{"sessions"},
			API:     []string{"dashboard"},
		},
	})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	addr := srv.Addr()
	baseURL := "http://" + addr.String()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	resp, err := http.Get(baseURL + "/api/v1/config/mock")
	if err != nil {
		t.Fatalf("GET /api/v1/config/mock: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	var result api.MockConfigResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if !result.Enabled {
		t.Error("Enabled = false, want true")
	}
	if len(result.Web) != 2 || result.Web[0] != "dashboard" || result.Web[1] != "sessions" {
		t.Errorf("Web = %v, want [dashboard sessions]", result.Web)
	}
	if len(result.TUI) != 1 || result.TUI[0] != "sessions" {
		t.Errorf("TUI = %v, want [sessions]", result.TUI)
	}
	if len(result.API) != 1 || result.API[0] != "dashboard" {
		t.Errorf("API = %v, want [dashboard]", result.API)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// TestServer_TranscriptDownload_MissingFileNames404sWithPathAndCommand proves
// the transcript-download handler, when the database has a session row but
// the retained pair is gone from OutputDir, returns 404 naming BOTH the exact
// directory it looked in AND the `peasant harvest --force --session <id>`
// command the user must run to save the session again (design record
// section 6, "API transcript download").
func TestServer_TranscriptDownload_MissingFileNames404sWithPathAndCommand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		sessionID = "99999999-9999-9999-9999-999999999999"
		hostSlug  = "github.com-user-repo1"
	)

	output := t.TempDir()
	s := openTestStore(t)
	entry := makeStoreEntry(t, sessionID, hash1, hostSlug,
		defaults.HarnessClaudeCode, day1Ms, 100, 50, "project-missing", 1, 0, 60000)
	seedStore(t, s, []ingest.StoreEntry{entry})

	srv := api.NewServer(api.ServerConfig{Port: 0, Store: s, OutputDir: output})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	baseURL := "http://" + srv.Addr().String()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	resp, err := http.Get(baseURL + "/api/v1/sessions/" + sessionID + "/transcript")
	if err != nil {
		t.Fatalf("GET transcript: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	body, _ := io.ReadAll(resp.Body)
	wantDir := filepath.Join(output, hostSlug, sessionID)
	wantCommand := fmt.Sprintf("peasant harvest --force --session %s", sessionID)
	if got := string(body); !strings.Contains(got, wantDir) {
		t.Errorf("404 body = %q, want it to name the looked-in directory %q", got, wantDir)
	}
	if got := string(body); !strings.Contains(got, wantCommand) {
		t.Errorf("404 body = %q, want it to name the command %q", got, wantCommand)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestServer_MockConfig_NotConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := api.NewServer(api.ServerConfig{Port: 0})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	addr := srv.Addr()
	baseURL := "http://" + addr.String()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	resp, err := http.Get(baseURL + "/api/v1/config/mock")
	if err != nil {
		t.Fatalf("GET /api/v1/config/mock: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
