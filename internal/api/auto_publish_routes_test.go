package api

import (
	"bytes"
	"context"
	_ "embed"
	"net/http"
	"net/http/httptest"
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
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// The auto-publish world: three git repositories side by side, two Peasant
// has recorded sessions in and one it has not, served by the mounted local
// API. The first recorded repository holds a session the saved selection
// lists and one it leaves out.
const (
	recordedInsideSessionID  = "44445555-6666-4777-8888-9999aaaabbbb"
	recordedOutsideSessionID = "55556666-7777-4888-8999-aaaabbbbcccc"
	secondRecordedSessionID  = "66667777-8888-4999-8aaa-bbbbccccdddd"
)

type autoPublishWorld struct {
	*publishingWorld
	dir        string
	recorded   string
	second     string
	unrecorded string
}

// autoPublishWorldOptions shape a world.
type autoPublishWorldOptions struct {
	// remote gives the first recorded repository an origin remote.
	remote bool
	// level is the configured redaction level; empty keeps the default.
	level redact.RedactionLevel
}

func (w *autoPublishWorld) repository(name autopublishtest.RepositoryRole) string {
	return map[autopublishtest.RepositoryRole]string{"recorded": w.recorded, "second": w.second, "unrecorded": w.unrecorded}[name]
}

func (w *autoPublishWorld) rulesPath() string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(w.hs.Config))
}

