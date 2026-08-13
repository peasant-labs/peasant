package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
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

func TestServer_FeaturesConfig_ReportsExperimentalGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		experimental bool
	}{
		{name: "default server answers false", experimental: false},
		{name: "experimental server answers true", experimental: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			srv := api.NewServer(api.ServerConfig{Port: 0, Experimental: tc.experimental})
			if err := srv.Listen(ctx); err != nil {
				t.Fatalf("Listen: %v", err)
			}

			baseURL := "http://" + srv.Addr().String()

			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ctx) }()

			resp, err := http.Get(baseURL + "/api/v1/config/features")
			if err != nil {
				t.Fatalf("GET /api/v1/config/features: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}

			body, _ := io.ReadAll(resp.Body)
			var result api.FeaturesConfigResponse
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if result.Experimental != tc.experimental {
				t.Errorf("Experimental = %v, want %v", result.Experimental, tc.experimental)
			}

			cancel()
			if err := <-errCh; err != nil {
				t.Errorf("shutdown: %v", err)
			}
		})
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
