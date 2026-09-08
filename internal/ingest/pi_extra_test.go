package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_usage_carriers.yaml
var piUsageYAML []byte

//go:embed testdata/pi_usage_carriers.manifest.yaml
var piUsageManifest []byte

func TestPiUsageCarrierBoundaries(t *testing.T) {
	var f struct {
		Cases []struct {
			Name         string `yaml:"name"`
			Usage        string `yaml:"usage"`
			Completeness string `yaml:"completeness"`
			Cost         string `yaml:"cost"`
			Error        bool   `yaml:"error"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(piUsageYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing YAML: %v", err)
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
			if string(u.Completeness) != c.Completeness || u.SourceEntryRef != ingest.PiPublicRef(testutil.TestSessionUUID, "entry", "native-entry") || string(u.OwnerID) != ingest.PiPublicRef(testutil.TestSessionUUID, "owner", "native-entry") {
				t.Fatalf("usage owner/completeness mismatch: %+v", u)
			}
			cost := ""
			if u.Cost != nil && u.Cost.Total != nil {
				cost = string(*u.Cost.Total)
			}
			if cost != c.Cost {
				t.Fatalf("recorded cost=%q want %q", cost, c.Cost)
			}
			extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraUsage, Harness: schema.HarnessPi, SourceRef: u.SourceEntryRef, Usage: &u})
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
