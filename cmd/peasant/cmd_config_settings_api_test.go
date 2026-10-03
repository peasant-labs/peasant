package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestConfigShowsASettingSavedThroughTheLocalAPI saves the default license
// through PATCH /api/v1/settings on a running local server, then opens
// `peasant config` on the same file: the mounted editor selects the license
// the web saved.
func TestConfigShowsASettingSavedThroughTheLocalAPI(t *testing.T) {
	t.Parallel()
	world := newConfigScreenWorld(t, 30)
	const selected = "(•) CC-BY-SA-4.0"
	if views := configEditorViews(t, world); strings.Contains(views, selected) {
		t.Fatalf("peasant config already selects CC-BY-SA-4.0 before the update, so it cannot show the update:\n%s", views)
	}

	server := api.NewServer(api.ServerConfig{Config: configScreenConfig(t, world.configPath), ConfigPath: world.configPath})
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the local server did not stop")
		}
	})
	origin := "http://" + server.Addr().String()
	request, err := http.NewRequest(http.MethodPatch, origin+defaults.RouteSettings.String(), strings.NewReader(`{"key": "push.license", "value": "CC-BY-SA-4.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(defaults.HeaderOrigin, origin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PATCH %s: status = %d; body: %s", defaults.RouteSettings, response.StatusCode, body)
	}

	if views := configEditorViews(t, world); !strings.Contains(views, selected) {
		t.Errorf("peasant config does not select the license the web saved:\n%s", views)
	}
}

// configEditorViews runs `peasant config` on the world's file and returns the
// actual content license section.
func configEditorViews(t *testing.T, world *configScreenWorld) string {
	t.Helper()
	deps := world.dependencies(t)
	var views string
	deps.run = func(model tea.Model) (tea.Model, error) {
		model = configScreenDrain(model, model.Init())
		model = configScreenUpdate(model, tea.WindowSizeMsg{Width: 100, Height: 28})
		model = configScreenSelectSection(t, model, "content license")
		views = ansiPattern.ReplaceAllString(model.View().Content, "")
		return model, nil
	}
	if _, err := executeConfigScreenCommand(t, buildConfigCommand(deps), world, "config"); err != nil {
		t.Fatalf("peasant config: %v", err)
	}
	if !strings.Contains(views, "content license") {
		t.Fatalf("the rendered sections do not reach the content license:\n%s", views)
	}
	return views
}
