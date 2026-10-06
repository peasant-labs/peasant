package api

import (
	"bytes"
	_ "embed"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/auto-publish-session-targets.yaml
var autoPublishSessionTargetsYAML []byte

type autoPublishSessionTargetFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name      string `yaml:"name"`
		Directory string `yaml:"directory"`
		Status    int    `yaml:"status"`
	} `yaml:"cases"`
}

// This runs the real mounted PUT and Git resolver: a fake root cannot observe
// symlink or linked-worktree normalization. Each case owns its store and repos.
func TestAutoPublishSessionTargets(t *testing.T) {
	t.Parallel()
	var fixtures autoPublishSessionTargetFixture
	if err := testutil.DecodeNamedFixtureYAML(autoPublishSessionTargetsYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, row := range fixtures.Cases {
		t.Run(row.Name, func(t *testing.T) {
			world := newAutoPublishWorld(t, autoPublishWorldOptions{})
			world.save(t, "work", schema.AutoPublishRuleRequest{Kind: schema.AutoPublishRuleFolder, Match: world.second, Events: []schema.AutoPublishEvent{}, Collectives: []schema.VillageUUID{}})
			before, err := os.ReadFile(world.rulesPath())
			if err != nil {
				t.Fatal(err)
			}
			root, directory := world.recorded, world.recorded
			switch row.Directory {
			case "root", "missing", "ambiguous", "no-store":
			case "subdirectory":
				directory = filepath.Join(root, "services")
			case "symlink":
				directory = filepath.Join(world.dir, "alias")
				if err := os.Symlink(root, directory); err != nil {
					t.Fatal(err)
				}
			case "linked":
				runGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "--quiet", "-m", "initialize repository")
				directory = filepath.Join(world.dir, "linked")
				runGit(t, root, "worktree", "add", "--quiet", "-b", "linked", directory)
			case "glob":
				root = filepath.Join(world.dir, "project[one]")
				directory = root
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "init", "--quiet")
			case "gone":
				directory = filepath.Join(root, "removed")
			case "empty":
				directory = ""
			default:
				t.Fatalf("unknown directory fixture %q", row.Directory)
			}
			const targetID = "77778888-9999-4aaa-8bbb-ccccddddeeee"
			if row.Directory != "missing" {
				seedRecordedSession(t, world.db, targetID, schema.ProjectHash("7777777777777777777777777777777777777777777777777777777777777777"), "https://github.com/acme/tools.git", directory)
			}
			body := `{"sessionId":"` + targetID + `","events":["pre-push"],"collectives":[]}`
			if row.Directory == "ambiguous" {
				body = `{"sessionId":"` + targetID + `","kind":"folder","match":"/repo","events":["pre-push"],"collectives":[]}`
			}
			baseURL := world.baseURL
			if row.Directory == "no-store" {
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
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, baseURL+ruleRoute("work"), bytes.NewBufferString(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var response bytes.Buffer
			if _, err := response.ReadFrom(resp.Body); err != nil {
				t.Fatal(err)
			}
			if row.Status == http.StatusOK {
				var saved schema.AutoPublishRule
				decodeContract(t, resp.StatusCode, response.Bytes(), &saved)
				if saved.Kind != schema.AutoPublishRuleFolder || saved.Match != autopublish.FolderMatch(root) || len(saved.Repositories) != 1 || saved.Repositories[0].Path != root {
					t.Fatalf("resolved rule = %+v; want exactly %s", saved, root)
				}
				if len(saved.Repositories[0].Hooks) != 1 || saved.Repositories[0].Hooks[0].Status != schema.AutoPublishHookAbsent {
					t.Fatalf("save installed a hook: %+v", saved.Repositories)
				}
				rules, err := autopublish.Load(world.rulesPath())
				if err != nil || len(rules) != 1 || rules[0].Match != saved.Match || !slices.Equal(rules[0].Events, []schema.AutoPublishEvent{schema.AutoPublishPrePush}) {
					t.Fatalf("persisted rules=%+v, %v", rules, err)
				}
				if !rules[0].Covers(autopublish.Repository{Root: root}) || rules[0].Covers(autopublish.Repository{Root: world.second}) || rules[0].Covers(autopublish.Repository{Root: filepath.Join(world.dir, "projecto")}) {
					t.Fatal("exact folder rule broadens repository consent")
				}
				resolved, err := autopublish.Resolve(t.Context(), &ingest.ExecGitResolver{}, directory, false)
				if err != nil || !rules[0].Covers(resolved) {
					t.Fatalf("saved rule excludes target checkout: %+v, %v", resolved, err)
				}
			} else {
				code := autoPublishInvalidCode
				if row.Status == http.StatusServiceUnavailable {
					code = autoPublishUnavailableCode
				}
				decodeRefusal(t, resp.StatusCode, response.Bytes(), row.Status, code)
				after, err := os.ReadFile(world.rulesPath())
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("refused target changed rules: %q, %v", after, err)
				}
			}
			if prePushHook(t, world.recorded) != nil || prePushHook(t, world.second) != nil {
				t.Fatal("saving a target installed hooks")
			}
		})
	}
}
