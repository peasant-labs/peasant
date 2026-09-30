package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The auto-publish world: two git repositories side by side, one Peasant has
// recorded sessions in and one it has not, served by the mounted local API.
// The recorded repository holds a session the saved selection lists and one
// it leaves out.
const (
	recordedInsideSessionID  = "44445555-6666-4777-8888-9999aaaabbbb"
	recordedOutsideSessionID = "55556666-7777-4888-8999-aaaabbbbcccc"
)

type autoPublishWorld struct {
	*publishingWorld
	dir        string
	recorded   string
	unrecorded string
}

func (w *autoPublishWorld) repository(name string) string {
	switch name {
	case "recorded":
		return w.recorded
	case "unrecorded":
		return w.unrecorded
	}
	return ""
}

func newAutoPublishWorld(t *testing.T) *autoPublishWorld {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	world := &autoPublishWorld{dir: dir, recorded: filepath.Join(dir, "recorded"), unrecorded: filepath.Join(dir, "unrecorded")}
	for _, repo := range []string{world.recorded, world.unrecorded} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", repo, "init", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", repo, err, out)
		}
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
	seedRecordedSession(t, db, recordedInsideSessionID, insideProject, "https://github.com/acme/tools.git", filepath.Join(world.recorded, "services"))
	seedRecordedSession(t, db, recordedOutsideSessionID, outsideProject, "git@github.com:user/repo.git", world.recorded)
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.BaseConfig()
	cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{
		defaults.HarnessClaudeCode.String(): {Projects: []config.ProjectSelection{{GitRemote: "git@github.com:acme/tools.git"}}},
	}}
	policy, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := NewServer(hs.config(ServerConfig{Port: 0, Store: db, Config: cfg, Provider: NewStoreDataProvider(db, policy)}))
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	world.publishingWorld = &publishingWorld{hs: hs, db: db, baseURL: "http://" + server.Addr().String()}
	return world
}

// seedRecordedSession stores a session recorded in worktree.
func seedRecordedSession(t *testing.T, db *store.Store, sessionID string, project schema.ProjectHash, remote, worktree string) {
	t.Helper()
	const startMs int64 = 1700000000000
	ingested := startMs + 120000
	branch := "main"
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = schema.SessionID(sessionID)
	meta.HostSlug = schema.HostSlug("github.com-synthetic")
	meta.ModelHarness = defaults.HarnessClaudeCode
	meta.Model = testutil.TestModel
	meta.Timestamp = ingest.TimestampInfo{Start: startMs, End: startMs + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/synthetic/" + sessionID + ".jsonl", Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: project, Name: "synthetic", FilePath: worktree}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000}
	meta.Git = ingest.GitContext{Remote: &remote, Branch: &branch, Worktree: &worktree}
	text := "a synthetic turn"
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{{SessionID: meta.SessionID, EntryIndex: 1, Role: schema.RoleUser, Harness: schema.Harness(defaults.HarnessClaudeCode), EntryType: schema.EntryTypeText, ContentPreview: &text}})
}

// prePushHook reads the recorded pre-push hook of repo, or nil when there is
// none.
func prePushHook(t *testing.T, repo string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "pre-push"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAutoPublishInstallRoutes runs the install cases of
// internal/autopublish/testdata/auto-publish-rules.yaml on the mounted routes:
// saving a rule installs nothing and lists only recorded repositories, and an
// install writes a hook only in a recorded repository the rule covers, never
// over a hook Peasant did not write.
func TestAutoPublishInstallRoutes(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverInstall) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			world := newAutoPublishWorld(t)
			if c.ForeignHook != "" {
				if err := os.WriteFile(filepath.Join(world.recorded, ".git", "hooks", "pre-push"), []byte(c.ForeignHook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			rules := c.RulesFor(world.dir, "")
			for _, rule := range rules {
				status, body := world.request(t, http.MethodPut, strings.Replace(defaults.RouteAutoPublishRule.String(), "{id}", rule.ID, 1), rule.Request())
				var saved schema.AutoPublishRule
				decodeContract(t, status, body, &saved)
				for _, repository := range saved.Repositories {
					if repository.Path != world.recorded {
						t.Errorf("rule %s lists %s; a rule lists only repositories Peasant recorded", rule.ID, repository.Path)
					}
				}
			}
			if hook := prePushHook(t, world.recorded); c.ForeignHook == "" && hook != nil {
				t.Fatal("saving a rule installed a hook")
			}

			status, body := world.request(t, http.MethodPost, strings.Replace(defaults.RouteAutoPublishInstall.String(), "{id}", rules[0].ID, 1), schema.AutoPublishInstallRequest{Path: world.repository(c.Install)})
			if c.Expect.Status == http.StatusOK {
				var installed schema.AutoPublishRepository
				decodeContract(t, status, body, &installed)
				got := map[schema.AutoPublishEvent]schema.AutoPublishHookStatus{}
				for _, hook := range installed.Hooks {
					got[hook.Event] = hook.Status
					if hook.Remedy == nil {
						continue
					}
					for _, want := range c.Expect.RemedyContains {
						if !strings.Contains(hook.Remedy.Message, want) {
							t.Errorf("%s remedy does not say %q:\n%s", hook.Event, want, hook.Remedy.Message)
						}
					}
					for _, want := range c.Expect.SnippetContains {
						if !strings.Contains(hook.Remedy.Snippet, want) {
							t.Errorf("%s snippet does not hold %q:\n%s", hook.Event, want, hook.Remedy.Snippet)
						}
					}
				}
				if len(got) != len(c.Expect.Hooks) {
					t.Errorf("hooks = %v, want %v", got, c.Expect.Hooks)
				}
				for event, want := range c.Expect.Hooks {
					if got[event] != want {
						t.Errorf("%s hook = %q, want %q", event, got[event], want)
					}
				}
			} else {
				decodeRefusal(t, status, body, c.Expect.Status, c.Expect.Code)
			}

			for _, name := range []string{"recorded", "unrecorded"} {
				hook := prePushHook(t, world.repository(name))
				if managed := githooks.IsManaged(hook); managed != slices.Contains(c.Expect.Installed, name) {
					t.Errorf("%s repository holds a managed pre-push hook = %v, want %v", name, managed, !managed)
				}
			}
			if c.ForeignHook != "" && string(prePushHook(t, world.recorded)) != c.ForeignHook {
				t.Error("the hook Peasant did not write was changed")
			}
		})
	}
}

