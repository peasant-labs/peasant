package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// oversizedPayload builds a valid JSON string payload of exactly size bytes.
func oversizedPayload(size int) json.RawMessage {
	return json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
}

// retainedRefusalAllocationBound states the no-copy invariant shared by the
// allocation proof and the boundary test: a transfer refusal allocates
// nothing proportional to the payload. The observed deltas are about 1.24-1.46
// KiB, so 1 MiB stays about 700x+ above the behavior while catching a
// regression that copies even a fraction of a large payload.
const retainedRefusalAllocationBound = 1 << 20

func publicPosition(record int64) ingest.UnknownSourcePosition {
	return ingest.UnknownSourcePosition{
		Line:   int(record) + 1,
		Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: record, Position: record},
	}
}

func carrierEntry(t *testing.T, position ingest.UnknownSourcePosition, payload json.RawMessage) schema.SessionEntry {
	t.Helper()
	record, err := ingest.NewRetainedUnknown(schema.HarnessCodex, "record", "future", position, payload)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ingest.RetainedUnknownEntry(testutil.TestSessionUUID, 0, record)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// TestProjectRetainedUnknownRefusesOversizedPayloadBeforeMaterializing proves
// the transfer refusal is decided from the stored bytes: an oversized payload
// is refused without decoding or copying the payload itself. The allocated-byte
// delta is measured for the refusal call alone, after the entry already exists
// in memory, and asserted far below the payload size. Two bounds apply: the
// payload/4 calibration bound from the probe-off mutation story, and the
// 1 MiB constant bound that states the real invariant, that the refusal
// allocates nothing proportional to the payload. The observed deltas are
// about 1.24-1.46 KiB, so the constant bound stays about 700x+ above the
// behavior while catching a regression that copies even a fraction of a
// 64 MiB payload. The closed encoding set is driven by
// the noMaterializationCases fixture, including the escaped-owned-string,
// escaped-owned-key, and escaped-evidence-root-key shapes that once lost the
// early refusal. Every encoding carries a JSON-valid payload, so with the
// probe forced off each row reaches the projection-time size backstop and
// fails on the allocation bound rather than on the refusal text.
//
// Calibration: forcing the in-place probe off makes this same 64 MiB payload
// allocate ~3.5-3.8 GB depending on the encoding before the refusal, so the
// payload/4 bound sits about 208-228 times (over two orders of magnitude)
// below the allocation the probe removes. The calibration is therefore
// reproducible with the one-line probe-off mutation documented here, not from
// this committed test alone.
func TestProjectRetainedUnknownRefusesOversizedPayloadBeforeMaterializing(t *testing.T) {
	const payloadBytes = 64 << 20
	overDigits := `"` + strings.Repeat("1", payloadBytes-2) + `"`
	rawLiteral := `"` + strings.Repeat("x", payloadBytes-2) + `"`
	rawExtra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"source-0","recordIndex":0,"position":0}},"payload":` + rawLiteral + `}]}`
	escapedKeyExtra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"source-0","recordIndex":0,"position":0}},"pay\u006CoadText":` + overDigits + `}]}`
	escapedRootKeyExtra := `{"retainedUnk\u006Eown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"source-0","recordIndex":0,"position":0}},"payloadText":` + overDigits + `}]}`
	escapedRecord, err := ingest.NewRetainedUnknown(schema.HarnessCodex, "record", "future&more", publicPosition(0), oversizedPayload(payloadBytes))
	if err != nil {
		t.Fatal(err)
	}
	escapedEntry, err := ingest.RetainedUnknownEntry(testutil.TestSessionUUID, 0, escapedRecord)
	if err != nil {
		t.Fatal(err)
	}
	if escapedEntry.Extra == nil || !strings.Contains(*escapedEntry.Extra, `\u0026`) {
		t.Fatal("escapedKind entry does not carry an escaped owned string in its stored form, so it no longer covers the shape it names")
	}
	escapedKeyEntry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &escapedKeyExtra}
	if !strings.Contains(*escapedKeyEntry.Extra, `\u006C`) {
		t.Fatal("escapedKey entry does not carry an escaped owned key in its stored form, so it no longer covers the shape it names")
	}
	escapedRootKeyEntry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &escapedRootKeyExtra}
	if !strings.Contains(*escapedRootKeyEntry.Extra, `\u006Eown`) {
		t.Fatal("escapedRootKey entry does not carry an escaped evidence root key in its stored form, so it no longer covers the shape it names")
	}
	fixtureCases := loadNoMaterializationCases(t)
	entries := map[string]schema.SessionEntry{
		"payloadText":    carrierEntry(t, publicPosition(0), oversizedPayload(payloadBytes)),
		"rawPayload":     {Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &rawExtra},
		"escapedKind":    escapedEntry,
		"escapedKey":     escapedKeyEntry,
		"escapedRootKey": escapedRootKeyEntry,
	}
	for _, encoding := range ingest.RetainedNoMaterializationEncodings {
		if _, ok := entries[encoding]; !ok {
			t.Fatalf("allocation proof has no entry for encoding %q, must cover %s", encoding, strings.Join(ingest.RetainedNoMaterializationEncodings, ", "))
		}
	}
	for _, c := range fixtureCases {
		t.Run(c.Name, func(t *testing.T) {
			entry, ok := entries[c.Encoding]
			if !ok {
				t.Fatalf("no-materialization case %q encodes %q, must be %s", c.Name, c.Encoding, strings.Join(ingest.RetainedNoMaterializationEncodings, ", "))
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
			runtime.ReadMemStats(&after)

			if err == nil || !strings.Contains(err.Error(), "8 MiB transfer limit") {
				t.Fatalf("oversized payload was not refused with the transfer limit: %v", err)
			}
			delta := int64(after.TotalAlloc - before.TotalAlloc)
			t.Logf("refusal allocated %d bytes for a %d-byte payload", delta, payloadBytes)
			if bound := int64(payloadBytes / 4); delta >= bound {
				t.Fatalf("refusal allocated %d bytes, not far below the %d-byte payload (bound %d)", delta, payloadBytes, bound)
			}
			if delta >= retainedRefusalAllocationBound {
				t.Fatalf("refusal allocated %d bytes, above the %d-byte constant bound: the refusal must not copy the %d-byte payload", delta, retainedRefusalAllocationBound, payloadBytes)
			}
		})
	}
}

