package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
)

func TestNewOpenCodeAdapterRetainsCapableCandidateConstructionFailure(t *testing.T) {
	t.Parallel()
	filesystem := &OSFileSystem{}
	adapter := newOpenCodeAdapter(filesystem, noGitResolver{}, salt.Salt{}, filesystem, true, "latest", fixedOpenCodeAdapterEnvironment{}, nil, DefaultOpenCodeSQLiteSourceOptions())
	root, err := NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	_, discoverErr := adapter.Discover(context.Background(), SourceConfig{Enabled: true, Paths: []ResolvedPath{root}})
	if discoverErr == nil {
		t.Fatal("capable adapter construction failure silently downgraded discovery to legacy-only")
	}
	for _, phrase := range []string{"candidate-probe construction failed", "where:", "impact:", "fix:"} {
		if !strings.Contains(discoverErr.Error(), phrase) {
			t.Errorf("candidate construction diagnostic %q does not explain %q", discoverErr, phrase)
		}
	}
}

type fixedOpenCodeAdapterEnvironment struct{}

func (fixedOpenCodeAdapterEnvironment) LookupEnv(string) (string, bool) { return "", false }

type noGitResolver struct{}

func (noGitResolver) Branch(context.Context, string) (string, error)         { return "", nil }
func (noGitResolver) RemoteURL(context.Context, string) (string, error)      { return "", nil }
func (noGitResolver) Worktree(context.Context, string) (string, error)       { return "", nil }
func (noGitResolver) TrackingBranch(context.Context, string) (string, error) { return "", nil }
func (noGitResolver) UserEmail(context.Context) (string, error)              { return "", nil }
func (noGitResolver) WalkUpRemoteURL(context.Context, string) (string, string, error) {
	return "", "", nil
}