// TestAutoPublishRoutesChangeNoHookOnTheirOwn walks one rule through the
// routes: saving it installs nothing, installing is the one act that writes a
// hook, publications then report autoPublish for the session the saved
// selection lists, and removing the rule leaves the hook as it is.
func TestAutoPublishRoutesChangeNoHookOnTheirOwn(t *testing.T) {
	t.Parallel()
	world := newAutoPublishWorld(t)
	route := strings.Replace(defaults.RouteAutoPublishRule.String(), "{id}", "work", 1)
	install := strings.Replace(defaults.RouteAutoPublishInstall.String(), "{id}", "work", 1)
	rule := schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.dir + "/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}}
	autoPublish := func() map[string]bool {
		t.Helper()
		var publications schema.LocalPublicationsResponse
		world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds="+recordedInsideSessionID+","+recordedOutsideSessionID, &publications)
		got := map[string]bool{}
		for _, row := range publications.Publications {
			got[row.SessionID] = row.AutoPublish
		}
		return got
	}

	var saved schema.AutoPublishRule
	status, body := world.request(t, http.MethodPut, route, rule)
	decodeContract(t, status, body, &saved)
	if len(saved.Repositories) != 1 || saved.Repositories[0].Hooks[0].Status != schema.AutoPublishHookAbsent {
		t.Fatalf("saved rule = %+v; it lists the recorded repository with no hook", saved)
	}
	if got := autoPublish(); got[recordedInsideSessionID] || got[recordedOutsideSessionID] {
		t.Fatalf("autoPublish before any install = %v", got)
	}
	rules, err := autopublish.Load(autopublish.Path(defaults.ResolveConfigDirPathWith(world.hs.Config)))
	if err != nil || len(rules) != 1 || rules[0].ID != "work" {
		t.Fatalf("hooks.yaml after the save = %+v, %v", rules, err)
	}

	var installed schema.AutoPublishRepository
	status, body = world.request(t, http.MethodPost, install, schema.AutoPublishInstallRequest{Path: world.recorded})
	decodeContract(t, status, body, &installed)
	if installed.Hooks[0].Status != schema.AutoPublishHookInstalled {
		t.Fatalf("install = %+v", installed)
	}
	if hook := string(prePushHook(t, world.recorded)); !strings.Contains(hook, "--config-dir "+githooks.ShellQuote(world.hs.Config)) {
		t.Errorf("the installed hook does not read this server's config directory, so it would not read its rules:\n%s", hook)
	}
	if got := autoPublish(); !got[recordedInsideSessionID] || got[recordedOutsideSessionID] {
		t.Fatalf("autoPublish after the install = %v; the listed session publishes and the one the selection leaves out does not", got)
	}

	var removed schema.AutoPublishRemovalResponse
	status, body = world.request(t, http.MethodDelete, route, nil)
	decodeContract(t, status, body, &removed)
	if len(removed.Repositories) != 1 || removed.Repositories[0].Hooks[0].Status != schema.AutoPublishHookInstalled {
		t.Fatalf("removal = %+v; it reports the hook as it is", removed)
	}
	if !githooks.IsManaged(prePushHook(t, world.recorded)) {
		t.Fatal("removing the rule removed its hook")
	}

	status, body = world.request(t, http.MethodDelete, route, nil)
	decodeRefusal(t, status, body, http.StatusNotFound, autoPublishNotFoundCode)
	status, body = world.request(t, http.MethodPost, install, schema.AutoPublishInstallRequest{Path: world.recorded})
	decodeRefusal(t, status, body, http.StatusNotFound, autoPublishNotFoundCode)
	status, body = world.request(t, http.MethodPut, route, map[string]any{"kind": "folder", "match": world.dir, "events": []string{}, "collectives": []string{}, "visibility": "public"})
	decodeRefusal(t, status, body, http.StatusBadRequest, autoPublishInvalidCode)
	status, body = world.request(t, http.MethodPut, route, map[string]any{"kind": "folder", "match": world.dir, "events": nil, "collectives": []string{}})
	decodeRefusal(t, status, body, http.StatusBadRequest, autoPublishInvalidCode)
	rule.Match = "relative/*"
	status, body = world.request(t, http.MethodPut, route, rule)
	decodeRefusal(t, status, body, http.StatusBadRequest, autoPublishInvalidCode)
	if rules, err := autopublish.Load(autopublish.Path(defaults.ResolveConfigDirPathWith(world.hs.Config))); err != nil || len(rules) != 0 {
		t.Fatalf("hooks.yaml after refused saves = %+v, %v; a refused save changes nothing", rules, err)
	}
}
