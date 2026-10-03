package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/autopublish"
	"github.com/peasant-labs/peasant/internal/autopublish/autopublishtest"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The auto-publish world's sessions. The first is recorded in the world's
// repository; each extra session in the place its role names; the later ones
// are published after the first, in order. Each transcript on the Village
// double has its session's ID.
const autoPublishSessionID = "abcd1234-abcd-4bcd-8bcd-abcdef123456"

var autoPublishExtraIDs = map[autopublishtest.SessionRole]string{
	"clone":             "abcd1234-abcd-4bcd-8bcd-abcdef120000",
	"gone":              "abcd1234-abcd-4bcd-8bcd-abcdef120010",
	"linked":            "abcd1234-abcd-4bcd-8bcd-abcdef120020",
	"unrelated":         "abcd1234-abcd-4bcd-8bcd-abcdef120030",
	"gone-subfolder":    "abcd1234-abcd-4bcd-8bcd-abcdef120040",
	"empty-alpha":       "abcd1234-abcd-4bcd-8bcd-abcdef120050",
	"empty-beta":        "abcd1234-abcd-4bcd-8bcd-abcdef120051",
	"gone-shared-alpha": "abcd1234-abcd-4bcd-8bcd-abcdef120052",
	"gone-shared-beta":  "abcd1234-abcd-4bcd-8bcd-abcdef120053",
}

// autoPublishLaterIDs are the sessions a later publication publishes, in order.
var autoPublishLaterIDs = []string{"abcd1234-abcd-4bcd-8bcd-abcdef120001", "abcd1234-abcd-4bcd-8bcd-abcdef120002"}

// autoPublishRemote is the world's origin, and autoPublishMatch its bare form,
// which a rule for exactly that remote uses. The alpha and beta remotes belong
// to the sessions that recorded no directory, or one shared gone directory, so
// a case can hold two sessions whose recorded path alone does not tell them
// apart.
const (
	autoPublishRemote      = "https://github.com/acme/tools.git"
	autoPublishMatch       = "github.com/acme/tools"
	autoPublishAlphaRemote = "https://github.com/acme/alpha.git"
	autoPublishBetaRemote  = "https://github.com/acme/beta.git"
)

// autoPublishExtraStartMs is the newest start time of an extra session. A case
// lists its sessions newest first, so the first role of the list is the first
// candidate the push matches, and listing the same two roles in the other order
// exercises the other candidate order.
const autoPublishExtraStartMs = 1700000050000

// autoPublishWorld is a recorded repository with one ready session, a second
// clone of the same remote, a Village double that knows the fixture's
// collectives, and a computer signed in to it. dir holds the config, data, and
// state roots every command runs with.
type autoPublishWorld struct {
	fixture  autopublishtest.Fixture
	dir      string
	world    string
	repo     string
	fresh    string
	remote   bool
	village  *testutil.CollectiveVillage
	cfgPath  string
	sessions []string
}

func newAutoPublishWorld(t *testing.T, fixture autopublishtest.Fixture, c autopublishtest.Case) *autoPublishWorld {
	t.Helper()
	world, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &autoPublishWorld{fixture: fixture, dir: t.TempDir(), world: world, repo: filepath.Join(world, "tools"), fresh: filepath.Join(world, "fresh"), remote: !c.NoRemote}
	for _, repo := range []string{w.repo, w.fresh} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
		hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")
		if w.remote {
			hooksGit(t, repo, "", "remote", "add", "origin", autoPublishRemote)
		}
	}
	collectives := make([]testutil.VillageCollective, 0, len(fixture.Collectives))
	for _, alias := range []string{"platform", "research", "review"} {
		collective := fixture.Collectives[alias]
		collectives = append(collectives, testutil.VillageCollective{ID: collective.ID, Name: collective.Name, Acceptance: collective.Acceptance, Member: true})
	}
	w.village = testutil.NewCollectiveVillage(t, collectives...)
	writeTestCredentialsFor(t, w.dir, w.village.URL())

	w.seed(t, autoPublishSessionID, w.repo)
	for order, role := range c.Sessions {
		w.seedExtra(t, role, order)
	}
	body := "version: 1\noutput:\n  basePath: " + filepath.Join(w.dir, "peasant-sync") + "\npush:\n  method: all\n"
	if c.Config.Visibility != "" {
		body += "  visibility: " + string(c.Config.Visibility) + "\n"
	}
	if c.Config.License != "" {
		body += "  license: " + string(c.Config.License) + "\n"
	}
	if c.Selected {
		body += "selection:\n  mode: selected\n  harnesses:\n    claude-code:\n      sessions: [" + autoPublishSessionID + "]\n"
	}
	w.cfgPath = writeCfg(t, w.dir, "auto-publish.yaml", body)
	for i := 0; i < c.Historical; i++ {
		w.seed(t, fmt.Sprintf("abcd1234-abcd-4bcd-8bcd-%012d", 1000+i), w.repo)
	}
	return w
}