//go:embed testdata/retained_payload_size_probe.yaml
var retainedPayloadSizeProbeFixtureData []byte

const retainedPayloadSizeProbeFixturePath = "internal/ingest/testdata/retained_payload_size_probe.yaml"

// loadNoMaterializationCases loads the closed encoding set for the allocation
// proof and enforces its required-name manifest: every name in requiredNames
// that starts with no_materialization_ must appear here, so deleting an
// encoding fails while adding one is allowed until its name is required.
func loadNoMaterializationCases(t *testing.T) []struct {
	Name     string `yaml:"name"`
	Encoding string `yaml:"encoding"`
} {
	t.Helper()
	var raw struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string `yaml:"name"`
			Encoding string `yaml:"encoding"`
		} `yaml:"noMaterializationCases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(retainedPayloadSizeProbeFixtureData))
	decoder.KnownFields(false)
	if err := decoder.Decode(&raw); err != nil {
		t.Fatalf("decode committed fixture %s: %v", retainedPayloadSizeProbeFixturePath, err)
	}
	if len(raw.Cases) == 0 {
		t.Fatalf("committed fixture %s needs noMaterializationCases", retainedPayloadSizeProbeFixturePath)
	}
	names := make(map[string]bool)
	for _, c := range raw.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate no-materialization case name %q", c.Name)
		}
		names[c.Name] = true
		if !ingest.IsRetainedNoMaterializationEncoding(c.Encoding) {
			t.Fatalf("no-materialization case %q encodes %q, must be %s", c.Name, c.Encoding, strings.Join(ingest.RetainedNoMaterializationEncodings, ", "))
		}
	}
	for _, name := range raw.RequiredNames {
		if !strings.HasPrefix(name, "no_materialization_") {
			continue
		}
		if !names[name] {
			t.Fatalf("required fixture %q missing from %s", name, retainedPayloadSizeProbeFixturePath)
		}
	}
	covered := make(map[string]bool, len(raw.Cases))
	for _, c := range raw.Cases {
		covered[c.Encoding] = true
	}
	for _, encoding := range ingest.RetainedNoMaterializationEncodings {
		if !covered[encoding] {
			t.Fatalf("allocation proof never exercises encoding %q", encoding)
		}
	}
	return raw.Cases
}

// loadCapBoundaryCases loads the at-cap boundary matrix for
// TestProjectRetainedUnknownAcceptsAtCapPayloads and enforces its
// required-name manifest: every name in requiredNames that starts with
// cap_boundary_ must appear here, so shrinking the matrix fails while adding
// a row is allowed until its name is required.
func loadCapBoundaryCases(t *testing.T) []struct {
	Name     string `yaml:"name"`
	Encoding string `yaml:"encoding"`
	Position string `yaml:"position"`
} {
	t.Helper()
	var raw struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string `yaml:"name"`
			Encoding string `yaml:"encoding"`
			Position string `yaml:"position"`
		} `yaml:"capBoundaryCases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(retainedPayloadSizeProbeFixtureData))
	decoder.KnownFields(false)
	if err := decoder.Decode(&raw); err != nil {
		t.Fatalf("decode committed fixture %s: %v", retainedPayloadSizeProbeFixturePath, err)
	}
	if len(raw.Cases) == 0 {
		t.Fatalf("committed fixture %s needs capBoundaryCases", retainedPayloadSizeProbeFixturePath)
	}
	names := make(map[string]bool)
	for _, c := range raw.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate cap-boundary case name %q", c.Name)
		}
		names[c.Name] = true
		switch c.Encoding {
		case "payloadText", "rawPayload":
		default:
			t.Fatalf("cap-boundary case %q encodes %q, must be payloadText or rawPayload", c.Name, c.Encoding)
		}
		switch c.Position {
		case "atCap", "oneOver":
		default:
			t.Fatalf("cap-boundary case %q sits %q, must be atCap or oneOver", c.Name, c.Position)
		}
	}
	for _, name := range raw.RequiredNames {
		if !strings.HasPrefix(name, "cap_boundary_") {
			continue
		}
		if !names[name] {
			t.Fatalf("required fixture %q missing from %s", name, retainedPayloadSizeProbeFixturePath)
		}
	}
	return raw.Cases
}

