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

// remoteRecordingGit remembers every directory it was asked for a remote.
type remoteRecordingGit struct {
	*testutil.StubGitResolver
	asked []string
}

func (g *remoteRecordingGit) RemoteURL(ctx context.Context, dir string) (string, error) {
	g.asked = append(g.asked, dir)
	return g.StubGitResolver.RemoteURL(ctx, dir)
}

// TestSessionRemoteForPrecedence holds the one function the tree and the import
// filter both call to the precedence it documents. Both surfaces calling it is
// held by the discovery agreement test.
func TestSessionRemoteForPrecedence(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string   `yaml:"name"`
			CWD          string   `yaml:"cwd"`
			SourcePath   string   `yaml:"sourcePath"`
			OriginalRoot string   `yaml:"originalRoot"`
			StoredRemote string   `yaml:"storedRemote"`
			StoredKnown  bool     `yaml:"storedKnown"`
			Remote       string   `yaml:"remote"`
			RemoteFails  bool     `yaml:"remoteFails"`
			NoResolver   bool     `yaml:"noResolver"`
			WantRemote   string   `yaml:"wantRemote"`
			AskedDirs    []string `yaml:"askedDirs"`
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
			git := &remoteRecordingGit{StubGitResolver: stub}
			var resolver ingest.GitResolver = git
			if c.NoResolver {
				resolver = nil
			}
			session := ingest.DiscoveredSession{
				Harness:      ingest.HarnessCursor,
				CWD:          c.CWD,
				SourcePath:   ingest.ResolvedPath(c.SourcePath),
				OriginalRoot: ingest.ResolvedPath(c.OriginalRoot),
			}
			got := ingest.SessionRemoteFor(t.Context(), resolver, session, ingest.KnownSession{GitRemote: c.StoredRemote}, c.StoredKnown)
			if got != c.WantRemote {
				t.Fatalf("remote = %q, want %q", got, c.WantRemote)
			}
			if !slices.Equal(git.asked, c.AskedDirs) {
				t.Fatalf("git was asked for the remote of %v, want %v", git.asked, c.AskedDirs)
			}
		})
	}
}