func newAutoPublishWorld(t *testing.T, options autoPublishWorldOptions) *autoPublishWorld {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	world := &autoPublishWorld{dir: dir, recorded: filepath.Join(dir, "recorded"), second: filepath.Join(dir, "second"), unrecorded: filepath.Join(dir, "unrecorded")}
	for _, repo := range []string{world.recorded, world.second, world.unrecorded} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "init", "--quiet")
	}
	if options.remote {
		runGit(t, world.recorded, "remote", "add", "origin", "https://github.com/acme/tools.git")
	}
	services := filepath.Join(world.recorded, "services")
	if err := os.MkdirAll(services, 0o700); err != nil {
		t.Fatal(err)
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
	seedRecordedSession(t, db, recordedInsideSessionID, insideProject, "https://github.com/acme/tools.git", services)
	seedRecordedSession(t, db, recordedOutsideSessionID, outsideProject, "git@github.com:user/repo.git", world.recorded)
	seedRecordedSession(t, db, secondRecordedSessionID, schema.ProjectHash(strings.Repeat("6", 64)), "git@github.com:user/other.git", world.second)
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.BaseConfig()
	if options.level != "" {
		cfg.Redaction.Level = options.level
	}
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
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

// prePushHook reads the pre-push hook of repo, or nil when there is none.
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

func ruleRoute(id string) string {
	return strings.Replace(defaults.RouteAutoPublishRule.String(), "{id}", id, 1)
}

func installRoute(id string) string {
	return strings.Replace(defaults.RouteAutoPublishInstall.String(), "{id}", id, 1)
}

// save PUTs one rule and decodes the answer with the contract type.
func (w *autoPublishWorld) save(t *testing.T, id string, request schema.AutoPublishRuleRequest) schema.AutoPublishRule {
	t.Helper()
	status, body := w.request(t, http.MethodPut, ruleRoute(id), request)
	var saved schema.AutoPublishRule
	decodeContract(t, status, body, &saved)
	return saved
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
			world := newAutoPublishWorld(t, autoPublishWorldOptions{remote: c.RemoteRecorded})
			if c.ForeignHook != "" {
				if err := os.WriteFile(filepath.Join(world.recorded, ".git", "hooks", "pre-push"), []byte(c.ForeignHook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			rules := c.RulesFor(world.dir, "")
			for _, rule := range rules {
				saved := world.save(t, rule.ID, rule.Request())
				for _, repository := range saved.Repositories {
					if repository.Path == world.unrecorded {
						t.Errorf("rule %s lists %s; a rule lists only repositories Peasant recorded", rule.ID, repository.Path)
					}
				}
			}
			if hook := prePushHook(t, world.recorded); c.ForeignHook == "" && hook != nil {
				t.Fatal("saving a rule installed a hook")
			}

			status, body := world.request(t, http.MethodPost, installRoute(rules[0].ID), schema.AutoPublishInstallRequest{Path: world.repository(c.Install)})
			if c.Expect.Status == http.StatusOK {
				var installed schema.AutoPublishRepository
				decodeContract(t, status, body, &installed)
				if installed.Label != c.Expect.Label {
					t.Errorf("label = %q, want %q", installed.Label, c.Expect.Label)
				}
				got := map[schema.AutoPublishEvent]schema.AutoPublishHookStatus{}
				for _, hook := range installed.Hooks {
					got[hook.Event] = hook.Status
					if hook.Remedy == nil {
						if len(c.Expect.RemedyContains) > 0 || len(c.Expect.SnippetContains) > 0 {
							t.Fatalf("%s hook has no remedy, but the fixture requires remedy text", hook.Event)
						}
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

			for _, name := range []string{"recorded", "second", "unrecorded"} {
				hook := prePushHook(t, world.repository(autopublishtest.RepositoryRole(name)))
				if managed := githooks.IsManaged(hook); managed != slices.Contains(c.Expect.Installed, autopublishtest.RepositoryRole(name)) {
					t.Errorf("%s repository holds a managed pre-push hook = %v, want %v", name, managed, !managed)
				}
			}
			if c.ForeignHook != "" && string(prePushHook(t, world.recorded)) != c.ForeignHook {
				t.Error("the hook Peasant did not write was changed")
			}
		})
	}
}

// TestAutoPublishRoutesRefuseBodies runs the rule-body cases: a body outside
// the contract is refused and hooks.yaml is not written.
func TestAutoPublishRoutesRefuseBodies(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverRuleBody) {
		t.Run(c.Name, func(t *testing.T) {
			world := newAutoPublishWorld(t, autoPublishWorldOptions{})
			if c.RulesFile != "" {
				if err := os.MkdirAll(filepath.Dir(world.rulesPath()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(world.rulesPath(), []byte(c.RulesFile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			route := ruleRoute("work")
			if c.Method == http.MethodPost {
				route = installRoute("work")
			}
			baseURL := world.baseURL
			if c.Unavailable {
				api := NewServer(world.hs.config(ServerConfig{Config: config.BaseConfig()}))
				if err := api.Listen(t.Context()); err != nil {
					t.Fatal(err)
				}
				defer func() {
					for _, listener := range api.lns {
						_ = listener.Close()
					}
				}()
				server := httptest.NewServer(api.server.Handler)
				defer server.Close()
				baseURL = server.URL
			}
			req, err := http.NewRequestWithContext(t.Context(), c.Method, baseURL+route, bytes.NewReader([]byte(c.Body)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var raw bytes.Buffer
			if _, err := raw.ReadFrom(resp.Body); err != nil {
				t.Fatal(err)
			}
			decodeRefusal(t, resp.StatusCode, raw.Bytes(), c.Expect.Status, c.Expect.Code)
			if c.RulesFile != "" {
				raw, err := os.ReadFile(world.rulesPath())
				if err != nil || string(raw) != c.RulesFile {
					t.Fatalf("a refused save changed hooks.yaml: %q, %v", raw, err)
				}
			} else if _, err := os.Stat(world.rulesPath()); !os.IsNotExist(err) {
				t.Fatalf("a refused body wrote hooks.yaml: %v", err)
			}
		})
	}
}

// TestAutoPublishRoutesChangeNoHookOnTheirOwn walks rules through the routes:
// saving installs nothing and lists every covered recorded repository,
// replacing and removing a rule touches only that rule, installing is the one
// act that writes a hook, publications then report autoPublish for the
// session the saved selection lists, and removing the rule retains hook files.
func TestAutoPublishRoutesChangeNoHookOnTheirOwn(t *testing.T) {
	t.Parallel()
	world := newAutoPublishWorld(t, autoPublishWorldOptions{})
	platform, research := publishingCollectives["platform"].ID, publishingCollectives["research"].ID
	rule := schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.dir + "/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{platform}}
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

	saved := world.save(t, "work", rule)
	var paths []string
	for _, repository := range saved.Repositories {
		paths = append(paths, repository.Path)
		if repository.Hooks[0].Status != schema.AutoPublishHookAbsent {
			t.Errorf("%s hook = %q before any install", repository.Path, repository.Hooks[0].Status)
		}
	}
	if !slices.Equal(paths, []string{world.recorded, world.second}) {
		t.Fatalf("the rule lists %v; it covers both recorded repositories and not the unrecorded one", paths)
	}
	if got := autoPublish(); got[recordedInsideSessionID] || got[recordedOutsideSessionID] {
		t.Fatalf("autoPublish before any install = %v", got)
	}

	// Replacing a rule keeps one rule; removing the middle one of three leaves
	// the others as they were.
	rule.Collectives = []schema.VillageUUID{research}
	world.save(t, "work", rule)
	other := schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleRemote, Match: "github.com/acme/*", Events: []schema.AutoPublishEvent{}, Collectives: []schema.VillageUUID{platform}}
	world.save(t, "other", other)
	late := schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.dir + "/second", Events: []schema.AutoPublishEvent{schema.AutoPublishPostCommit}, Collectives: []schema.VillageUUID{research}}
	world.save(t, "late", late)
	var removedOther schema.AutoPublishRemovalResponse
	status, body := world.request(t, http.MethodDelete, ruleRoute("other"), nil)
	decodeContract(t, status, body, &removedOther)
	rules, err := autopublish.Load(world.rulesPath())
	if err != nil || len(rules) != 2 || rules[0].ID != "work" || !slices.Equal(rules[0].Collectives, []schema.VillageUUID{research}) || rules[0].Match != rule.Match || rules[1].ID != "late" || rules[1].Match != late.Match {
		t.Fatalf("hooks.yaml = %+v, %v; want the replaced rule and the late one", rules, err)
	}
	status, body = world.request(t, http.MethodDelete, ruleRoute("late"), nil)
	decodeContract(t, status, body, &removedOther)

	var installed schema.AutoPublishRepository
	status, body = world.request(t, http.MethodPost, installRoute("work"), schema.AutoPublishInstallRequest{Path: world.recorded})
	decodeContract(t, status, body, &installed)
	if installed.Hooks[0].Status != schema.AutoPublishHookInstalled {
		t.Fatalf("install = %+v", installed)
	}
	if hook := string(prePushHook(t, world.recorded)); !strings.Contains(hook, "--config-dir "+githooks.ShellQuote(world.hs.Config)) || !strings.Contains(hook, "--data-dir "+githooks.ShellQuote(world.hs.Data)) || !strings.Contains(hook, "--state-dir "+githooks.ShellQuote(world.hs.State)) {
		t.Errorf("the installed hook does not bind this server's config and data directories, so it would not read its rules and store:\n%s", hook)
	}
	if got := autoPublish(); !got[recordedInsideSessionID] || got[recordedOutsideSessionID] {
		t.Fatalf("autoPublish after the install = %v; the listed session publishes and the one the selection leaves out does not", got)
	}

	var removed schema.AutoPublishRemovalResponse
	status, body = world.request(t, http.MethodDelete, ruleRoute("work"), nil)
	decodeContract(t, status, body, &removed)
	if len(removed.Repositories) != 2 || removed.Repositories[0].Hooks[0].Status != schema.AutoPublishHookInstalled {
		t.Fatalf("removal = %+v; it reports the hooks as they are", removed)
	}
	if !githooks.IsManaged(prePushHook(t, world.recorded)) {
		t.Fatal("removing the rule removed its hook")
	}
	status, body = world.request(t, http.MethodDelete, ruleRoute("work"), nil)
	decodeRefusal(t, status, body, http.StatusNotFound, autoPublishNotFoundCode)
	status, body = world.request(t, http.MethodPost, installRoute("work"), schema.AutoPublishInstallRequest{Path: world.recorded})
	decodeRefusal(t, status, body, http.StatusNotFound, autoPublishNotFoundCode)
}

// TestAutoPublishInstallRefusesAnUnsupportedRedactionLevel pins that no hook
// is installed when every upload it ran would be refused.
func TestAutoPublishInstallRefusesAnUnsupportedRedactionLevel(t *testing.T) {
	t.Parallel()
	world := newAutoPublishWorld(t, autoPublishWorldOptions{level: redact.Maximum})
	world.save(t, "work", schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.dir + "/*", Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{}})
	status, body := world.request(t, http.MethodPost, installRoute("work"), schema.AutoPublishInstallRequest{Path: world.recorded})
	refusal := decodeRefusal(t, status, body, http.StatusBadRequest, autoPublishInvalidCode)
	if !strings.Contains(refusal.Error, "redaction.level") || prePushHook(t, world.recorded) != nil {
		t.Fatalf("refusal %q; it names the level and installs nothing", refusal.Error)
	}
}

//go:embed testdata/publication_hook_bindings.yaml
var publicationHookBindingsYAML []byte

func TestPublicationHookBindingFixtures(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string                    `yaml:"name"`
			Guarded        bool                      `yaml:"guarded"`
			Manual         bool                      `yaml:"manual"`
			Nonexecutable  bool                      `yaml:"nonexecutable"`
			Moved          bool                      `yaml:"moved"`
			Resume         bool                      `yaml:"resume"`
			Corrupt        bool                      `yaml:"corrupt"`
			Events         []schema.AutoPublishEvent `yaml:"events"`
			RetainedEvents []schema.AutoPublishEvent `yaml:"retainedEvents"`
			Delete         bool                      `yaml:"delete"`
			Expect         bool                      `yaml:"expect"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(publicationHookBindingsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			world := newAutoPublishWorld(t, autoPublishWorldOptions{})
			if c.Events != nil {
				world.save(t, "work", schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.recorded, Events: c.Events, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}})
			}
			binding := githooks.Binding{ConfigDir: world.hs.Config, DataDir: world.hs.Data, StateDir: world.hs.State, RequireAutoPublishRule: c.Guarded}
			hookPath := filepath.Join(world.recorded, ".git", "hooks", "pre-push")
			if c.Manual {
				snippet, err := githooks.ManualSnippet(githooks.EventPrePush, world.recorded, hookPath, binding)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hookPath, []byte("#!/bin/sh\n"+snippet), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				report, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{Dir: world.recorded, Events: []githooks.Event{githooks.EventPrePush}, Binding: binding})
				if err != nil || report.Blocked() {
					t.Fatalf("install: %+v, %v", report, err)
				}
			}
			if c.Nonexecutable {
				if err := os.Chmod(hookPath, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.Moved {
				written := prePushHook(t, world.recorded)
				moved := strings.Replace(string(written), "peasant_hook_repository="+githooks.ShellQuote(world.recorded), "peasant_hook_repository="+githooks.ShellQuote(world.dir+"/moved"), 1)
				if moved == string(written) || !githooks.IsManaged([]byte(moved)) {
					t.Fatal("the hook could not be pointed at a moved repository")
				}
				if err := os.WriteFile(hookPath, []byte(moved), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.Resume {
				world.save(t, "work", schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.recorded, Events: []schema.AutoPublishEvent{schema.AutoPublishPrePush}, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}})
			}
			if c.RetainedEvents != nil {
				world.save(t, "retained", schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.recorded, Events: c.RetainedEvents, Collectives: []schema.VillageUUID{publishingCollectives["platform"].ID}})
			}
			if c.Delete {
				var removed schema.AutoPublishRemovalResponse
				status, body := world.request(t, http.MethodDelete, ruleRoute("work"), nil)
				decodeContract(t, status, body, &removed)
			}
			if c.Corrupt {
				if err := os.MkdirAll(filepath.Dir(world.rulesPath()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(world.rulesPath(), []byte("autoPublish: [unfinished"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var got schema.LocalPublicationsResponse
			world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds="+recordedInsideSessionID, &got)
			if len(got.Publications) != 1 || got.Publications[0].AutoPublish != c.Expect {
				t.Fatalf("publications: %+v; expected autoPublish=%v", got.Publications, c.Expect)
			}
			if raw := prePushHook(t, world.recorded); raw == nil {
				t.Fatal("binding deletion must retain hook bytes")
			}
		})
	}
}
