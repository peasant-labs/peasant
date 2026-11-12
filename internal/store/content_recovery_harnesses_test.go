package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_recovery_harnesses.yaml
var contentRecoveryHarnessesYAML []byte

//go:embed testdata/content_recovery_harnesses.manifest.yaml
var contentRecoveryHarnessesManifestYAML []byte

type contentRecoverySession struct {
	ID      string `yaml:"id"`
	Harness string `yaml:"harness"`
}

type contentRecoveryHarnessCase struct {
	Name     string                   `yaml:"name"`
	Sessions []contentRecoverySession `yaml:"sessions"`
	Limit    int                      `yaml:"limit"`
	Want     []string                 `yaml:"want"`
	Mutation string                   `yaml:"mutation"`
}

type contentRecoveryHarnessFixture struct {
	Cases []contentRecoveryHarnessCase `yaml:"cases"`
}

func decodeContentRecoveryYAML(t *testing.T, label string, data []byte, into any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s must contain exactly one YAML document: %v", label, err)
	}
}

func loadContentRecoveryHarnessFixture(t *testing.T) contentRecoveryHarnessFixture {
	t.Helper()
	var fixture contentRecoveryHarnessFixture
	decodeContentRecoveryYAML(t, "content recovery harness fixture", contentRecoveryHarnessesYAML, &fixture)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decodeContentRecoveryYAML(t, "content recovery harness manifest", contentRecoveryHarnessesManifestYAML, &manifest)
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if present[fixtureCase.Name] {
			t.Fatalf("content recovery harness case %q is declared twice", fixtureCase.Name)
		}
		if fixtureCase.Mutation == "" || fixtureCase.Limit <= 0 || len(fixtureCase.Sessions) == 0 {
			t.Fatalf("content recovery harness case %q needs sessions, a positive limit and a mutation", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required content recovery harness case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// TestContentRecoveryWalkListsOnlyHarvestableHarnesses reads the recovery page
// through the production store method on a golden-copy store, seeded with rows
// whose harness the fixture names literally.
func TestContentRecoveryWalkListsOnlyHarvestableHarnesses(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadContentRecoveryHarnessFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			s := storetest.Open(t)
			ctx := context.Background()
			for _, seeded := range fixtureCase.Sessions {
				var harness schema.Harness
				if err := harness.UnmarshalText([]byte(seeded.Harness)); err != nil || !harness.IsKnown() {
					t.Fatalf("fixture session %s names harness %q, which is not a known harness value (%v); the store CHECK would refuse it", seeded.ID, seeded.Harness, err)
				}
				hash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				entry := makeStoreEntry(t, seeded.ID, hash, "github.com-test", harness, 1700000000000, 1, 1)
				if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
					t.Fatalf("seed session %s with harness %q: %v", seeded.ID, seeded.Harness, err)
				}
			}

			targets, err := s.ListContentCaptureIncompleteSessionsAfter(ctx, "", fixtureCase.Limit, ingest.AllStoredSessions())
			if err != nil {
				t.Fatalf("ListContentCaptureIncompleteSessionsAfter: %v", err)
			}
			got := make([]string, 0, len(targets))
			for _, target := range targets {
				got = append(got, string(target.SessionID))
			}
			if !slices.Equal(got, fixtureCase.Want) {
				t.Errorf("recovery page = %v, want %v (red under: %s)", got, fixtureCase.Want, fixtureCase.Mutation)
			}
		})
	}
}
