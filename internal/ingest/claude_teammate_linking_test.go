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

//go:embed testdata/claude_teammate_linking.yaml
var claudeTeammateLinkingYAML []byte

type claudeTeammateFixtures struct {
	DeclaredRows int                     `yaml:"declared_rows"`
	Cases        []claudeTeammateFixture `yaml:"cases"`
}

type claudeTeammateFixture struct {
	Name     string                    `yaml:"name"`
	Files    []claudeTeammateFile      `yaml:"files"`
	Expected []claudeExpectedDiscovery `yaml:"expected"`
}

type claudeTeammateFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

type claudeExpectedDiscovery struct {
	SessionID     string   `yaml:"session_id"`
	ParentUUID    string   `yaml:"parent_uuid"`
	SubagentPaths []string `yaml:"subagent_paths"`
}

func loadClaudeTeammateFixtures(t *testing.T) claudeTeammateFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeTeammateLinkingYAML))
	decoder.KnownFields(true)
	var fixtures claudeTeammateFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode Claude teammate fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("Claude teammate fixture must contain exactly one YAML document: %v", err)
	}
	const expectedRows = 11
	if fixtures.DeclaredRows != expectedRows || len(fixtures.Cases) != expectedRows {
		t.Fatalf("Claude teammate fixture row guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredRows, len(fixtures.Cases), expectedRows)
	}
	return fixtures
}

func TestClaudeAdapter_DiscoverTeammateLineage(t *testing.T) {
	fixtures := loadClaudeTeammateFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			for _, file := range fixture.Files {
				if err := mfs.WriteFile("/claude/"+file.Path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
					t.Fatalf("write transcript fixture %q: %v", file.Path, err)
				}
			}

			adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{"/claude"},
				Enabled: true,
			})
			if err != nil {
				t.Fatalf("discover Claude fixture: %v", err)
			}
			if len(sessions) != len(fixture.Expected) {
				t.Fatalf("discovered %d sessions, want %d", len(sessions), len(fixture.Expected))
			}

			byID := make(map[string]ingest.DiscoveredSession, len(sessions))
			for _, session := range sessions {
				byID[string(session.SessionID)] = session
			}
			for _, expected := range fixture.Expected {
				session, ok := byID[expected.SessionID]
				if !ok {
					t.Fatalf("session %q not discovered", expected.SessionID)
				}
				gotParent := ""
				if session.ParentUUID != nil {
					gotParent = string(*session.ParentUUID)
				}
				if gotParent != expected.ParentUUID {
					t.Errorf("session %q parent = %q, want %q", expected.SessionID, gotParent, expected.ParentUUID)
				}
				gotPaths := make([]string, len(session.SubagentPaths))
				for index, path := range session.SubagentPaths {
					gotPaths[index] = string(path)
				}
				if strings.Join(gotPaths, "\n") != strings.Join(expected.SubagentPaths, "\n") {
					t.Errorf("session %q subagent paths = %q, want %q", expected.SessionID, gotPaths, expected.SubagentPaths)
				}
			}
		})
	}
}