// TestProjectRetainedUnknownRefusalMessageStable pins the refusal text through
// the public entry point.
func TestProjectRetainedUnknownRefusalMessageStable(t *testing.T) {
	entry := carrierEntry(t, publicPosition(0), oversizedPayload(8<<20+64))
	_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
	if err == nil {
		t.Fatal("oversized payload was not refused")
	}
	if got, want := err.Error(), "export retained evidence: payload exceeds the published 8 MiB transfer limit; complete source data remains stored locally; nothing exported or uploaded; use a receiver and contract supporting larger transfers when available"; got != want {
		t.Fatalf("transfer refusal text changed:\n got: %s\nwant: %s", got, want)
	}
}

// TestProjectRetainedUnknownAcceptsAtCapPayloads pins the strict boundary at
// the published cap through the real entry: a payload of exactly 8 MiB is
// accepted for both encodings, and one byte over is refused without
// materializing. The matrix lives in the capBoundaryCases fixture with a
// required-name manifest; only this test composes the boundary with the
// production call site, so a drift that passes limit-1 (refusing legitimate
// at-cap records) fails here while the fixture rows stay green. The 8 MiB
// literals are built in Go; the allocation bound is asserted on the refusal
// side of each encoding.
func TestProjectRetainedUnknownAcceptsAtCapPayloads(t *testing.T) {
	const capBytes = 8 << 20
	// A payloadText value carries two quote bytes around its decoded text, so
	// capBytes-2 content bytes decode to exactly the cap; a legacy raw value
	// is measured by extent, so the same spelling is exactly the cap there.
	textAtCap := `"` + strings.Repeat("1", capBytes-2) + `"`
	textOneOver := `"` + strings.Repeat("1", capBytes-1) + `"`
	rawAtCap := textAtCap
	rawOneOver := textOneOver
	rawExtra := func(payload string) string {
		return `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"s","recordIndex":0,"position":0}},"payload":` + payload + `}]}`
	}
	atCapRawExtra := rawExtra(rawAtCap)
	oneOverRawExtra := rawExtra(rawOneOver)
	entries := map[string]schema.SessionEntry{
		"payloadText/atCap":   carrierEntry(t, publicPosition(0), json.RawMessage(textAtCap)),
		"payloadText/oneOver": carrierEntry(t, publicPosition(0), json.RawMessage(textOneOver)),
		"rawPayload/atCap":    {Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &atCapRawExtra},
		"rawPayload/oneOver":  {Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &oneOverRawExtra},
	}
	for _, c := range loadCapBoundaryCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			entry, ok := entries[c.Encoding+"/"+c.Position]
			if !ok {
				t.Fatalf("cap-boundary case %q encodes %q at %q, must be payloadText/rawPayload at atCap/oneOver", c.Name, c.Encoding, c.Position)
			}
			if c.Position == "atCap" {
				projected, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
				if err != nil || len(projected) != 1 {
					t.Fatalf("at-cap payload was not accepted: records=%d err=%v", len(projected), err)
				}
				if len(projected[0].Payload) != capBytes {
					t.Fatalf("at-cap projected payload is %d bytes, want exactly %d", len(projected[0].Payload), capBytes)
				}
				return
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
			runtime.ReadMemStats(&after)
			if err == nil || !strings.Contains(err.Error(), "8 MiB transfer limit") {
				t.Fatalf("one-over payload was not refused for size: %v", err)
			}
			if delta := int64(after.TotalAlloc - before.TotalAlloc); delta >= retainedRefusalAllocationBound {
				t.Fatalf("one-over refusal allocated %d bytes, above the %d-byte bound", delta, retainedRefusalAllocationBound)
			}
		})
	}
}

