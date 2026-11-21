package discovery_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/listing_origins.yaml
var listingOriginsData []byte

type listingOriginCase struct {
	Name       string `yaml:"name"`
	Discovered string `yaml:"discovered"`
	Want       string `yaml:"want"`
}

type listingOriginDocument struct {
	RequiredCaseNames []string            `yaml:"requiredCaseNames"`
	Cases             []listingOriginCase `yaml:"cases"`
}

// TestResolveListingOrigin runs the listing-origin fail-safe: missing evidence
// and a token outside the closed menu both resolve to unknown, and every menu
// value is kept as mined.
func TestResolveListingOrigin(t *testing.T) {
	t.Parallel()
	var document listingOriginDocument
	if err := testutil.DecodeFixtureYAML(listingOriginsData, &document); err != nil {
		t.Fatalf("decode listing_origins.yaml: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		if present[row.Name] {
			t.Fatalf("listing_origins.yaml repeats case %q", row.Name)
		}
		present[row.Name] = true
	}
	for _, name := range document.RequiredCaseNames {
		if !present[name] {
			t.Fatalf("listing_origins.yaml is missing required case %q", name)
		}
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			want, err := sessionorigin.Parse(row.Want)
			if err != nil {
				t.Fatalf("fixture want %q is not a menu value: %v", row.Want, err)
			}
			got := discovery.ResolveListingOrigin(sessionorigin.Origin(row.Discovered))
			if got != want {
				t.Fatalf("ResolveListingOrigin(%q) = %q, want %q", row.Discovered, got, want)
			}
		})
	}
}
