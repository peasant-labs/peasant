package ingest_test

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/session_branch.yaml
var sessionBranchData []byte

//go:embed testdata/session_project_directory.yaml
var sessionProjectDirectoryData []byte

func TestResolveSessionBranchPrecedence(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name              string   `yaml:"name"`
			Harnesses         []string `yaml:"harnesses"`
			Directory         string   `yaml:"directory"`
			RecordedBranch    string   `yaml:"recordedBranch"`
			StoredBranch      string   `yaml:"storedBranch"`
			StoredKnown       bool     `yaml:"storedKnown"`
			Checkout          string   `yaml:"checkout"`
			WantBranch        string   `yaml:"wantBranch"`
			WantCheckoutCalls int      `yaml:"wantCheckoutCalls"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(sessionBranchData, &fixtures); err != nil {
		t.Fatalf("decode session branch fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/ingest/testdata/session_branch.yaml", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		harnesses := c.Harnesses
		if len(harnesses) == 0 {
			harnesses = []string{ingest.HarnessClaudeCode.String()}
		}
		for _, harness := range harnesses {
			t.Run(c.Name+"/"+harness, func(t *testing.T) {
				t.Parallel()
				git := &checkoutCountingGit{StubGitResolver: &testutil.StubGitResolver{BranchName: c.Checkout}}
				session := ingest.DiscoveredSession{Harness: ingest.Harness(harness), Branch: c.RecordedBranch, CWD: c.Directory}
				got := ingest.SessionBranchFor(t.Context(), git, session, ingest.KnownSession{Branch: c.StoredBranch}, c.StoredKnown)
				if got != c.WantBranch {
					t.Fatalf("branch = %q, want %q", got, c.WantBranch)
				}
				if git.calls != c.WantCheckoutCalls {
					t.Fatalf("checked-out branch asked %d times, want %d", git.calls, c.WantCheckoutCalls)
				}
			})
		}
	}
}

// TestHarnessBranchSourcesCoverTheAdapterRegistry holds the declaration to the
// registry it hangs off: every registered harness declares where its branch
// comes from, nothing else does, and no declaration is the unknown zero value. A
// harness added without a declaration fails here instead of silently getting the
// general rule.
func TestHarnessBranchSourcesCoverTheAdapterRegistry(t *testing.T) {
	t.Parallel()
	for harness := range ingest.DefaultAdapterRegistry {
		source, declared := ingest.HarnessBranchSources[harness]
		if !declared {
			t.Errorf("registered harness %q declares no branch source; add it to ingest.HarnessBranchSources", harness)
			continue
		}
		if source.String() == ingest.BranchSourceUnknown.String() {
			t.Errorf("harness %q declares the branch source %d, which is not one of the declared sources", harness, source)
		}
	}
	for harness := range ingest.HarnessBranchSources {
		if _, registered := ingest.DefaultAdapterRegistry[harness]; !registered {
			t.Errorf("branch source declared for %q, which has no registered adapter", harness)
		}
	}
}

func TestSessionProjectDirectory(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name                   string `yaml:"name"`
			Harness                string `yaml:"harness"`
			CWD                    string `yaml:"cwd"`
			Source                 string `yaml:"source"`
			Original               string `yaml:"original"`
			Want                   string `yaml:"want"`
			WantTemporaryDirectory bool   `yaml:"wantTemporaryDirectory"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(sessionProjectDirectoryData, &fixtures); err != nil {
		t.Fatalf("decode session project directory fixture: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/ingest/testdata/session_project_directory.yaml", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			physical, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			slug := strings.ReplaceAll(filepath.Clean(physical), string(filepath.Separator), "-")
			session := ingest.DiscoveredSession{
				Harness:      ingest.Harness(c.Harness),
				CWD:          c.CWD,
				SourcePath:   ingest.ResolvedPath(strings.ReplaceAll(c.Source, "{slug}", slug)),
				OriginalRoot: ingest.ResolvedPath(c.Original),
			}
			want := c.Want
			if c.WantTemporaryDirectory {
				want = physical
			}
			if got := ingest.SessionProjectDirectory(session); got != want {
				t.Fatalf("project directory = %q, want %q", got, want)
			}
		})
	}
}
