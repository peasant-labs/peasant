package ingest_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_hints_title.yaml
var claudeHintsTitleYAML []byte

const (
	claudeHintsTitleRoot     = "/claude"
	claudeHintsTitleSession  = "11111111-1111-4111-8111-111111111111"
	claudeHintsTitleFilePath = claudeHintsTitleRoot + "/-workspace/" + claudeHintsTitleSession + ".jsonl"
)

type claudeHintsTitleFixtures struct {
	RequiredNames []string               `yaml:"required_names"`
	Cases         []claudeHintsTitleCase `yaml:"cases"`
}

type claudeHintsTitleCase struct {
	Name           string   `yaml:"name"`
	Lines          []string `yaml:"lines"`
	ExpectedTitle  string   `yaml:"expected_title"`
	ExpectedCWD    string   `yaml:"expected_cwd"`
	ExpectedBranch string   `yaml:"expected_branch"`
	MatchFirstUser bool     `yaml:"match_first_user"`
}

func loadClaudeHintsTitleFixtures(t *testing.T) claudeHintsTitleFixtures {
	t.Helper()
	var fixtures claudeHintsTitleFixtures
	if err := testutil.DecodeFixtureYAML(claudeHintsTitleYAML, &fixtures); err != nil {
		t.Fatalf("decode Claude display-title fixtures: %v", err)
	}
	seen := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || len(fixture.Lines) == 0 {
			t.Fatalf("Claude display-title fixture has an incomplete case: %#v", fixture)
		}
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("duplicate Claude display-title fixture case %q", fixture.Name)
		}
		seen[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude display titles", "case", fixtures.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// discoverClaudeHintsTitle runs the production discovery path over one root
// transcript and returns the display title it derived.
func discoverClaudeHintsTitle(t *testing.T, fixture claudeHintsTitleCase) ingest.DiscoveredSession {
	t.Helper()
	fs := testutil.NewMemFS()
	body := strings.Join(fixture.Lines, "\n") + "\n"
	if err := fs.WriteFile(claudeHintsTitleFilePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write the transcript fixture: %v", err)
	}
	adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
	cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeHintsTitleRoot}, Enabled: true}
	sessions, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("discover the transcript: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("discovered %d sessions, want exactly 1", len(sessions))
	}
	if string(sessions[0].SessionID) != claudeHintsTitleSession {
		t.Fatalf("discovered session %q, want %q", sessions[0].SessionID, claudeHintsTitleSession)
	}
	got := sessions[0]
	if fixture.ExpectedCWD != "" && got.CWD != fixture.ExpectedCWD {
		t.Fatalf("cwd = %q, want %q", got.CWD, fixture.ExpectedCWD)
	}
	if fixture.ExpectedBranch != "" && got.Branch != fixture.ExpectedBranch {
		t.Fatalf("branch = %q, want %q", got.Branch, fixture.ExpectedBranch)
	}
	if fixture.MatchFirstUser {
		entries, err := ingest.NewClaudeIndexer(fs).IndexTranscript(context.Background(), got)
		if err != nil {
			t.Fatal(err)
		}
		firstUser := ""
		for _, entry := range entries {
			if entry.Role == ingest.RoleUser && entry.ContentPreview != nil {
				firstUser = *entry.ContentPreview
				break
			}
		}
		if firstUser == "" || firstUser != got.Title {
			t.Fatalf("discovery title %q disagrees with first indexed user text %q", got.Title, firstUser)
		}
	}
	return got
}

// TestClaudeAdapter_DisplayTitleSkipsInjectedTurns proves the discovery display
// title comes from the first user record that still holds user prose once the
// harness markup is removed.
func TestClaudeAdapter_DisplayTitleSkipsInjectedTurns(t *testing.T) {
	fixtures := loadClaudeHintsTitleFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			got := discoverClaudeHintsTitle(t, fixture).Title
			if got != fixture.ExpectedTitle {
				t.Fatalf("display title = %q, want %q", got, fixture.ExpectedTitle)
			}
			if strings.ContainsAny(got, "<>") {
				t.Fatalf("display title %q still carries harness markup", got)
			}
		})
	}
}
