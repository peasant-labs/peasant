package api

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
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

// TestSettingsListTheSavedAutoPublishRules saves a rule through the mounted
// auto-publish route and reads the settings: the GET answer lists that rule
// exactly as the save answered it, with the recorded repository it covers and
// its hook. A rules file that cannot be read fails the read rather than drop
// the rule from the list.
func TestSettingsListTheSavedAutoPublishRules(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(dir, "recorded")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repository, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hs := newTestXDGHomes(t)
	if err := os.MkdirAll(filepath.Dir(hs.dbPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	storetest.CopyGoldenTo(t, hs.dbPath())
	db, err := store.Open(hs.dbPath(), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedRecordedSession(t, db, recordedInsideSessionID, insideProject, "https://github.com/acme/tools.git", repository)
	path := defaults.ResolveConfigFilePathWith(hs.Config).String()
	cfg := config.BaseConfig()
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	_, baseURL := startHelperGroupServerHandle(t, hs.config(ServerConfig{Store: db, Config: cfg, ConfigPath: path}))
	world := &publishingWorld{hs: hs, db: db, baseURL: baseURL}

	var before schema.LocalSettingsResponse
	world.decode(t, http.MethodGet, defaults.RouteSettings.String(), &before)
	if len(before.AutoPublish) != 0 {
		t.Fatalf("autoPublish before any rule = %+v", before.AutoPublish)
	}

	rule := schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: dir + "/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}}
	var saved schema.AutoPublishRule
	status, body := world.request(t, http.MethodPut, strings.Replace(defaults.RouteAutoPublishRule.String(), "{id}", "work", 1), rule)
	decodeContract(t, status, body, &saved)
	if len(saved.Repositories) != 1 {
		t.Fatalf("saved rule = %+v; it covers the recorded repository", saved)
	}
	var after schema.LocalSettingsResponse
	world.decode(t, http.MethodGet, defaults.RouteSettings.String(), &after)
	if len(after.AutoPublish) != 1 || !reflect.DeepEqual(after.AutoPublish[0], saved) {
		t.Fatalf("autoPublish = %+v, want exactly the saved rule %+v", after.AutoPublish, saved)
	}

	rulesPath := autopublish.Path(defaults.ResolveConfigDirPathWith(hs.Config))
	if err := os.WriteFile(rulesPath, []byte("version: 1\nrules:\n  - id: work\n    kind: sideways\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body = world.request(t, http.MethodGet, defaults.RouteSettings.String(), nil)
	refusal := decodeRefusal(t, status, body, http.StatusInternalServerError, "settings_auto_publish_unreadable")
	if !strings.Contains(refusal.Error, rulesPath) {
		t.Errorf("the refusal does not name the rules file %s: %s", rulesPath, refusal.Error)
	}
}
