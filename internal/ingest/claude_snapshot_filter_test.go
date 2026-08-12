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

//go:embed testdata/claude_snapshot_filter.yaml
var claudeSnapshotFilterYAML []byte

type claudeSnapshotFilterFixtures struct {
	DeclaredCases int                           `yaml:"declared_cases"`
	Cases         []claudeSnapshotFilterFixture `yaml:"cases"`
}

type claudeSnapshotFilterFixture struct {
	Name     string                     `yaml:"name"`
	Files    []claudeSnapshotFilterFile `yaml:"files"`
	Expected []string                   `yaml:"expected"`
}

type claudeSnapshotFilterFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

func loadClaudeSnapshotFilterFixtures(t *testing.T) claudeSnapshotFilterFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeSnapshotFilterYAML))
	decoder.KnownFields(true)
	var fixtures claudeSnapshotFilterFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode Claude snapshot filter fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("Claude snapshot filter fixture must contain exactly one YAML document: %v", err)
	}
	const expectedCases = 6
	if fixtures.DeclaredCases != expectedCases || len(fixtures.Cases) != expectedCases {
		t.Fatalf("Claude snapshot filter fixture case guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredCases, len(fixtures.Cases), expectedCases)
	}
	return fixtures
}

func TestClaudeAdapter_DiscoverRequiresConversationRecords(t *testing.T) {
	for _, fixture := range loadClaudeSnapshotFilterFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			for _, file := range fixture.Files {
				if err := fs.WriteFile("/claude/"+file.Path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
					t.Fatalf("write Claude fixture %q: %v", file.Path, err)
				}
			}

			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{Paths: []ingest.ResolvedPath{"/claude"}, Enabled: true})
			if err != nil {
				t.Fatalf("discover Claude fixture: %v", err)
			}
			if len(sessions) != len(fixture.Expected) {
				t.Fatalf("discovered %d sessions, want %d", len(sessions), len(fixture.Expected))
			}
			for _, expected := range fixture.Expected {
				found := false
				for _, session := range sessions {
					if string(session.SessionID) == expected {
						found = true
					}
				}
				if !found {
					t.Errorf("session %q not discovered", expected)
				}
			}
		})
	}
}
