package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// autoPublishSessionID is the one session the auto-publish world records, in
// its repository. Its transcript on the Village double has the same ID.
const autoPublishSessionID = "abcd1234-abcd-4bcd-8bcd-abcdef123456"

// autoPublishRemote is the world repository's origin, and autoPublishMatch its
// bare form, which a rule for exactly that remote uses.
const (
	autoPublishRemote = "https://github.com/acme/tools.git"
	autoPublishMatch  = "github.com/acme/tools"
)

// autoPublishWorld is a recorded repository with one ready session, a second
// repository with none, a Village double that knows the fixture's
// collectives, and a computer signed in to it. dir holds the config, data, and
// state roots every command runs with.
type autoPublishWorld struct {
	fixture autopublishtest.Fixture
	dir     string
	world   string
	repo    string
	fresh   string
	village *testutil.CollectiveVillage
	cfgPath string
}

func newAutoPublishWorld(t *testing.T, fixture autopublishtest.Fixture, visibility schema.Visibility, license schema.License) *autoPublishWorld {
	t.Helper()
	world, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &autoPublishWorld{fixture: fixture, dir: t.TempDir(), world: world, repo: filepath.Join(world, "tools"), fresh: filepath.Join(world, "fresh")}
	for _, repo := range []string{w.repo, w.fresh} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")
		hooksGit(t, repo, "", "remote", "add", "origin", autoPublishRemote)
	}

	collectives := make([]testutil.VillageCollective, 0, len(fixture.Collectives))
	for _, alias := range []string{"platform", "research", "review"} {
		c := fixture.Collectives[alias]
		collectives = append(collectives, testutil.VillageCollective{ID: c.ID, Name: c.Name, Acceptance: c.Acceptance, Member: true})
	}
	w.village = testutil.NewCollectiveVillage(t, collectives...)
	writeTestCredentialsFor(t, w.dir, w.village.URL())

	dbPath := string(defaults.ResolveDBFilePathWith(w.dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := ingest.DeriveProjectIdentifiersWithGit(t.Context(), db.InstallationSalt(), &ingest.ExecGitResolver{}, "", w.repo)
	if err != nil {
		t.Fatal(err)
	}
	entry := makeCmdStoreEntry(t, autoPublishSessionID, "github.com-acme-tools", autoPublishRemote, "main", 1700000000000, w.repo)
	entry.Metadata.Project.Hash = hash
	testutil.SeedReadyPublication(t, db, entry.Metadata, nil)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	body := "version: 1\noutput:\n  basePath: " + filepath.Join(w.dir, "peasant-sync") + "\npush:\n  method: all\n"
	if visibility != "" {
		body += "  visibility: " + string(visibility) + "\n"
	}
	if license != "" {
		body += "  license: " + string(license) + "\n"
	}
	w.cfgPath = writeCfg(t, w.dir, "auto-publish.yaml", body)
	return w
}

// rulesPath is the hooks.yaml every command of the world reads.
func (w *autoPublishWorld) rulesPath() string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(w.dir))
}

