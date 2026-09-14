package api

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/detail_navigation.yaml
var detailNavigationFixtureYAML []byte

//go:embed testdata/detail_navigation.manifest.yaml
var detailNavigationManifestYAML []byte

type detailNavigationExpected struct {
	Kind         string `yaml:"kind"`
	Status       string `yaml:"status"`
	LocalID      string `yaml:"localId,omitempty"`
	TranscriptID string `yaml:"transcriptId,omitempty"`
}

type detailNavigationCase struct {
	Name              string                       `yaml:"name"`
	Relationships     []schema.SessionRelationship `yaml:"relationships"`
	Stored            []string                     `yaml:"stored"`
	ExpectedLookupIDs []string                     `yaml:"expectedLookupIDs"`
	Expected          []detailNavigationExpected   `yaml:"expected"`
}

type detailNavigationFixture struct {
	Cases []detailNavigationCase `yaml:"cases"`
}

func loadDetailNavigationFixture(t *testing.T) detailNavigationFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(detailNavigationFixtureYAML))
	decoder.KnownFields(true)
	var fixture detailNavigationFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode detail navigation fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("detail navigation fixture must contain exactly one YAML document: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(detailNavigationManifestYAML, &manifest); err != nil {
		t.Fatalf("decode detail navigation manifest: %v", err)
	}
	names := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] {
			t.Fatalf("detail navigation case %q is missing or duplicated", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("detail navigation", "case", manifest.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// recordingLookup is a stored-target lookup that reports exactly the fixture's
// stored identifiers as present and records every identifier it was asked
// about, so a case can prove navigation consults the stored target rather than
// list selection.
type recordingLookup struct {
	stored    map[string]bool
	requested []string
}

func (l *recordingLookup) resolve(_ context.Context, ids []string) ([]StoredTarget, error) {
	l.requested = append(l.requested, ids...)
	targets := make([]StoredTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, StoredTarget{ID: id, Found: l.stored[id]})
	}
	return targets, nil
}

// TestResolveRelationshipNavigationFixture drives the real production resolver
// through the named fixture cases. It asserts the exact lookup identifiers, the
// emitted navigation set, and that every emitted entry validates against the
// published contract.
func TestResolveRelationshipNavigationFixture(t *testing.T) {
	fixture := loadDetailNavigationFixture(t)
	if len(fixture.Cases) == 0 {
		t.Fatal("detail navigation fixture has no cases")
	}
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			stored := make(map[string]bool, len(fixtureCase.Stored))
			for _, id := range fixtureCase.Stored {
				stored[id] = true
			}
			lookup := &recordingLookup{stored: stored}
			got, err := resolveRelationshipNavigation(context.Background(), fixtureCase.Relationships, lookup.resolve)
			if err != nil {
				t.Fatalf("resolve navigation: %v", err)
			}
			if len(lookup.requested) != len(fixtureCase.ExpectedLookupIDs) {
				t.Fatalf("lookup asked for %v, want exactly %v", lookup.requested, fixtureCase.ExpectedLookupIDs)
			}
			for i, want := range fixtureCase.ExpectedLookupIDs {
				if lookup.requested[i] != want {
					t.Fatalf("lookup id %d = %q, want %q", i, lookup.requested[i], want)
				}
			}
			if len(got) != len(fixtureCase.Expected) {
				t.Fatalf("navigation = %+v, want %d entries", got, len(fixtureCase.Expected))
			}
			for i, want := range fixtureCase.Expected {
				entry := got[i]
				if err := entry.Validate(); err != nil {
					t.Fatalf("navigation %d does not validate: %v", i, err)
				}
				if string(entry.Kind) != want.Kind || string(entry.Status) != want.Status {
					t.Fatalf("navigation %d = (%s,%s), want (%s,%s)", i, entry.Kind, entry.Status, want.Kind, want.Status)
				}
				gotLocalID := ""
				if entry.LocalID != nil {
					gotLocalID = string(*entry.LocalID)
				}
				if gotLocalID != want.LocalID {
					t.Fatalf("navigation %d localId = %q, want %q", i, gotLocalID, want.LocalID)
				}
				gotTranscriptID := ""
				if entry.TranscriptID != nil {
					gotTranscriptID = string(*entry.TranscriptID)
				}
				if gotTranscriptID != want.TranscriptID {
					t.Fatalf("navigation %d transcriptId = %q, want %q", i, gotTranscriptID, want.TranscriptID)
				}
			}
		})
	}
}
