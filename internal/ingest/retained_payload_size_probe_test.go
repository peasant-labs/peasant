package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_payload_size_probe.yaml
var retainedPayloadSizeProbeFixtureData []byte

const retainedPayloadSizeProbeFixturePath = "internal/ingest/testdata/retained_payload_size_probe.yaml"

type retainedPayloadSizeProbeFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	MeasureCases  []struct {
		Name         string `yaml:"name"`
		Raw          string `yaml:"raw"`
		Want         int    `yaml:"want"`
		Limit        int    `yaml:"limit"`
		WantExceeded bool   `yaml:"wantExceeded"`
	} `yaml:"measureCases"`
	ProbeCases []struct {
		Name     string `yaml:"name"`
		Limit    int    `yaml:"limit"`
		Extra    string `yaml:"extra"`
		WantOver bool   `yaml:"wantOver"`
	} `yaml:"probeCases"`
}

func loadRetainedPayloadSizeProbeFixtures(t *testing.T) retainedPayloadSizeProbeFixtures {
	t.Helper()
	var fixtures retainedPayloadSizeProbeFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(retainedPayloadSizeProbeFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", retainedPayloadSizeProbeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", retainedPayloadSizeProbeFixturePath, err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.MeasureCases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate measure case name %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, c := range fixtures.ProbeCases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate probe case name %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required fixture %q missing from %s", name, retainedPayloadSizeProbeFixturePath)
		}
	}
	return fixtures
}

// TestScanJSONStringDecodedLength pins the decoded byte length the size probe
// derives from a stored JSON string, including escapes and surrogate pairs, and
// pins that measurement stops once the limit is exceeded. Every non-exceeded
// case is cross-checked against encoding/json's own decoding, so the probe's
// length cannot drift from the length the authoritative read path measures.
func TestScanJSONStringDecodedLength(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.MeasureCases {
		t.Run(c.Name, func(t *testing.T) {
			decoded, end, exceeded, ok := scanJSONString(c.Raw, 0, c.Limit)
			if !ok {
				t.Fatalf("scan rejected valid JSON string %q", c.Raw)
			}
			if c.WantExceeded {
				if !exceeded {
					t.Fatalf("scan did not stop at limit %d for %q (decoded %d)", c.Limit, c.Raw, decoded)
				}
				return
			}
			if exceeded {
				t.Fatalf("scan exceeded limit %d for %q", c.Limit, c.Raw)
			}
			if decoded != c.Want || end != len(c.Raw) {
				t.Fatalf("scan measured decoded=%d end=%d, want %d/%d for %q", decoded, end, c.Want, len(c.Raw), c.Raw)
			}
			var decodedValue string
			if err := json.Unmarshal([]byte(c.Raw), &decodedValue); err != nil {
				t.Fatalf("encoding/json rejected %q: %v", c.Raw, err)
			}
			if len(decodedValue) != c.Want {
				t.Fatalf("probe measured %d bytes but encoding/json measured %d for %q", decoded, len(decodedValue), c.Raw)
			}
		})
	}
}

// TestStoredRetainedPayloadExceedsTransferLimit pins the in-place refusal
// predicate: it measures a canonical stored payload, soundly declines to refuse
// records whose public coordinates are absent, envelopes outside the owned
// member set, ambiguous payload encodings, and unrelated extension fields.
func TestStoredRetainedPayloadExceedsTransferLimit(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.ProbeCases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Limit <= 0 {
				t.Fatalf("probe case %q needs a positive limit", c.Name)
			}
			if got := storedRetainedExtraExceedsTransferLimit(c.Extra, c.Limit); got != c.WantOver {
				t.Fatalf("probe verdict = %v, want %v for %s", got, c.WantOver, strings.TrimSpace(c.Extra))
			}
		})
	}
}
