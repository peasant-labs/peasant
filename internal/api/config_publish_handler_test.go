package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
)

// servePublishConfig spins the real server with the given loaded config (nil
// allowed) and returns the parsed GET /api/v1/config/publish body.
func servePublishConfig(t *testing.T, cfg *config.Config) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := api.NewServer(api.ServerConfig{Port: 0, Config: cfg})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := srv.Addr()
	if addr == nil {
		t.Fatal("Addr() returned nil after Listen")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	resp, err := http.Get("http://" + addr.String() + "/api/v1/config/publish")
	if err != nil {
		t.Fatalf("GET /api/v1/config/publish: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("unmarshal %q: %v", string(body), err)
		}
	}
	return resp.StatusCode, parsed
}

// The endpoint reports the effective default push license so the web Share flow
// can name the license choice before submit. It reports the license verbatim
// for each configured choice, and an empty string (never a 5xx) when no license
// or no config is set, so the page can always render the notice link.
func TestHandlePublishConfig_License(t *testing.T) {
	withLicense := func(l config.License) *config.Config {
		c := &config.Config{}
		c.Push.License = l
		return c
	}
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{name: "cc-by", cfg: withLicense(config.LicenseCCBY), want: "CC-BY-4.0"},
		{name: "cc-by-sa", cfg: withLicense(config.LicenseCCBYSA), want: "CC-BY-SA-4.0"},
		{name: "cc0", cfg: withLicense(config.LicenseCC0), want: "CC0-1.0"},
		{name: "no-license", cfg: withLicense(""), want: ""},
		{name: "nil-config", cfg: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := servePublishConfig(t, tc.cfg)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			got, ok := body["license"].(string)
			if !ok {
				t.Fatalf("response missing string field \"license\": %v", body)
			}
			if got != tc.want {
				t.Errorf("license = %q, want %q", got, tc.want)
			}
		})
	}
}
