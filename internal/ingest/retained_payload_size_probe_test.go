package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
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
	// TransferLimitMiB is the published retained-evidence transfer limit the
	// refusal label must state. It is an independent literal contract pin for
	// the production constant, not derived from it.
	TransferLimitMiB int `yaml:"transferLimitMiB"`
	// NoMaterializationPayloadBytes sizes the allocation proof's over-limit
	// payload. It is read here only so the strict fixture decoder accepts the
	// field; the proof's own loader reads it directly.
	NoMaterializationPayloadBytes int `yaml:"noMaterializationPayloadBytes"`
	MeasureCases                  []struct {
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
	CapBoundaryCases []struct {
		Name     string `yaml:"name"`
		Encoding string `yaml:"encoding"`
		Position string `yaml:"position"`
	} `yaml:"capBoundaryCases"`
}

// expandProbeTokens replaces fixture tokens that YAML cannot spell literally:
// {{FF}} is one raw 0xFF byte (invalid UTF-8), {{DEEP}} is a 12,001-deep raw
// array for the depth-budget row, {{DEEP9997}} is a 9,997-deep raw array one
// past the probe's raw-payload budget (declined by the probe, accepted by the
// decoder), {{DEEP9996}} is a 9,996-deep raw array exactly at the probe's
// raw-payload budget (accepted by both), {{DEEP9999}} is a 9,999-deep raw
// array exactly at the extension budget (accepted by both), and {{DEEP10000}}
// is a 10,000-deep raw array one past the extension budget (declined by the
// probe, refused by the decoder). The depths derive from localRawEvidenceDepth,
// the shared authoritative ceiling, rather than from the probe's own budget
// constants: narrowing a probe budget without moving the authority must turn
// the accept row red instead of silently retargeting it, while the values stay
// identical to the budget-relative spellings they replace.
func expandProbeTokens(extra string) string {
	if strings.Contains(extra, "{{FF}}") {
		extra = strings.ReplaceAll(extra, "{{FF}}", "\xff")
	}
	if strings.Contains(extra, "{{DEEP9997}}") {
		deep := strings.Repeat("[", localRawEvidenceDepth-3) + strings.Repeat("]", localRawEvidenceDepth-3)
		extra = strings.ReplaceAll(extra, "{{DEEP9997}}", deep)
	}
	if strings.Contains(extra, "{{DEEP9996}}") {
		deep := strings.Repeat("[", localRawEvidenceDepth-4) + strings.Repeat("]", localRawEvidenceDepth-4)
		extra = strings.ReplaceAll(extra, "{{DEEP9996}}", deep)
	}
	if strings.Contains(extra, "{{DEEP9999}}") {
		deep := strings.Repeat("[", localRawEvidenceDepth-1) + strings.Repeat("]", localRawEvidenceDepth-1)
		extra = strings.ReplaceAll(extra, "{{DEEP9999}}", deep)
	}
	if strings.Contains(extra, "{{DEEP10000}}") {
		deep := strings.Repeat("[", localRawEvidenceDepth) + strings.Repeat("]", localRawEvidenceDepth)
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
		if c.Declined && !(c.DecoderAccepts != c.DecoderRefuses) {
			t.Fatalf("probe case %q is declined and must set exactly one of decoderAccepts or decoderRefuses", c.Name)
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
		if !IsRetainedNoMaterializationEncoding(c.Encoding) {
			t.Fatalf("no-materialization case %q encodes %q, must be %s", c.Name, c.Encoding, strings.Join(RetainedNoMaterializationEncodings, ", "))
		}
	}
	for _, c := range fixtures.CapBoundaryCases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate cap-boundary case name %q", c.Name)
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
// Every declined row pins its authoritative outcome: decoderAccepts rows must
// decode cleanly through both RetainedUnknownOf and CollectRetainedUnknown
// (the probe is conservative there), and decoderRefuses rows must be refused
// by CollectRetainedUnknown, the projection path ProjectRetainedUnknown takes
// after a decline. The per-entry RetainedUnknownOf still accepts legacy shapes
// without public coordinates and envelopes whose harness matches the entry but
// not the export, so those rows pin decoderRefuses on the projection refusal
// while the entry decode accepts them.
func TestProbeAcceptedRowsMeetAuthoritativeDecoder(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	for _, c := range fixtures.ProbeCases {
		if c.Declined {
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
				if c.DecoderRefuses && collectErr == nil {
					t.Fatalf("declined row %q pins decoderRefuses but CollectRetainedUnknown accepted it", c.Name)
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

// TestProbeMemberMasksFit guards the 64-bit duplicate-detection masks: every
// owned key set must stay below 64 names, because jsonMemberBit declines a
// member at index 64 or beyond instead of issuing a zero bit that would
// silently disable duplicate and required-member detection for it.
func TestProbeMemberMasksFit(t *testing.T) {
	for _, keys := range [][]string{retainedEnvelopeKeys, retainedPositionKeys, retainedPublicKeys} {
		if len(keys) >= 64 {
			t.Fatalf("owned key set %q holds %d names, reaching the 64-bit duplicate-detection mask width", keys, len(keys))
		}
	}
}

// TestProbeRequiredMemberMaskResolves pins the required-member mask list to
// the closed envelope key set: every required name must resolve there, and an
// unowned name must fail the mask closed so the probe declines rather than
// silently shrinking the required set.
func TestProbeRequiredMemberMaskResolves(t *testing.T) {
	required := []string{"harness", "namespace", "kind", "position"}
	for _, name := range required {
		if _, owned := jsonMemberBit(retainedEnvelopeKeys, name); !owned {
			t.Fatalf("required envelope member %q does not resolve in retainedEnvelopeKeys %q", name, retainedEnvelopeKeys)
		}
	}
	if _, ok := jsonMemberMask(retainedEnvelopeKeys, required...); !ok {
		t.Fatalf("required envelope mask does not resolve in retainedEnvelopeKeys %q", retainedEnvelopeKeys)
	}
	if _, ok := jsonMemberMask(retainedEnvelopeKeys, append(append([]string{}, required...), "missingMember")...); ok {
		t.Fatalf("jsonMemberMask accepted an unowned name, must fail closed")
	}
	if _, ok := jsonMemberMask(retainedPublicKeys, retainedPublicKeys...); !ok {
		t.Fatalf("public coordinate mask does not resolve in retainedPublicKeys %q", retainedPublicKeys)
	}
}

// TestRetainedUnknownTransferLimitMatchesPublishedLabel pins the enforced probe
// limit, its defaults source, and the published refusal label. The production
// constant aliases the retained-unknown per-payload cap
// (defaults.RetainedUnknownPayloadCapBytes), which mirrors the schema
// validator's inner 8 MiB/depth-64 per-payload bound, and the refusal renders
// that same constant through the one human-byte-size formatter. Two independent
// guards protect the alias: it must equal defaults.RetainedUnknownPayloadCapBytes
// (the source), and it must equal the fixture's literal transferLimitMiB
// contract value, which is not derived from the production constant. A source
// revert to a decoupled literal fails both even though the message and the
// enforcement would still agree with each other. The full expected wording is
// asserted, including the larger-transfers clause, so a formatting-source or
// wording drift is caught too.
func TestRetainedUnknownTransferLimitMatchesPublishedLabel(t *testing.T) {
	fixtures := loadRetainedPayloadSizeProbeFixtures(t)
	if fixtures.TransferLimitMiB <= 0 {
		t.Fatalf("committed fixture %s needs a positive transferLimitMiB", retainedPayloadSizeProbeFixturePath)
	}
	if retainedUnknownTransferLimitBytes != defaults.RetainedUnknownPayloadCapBytes {
		t.Fatalf("probe limit is %d bytes, not the retained-unknown per-payload cap %d", retainedUnknownTransferLimitBytes, defaults.RetainedUnknownPayloadCapBytes)
	}
	wantBytes := fixtures.TransferLimitMiB << 20
	if retainedUnknownTransferLimitBytes != wantBytes {
		t.Fatalf("probe limit is %d bytes, want the fixture's %d MiB (%d)", retainedUnknownTransferLimitBytes, fixtures.TransferLimitMiB, wantBytes)
	}
	wantLabel := fmt.Sprintf("export retained evidence: payload exceeds the published %s transfer limit; complete source data remains stored locally; nothing exported or uploaded; use a receiver and contract supporting larger transfers when available", defaults.HumanByteSize(int64(wantBytes)))
	if got := retainedUnknownTransferLimitError(retainedUnknownTransferLimitBytes).Error(); got != wantLabel {
		t.Fatalf("published transfer refusal does not state the enforced limit:\n got: %s\nwant: %s", got, wantLabel)
	}
}
