package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/session_remote.yaml
var sessionRemoteData []byte

// remoteQuestionGit answers the three questions ResolveGitRemote asks about a
// remote, each from its own fixture value so the answers can differ: the
// checkout's remote and origin (remote), and the upstream of one branch
// (upstreamBranch tracks upstreamRemote). It remembers the directories it was
// asked about.
type remoteQuestionGit struct {
	*testutil.StubGitResolver
	origin         string
	upstreamBranch string
	upstreamRemote string
	asked          []string
}

var _ ingest.RecordedBranchRemoteResolver = (*remoteQuestionGit)(nil)

func (g *remoteQuestionGit) RemoteURL(ctx context.Context, dir string) (string, error) {
	g.asked = append(g.asked, dir)
	return g.StubGitResolver.RemoteURL(ctx, dir)
}

func (g *remoteQuestionGit) OriginRemoteURL(_ context.Context, dir string) (string, error) {
	g.asked = append(g.asked, dir)
	if g.StubGitResolver.RemoteErr != nil {
		return "", g.StubGitResolver.RemoteErr
	}
	return g.origin, nil
}

func (g *remoteQuestionGit) RemoteURLForBranch(_ context.Context, dir, branch string) (string, string, error) {
	g.asked = append(g.asked, dir)
	if g.upstreamRemote != "" && branch == g.upstreamBranch {
		return g.upstreamRemote, "upstream/" + branch, nil
	}
	return "", "", nil
}

// TestSessionRemoteForPrecedence holds the one function the tree and the import
// filter both call to the precedence it documents. Both surfaces calling it, and
// the extractors storing what it answers, is held by the discovery agreement
// test.
func TestSessionRemoteForPrecedence(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string   `yaml:"name"`
			CWD            string   `yaml:"cwd"`
			SourcePath     string   `yaml:"sourcePath"`
			OriginalRoot   string   `yaml:"originalRoot"`
			StoredRemote   string   `yaml:"storedRemote"`
			StoredKnown    bool     `yaml:"storedKnown"`
			RecordedBranch string   `yaml:"recordedBranch"`
			ExplicitRemote string   `yaml:"explicitRemote"`
			Remote         string   `yaml:"remote"`
			Origin         string   `yaml:"origin"`
			UpstreamBranch string   `yaml:"upstreamBranch"`
			UpstreamRemote string   `yaml:"upstreamRemote"`
			RemoteFails    bool     `yaml:"remoteFails"`
			NoResolver     bool     `yaml:"noResolver"`
			WantRemote     string   `yaml:"wantRemote"`
			AskedDirs      []string `yaml:"askedDirs"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(sessionRemoteData, &fixtures); err != nil {
		t.Fatalf("decode session remote fixture: %v", err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			stub := &testutil.StubGitResolver{Remote: c.Remote}
			if c.RemoteFails {
				stub.RemoteErr = errors.New("not a git repository")
			}
			origin := c.Origin
			if origin == "" {
				origin = c.Remote
			}
			git := &remoteQuestionGit{StubGitResolver: stub, origin: origin, upstreamBranch: c.UpstreamBranch, upstreamRemote: c.UpstreamRemote}
			var resolver ingest.GitResolver = git
			if c.NoResolver {
				resolver = nil
			}
			session := ingest.DiscoveredSession{
				Harness:        ingest.HarnessCursor,
				CWD:            c.CWD,
				SourcePath:     ingest.ResolvedPath(c.SourcePath),
				OriginalRoot:   ingest.ResolvedPath(c.OriginalRoot),
				Branch:         c.RecordedBranch,
				RecordedRemote: c.ExplicitRemote,
			}
			got := ingest.SessionRemoteFor(t.Context(), resolver, session, ingest.KnownSession{GitRemote: c.StoredRemote}, c.StoredKnown)
			if got != c.WantRemote {
				t.Fatalf("remote = %q, want %q", got, c.WantRemote)
			}
			if asked := slices.Compact(git.asked); !slices.Equal(asked, c.AskedDirs) {
				t.Fatalf("git was asked for the remote of %v, want %v", asked, c.AskedDirs)
			}
		})
	}
}