// seedExtra records the extra session of one role: in another clone, in a
// clone that is then removed, in a linked worktree inside the repository, or
// with no directory at all. order is the session's position in the case's
// list, newest first, which fixes the candidate order a case exercises.
func (w *autoPublishWorld) seedExtra(t *testing.T, role autopublishtest.SessionRole, order int) {
	t.Helper()
	switch role {
	case "gone-subfolder":
		sub := filepath.Join(w.repo, "services")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		w.seed(t, autoPublishExtraIDs[role], sub)
		if err := os.RemoveAll(sub); err != nil {
			t.Fatal(err)
		}
	case "unrelated":
		hooksGit(t, w.fresh, "", "remote", "set-url", "origin", "https://github.com/other/notes.git")
		w.seed(t, autoPublishExtraIDs[role], w.fresh)
	case "clone":
		w.seed(t, autoPublishExtraIDs[role], w.fresh)
	case "gone":
		gone := filepath.Join(w.world, "gone")
		if err := os.MkdirAll(gone, 0o700); err != nil {
			t.Fatal(err)
		}
		hooksGit(t, gone, "", "init", "--quiet", "--initial-branch=main")
		if w.remote {
			hooksGit(t, gone, "", "remote", "add", "origin", autoPublishRemote)
		}
		w.seed(t, autoPublishExtraIDs[role], gone)
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
	case "linked":
		linked := filepath.Join(w.repo, "feat")
		hooksGit(t, w.repo, "", "-c", "user.name=dev", "-c", "user.email=dev@example.test", "commit", "--quiet", "--allow-empty", "-m", "start")
		hooksGit(t, w.repo, "", "worktree", "add", "--quiet", "-b", "feat", linked)
		w.seed(t, autoPublishExtraIDs[role], linked)
	case autopublishtest.SessionEmptyAlpha, autopublishtest.SessionEmptyBeta:
		w.seedRecorded(t, autoPublishExtraIDs[role], "", autoPublishRemoteFor(role), autoPublishExtraStartMs-int64(order)*10000)
	case autopublishtest.SessionGoneSharedAlpha, autopublishtest.SessionGoneSharedBeta:
		shared := filepath.Join(w.world, "gone-shared")
		if err := os.MkdirAll(shared, 0o700); err != nil {
			t.Fatal(err)
		}
		w.seedRecorded(t, autoPublishExtraIDs[role], shared, autoPublishRemoteFor(role), autoPublishExtraStartMs-int64(order)*10000)
		if err := os.RemoveAll(shared); err != nil {
			t.Fatal(err)
		}
	}
}

// autoPublishRemoteFor is the recorded remote of the sessions whose path alone
// does not tell them apart.
func autoPublishRemoteFor(role autopublishtest.SessionRole) string {
	switch role {
	case autopublishtest.SessionEmptyAlpha, autopublishtest.SessionGoneSharedAlpha:
		return autoPublishAlphaRemote
	default:
		return autoPublishBetaRemote
	}
}

