package ingest_test

import (
	_ "embed"
	"os/exec"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/git_remote_attribution.yaml
var gitRemoteAttributionData []byte

func TestResolveGitRemoteAttribution(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name            string `yaml:"name"`
			RecordedBranch  string `yaml:"recordedBranch"`
			ExplicitRemote  string `yaml:"explicitRemote"`
			WantRemote      string `yaml:"wantRemote"`
			WantTracking    string `yaml:"wantTracking"`
			TrackedCheckout bool   `yaml:"trackedCheckout"`
			RemoveOrigin    bool   `yaml:"removeOrigin"`
			LocalUpstream   bool   `yaml:"localUpstream"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(gitRemoteAttributionData, &fixtures); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("git attribution", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			repo := t.TempDir()
			run := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			run("init", "--quiet", "--initial-branch=main")
			run("-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.com", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "initial")
			run("remote", "add", "origin", "git@github.com:testuser/testrepo.git")
			run("remote", "add", "canonical", "https://github.com/example/live.git")
			run("branch", "recorded")
			run("branch", "untracked")
			run("config", "branch.recorded.remote", "canonical")
			run("config", "branch.recorded.merge", "refs/heads/develop")
			if c.LocalUpstream {
				run("remote", "set-url", "canonical", repo+"/mirror.git")
			}
			if c.TrackedCheckout {
				run("config", "branch.main.remote", "canonical")
				run("config", "branch.main.merge", "refs/heads/main")
			}
			if c.RemoveOrigin {
				run("remote", "remove", "origin")
			}
			remote, tracking := ingest.ResolveGitRemote(t.Context(), &ingest.ExecGitResolver{}, repo, c.RecordedBranch, c.ExplicitRemote)
			if remote != c.WantRemote || tracking != c.WantTracking {
				t.Fatalf("got (%q, %q), want (%q, %q)", remote, tracking, c.WantRemote, c.WantTracking)
			}
		})
	}
}
