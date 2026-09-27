package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/retained_payload_size_probe.yaml
var retainedPayloadSizeProbeFixtureData []byte

const retainedPayloadSizeProbeFixturePath = "internal/ingest/testdata/retained_payload_size_probe.yaml"

type retainedPayloadSizeProbeFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	// ProbeHarness is the export harness the probe cases are asked for unless a
	// case overrides it.
	ProbeHarness string `yaml:"probeHarness"`
	MeasureCases []struct {
		Name string `yaml:"name"`
		Raw  string `yaml:"raw"`
		Want int    `yaml:"want"`
	} `yaml:"measureCases"`
	RawExtentCases []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
		Want  int    `yaml:"want"`
	} `yaml:"rawExtentCases"`
	ProbeCases []struct {
		Name    string `yaml:"name"`
		Limit   int    `yaml:"limit"`
		Harness string `yaml:"harness"`
		// EntryHarness overrides the carrier entry's harness for the case;
		// empty means the entry carries the export harness.
		EntryHarness string `yaml:"entryHarness"`
		Extra        string `yaml:"extra"`
		WantOver     bool   `yaml:"wantOver"`
		Declined     bool   `yaml:"declined"`
	} `yaml:"probeCases"`
}

// loadRetainedPayloadSizeProbeFixtures loads the committed fixture and enforces
// the requiredNames manifest.
//
// requiredNames is a deletion guard by design, per the workspace rule that
// deletion protection uses required-NAME manifests and never a bare count: the
// list must name every case so deleting or renaming one fails, while adding a
// case is allowed and guards nothing until its name is added here. Asserting set
// equality instead would go red on every legitimate addition and churn across
// parallel changes, so it is deliberately not asserted.
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
	for _, c := range fixtures.RawExtentCases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate raw extent case name %q", c.Name)
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
// derives from a stored JSON string, including escapes and surrogate pairs.
// Every case is cross-checked against encoding/json's own decoding, so the
// probe's length cannot drift from the length the authoritative read path
// measures.
func TestScanJSONStringDecodedLength(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.MeasureCases {
		t.Run(c.Name, func(t *testing.T) {
			decoded, end, ok := scanJSONString(c.Raw, 0)
			if !ok {
				t.Fatalf("scan rejected valid JSON string %q", c.Raw)
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

// TestJSONValueRawExtentMatchesAuthoritativePayload cross-checks the quantity the
// probe measures for the legacy raw `payload` encoding against the
// authoritative read path. UnmarshalJSON stores the raw value verbatim, so
// len(record.Payload) is the byte extent skipJSONValue reports, including quotes
// and escape bytes, and never the decoded length of a string payload. See the
// two-quantity contract at the top of retained_payload_size_probe.go. The
// encoding/json comparison pins the extent; the RetainedUnknownOf comparison
// pins it to the stored record length the projection-time backstop measures.
func TestJSONValueRawExtentMatchesAuthoritativePayload(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.RawExtentCases {
		t.Run(c.Name, func(t *testing.T) {
			end, ok := skipJSONValue(c.Value, 0)
			if !ok || end != len(c.Value) {
				t.Fatalf("skipJSONValue did not consume %q (end=%d ok=%v)", c.Value, end, ok)
			}
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(c.Value), &raw); err != nil {
				t.Fatalf("encoding/json rejected %q: %v", c.Value, err)
			}
			if len(raw) != c.Want {
				t.Fatalf("fixture want %d but len(json.RawMessage)=%d for %q", c.Want, len(raw), c.Value)
			}
			if end != len(raw) {
				t.Fatalf("probe raw extent %d != len(record.Payload) %d for %q", end, len(raw), c.Value)
			}
			extra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"s","recordIndex":0,"position":0}},"payload":` + c.Value + `}]}`
			entry := schema.SessionEntry{Harness: schema.HarnessCodex, Extra: &extra}
			records, err := RetainedUnknownOf(entry)
			if err != nil {
				t.Fatalf("authoritative decode rejected canonical raw payload %q: %v", c.Value, err)
			}
			if len(records) != 1 || len(records[0].Payload) != end {
				t.Fatalf("authoritative len(record.Payload)=%d != probe raw extent %d for %q", len(records[0].Payload), end, c.Value)
			}
		})
	}
}

// TestStoredRetainedPayloadExceedsTransferLimit pins the in-place refusal
// predicate: it measures a canonical stored payload, soundly declines to refuse
// records whose public coordinates are absent, envelopes outside the owned
// member set, ambiguous payload encodings, invalid harnesses and coordinates,
// and unrelated extension fields, and reaches the same verdict whichever member
// order carries the corruption. A case marked declined must report ok=false with
// over=false so the authoritative path owns the refusal; any other case must
// report ok=true.
func TestStoredRetainedPayloadExceedsTransferLimit(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.ProbeCases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Limit <= 0 {
				t.Fatalf("probe case %q needs a positive limit", c.Name)
			}
			harness := c.Harness
			if harness == "" {
				harness = fixtures.ProbeHarness
			}
			entryHarness := c.EntryHarness
			if entryHarness == "" {
				entryHarness = harness
			}
			over, ok := storedRetainedExtraExceedsTransferLimit(c.Extra, Harness(harness), Harness(entryHarness), c.Limit)
			if over != c.WantOver || ok != !c.Declined {
				t.Fatalf("probe verdict over=%v ok=%v, want over=%v declined=%v for %s", over, ok, c.WantOver, c.Declined, strings.TrimSpace(c.Extra))
			}
		})
	}
}
