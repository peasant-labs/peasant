package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_projection_branch_attribution.yaml
var openCodeProjectionBranchAttributionYAML []byte

// projectionBranchGit answers one checked-out branch and counts the questions.
type projectionBranchGit struct {
	semanticNoGit
	checkout string
	calls    int
}

func (g *projectionBranchGit) Branch(context.Context, string) (string, error) {
	g.calls++
	return g.checkout, nil
}

// TestOpenCodeManagedProjectionBranchAttribution drives the production metadata
// assembly of a managed OpenCode projection, whose branch rule lives apart from
// the other OpenCode path.
func TestOpenCodeManagedProjectionBranchAttribution(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name              string `yaml:"name"`
			CWD               string `yaml:"cwd"`
			ProjectWorktree   string `yaml:"projectWorktree"`
			RecordedBranch    string `yaml:"recordedBranch"`
			Checkout          string `yaml:"checkout"`
			WantBranch        string `yaml:"wantBranch"`
			WantCheckoutCalls int    `yaml:"wantCheckoutCalls"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeProjectionBranchAttributionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode opencode projection branch fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !present[name] {
			t.Fatalf("opencode projection branch fixture is missing required case %q", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			id, err := NewSessionID("ses_branchprojection123")
			if err != nil {
				t.Fatal(err)
			}
			git := &projectionBranchGit{checkout: c.Checkout}
			adapter := NewOpenCodeAdapter(&OSFileSystem{}, git, salt.Salt{}, WithOpenCodeEnvironment(fixedOpenCodeAdapterEnvironment{}))
			session := DiscoveredSession{
				SessionID:        id,
				Harness:          HarnessOpenCode,
				TranscriptOrigin: TranscriptOriginOpenCodeLegacySQLite,
				CWD:              c.CWD,
				ProjectWorktree:  c.ProjectWorktree,
				Branch:           c.RecordedBranch,
			}
			projection := openCodeLegacyProjection{Format: openCodeLegacyProjectionFormat, Version: openCodeLegacyProjectionVersion, SessionID: id.String()}
			metadata, err := adapter.metadataFromManagedProjection(t.Context(), session, projection)
			if err != nil {
				t.Fatalf("metadataFromManagedProjection: %v", err)
			}
			got := ""
			if metadata.Git.Branch != nil {
				got = *metadata.Git.Branch
			}
			if got != c.WantBranch {
				t.Fatalf("stored branch = %q, want %q", got, c.WantBranch)
			}
			if git.calls != c.WantCheckoutCalls {
				t.Fatalf("checked-out branch asked %d times, want %d", git.calls, c.WantCheckoutCalls)
			}
		})
	}
}