func (w *autoPublishWorld) writeRules(t *testing.T, c autopublishtest.Case) {
	t.Helper()
	raw := []byte(c.RulesFile)
	if c.RulesFile == "" {
		var err error
		raw, err = yaml.Marshal(map[string]any{"version": 1, "autoPublish": c.RulesFor(w.world, autoPublishMatch)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(w.rulesPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.rulesPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// run executes one command under the world's roots and returns its standard
// output, its standard error, and its error.
func (w *autoPublishWorld) run(t *testing.T, sub *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(sub)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--data-dir", w.dir, "--config-dir", w.dir, "--state-dir", w.dir, sub.Name()}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// push runs the upload a managed hook runs for the world's repository.
func (w *autoPublishWorld) push(t *testing.T, flags ...string) (string, string, error) {
	t.Helper()
	return w.run(t, BuildPushCommand(), append([]string{"--config", w.cfgPath, "--non-interactive", "--quiet", "--repository", w.repo}, flags...)...)
}

// audience is who can read the session's transcript, by collective alias.
func (w *autoPublishWorld) audience() map[string]schema.VillageShareStatus {
	byID := map[schema.VillageUUID]string{}
	for alias, collective := range w.fixture.Collectives {
		byID[collective.ID] = alias
	}
	audience := map[string]schema.VillageShareStatus{}
	for id, status := range w.village.Audience(autoPublishSessionID) {
		audience[byID[id]] = status
	}
	return audience
}

func (w *autoPublishWorld) managedPrePush(t *testing.T, repo string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "pre-push"))
	return err == nil && githooks.IsManaged(raw)
}

func assertErrorContains(t *testing.T, err error, want []string, output string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("the command failed: %v\n%s", err, output)
		}
		return
	}
	if err == nil {
		t.Fatalf("the command succeeded; want an error saying %q\n%s", want, output)
	}
	for _, part := range want {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the error does not say %q:\n%v", part, err)
		}
	}
}

// TestAutoPublishPush runs the push cases of
// internal/autopublish/testdata/auto-publish-rules.yaml: a push a rule covers
// publishes its sessions private with no license and shares each transcript
// it sent with the rule's collectives, and a push no rule covers publishes as
// it always did.
func TestAutoPublishPush(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverPush) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Expect.Publishes == nil || c.Expect.Audience == nil {
				t.Fatal("a push case states its publishes and its audience ({} for none)")
			}
			w := newAutoPublishWorld(t, fixture, c.Config.Visibility, c.Config.License)
			w.writeRules(t, c)
			for _, alias := range c.FailShare {
				w.village.FailShare(fixture.Collectives[alias].ID)
			}
			stdout, stderr, err := w.push(t, c.Flags...)
			assertErrorContains(t, err, c.Expect.ErrorContains, stdout+stderr)

			publishes := w.village.Publishes()
			if len(publishes) != *c.Expect.Publishes {
				t.Fatalf("Village received %d uploads, want %d\n%s%s", len(publishes), *c.Expect.Publishes, stdout, stderr)
			}
			for _, publish := range publishes {
				if publish.License != c.Expect.License {
					t.Errorf("the upload carried license %q, want %q", publish.License, c.Expect.License)
				}
			}
			if got := w.village.OwnerUpdates(); got != c.Expect.OwnerUpdates {
				t.Errorf("owner updates = %d, want %d", got, c.Expect.OwnerUpdates)
			}
			got := w.audience()
			if len(got) != len(c.Expect.Audience) {
				t.Errorf("audience = %v, want %v", got, c.Expect.Audience)
			}
			for alias, want := range c.Expect.Audience {
				if got[alias] != want {
					t.Errorf("%s share = %q, want %q", alias, got[alias], want)
				}
			}
		})
	}
}

// TestAutoPublishHookNeverBlocksThePush runs the hook cases: the upload under
// the rule fails against a Village that is gone, and the managed hook the
// rule installed runs through a real git push with an upload that fails with
// each status the upload exits with. The push still reaches its remote.
func TestAutoPublishHookNeverBlocksThePush(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverHook) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if len(c.UploadExits) == 0 || len(c.Rules) == 0 {
				t.Fatal("a hook case names its rule and the exit statuses of the failing upload")
			}
			w := newAutoPublishWorld(t, fixture, schema.VisibilityPrivate, "")
			w.writeRules(t, c)
			w.village.Close()
			// The upload a push runs counts a session it could not send as an
			// error in its result line, and publishes nothing.
			if stdout, stderr, _ := w.push(t); !strings.Contains(stdout, "pushed 0 session(s), 1 error(s)") || len(w.village.Publishes()) != 0 {
				t.Fatalf("the upload under the rule against a Village that is gone did not report its failure\n%s%s", stdout, stderr)
			}

			db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(w.dir)))
			if err != nil {
				t.Fatal(err)
			}
			recorded, err := autopublish.Recorded(t.Context(), db, &ingest.ExecGitResolver{})
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			hooks := autopublish.Hooks{Lifecycle: githooks.New(githooks.NewExecGit()), Binding: githooks.Binding{ConfigDir: w.dir, DataDir: w.dir, StateDir: w.dir}}
			installed, err := hooks.Install(t.Context(), c.RulesFor(w.world, autoPublishMatch)[0], w.repo, recorded)
			if err != nil || installed.Hooks[0].Status != schema.AutoPublishHookInstalled {
				t.Fatalf("install = %+v, %v", installed, err)
			}

			remote := filepath.Join(w.world, "remote.git")
			hooksGit(t, w.world, "", "init", "--quiet", "--bare", remote)
			hooksGit(t, w.repo, "", "remote", "add", "backup", remote)
			for i, exit := range c.UploadExits {
				binDir, logPath := hooksStubPeasant(t, exit)
				hooksGit(t, w.repo, "", "-c", "user.name=dev", "-c", "user.email=dev@example.test", "commit", "--quiet", "--allow-empty", "-m", "change "+strconv.Itoa(i))
				// hooksGit fails the test when git exits non-zero, so reaching
				// the next line is the push succeeding.
				output := hooksGit(t, w.repo, binDir, "push", "--quiet", "backup", "HEAD:refs/heads/main")
				head := strings.TrimSpace(hooksGit(t, w.repo, "", "rev-parse", "HEAD"))
				if pushed := strings.TrimSpace(hooksGit(t, remote, "", "rev-parse", "main")); pushed != head {
					t.Fatalf("upload exit %d: the remote holds %s, want %s", exit, pushed, head)
				}
				invocations, err := os.ReadFile(logPath)
				if err != nil || !strings.Contains(string(invocations), "push\n--non-interactive") || !strings.Contains(string(invocations), w.repo) {
					t.Fatalf("upload exit %d: the hook did not run the upload for the repository: %q, %v", exit, invocations, err)
				}
				if !strings.Contains(output, "carries on") && !strings.Contains(output, "unaffected") {
					t.Errorf("upload exit %d: the hook printed no warning that the push carries on:\n%s", exit, output)
				}
			}
		})
	}
}