//go:embed testdata/retained_projection_precedence.yaml
var retainedProjectionPrecedenceFixtureData []byte

const retainedProjectionPrecedenceFixturePath = "internal/ingest/testdata/retained_projection_precedence.yaml"

type retainedProjectionPrecedenceFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	// PayloadBytes is the total byte size of the over-limit payload literal
	// the test builds per token; the 8 MiB payload cannot be written literally.
	PayloadBytes int `yaml:"payloadBytes"`
	Cases        []struct {
		Name   string   `yaml:"name"`
		Extras []string `yaml:"extras"`
		// Want is one of integrity, legacy, or size.
		Want          string `yaml:"want"`
		ExportHarness string `yaml:"exportHarness"`
		EntryHarness  string `yaml:"entryHarness"`
		// Authoritative names the CollectRetainedUnknown outcome for size
		// rows: accepts for the motivating canonical path, integrity for
		// the named deliberate precedences. Required when want is size.
		Authoritative string `yaml:"authoritative"`
		// DeliberateSizePrecedence marks the named deliberate set: size
		// rows the authoritative path would refuse for integrity, kept on
		// size because deciding them needs the decode or global state the
		// probe avoids. Only size rows with authoritative integrity may set it.
		DeliberateSizePrecedence bool `yaml:"deliberateSizePrecedence"`
	} `yaml:"precedenceCases"`
}

