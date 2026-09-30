package api

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestSettingsSavedThroughTheDashboardApplyAtOnce saves a custom redaction
// pattern through the mounted PATCH route, from the dashboard's own origin,
// and reads the redaction preview of a recorded session before and after. The
// preview applies the saved pattern without a restart, so what the settings
// page shows is what the next publish from the dashboard redacts.
func TestSettingsSavedThroughTheDashboardApplyAtOnce(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ffff6666-ffff-4fff-8fff-ffffffffffff"
		probeRule = "settings-live-probe"
	)
	hs := newTestXDGHomes(t)
	basePath := filepath.Join(hs.Data, "peasant-sync")
	db := seedSyncDoorSession(t, hs.dbPath(), sessionID, basePath)
	t.Cleanup(func() { _ = db.Close() })
	path := defaults.ResolveConfigFilePathWith(hs.Config).String()
	cfg := config.BaseConfig()
	cfg.Output.BasePath = basePath
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	_, baseURL := startHelperGroupServerHandle(t, hs.config(ServerConfig{Store: db, Config: cfg, ConfigPath: path}))

	preview := func() string {
		t.Helper()
		response, err := http.Get(baseURL + defaults.RouteSyncRedactions.String() + "?session_id=" + sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("redaction preview status = %d; body: %s", response.StatusCode, body)
		}
		return string(body)
	}
	if before := preview(); strings.Contains(before, probeRule) {
		t.Fatalf("the preview applies %s before it is saved, so it cannot show the save taking effect: %s", probeRule, before)
	}

	update := `{"key": "redaction.custom_patterns", "value": [{"id": "` + probeRule + `", "category": "pii", "pattern": "thanks", "replacement": "[THANKS]"}]}`
	request, err := http.NewRequest(http.MethodPatch, baseURL+defaults.RouteSettings.String(), strings.NewReader(update))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	request.Header.Set(defaults.HeaderOrigin, baseURL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PATCH from the dashboard's own origin: status = %d; body: %s", response.StatusCode, body)
	}

	if after := preview(); !strings.Contains(after, probeRule) {
		t.Errorf("the saved pattern %s is in config.yaml but the running dashboard does not apply it: %s", probeRule, after)
	}
}
