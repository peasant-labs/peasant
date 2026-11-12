package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/git_branch_attribution.yaml
var gitBranchAttributionData []byte

// checkoutCountingGit counts the questions asked about the checked-out branch
// (calls) and every question asked of git at all (all).
type checkoutCountingGit struct {
	*testutil.StubGitResolver
	calls int
	all   int
	// dirs lists the directory of every question, in the order they were asked.
	dirs []string
}

func (g *checkoutCountingGit) ask(dir string) {
	g.all++
	g.dirs = append(g.dirs, dir)
}

var _ ingest.RecordedBranchRemoteResolver = (*checkoutCountingGit)(nil)

func (g *checkoutCountingGit) Branch(ctx context.Context, dir string) (string, error) {
	g.calls++
	g.ask(dir)
	return g.StubGitResolver.Branch(ctx, dir)
}

func (g *checkoutCountingGit) RemoteURL(ctx context.Context, dir string) (string, error) {
	g.ask(dir)
	return g.StubGitResolver.RemoteURL(ctx, dir)
}

func (g *checkoutCountingGit) OriginRemoteURL(ctx context.Context, dir string) (string, error) {
	g.ask(dir)
	return g.StubGitResolver.OriginRemoteURL(ctx, dir)
}

func (g *checkoutCountingGit) RemoteURLForBranch(ctx context.Context, dir, branch string) (string, string, error) {
	g.ask(dir)
	return g.StubGitResolver.RemoteURLForBranch(ctx, dir, branch)
}

func (g *checkoutCountingGit) Worktree(ctx context.Context, dir string) (string, error) {
	g.ask(dir)
	return g.StubGitResolver.Worktree(ctx, dir)
}

func (g *checkoutCountingGit) TrackingBranch(ctx context.Context, dir string) (string, error) {
	g.ask(dir)
	return g.StubGitResolver.TrackingBranch(ctx, dir)
}

func (g *checkoutCountingGit) WalkUpRemoteURL(ctx context.Context, dir string) (string, string, error) {
	g.ask(dir)
	return g.StubGitResolver.WalkUpRemoteURL(ctx, dir)
}

func TestResolveGitBranchAttribution(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name              string `yaml:"name"`
			Dir               string `yaml:"dir"`
			RecordedBranch    string `yaml:"recordedBranch"`
			Checkout          string `yaml:"checkout"`
			CheckoutFails     bool   `yaml:"checkoutFails"`
			NoResolver        bool   `yaml:"noResolver"`
			WantBranch        string `yaml:"wantBranch"`
			WantCheckoutCalls int    `yaml:"wantCheckoutCalls"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(gitBranchAttributionData, &fixtures); err != nil {
		t.Fatalf("decode git branch attribution fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/ingest/testdata/git_branch_attribution.yaml", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			stub := &testutil.StubGitResolver{BranchName: c.Checkout}
			if c.CheckoutFails {
				stub.BranchErr = errors.New("not a git repository")
			}
			git := &checkoutCountingGit{StubGitResolver: stub}
			var resolver ingest.GitResolver = git
			if c.NoResolver {
				resolver = nil
			}
			got := ingest.ResolveGitBranch(t.Context(), resolver, c.Dir, c.RecordedBranch)
			if got != c.WantBranch {
				t.Fatalf("branch = %q, want %q", got, c.WantBranch)
			}
			if git.calls != c.WantCheckoutCalls {
				t.Fatalf("checked-out branch asked %d times, want %d", git.calls, c.WantCheckoutCalls)
			}
		})
	}
}