// seedRecorded records one ready session in dir with an explicit recorded
// remote, so a case can hold two sessions whose recorded directory is equal
// and whose repository therefore differs only by the remote.
func (w *autoPublishWorld) seedRecorded(t *testing.T, sessionID, dir, remote string, startMs int64) {
	t.Helper()
	db := w.openStore(t)
	defer db.Close()
	hash, _, err := ingest.DeriveProjectIdentifiers(db.InstallationSalt(), remote, dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := makeCmdStoreEntry(t, sessionID, "github.com-acme-tools", remote, "main", startMs, dir)
	entry.Metadata.Project.Hash = hash
	testutil.SeedReadyPublication(t, db, entry.Metadata, nil)
	w.sessions = append(w.sessions, sessionID)
}

// seed records one ready session in dir, under the project identity a push
// scoped to dir derives.
func (w *autoPublishWorld) seed(t *testing.T, sessionID, dir string) {
	t.Helper()
	db := w.openStore(t)
	defer db.Close()
	hash, _, err := ingest.DeriveProjectIdentifiersWithGit(t.Context(), db.InstallationSalt(), &ingest.ExecGitResolver{}, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	remote := ""
	if w.remote {
		remote, _ = (&ingest.ExecGitResolver{}).RemoteURL(t.Context(), dir)
	}
	entry := makeCmdStoreEntry(t, sessionID, "github.com-acme-tools", remote, "main", 1700000000000, dir)
	entry.Metadata.Project.Hash = hash
	testutil.SeedReadyPublication(t, db, entry.Metadata, nil)
	w.sessions = append(w.sessions, sessionID)
}

func (w *autoPublishWorld) openStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := string(defaults.ResolveDBFilePathWith(w.dir))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// rulesPath is the hooks.yaml every command of the world reads.
func (w *autoPublishWorld) rulesPath() string {
	return autopublish.Path(defaults.ResolveConfigDirPathWith(w.dir))
}

// binding is what a hook installed by a command of the world binds.
func (w *autoPublishWorld) binding() githooks.Binding {
	return githooks.Binding{ConfigDir: w.dir, DataDir: w.dir, StateDir: w.dir, RequireAutoPublishRule: true, AutoPublishEvent: githooks.EventPrePush}
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

// push runs the upload a managed hook runs for the world's repository, or an
// unscoped push. It is quiet, as a hook's is, unless the flags ask for
// --verbose.
func (w *autoPublishWorld) push(t *testing.T, scoped bool, flags ...string) (string, string, error) {
	t.Helper()
	args := []string{"--config", w.cfgPath, "--non-interactive"}
	if !slices.Contains(flags, "--verbose") {
		args = append(args, "--quiet")
	}
	if scoped {
		args = append(args, "--repository", w.repo)
	}
	return w.run(t, BuildPushCommand(), append(args, flags...)...)
}

// audience is who can read the session's transcript, by collective alias.
func (w *autoPublishWorld) audience(sessionID string) map[string]schema.VillageShareStatus {
	audience := map[string]schema.VillageShareStatus{}
	for id, status := range w.village.Audience(schema.TranscriptID(sessionID)) {
		audience[w.fixture.Alias(id)] = status
	}
	return audience
}

// share records the shares on Village, as a collective owner or the
// developer would make them.
func (w *autoPublishWorld) share(sessionID string, shares map[string]schema.VillageShareStatus) {
	for alias, status := range shares {
		w.village.Share(schema.TranscriptID(sessionID), w.fixture.Collectives[alias].ID, status)
	}
}

func (w *autoPublishWorld) managedPrePush(t *testing.T, repo string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "pre-push"))
	if err != nil || !githooks.IsManaged(raw) {
		return nil
	}
	return raw
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

func assertAudience(t *testing.T, label string, got, want map[string]schema.VillageShareStatus) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s audience = %v, want %v", label, got, want)
	}
	for alias, status := range want {
		if got[alias] != status {
			t.Errorf("%s %s share = %q, want %q", label, alias, got[alias], status)
		}
	}
}

