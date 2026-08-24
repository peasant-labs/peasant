package ingest_test

import (
	"bytes"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_content_aware_selection.yaml
var openCodeContentAwareSelectionYAML []byte

type openCodeContentAwareSelectionFixture struct {
	SourceFixture string                              `yaml:"source_fixture"`
	RequiredCases []string                            `yaml:"required_cases"`
	Cases         []openCodeContentAwareSelectionCase `yaml:"cases"`
}

type openCodeContentAwareSelectionCase struct {
	Name            string `yaml:"name"`
	SessionID       string `yaml:"session_id"`
	ExpectedOrigin  string `yaml:"expected_origin"`
	ExpectedContent string `yaml:"expected_content"`
}

func loadOpenCodeContentAwareSelectionFixture(t *testing.T) openCodeContentAwareSelectionFixture {
	t.Helper()
	var fixture openCodeContentAwareSelectionFixture
	if err := yaml.Unmarshal(openCodeContentAwareSelectionYAML, &fixture); err != nil {
		t.Fatalf("decode content-aware selection fixture: %v", err)
	}
	if fixture.SourceFixture == "" {
		t.Fatal("content-aware selection fixture has no source fixture")
	}
	byName := make(map[string]openCodeContentAwareSelectionCase, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || fixtureCase.SessionID == "" || fixtureCase.ExpectedOrigin == "" || fixtureCase.ExpectedContent == "" {
			t.Fatalf("content-aware selection fixture case is incomplete: %+v", fixtureCase)
		}
		if _, duplicate := byName[fixtureCase.Name]; duplicate {
			t.Fatalf("content-aware selection fixture duplicated case %q", fixtureCase.Name)
		}
		byName[fixtureCase.Name] = fixtureCase
	}
	// The required-name manifest asserts exact membership: every declared name
	// must be present, and no case may appear outside the manifest. Removing a
	// case goes red by name; adding one requires naming it here on purpose.
	for _, required := range fixture.RequiredCases {
		if _, ok := byName[required]; !ok {
			t.Fatalf("content-aware selection fixture is missing required case %q", required)
		}
	}
	requiredSet := make(map[string]struct{}, len(fixture.RequiredCases))
	for _, required := range fixture.RequiredCases {
		requiredSet[required] = struct{}{}
	}
	for name := range byName {
		if _, ok := requiredSet[name]; !ok {
			t.Fatalf("content-aware selection fixture case %q is not in the required-name manifest", name)
		}
	}
	return fixture
}

func openCodeContentAwareExpectedOrigin(t *testing.T, name string) ingest.TranscriptOrigin {
	t.Helper()
	switch name {
	case "legacy_sqlite":
		return ingest.TranscriptOriginOpenCodeLegacySQLite
	case "current_sqlite":
		return ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("content-aware selection fixture names unknown origin %q", name)
		return ingest.TranscriptOrigin(0)
	}
}

// TestOpenCodeContentAwareCanonicalSelection proves that a hybrid session
// prefers its current projection only when that projection holds a substantive
// turn. A control-only current projection loses to a legacy sibling that still
// carries the conversation, a control-only current projection with no legacy
// sibling still materializes as before, and a substantive current projection
// keeps winning over its legacy sibling.
//
// Mutation proof: ranking the current representation above legacy regardless of
// its content (restoring the blind current preference in
// effectiveOpenCodeCanonicalRank) makes the first case resolve to the
// control-only current projection, so the materialized transcript no longer
// contains LEGACY_CONVERSATION_KEPT and the assertion fails.
func TestOpenCodeContentAwareCanonicalSelection(t *testing.T) {
	fixture := loadOpenCodeContentAwareSelectionFixture(t)
	materialized := testfixture.MaterializeByName(t, fixture.SourceFixture)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover content-aware hybrid database: %v", err)
	}
	byID := make(map[string]ingest.DiscoveredSession, len(discovered))
	for _, session := range discovered {
		byID[string(session.SessionID)] = session
	}
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			session, ok := byID[fixtureCase.SessionID]
			if !ok {
				t.Fatalf("discovery omitted session %q", fixtureCase.SessionID)
			}
			wantOrigin := openCodeContentAwareExpectedOrigin(t, fixtureCase.ExpectedOrigin)
			if session.TranscriptOrigin != wantOrigin {
				t.Fatalf("session %q selected origin %d, want %d", fixtureCase.SessionID, session.TranscriptOrigin, wantOrigin)
			}
			_, projection, err := adapter.MaterializeTranscript(t.Context(), session)
			if err != nil {
				t.Fatalf("materialize selected session %q: %v", fixtureCase.SessionID, err)
			}
			if !bytes.Contains(projection, []byte(fixtureCase.ExpectedContent)) {
				t.Fatalf("selected session %q projection does not contain %q: %s", fixtureCase.SessionID, fixtureCase.ExpectedContent, projection)
			}
		})
	}
}
