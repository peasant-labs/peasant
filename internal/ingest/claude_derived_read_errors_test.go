package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_derived_read_errors.yaml
var claudeDerivedReadErrorsYAML []byte

type claudeDerivedReadErrorFixtures struct {
	RequiredNames []string                     `yaml:"required_names"`
	Cases         []claudeDerivedReadErrorCase `yaml:"cases"`
}

type claudeDerivedReadErrorCase struct {
	Name   string               `yaml:"name"`
	Method string               `yaml:"method"`
	Want   string               `yaml:"want"`
	Files  []claudeTeammateFile `yaml:"files"`
}

func loadClaudeDerivedReadErrorFixtures(t *testing.T) claudeDerivedReadErrorFixtures {
	t.Helper()
	var fixtures claudeDerivedReadErrorFixtures
	if err := yaml.Unmarshal(claudeDerivedReadErrorsYAML, &fixtures); err != nil {
		t.Fatalf("decode Claude derived-read error fixtures: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, testCase := range fixtures.Cases {
		present[testCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude derived-read error fixture", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// failingClaudeDerivedLookup fails exactly the indexed read the case names, so
// the propagated error of each of the three teammate-link reads is exercised.
type failingClaudeDerivedLookup struct {
	declarers   int
	spawners    int
	declarerErr error
	spawnerErr  error
	pathErr     error
}

var _ ingest.DerivedLookupReader = (*failingClaudeDerivedLookup)(nil)

func (f *failingClaudeDerivedLookup) CountClaudeIdentityDeclarers(context.Context, ingest.ClaudeTeammateIdentity) (int, error) {
	if f.declarerErr != nil {
		return 0, f.declarerErr
	}
	return f.declarers, nil
}

func (f *failingClaudeDerivedLookup) CountClaudeIdentitySpawners(context.Context, ingest.ClaudeTeammateIdentity) (int, error) {
	if f.spawnerErr != nil {
		return 0, f.spawnerErr
	}
	return f.spawners, nil
}

func (f *failingClaudeDerivedLookup) ClaudeIdentitySpawnerPaths(context.Context, string, string) ([]string, error) {
	if f.pathErr != nil {
		return nil, f.pathErr
	}
	return nil, nil
}

func (*failingClaudeDerivedLookup) CountCodexRolloutsBySession(context.Context, ingest.SessionID) (int, error) {
	return 0, nil
}

func (*failingClaudeDerivedLookup) CodexRolloutPaths(context.Context, ingest.SessionID) ([]ingest.ResolvedPath, error) {
	return nil, nil
}

// TestClaudeAdapter_DerivedReadErrorsAreReported proves each indexed read the
// teammate decision makes propagates its failure as an actionable discovery
// error instead of silently omitting the link.
func TestClaudeAdapter_DerivedReadErrorsAreReported(t *testing.T) {
	fixtures := loadClaudeDerivedReadErrorFixtures(t)
	for _, testCase := range fixtures.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			for _, file := range testCase.Files {
				body := strings.Join(file.Lines, "\n") + "\n"
				if err := mfs.WriteFile("/claude/"+file.Path, []byte(body), 0o644); err != nil {
					t.Fatalf("write transcript fixture %q: %v", file.Path, err)
				}
			}

			// The build's own view contributes the declarer; the fake store
			// holds the spawner. That is the state in which a unique identity
			// reaches the spawner-path read.
			reader := &failingClaudeDerivedLookup{spawners: 1}
			switch testCase.Method {
			case "declarers":
				reader.declarerErr = errors.New("synthetic declarer count failure")
			case "spawners":
				reader.spawnerErr = errors.New("synthetic spawner count failure")
			case "spawnerPaths":
				reader.pathErr = errors.New("synthetic spawner path failure")
			default:
				t.Fatalf("fixture method %q is outside the declared set", testCase.Method)
			}

			adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			ingest.AttachClaudeEvidenceCache(adapter, newFakeClaudeLinkingEvidenceCache())
			ingest.AttachClaudeDerivedLookup(adapter, reader)
			ingest.AttachSessionLocationLookup(adapter, newFakeSessionLocationLookup())

			_, err := adapter.Discover(context.Background(), ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{"/claude"},
				Enabled: true,
			})
			if err == nil {
				t.Fatalf("%s did not fail discovery; an unreadable identity count must not become a silent omission", testCase.Method)
			}
			if !strings.Contains(err.Error(), testCase.Want) {
				t.Errorf("error %q does not name %q", err, testCase.Want)
			}
			if !strings.Contains(err.Error(), "fix:") {
				t.Errorf("error %q carries no fix: clause; a read failure must be actionable", err)
			}
		})
	}
}