// TestAutoPublishPush runs the push cases of
// internal/autopublish/testdata/auto-publish-rules.yaml: a push that sends a
// session a rule binds publishes private with no license and shares each
// transcript it sent with its rule's collectives, and a push no rule binds
// publishes as it always did.
func TestAutoPublishPush(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverPush) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.Expect.Publishes == nil || c.Expect.Audience == nil || c.Expect.Public == nil || c.Expect.VisibilityIntent == "" {
				t.Fatal("a push case states its publishes, visibility and audience ({} for none)")
			}
			w := newAutoPublishWorld(t, fixture, c)
			scoped := !c.Unscoped
			var output strings.Builder
			if c.Before {
				stdout, stderr, err := w.push(t, scoped)
				if err != nil {
					t.Fatalf("the push before the rules failed: %v\n%s%s", err, stdout, stderr)
				}
				w.village.SetPublic(autoPublishSessionID, c.VillagePublic)
			}
			w.writeRules(t, c)
			w.village.FailTranscriptRead(c.FailTranscriptRead)
			w.village.FailShareRead(c.FailShareRead)
			for _, alias := range c.FailShare {
				w.village.FailShare(fixture.Collectives[alias].ID)
			}
			if c.StallShare {
				w.village.StallShare(time.Minute)
			}
			stdout, stderr, err := w.push(t, scoped, c.Flags...)
			output.WriteString(stdout + stderr)
			if c.Again != nil {
				if err != nil {
					t.Fatalf("the first push failed: %v\n%s", err, output.String())
				}
				if c.DeleteRule != "" {
					if err := autopublish.Update(w.rulesPath(), func(rules []autopublish.Rule) ([]autopublish.Rule, error) {
						return slices.DeleteFunc(rules, func(rule autopublish.Rule) bool { return rule.ID == c.DeleteRule }), nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				w.share(autoPublishSessionID, c.VillageDecides)
				if c.PrivateBeforeAgain {
					w.village.SetPublic(autoPublishSessionID, false)
				}
				stdout, stderr, err = w.push(t, scoped, c.Again...)
				output.WriteString(stdout + stderr)
			}
			assertErrorContains(t, err, c.Expect.ErrorContains, output.String())
			for _, part := range c.Expect.OutputContains {
				if !strings.Contains(output.String(), part) {
					t.Errorf("the output does not say %q:\n%s", part, output.String())
				}
			}

			for _, part := range c.Expect.OutputOmits {
				if strings.Contains(output.String(), part) {
					t.Errorf("output must omit %q: %s", part, output.String())
				}
			}
			if c.Expect.Reads != nil && w.village.TranscriptReads() != *c.Expect.Reads {
				t.Errorf("transcript reads = %d, want %d", w.village.TranscriptReads(), *c.Expect.Reads)
			}
			publishes := w.village.Publishes()
			if len(publishes) != *c.Expect.Publishes {
				t.Fatalf("Village received %d uploads, want %d\n%s", len(publishes), *c.Expect.Publishes, output.String())
			}
			for _, publish := range publishes[boolIndex(c.Before):] {
				if publish.VisibilityIntent != c.Expect.VisibilityIntent {
					t.Errorf("an upload carried visibility %q, want %q", publish.VisibilityIntent, c.Expect.VisibilityIntent)
				}
				if publish.License != c.Expect.License {
					t.Errorf("an upload carried license %q, want %q", publish.License, c.Expect.License)
				}
			}
			if got := w.village.Public(autoPublishSessionID); got != *c.Expect.Public {
				t.Errorf("Village public audience = %t, want %t", got, *c.Expect.Public)
			}
			if got := w.village.OwnerVisibilities(); !slices.Equal(got, c.Expect.OwnerVisibilities) {
				t.Errorf("requested owner visibilities = %v, want %v", got, c.Expect.OwnerVisibilities)
			}
			if got := w.village.OwnerUpdates(); got != c.Expect.OwnerUpdates {
				t.Errorf("owner updates = %d, want %d", got, c.Expect.OwnerUpdates)
			}
			assertAudience(t, "the session's", w.audience(autoPublishSessionID), c.Expect.Audience)
			if len(c.Expect.Others) != len(c.Sessions) {
				t.Fatalf("a push case states the audience of each extra session: %v for %v", c.Expect.Others, c.Sessions)
			}
			for _, role := range c.Sessions {
				assertAudience(t, "the "+string(role)+" session's", w.audience(autoPublishExtraIDs[role]), c.Expect.Others[role])
			}
			if c.Expect.AttemptContains != "" {
				assertLatestAttempt(t, w, autoPublishSessionID, c.Expect.AttemptContains)
			}
		})
	}
}

