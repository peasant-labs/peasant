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
		// DecoderAccepts pins that the authoritative decode accepts a
		// declined row (the probe is conservative there); DecoderRefuses
		// pins that it refuses one. Both are declined rows only.
		DecoderAccepts bool `yaml:"decoderAccepts"`
		DecoderRefuses bool `yaml:"decoderRefuses"`
	} `yaml:"probeCases"`
	NoMaterializationCases []struct {
		Name     string `yaml:"name"`
		Encoding string `yaml:"encoding"`
	} `yaml:"noMaterializationCases"`
}

// expandProbeTokens replaces fixture tokens that YAML cannot spell literally:
// {{FF}} is one raw 0xFF byte (invalid UTF-8), {{DEEP}} is a 12,001-deep raw
// array for the depth-budget row, {{DEEP9997}} is one past the probe's
// raw-payload budget (declined by the probe, accepted by the decoder),
// {{DEEP9996}} is exactly the probe's raw-payload budget (accepted by both),
// {{DEEP9999}} is exactly the extension budget (accepted by both), and
// {{DEEP10000}} is one past the extension budget (declined by the probe,
// refused by the decoder). The depths derive from retainedRawPayloadDepthBudget
// and strictRetainedExtensionDepth, so the boundary rows move with the
// constants instead of going stale when the shared depth constant changes.
func expandProbeTokens(extra string) string {
	if strings.Contains(extra, "{{FF}}") {
		extra = strings.ReplaceAll(extra, "{{FF}}", "\xff")
	}
	if strings.Contains(extra, "{{DEEP9997}}") {
		deep := strings.Repeat("[", retainedRawPayloadDepthBudget+1) + strings.Repeat("]", retainedRawPayloadDepthBudget+1)
		extra = strings.ReplaceAll(extra, "{{DEEP9997}}", deep)
	}
	if strings.Contains(extra, "{{DEEP9996}}") {
		deep := strings.Repeat("[", retainedRawPayloadDepthBudget) + strings.Repeat("]", retainedRawPayloadDepthBudget)
		extra = strings.ReplaceAll(extra, "{{DEEP9996}}", deep)
	}
	if strings.Contains(extra, "{{DEEP9999}}") {
		deep := strings.Repeat("[", strictRetainedExtensionDepth) + strings.Repeat("]", strictRetainedExtensionDepth)
		extra = strings.ReplaceAll(extra, "{{DEEP9999}}", deep)
	}
	if strings.Contains(extra, "{{DEEP10000}}") {
		deep := strings.Repeat("[", strictRetainedExtensionDepth+1) + strings.Repeat("]", strictRetainedExtensionDepth+1)
		extra = strings.ReplaceAll(extra, "{{DEEP10000}}", deep)
	}
	if strings.Contains(extra, "{{DEEP}}") {
		deep := strings.Repeat("[", 12001) + strings.Repeat("]", 12001)
		extra = strings.ReplaceAll(extra, "{{DEEP}}", deep)
	}
	return extra
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
		if c.DecoderAccepts && c.DecoderRefuses {
			t.Fatalf("probe case %q sets both decoderAccepts and decoderRefuses", c.Name)
		}
		if (c.DecoderAccepts || c.DecoderRefuses) && !c.Declined {
			t.Fatalf("probe case %q pins a decoder outcome without declined: true", c.Name)
		}
		if c.Declined && c.WantOver {
			t.Fatalf("probe case %q is declined yet wants over=true", c.Name)
		}
	}
	for _, c := range fixtures.NoMaterializationCases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate no-materialization case name %q", c.Name)
		}
		names[c.Name] = true
		switch c.Encoding {
		case "payloadText", "rawPayload", "escapedKind", "escapedKey", "escapedRootKey":
		default:
			t.Fatalf("no-materialization case %q encodes %q, must be payloadText, rawPayload, escapedKind, escapedKey, or escapedRootKey", c.Name, c.Encoding)
		}
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
// document-level strictness (invalid UTF-8, unpaired surrogates, depth, null
// text, leading-zero integers), and unrelated extension fields, and reaches
// the same verdict whichever member order carries the corruption. A case marked
// declined must report ok=false with over=false so the authoritative path owns
// the refusal; any other case must report ok=true.
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
			extra := expandProbeTokens(c.Extra)
			over, ok := storedRetainedExtraExceedsTransferLimit(extra, Harness(harness), Harness(entryHarness), c.Limit)
			if over != c.WantOver || ok != !c.Declined {
				t.Fatalf("probe verdict over=%v ok=%v, want over=%v declined=%v for %s", over, ok, c.WantOver, c.Declined, strings.TrimSpace(extra))
			}
		})
	}
}

