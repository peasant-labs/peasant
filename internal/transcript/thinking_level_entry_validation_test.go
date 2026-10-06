package transcript

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/thinking_level_entry_validation.yaml
var thinkingEntryValidationYAML []byte

//go:embed testdata/thinking_level_entry_validation.manifest.yaml
var thinkingEntryValidationManifestYAML []byte

type thinkingEntryValidationFixture struct {
	ErrorFragments []string `yaml:"errorFragments"`
	Cases          []struct {
		Name     string      `yaml:"name"`
		Role     schema.Role `yaml:"role"`
		Extra    string      `yaml:"extra"`
		Accepted bool        `yaml:"accepted"`
	} `yaml:"cases"`
}

func loadThinkingEntryValidationFixture(t *testing.T) thinkingEntryValidationFixture {
	t.Helper()
	var fixture thinkingEntryValidationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(thinkingEntryValidationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must contain one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingEntryValidationManifestYAML, "thinking entry validation")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking entry validation"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestValidateThinkingLevelEntries proves the indexed-entry boundary refuses a
// non-string value, an out-of-set canonical level, an invalid raw spelling, and
// non-assistant attribution instead of silently folding the evidence to absent.
// Each refused case is also pushed through EntriesToProjectionValidated, the
// boundary the WebSocket viewer and the export path share, so the wiring at the
// fold is proven rather than only the standalone validator.
func TestValidateThinkingLevelEntries(t *testing.T) {
	t.Parallel()
	fixture := loadThinkingEntryValidationFixture(t)
	for i, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			entry := schema.SessionEntry{EntryIndex: i + 1, Role: c.Role, EntryType: schema.EntryTypeText}
			if c.Extra != "" {
				value := c.Extra
				entry.Extra = &value
			}
			err := ValidateThinkingLevelEntries([]schema.SessionEntry{entry})
			if (err == nil) != c.Accepted {
				t.Fatalf("accepted=%t, want %t: %v", err == nil, c.Accepted, err)
			}
			if err == nil {
				return
			}
			for _, fragment := range fixture.ErrorFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error missing %q: %v", fragment, err)
				}
			}
			_, boundaryErr := EntriesToProjectionValidated([]schema.SessionEntry{entry}, ProjectionOptions{})
			if boundaryErr == nil {
				t.Fatal("the entries-to-projection boundary accepted evidence the validator refused")
			}
			if !strings.Contains(boundaryErr.Error(), "thinking-level entry validation failed") {
				t.Fatalf("the boundary refused for an unrelated reason: %v", boundaryErr)
			}
		})
	}
}