// boolIndex is 1 for true: the uploads made before the rules are not the
// case's.
func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

func assertLatestAttempt(t *testing.T, w *autoPublishWorld, sessionID, want string) {
	t.Helper()
	db := w.openStore(t)
	defer db.Close()
	creds, err := auth.LoadCredentialsFrom(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := db.SessionPublicationAttempts(t.Context(), creds.VillageURL, creds.UserID, []string{sessionID})
	if err != nil || !strings.Contains(attempts[sessionID].Message, want) {
		t.Errorf("latest failed attempt = %+v, %v; want one saying %q", attempts[sessionID], err, want)
	}
}

// publish publishes the world's first session with no rule and shares it as
// the fixture says.
func (w *autoPublishWorld) publish(t *testing.T, publications []autopublishtest.Publication) {
	t.Helper()
	for i, publication := range publications {
		sessionID := autoPublishSessionID
		if i > 0 {
			sessionID = autoPublishLaterIDs[i-1]
			w.seed(t, sessionID, w.repo)
		}
		if stdout, stderr, err := w.push(t, true); err != nil {
			t.Fatalf("publish session %d: %v\n%s%s", i, err, stdout, stderr)
		}
		w.share(sessionID, publication.Shared)
	}
}

// TestAutoPublishHookNeverBlocksThePush runs the hook cases: `peasant village
// auto` installs the rule's hook, and the hook runs through a real git push,
// against a Village that is gone, with an upload that exits with each status
// the upload exits with. Every push reaches its remote, and the hook runs
// exactly the upload bound to the command's directories.
func TestAutoPublishHookNeverBlocksThePush(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverHook) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if len(c.UploadExits) == 0 {
				t.Fatal("a hook case names the exit statuses of the failing upload")
			}
			w := newAutoPublishWorld(t, fixture, c)
			w.publish(t, []autopublishtest.Publication{{Shared: map[string]schema.VillageShareStatus{"platform": schema.VillageShareStatusApproved}}})
			if stdout, stderr, err := w.run(t, BuildVillageCommand(), "auto", "--dir", w.repo); err != nil {
				t.Fatalf("village auto: %v\n%s%s", err, stdout, stderr)
			}

			remote := filepath.Join(w.world, "remote.git")
			hooksGit(t, w.world, "", "init", "--quiet", "--bare", remote)
			hooksGit(t, w.repo, "", "remote", "add", "backup", remote)
			want := strings.Join(githooks.RepositoryArgv(w.repo, w.binding())[1:], "\n") + "\n"
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
				if invocations, err := os.ReadFile(logPath); err != nil || string(invocations) != want {
					t.Fatalf("upload exit %d: the hook ran\n%s\nwant\n%s(%v)", exit, invocations, want, err)
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
// from the Village, installs its hook bound to the command's directories, and
// prints one line naming what the repository now publishes to.
func TestVillageAuto(t *testing.T) {
	t.Parallel()
	fixture := autopublishtest.Load(t)
	for _, c := range fixture.For(t, autopublishtest.DriverVillageAuto) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			w := newAutoPublishWorld(t, fixture, c)
			w.publish(t, c.Publications)
			w.village.FailTranscriptRead(c.FailTranscriptRead)
			// The rules are written after the publications, so the audience
			// the command reads is the one the case shares, not a rule's.
			if len(c.Rules) > 0 {
				w.writeRules(t, c)
			}
			if c.ForkUpstream {
				hooksGit(t, w.repo, "", "-c", "user.name=dev", "-c", "user.email=dev@example.test", "commit", "--quiet", "--allow-empty", "-m", "start")
				hooksGit(t, w.repo, "", "remote", "add", "fork", "https://github.com/dev/tools.git")
				hooksGit(t, w.repo, "", "config", "branch.main.remote", "fork")
				hooksGit(t, w.repo, "", "config", "branch.main.merge", "refs/heads/main")
				repo, resolveErr := autopublish.Resolve(t.Context(), &ingest.ExecGitResolver{}, w.repo, true)
				if resolveErr != nil || repo.Remote == repo.Origin {
					t.Fatalf("fork setup must have distinct origin and upstream: %+v, %v", repo, resolveErr)
				}
			}
			if c.HooksPath != "" {
				hooksGit(t, w.repo, "", "config", "core.hooksPath", c.HooksPath)
			}
			if c.SignedOut {
				if err := os.Remove(filepath.Join(string(defaults.ResolveConfigDirPathWith(w.dir)), string(defaults.CredentialsFile))); err != nil {
					t.Fatal(err)
				}
			}
			target := w.repo
			if c.Unrecorded {
				target = w.fresh
			}
			foreign := c.ForeignHook
			if c.HandAdded {
				slot := filepath.Join(target, ".git", "hooks", "pre-push")
				section, err := githooks.ManualSnippet(githooks.EventPrePush, target, slot, w.binding())
				if err != nil {
					t.Fatal(err)
				}
				foreign = "#!/bin/sh\n" + section
			}
			if foreign != "" {
				if err := os.WriteFile(filepath.Join(target, ".git", "hooks", "pre-push"), []byte(foreign), 0o755); err != nil {
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
			for _, part := range c.Expect.OutputContains {
				if !strings.Contains(stderr, part) {
					t.Errorf("standard error does not say %q:\n%s", part, stderr)
				}
			}

			rules, loadErr := autopublish.Load(w.rulesPath())
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if c.Expect.RuleIDs != nil {
				ids := make([]string, len(rules))
				for i, rule := range rules {
					ids[i] = rule.ID
				}
				if !slices.Equal(ids, c.Expect.RuleIDs) {
					t.Errorf("hooks.yaml rule ids = %v, want %v", ids, c.Expect.RuleIDs)
				}
			}
			if c.Expect.Rule != nil {
				want := *c.Expect.Rule
				want.Match = strings.ReplaceAll(want.Match, "{remote}", autoPublishMatch)
				if len(rules) != 1 || rules[0].Kind != want.Kind || rules[0].Match != want.Match || !slices.Equal(rules[0].Events, want.Events) || !slices.Equal(rules[0].Collectives, fixture.IDs(want.Collectives)) {
					t.Errorf("hooks.yaml = %+v, want the one rule %+v", rules, want)
				}
			} else if err != nil {
				if after, _ := os.ReadFile(w.rulesPath()); !bytes.Equal(before, after) {
					t.Errorf("hooks.yaml changed; a refusal changes nothing:\n%s", after)
				}
			}
			for name, repo := range map[string]string{"recorded": w.repo, "unrecorded": w.fresh} {
				if got := w.managedPrePush(t, repo) != nil; got != slices.Contains(c.Expect.Installed, autopublishtest.RepositoryRole(name)) {
					t.Errorf("%s repository holds a managed pre-push hook = %v", name, got)
				}
			}
			if c.Expect.Binding {
				embedded := githooks.EmbeddedCommand(w.managedPrePush(t, w.repo))
				if want := githooks.RepositoryCommand(w.repo, w.binding()); embedded != want {
					t.Errorf("the hook runs %q, want %q: it must read this command's rules, config, and store", embedded, want)
				}
			}
			if foreign != "" {
				if raw, _ := os.ReadFile(filepath.Join(target, ".git", "hooks", "pre-push")); string(raw) != foreign {
					t.Error("the hook Peasant did not write was changed")
				}
			}
		})
	}
}
