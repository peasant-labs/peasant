package store_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_evidence_read.yaml
var claudeEvidenceReadYAML []byte

//go:embed testdata/claude_evidence_read.manifest.yaml
var claudeEvidenceReadManifestYAML []byte

type evidenceReadSpec struct {
	Path         string         `yaml:"path"`
	Scope        string         `yaml:"scope"`
	IdentityTeam string         `yaml:"identityTeam"`
	IdentityName string         `yaml:"identityName"`
	Spawns       []derivedSpawn `yaml:"spawns"`
}

type evidenceReadCase struct {
	Name        string   `yaml:"name"`
	Paths       []string `yaml:"paths"`
	WantPresent []string `yaml:"wantPresent"`
}

type evidenceReadFixture struct {
	Evidence []evidenceReadSpec `yaml:"evidence"`
	Cases    []evidenceReadCase `yaml:"cases"`
}

func loadEvidenceReadFixture(t *testing.T) evidenceReadFixture {
	t.Helper()
	var fixture evidenceReadFixture
	if err := testutil.DecodeFixtureYAML(claudeEvidenceReadYAML, &fixture); err != nil {
		t.Fatalf("decode Claude evidence read fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeEvidenceReadManifestYAML, "Claude evidence read")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "Claude evidence read"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestStoredClaudeEvidenceForPrimaryKeyRead pins the by-path evidence read the
// scoped path uses. It returns exactly the cached paths it was asked for, and a
// path that is not cached is absent rather than fabricated.
func TestStoredClaudeEvidenceForPrimaryKeyRead(t *testing.T) {
	t.Parallel()
	fixture := loadEvidenceReadFixture(t)
	db := storetest.Open(t)
	ctx := context.Background()
	upserts := make([]ingest.ClaudeTranscriptEvidence, 0, len(fixture.Evidence))
	for _, spec := range fixture.Evidence {
		scope := ingest.ClaudeEvidenceScope(spec.Scope)
		if !scope.IsValid() {
			t.Fatalf("fixture evidence %s names unknown scope %q", spec.Path, spec.Scope)
		}
		record := ingest.ClaudeTranscriptEvidence{SourcePath: ingest.ResolvedPath(spec.Path), Scope: scope}
		if spec.IdentityTeam != "" || spec.IdentityName != "" {
			record.Identity = &ingest.ClaudeTeammateIdentity{Team: spec.IdentityTeam, Name: spec.IdentityName}
		}
		for _, spawn := range spec.Spawns {
			record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: spawn.Team, Name: spawn.Name})
		}
		upserts = append(upserts, record)
	}
	if err := db.SaveClaudeEvidence(ctx, upserts, nil); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			paths := make([]ingest.ResolvedPath, 0, len(testCase.Paths))
			for _, path := range testCase.Paths {
				paths = append(paths, ingest.ResolvedPath(path))
			}
			loaded, err := db.LoadClaudeEvidenceFor(ctx, paths)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != len(testCase.WantPresent) {
				t.Fatalf("LoadClaudeEvidenceFor = %d records, want %d", len(loaded), len(testCase.WantPresent))
			}
			for _, path := range testCase.WantPresent {
				record, ok := loaded[ingest.ResolvedPath(path)]
				if !ok {
					t.Fatalf("LoadClaudeEvidenceFor omitted %s", path)
				}
				if string(record.SourcePath) != path {
					t.Fatalf("loaded record path = %q, want %q", record.SourcePath, path)
				}
				if !record.Scope.IsValid() {
					t.Fatalf("loaded record %s carries an invalid scope", path)
				}
			}
		})
	}
}
