package main

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/codex_detail_timestamps.yaml
var codexDetailTimestampsYAML []byte

func TestSessionDetailCarriesCodexTimestamps(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string `yaml:"name"`
			SessionID string `yaml:"sessionID"`
			Year      int    `yaml:"year"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(codexDetailTimestampsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("invalid Codex timestamp fixture %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := harvestFixtureDetail(t, string(defaults.HarnessCodex), "codex-fixture", "sessions", c.SessionID)
			if len(s.Turns) == 0 {
				t.Fatal("no turns")
			}
			for i, turn := range s.Turns {
				if turn.Timestamp.Year() != c.Year {
					t.Errorf("turn %d (%s): timestamp %v, want recorded year %d", i, turn.Role, turn.Timestamp, c.Year)
				}
			}
		})
	}
	if err := testutil.RequireFixtureNames("Codex timestamps", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}
