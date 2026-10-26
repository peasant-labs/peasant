package ingest_test

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pi_usage_carriers.yaml
var piUsageYAML []byte

//go:embed testdata/pi_usage_carriers.manifest.yaml
var piUsageManifest []byte

//go:embed testdata/pi_thinking_level_extra.yaml
var piThinkingLevelExtraYAML []byte

//go:embed testdata/pi_thinking_level_extra.manifest.yaml
var piThinkingLevelExtraManifest []byte

func TestPiThinkingLevelExtraBoundaries(t *testing.T) {
	var fixtures struct {
		Cases []struct {
			Name      string `yaml:"name"`
			Level     string `yaml:"level"`
			Raw       string `yaml:"raw"`
			RawBase64 string `yaml:"rawBase64"`
			Extra     string `yaml:"extra"`
			Error     bool   `yaml:"error"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(piThinkingLevelExtraYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(piThinkingLevelExtraManifest, "Pi thinking-level extra")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixtures.Cases))
	for i, c := range fixtures.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "Pi thinking-level extra"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.RawBase64 != "" {
				raw, err := base64.StdEncoding.DecodeString(c.RawBase64)
				if err != nil {
					t.Fatal(err)
				}
				c.Raw = string(raw)
			}
			value := ingest.PiExtra{Kind: ingest.PiExtraState, Harness: schema.HarnessPi, ThinkingLevel: schema.ThinkingLevel(c.Level), ThinkingLevelRaw: schema.ThinkingLevelRaw(c.Raw)}
			var encoded *string
			var err error
			if c.Extra != "" {
				encoded = &c.Extra
			} else {
				encoded, err = ingest.EncodePiExtra(value)
			}
			if err == nil {
				var decoded ingest.PiExtra
				var pi bool
				decoded, pi, err = ingest.DecodePiExtra(encoded)
				if err == nil && (!pi || decoded.ThinkingLevel != value.ThinkingLevel || decoded.ThinkingLevelRaw != value.ThinkingLevelRaw) {
					t.Fatalf("round trip changed evidence: %+v", decoded)
				}
			}
			if (err != nil) != c.Error {
				t.Fatalf("error = %v, want error %t", err, c.Error)
			}
		})
	}
}

func TestPiUsageCarrierBoundaries(t *testing.T) {
	t.Parallel()
	var f struct {
		Cases []struct {
			Name         string `yaml:"name"`
			Usage        string `yaml:"usage"`
			Completeness string `yaml:"completeness"`
			Cost         string `yaml:"cost"`
			Error        bool   `yaml:"error"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(piUsageYAML, &f); err != nil {
		t.Fatal(err)
	}
	m, err := testutil.DecodeRequiredNamesManifest(piUsageManifest, "Pi usage")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(f.Cases))
	for i, c := range f.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(m, names, "Pi usage"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			u, err := ingest.PiUsageFromRaw(testutil.TestSessionUUID, "native-entry", schema.UsageScopeAssistant, json.RawMessage(c.Usage))
			if c.Error {
				if err == nil {
					t.Fatal("invalid native usage accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(u.Completeness) != c.Completeness || string(u.SourceEntryRef) != ingest.PiPublicRef(testutil.TestSessionUUID, "entry", "native-entry") || string(u.OwnerID) != ingest.PiPublicRef(testutil.TestSessionUUID, "owner", "native-entry") {
				t.Fatalf("usage owner/completeness mismatch: %+v", u)
			}
			cost := ""
			if u.Cost != nil && u.Cost.Total != nil {
				cost = string(*u.Cost.Total)
			}
			if cost != c.Cost {
				t.Fatalf("recorded cost=%q want %q", cost, c.Cost)
			}
			extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraUsage, Harness: schema.HarnessPi, SourceRef: string(u.SourceEntryRef), Usage: &u})
			if err != nil {
				t.Fatal(err)
			}
			decoded, pi, err := ingest.DecodePiExtra(extra)
			if err != nil || !pi || decoded.Usage == nil || decoded.Usage.OwnerID != u.OwnerID {
				t.Fatalf("carrier round trip: %+v pi=%t error=%v", decoded, pi, err)
			}
		})
	}
}