// TestProbeAcceptedRowsMeetAuthoritativeDecoder is the differential oracle:
// every probe-accepted fixture row must be accepted by the authoritative read
// path, which enforces no transfer limit. A passing row proves the mirror is
// closed in the accept direction; the next drift is falsifiable by
// construction because adding a probe-accepted row that the decoder refuses
// fails here. It also pins measurement equivalence: the probe's over verdict
// must equal the backstop's comparison of the longest decoded payload against
// the case limit, so an under-measuring probe cannot pass by refusing late.
// Declined rows are excluded unless they pin a decoder outcome: decoderAccepts
// rows must decode cleanly (the probe is conservative there) and decoderRefuses
// rows must not, which pins the depth-boundary margins in both directions.
func TestProbeAcceptedRowsMeetAuthoritativeDecoder(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.ProbeCases {
		if c.Declined {
			if !c.DecoderAccepts && !c.DecoderRefuses {
				continue
			}
			t.Run(c.Name, func(t *testing.T) {
				harness := c.Harness
				if harness == "" {
					harness = fixtures.ProbeHarness
				}
				entryHarness := c.EntryHarness
				if entryHarness == "" {
					entryHarness = harness
				}
				extra := expandProbeTokens(c.Extra)
				entry := schema.SessionEntry{Harness: schema.Harness(entryHarness), EntryIndex: 0, Extra: &extra}
				_, retainedErr := RetainedUnknownOf(entry)
				_, collectErr := CollectRetainedUnknown([]schema.SessionEntry{entry}, schema.Harness(harness))
				if c.DecoderAccepts && (retainedErr != nil || collectErr != nil) {
					t.Fatalf("declined row %q pins decoderAccepts but the authoritative decode refused it: retained=%v collect=%v", c.Name, retainedErr, collectErr)
				}
				if c.DecoderRefuses && (retainedErr == nil || collectErr == nil) {
					t.Fatalf("declined row %q pins decoderRefuses but the authoritative decode accepted it: retained=%v collect=%v", c.Name, retainedErr, collectErr)
				}
			})
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			harness := c.Harness
			if harness == "" {
				harness = fixtures.ProbeHarness
			}
			entryHarness := c.EntryHarness
			if entryHarness == "" {
				entryHarness = harness
			}
			extra := expandProbeTokens(c.Extra)
			entry := schema.SessionEntry{Harness: schema.Harness(entryHarness), EntryIndex: 0, Extra: &extra}
			over, ok := storedRetainedExtraExceedsTransferLimit(extra, Harness(harness), Harness(entryHarness), c.Limit)
			if !ok {
				t.Fatalf("probe declined non-declined row %q", c.Name)
			}
			records, err := RetainedUnknownOf(entry)
			if err != nil {
				t.Fatalf("probe accepted %q but the authoritative decode refused it: %v", c.Name, err)
			}
			if _, err := CollectRetainedUnknown([]schema.SessionEntry{entry}, schema.Harness(harness)); err != nil {
				t.Fatalf("probe accepted %q but CollectRetainedUnknown refused it: %v", c.Name, err)
			}
			measuredOver := false
			for _, record := range records {
				if len(record.Payload) > c.Limit {
					measuredOver = true
				}
			}
			if over != measuredOver {
				t.Fatalf("probe over=%v but the longest decoded payload is over=%v at limit %d for %q", over, measuredOver, c.Limit, c.Name)
			}
		})
	}
}

// TestRetainedUnknownTransferLimitMatchesPublishedLabel pins the probe limit
// to the 8 MiB the refusal text publishes. The message is built from a literal
// while the enforced limit comes from defaults.SessionDetailDocumentCapBytes,
// so a defaults change that moves the limit without the label would otherwise
// stay green while the refusal overstates what it enforces.
func TestRetainedUnknownTransferLimitMatchesPublishedLabel(t *testing.T) {
	if retainedUnknownTransferLimitBytes != 8<<20 {
		t.Fatalf("probe limit is %d bytes, want 8 MiB (8388608) to match the published refusal label", retainedUnknownTransferLimitBytes)
	}
}