// TestVillageAuto runs the village-auto cases: one command saves a rule for
// the repository with the collectives the developer published to last, read
// from the Village, installs its hook, and prints one line.
func TestVillageAuto(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverVillageAuto) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			w := newAutoPublishWorld(t, fixture, schema.VisibilityPrivate, "")
			if c.Published {
				if stdout, stderr, err := w.run(t, BuildPushCommand(), "--config", w.cfgPath, "--non-interactive", "--quiet", "--repository", w.repo); err != nil {
					t.Fatalf("publish the session first: %v\n%s%s", err, stdout, stderr)
				}
				for alias, status := range c.Shared {
					w.village.Share(autoPublishSessionID, fixture.Collectives[alias].ID, status)
				}
			}
			// The rules are written after the publish, so the audience the
			// command reads is the one the case shares, not a rule's.
			if len(c.Rules) > 0 {
				w.writeRules(t, c)
			}
			target := w.repo
			if c.Unrecorded {
				target = w.fresh
			}
			if c.ForeignHook != "" {
				if err := os.WriteFile(filepath.Join(target, ".git", "hooks", "pre-push"), []byte(c.ForeignHook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(w.rulesPath())

			stdout, stderr, err := w.run(t, BuildVillageCommand(), "auto", "--dir", target)
			assertErrorContains(t, err, c.Expect.ErrorContains, stdout+stderr)
			if c.Expect.Output != "" && stdout != c.Expect.Output {
				t.Errorf("stdout = %q, want %q", stdout, c.Expect.Output)
			}
			if err != nil && stdout != "" {
				t.Errorf("a failed run printed %q; the one line is the success line", stdout)
			}

			rules, loadErr := autopublish.Load(w.rulesPath())
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if c.Expect.Rule == nil {
				if after, _ := os.ReadFile(w.rulesPath()); !bytes.Equal(before, after) {
					t.Errorf("hooks.yaml changed; nothing may change:\n%s", after)
				}
			} else {
				want := *c.Expect.Rule
				want.Match = strings.ReplaceAll(want.Match, "{remote}", autoPublishMatch)
				if len(rules) != 1 || rules[0].Kind != want.Kind || rules[0].Match != want.Match || !slices.Equal(rules[0].Events, want.Events) || !slices.Equal(rules[0].Collectives, fixture.IDs(want.Collectives)) {
					t.Errorf("hooks.yaml = %+v, want the one rule %+v", rules, want)
				}
			}
			for _, name := range []string{"recorded", "unrecorded"} {
				repo := map[string]string{"recorded": w.repo, "unrecorded": w.fresh}[name]
				if got := w.managedPrePush(t, repo); got != slices.Contains(c.Expect.Installed, name) {
					t.Errorf("%s repository holds a managed pre-push hook = %v", name, got)
				}
			}
			if c.ForeignHook != "" {
				if raw, _ := os.ReadFile(filepath.Join(target, ".git", "hooks", "pre-push")); string(raw) != c.ForeignHook {
					t.Error("the hook Peasant did not write was changed")
				}
			}
		})
	}
}
