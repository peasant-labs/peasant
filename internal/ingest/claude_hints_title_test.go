package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_hints_title.yaml
var claudeHintsTitleYAML []byte

const (
	claudeHintsTitleRoot     = "/claude"
	claudeHintsTitleSession  = "11111111-1111-4111-8111-111111111111"
	claudeHintsTitleRows     = 15
	claudeHintsTitleFilePath = claudeHintsTitleRoot + "/-workspace/" + claudeHintsTitleSession + ".jsonl"
)

type claudeHintsTitleFixtures struct {
	DeclaredRows int                    `yaml:"declared_rows"`
	Cases        []claudeHintsTitleCase `yaml:"cases"`
}

type claudeHintsTitleCase struct {
	Name          string   `yaml:"name"`
	Lines         []string `yaml:"lines"`
	ExpectedTitle string   `yaml:"expected_title"`
}

func loadClaudeHintsTitleFixtures(t *testing.T) claudeHintsTitleFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeHintsTitleYAML))
	decoder.KnownFields(true)
	var fixtures claudeHintsTitleFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode Claude display-title fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("Claude display-title fixture must contain exactly one YAML document: %v", err)
	}
	if fixtures.DeclaredRows != claudeHintsTitleRows || len(fixtures.Cases) != claudeHintsTitleRows {
		t.Fatalf("Claude display-title fixture row guard failed: declared=%d actual=%d expected=%d",
			fixtures.DeclaredRows, len(fixtures.Cases), claudeHintsTitleRows)
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || len(fixture.Lines) == 0 {
			t.Fatalf("Claude display-title fixture has an incomplete case: %#v", fixture)
		}
		if _, exists := seen[fixture.Name]; exists {
			t.Fatalf("duplicate Claude display-title fixture case %q", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
	}
	return fixtures
}

// discoverClaudeHintsTitle runs the production discovery path over one root
// transcript and returns the display title it derived.
func discoverClaudeHintsTitle(t *testing.T, lines []string) string {
	t.Helper()
	fs := testutil.NewMemFS()
	body := strings.Join(lines, "\n") + "\n"
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
	return sessions[0].Title
}

// TestClaudeAdapter_DisplayTitleSkipsInjectedTurns proves the discovery display
// title comes from the first user record that still holds user prose once the
// harness markup is removed.
func TestClaudeAdapter_DisplayTitleSkipsInjectedTurns(t *testing.T) {
	fixtures := loadClaudeHintsTitleFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			got := discoverClaudeHintsTitle(t, fixture.Lines)
			if got != fixture.ExpectedTitle {
				t.Fatalf("display title = %q, want %q", got, fixture.ExpectedTitle)
			}
			if strings.ContainsAny(got, "<>") {
				t.Fatalf("display title %q still carries harness markup", got)
			}
		})
	}
}
