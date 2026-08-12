package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

func TestWebDiscoveryMountedRoute(t *testing.T) {
	db := storetest.Open(t)
	cfg := &config.Config{Selection: config.SelectionConfig{Mode: config.SelectionModeAll}}
	server := NewServer(ServerConfig{Port: 0, Store: db, Config: cfg})
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})

	response, err := http.Get("http://" + server.Addr().String() + "/api/v1/web/discovery")
	if err != nil {
		t.Fatalf("GET mounted discovery route: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload) != 1 || payload["items"] == nil {
		t.Fatalf("shape = %v, want exact items envelope", payload)
	}
	if string(payload["items"]) != "[]" {
		t.Fatalf("items = %s, want []", payload["items"])
	}
}