// loadRetainedProjectionPrecedenceFixtures loads the committed precedence
// fixture and enforces the requiredNames manifest: the list must name every
// case so deleting or renaming one fails, while adding a case is allowed until
// its name is added here.
func loadRetainedProjectionPrecedenceFixtures(t *testing.T) retainedProjectionPrecedenceFixtures {
	t.Helper()
	var fixtures retainedProjectionPrecedenceFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(retainedProjectionPrecedenceFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode committed fixture %s: %v", retainedProjectionPrecedenceFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("committed fixture %s must contain exactly one YAML document, trailing decode: %v", retainedProjectionPrecedenceFixturePath, err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("missing or duplicate precedence case name %q", c.Name)
		}
		names[c.Name] = true
		switch c.Want {
		case "integrity", "legacy", "size":
		default:
			t.Fatalf("precedence case %q wants %q, must be integrity, legacy, or size", c.Name, c.Want)
		}
		switch c.Authoritative {
		case "", "integrity", "legacy", "accepts":
		default:
			t.Fatalf("precedence case %q has authoritative %q, must be integrity, legacy, accepts, or empty", c.Name, c.Authoritative)
		}
		if c.Want == "size" {
			switch c.Authoritative {
			case "integrity", "accepts":
			default:
				t.Fatalf("precedence size case %q needs authoritative integrity or accepts, got %q", c.Name, c.Authoritative)
			}
			if c.DeliberateSizePrecedence && c.Authoritative != "integrity" {
				t.Fatalf("precedence case %q marks deliberateSizePrecedence but authoritative is %q, must be integrity", c.Name, c.Authoritative)
			}
			if !c.DeliberateSizePrecedence && c.Authoritative != "accepts" {
				t.Fatalf("precedence size case %q without deliberateSizePrecedence must authoritative accepts, got %q", c.Name, c.Authoritative)
			}
		} else if c.DeliberateSizePrecedence {
			t.Fatalf("precedence case %q marks deliberateSizePrecedence but want is %q, only size rows may be deliberate", c.Name, c.Want)
		}
		if len(c.Extras) == 0 {
			t.Fatalf("precedence case %q needs at least one stored Extra", c.Name)
		}
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required fixture %q missing from %s", name, retainedProjectionPrecedenceFixturePath)
		}
	}
	return fixtures
}

// TestProjectRetainedUnknownPrecedence documents which refusals outrank the
// transfer refusal after the early decision, and that the outcome does not
// depend on member or entry order. Every integrity row that can carry a
// payload carries an over-limit payload whose decoded text is itself a valid
// JSON document, so the authoritative path reaches the named shape refusal
// rather than stopping at payload content: a passing row proves the probe
// declined and the authoritative error surfaced, not merely that some
// integrity error appeared. The missing-payload-encoding and lone-surrogate
// rows cannot carry an over-limit payload, so they pin the refusal type and
// the authoritative outcome only.
//
// Missing traversal coordinates and every envelope the in-place probe declines
// keep their existing errors, including corrupt coordinates, document-level
// strictness (invalid UTF-8, unpaired surrogates, depth, null text,
// leading-zero integers), trailing content, lax grammar, and an empty evidence
// array in any member or entry order. A record whose coordinates order
// corruptly (position < recordIndex) keeps the position refusal: the probe
// mirrors the ordering rule in place.
//
// Three deliberate size precedences are asserted as the intended consequence of
// refusing before materialization: a payloadText whose decoded content the
// shared scanner refuses (invalid syntax, repeated object member names, a
// double-escaped unpaired escape the stored-level pre-scan cannot see, or
// depth), a legacy raw payload value with invalid JSON syntax, including
// repeated object member names (depth beyond the payload budget declines
// instead), and cross-record ordering or pointer uniqueness, which the
// authoritative path decides over the coordinate-sorted record set. Each size
// row also asserts its authoritative outcome: canonical rows are accepted by
// CollectRetainedUnknown apart from size, deliberate rows carry an integrity
// error there.
// deliberateSizePrecedenceNames is the closed deliberate set named in the
// probe header: size rows the authoritative path would refuse for integrity,
// kept on size because deciding them needs the decode or global state the
// probe avoids. The precedence runner asserts this membership exactly, so a
// new deliberate class cannot be added (or an existing one removed) with
// every test still green.
var deliberateSizePrecedenceNames = []string{
	"oversized_malformed_payload_content_is_refused_for_size",
	"oversized_duplicate_key_payload_text_is_refused_for_size",
	"oversized_raw_invalid_syntax_is_refused_for_size",
	"oversized_raw_duplicate_key_is_refused_for_size",
	"oversized_duplicate_pointers_across_records_is_refused_for_size",
	"oversized_non_monotonic_positions_across_records_is_refused_for_size",
	"oversized_deep_payload_text_with_over_companion_is_refused_for_size",
	"oversized_over_entry_before_deep_text_entry_is_refused_for_size",
	"oversized_double_escaped_surrogate_content_is_refused_for_size",
}

// overDeepPayloadTextDepth is one beyond the shared local depth ceiling the
// probe header names: a payloadText whose decoded text nests this deep is
// measured by the probe but refused for depth by the authoritative path.
const overDeepPayloadTextDepth = 10001

func TestProjectRetainedUnknownPrecedence(t *testing.T) {
	fixtures := loadRetainedProjectionPrecedenceFixtures(t)
	if fixtures.PayloadBytes <= 0 {
		t.Fatalf("precedence fixture %s needs a positive payloadBytes", retainedProjectionPrecedenceFixturePath)
	}
	marked := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.DeliberateSizePrecedence {
			marked[c.Name] = true
		}
	}
	if len(marked) != len(deliberateSizePrecedenceNames) {
		t.Fatalf("deliberate size-precedence set has %d rows, want exactly %d %v", len(marked), len(deliberateSizePrecedenceNames), deliberateSizePrecedenceNames)
	}
	for _, name := range deliberateSizePrecedenceNames {
		if !marked[name] {
			t.Fatalf("deliberate size-precedence row %q missing; the set must be exactly %v", name, deliberateSizePrecedenceNames)
		}
	}
	over := `"` + strings.Repeat("1", fixtures.PayloadBytes-2) + `"`
	overInvalid := `"` + strings.Repeat("x", fixtures.PayloadBytes-2) + `"`
	overRawInvalid := `[` + strings.Repeat(",", fixtures.PayloadBytes-2) + `]`
	// overDuplicateKey is a JSON string whose decoded text is valid JSON with
	// a repeated member name over the limit: the probe measures the decoded
	// length without validating content, while the authoritative path refuses
	// the duplicate key for integrity. overRawDuplicateKey is the same shape
	// as a legacy raw value, measured by extent. Both decoded forms are
	// exactly payloadBytes long, matching the other over-limit tokens.
	dupStoredPrefix := `{\"a\":1,\"a\":2,\"p\":\"`
	dupDecodedPrefix := `{"a":1,"a":2,"p":"`
	dupDigits := strings.Repeat("1", fixtures.PayloadBytes-len(dupDecodedPrefix)-2)
	dupDecoded := dupDecodedPrefix + dupDigits + `"}`
	overDuplicateKey := `"` + dupStoredPrefix + dupDigits + `\"}` + `"`
	overRawDuplicateKey := dupDecoded
	deep := strings.Repeat("[", 12001) + strings.Repeat("]", 12001)
	overDeepText := `"` + strings.Repeat("[", overDeepPayloadTextDepth) + strings.Repeat("1", fixtures.PayloadBytes) + strings.Repeat("]", overDeepPayloadTextDepth) + `"`
	// overDoubleEscaped is a stored payloadText literal whose decoded text is
	// exactly payloadBytes long and carries an unpaired surrogate escape the
	// stored-level pre-scan cannot see: the escape stays double-escaped in the
	// stored spelling, so only the decoded content trips the shared scanner.
	// The thirteen framing bytes around the content (ten opening, three
	// closing) make the stored literal five bytes longer than the decoded
	// content, which the precedence row below pins as a deliberate size
	// outcome.
	padDoubleEscaped := strings.Repeat("1", fixtures.PayloadBytes-8)
	overDoubleEscaped := `"\"\\uD800` + padDoubleEscaped + `\""`
	if len(overDoubleEscaped) != fixtures.PayloadBytes+5 {
		t.Fatalf("built double-escaped literal is %d bytes, want payloadBytes %d plus 5 framing bytes", len(overDoubleEscaped), fixtures.PayloadBytes)
	}
	if len(over) != fixtures.PayloadBytes {
		t.Fatalf("built over-limit literal is %d bytes, want payloadBytes %d", len(over), fixtures.PayloadBytes)
	}
	if len(dupDecoded) != fixtures.PayloadBytes || len(overRawDuplicateKey) != fixtures.PayloadBytes {
		t.Fatalf("built duplicate-key literals are %d/%d bytes, want payloadBytes %d", len(dupDecoded), len(overRawDuplicateKey), fixtures.PayloadBytes)
	}
	if len(overDeepText) != fixtures.PayloadBytes+2*overDeepPayloadTextDepth+2 {
		t.Fatalf("built over-limit deep text is %d bytes, want payloadBytes %d plus %d brackets and 2 quotes", len(overDeepText), fixtures.PayloadBytes, 2*overDeepPayloadTextDepth)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			exportHarness := c.ExportHarness
			if exportHarness == "" {
				exportHarness = string(schema.HarnessCodex)
			}
			entryHarness := c.EntryHarness
			if entryHarness == "" {
				entryHarness = string(schema.HarnessCodex)
			}
			entries := make([]schema.SessionEntry, 0, len(c.Extras))
			for index, template := range c.Extras {
				extra := strings.ReplaceAll(template, "{{OVER}}", over)
				extra = strings.ReplaceAll(extra, "{{OVER_INVALID}}", overInvalid)
				extra = strings.ReplaceAll(extra, "{{OVER_RAW_INVALID}}", overRawInvalid)
				extra = strings.ReplaceAll(extra, "{{OVER_DUPLICATE_KEY}}", overDuplicateKey)
				extra = strings.ReplaceAll(extra, "{{OVER_RAW_DUPLICATE_KEY}}", overRawDuplicateKey)
				extra = strings.ReplaceAll(extra, "{{OVER_DEEP_TEXT}}", overDeepText)
				extra = strings.ReplaceAll(extra, "{{OVER_DOUBLE_ESCAPED_SURROGATE}}", overDoubleEscaped)
				extra = strings.ReplaceAll(extra, "{{FF}}", "\xff")
				extra = strings.ReplaceAll(extra, "{{DEEP}}", deep)
				entries = append(entries, schema.SessionEntry{Harness: schema.Harness(entryHarness), EntryIndex: index, Extra: &extra})
			}
			_, err := ingest.ProjectRetainedUnknown(entries, schema.Harness(exportHarness))
			switch c.Want {
			case "integrity":
				var target *ingest.EvidenceIntegrityError
				if !errors.As(err, &target) {
					t.Fatalf("integrity refusal was masked by the transfer refusal: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "8 MiB transfer limit") {
					t.Fatalf("transfer refusal replaced the integrity refusal: %v", err)
				}
			case "legacy":
				if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
					t.Fatalf("missing coordinates did not outrank the transfer refusal: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "8 MiB transfer limit") {
					t.Fatalf("transfer refusal replaced the legacy coordinate refusal: %v", err)
				}
			case "size":
				if err == nil || !strings.Contains(err.Error(), "8 MiB transfer limit") {
					t.Fatalf("oversized record was not refused for size: %v", err)
				}
			}
			// Assert the authoritative outcome too, so the deliberate claim
			// stays pinned: canonical size rows are accepted apart from size,
			// deliberate size rows carry an integrity error there, and every
			// integrity or legacy row meets the same refusal without the
			// transfer limit in the way.
			_, authoritativeErr := ingest.CollectRetainedUnknown(entries, schema.Harness(exportHarness))
			switch c.Want {
			case "integrity":
				var target *ingest.EvidenceIntegrityError
				if !errors.As(authoritativeErr, &target) {
					t.Fatalf("authoritative path did not name integrity for %q: %v", c.Name, authoritativeErr)
				}
			case "legacy":
				if !errors.Is(authoritativeErr, ingest.ErrUnknownPositionUnavailable) {
					t.Fatalf("authoritative path did not name legacy absence for %q: %v", c.Name, authoritativeErr)
				}
			case "size":
				switch c.Authoritative {
				case "accepts":
					if authoritativeErr != nil {
						t.Fatalf("authoritative path refused canonical size row %q: %v", c.Name, authoritativeErr)
					}
				case "integrity":
					var target *ingest.EvidenceIntegrityError
					if !errors.As(authoritativeErr, &target) {
						t.Fatalf("authoritative path did not name integrity for deliberate size row %q: %v", c.Name, authoritativeErr)
					}
				}
			}
		})
	}
}
