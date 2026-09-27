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
// in memory, and asserted far below the payload size. A bound of payload/4 is
// generous for allocator noise while still an order of magnitude below what
// decoding the payload would allocate. The closed encoding set is driven by
// the noMaterializationCases fixture, including the escaped-owned-string shape
// that once lost the early refusal.
//
// Calibration: forcing the in-place probe off makes this same 64 MiB payload
// allocate ~3.83 GB before the refusal, so the payload/4 bound sits about 230
// times (over two orders of magnitude) below the allocation the probe removes.
// The bound is therefore reproducible from this committed test alone, not from
// an unrecorded local measurement.
func TestProjectRetainedUnknownRefusesOversizedPayloadBeforeMaterializing(t *testing.T) {
	const payloadBytes = 64 << 20
	rawLiteral := `"` + strings.Repeat("x", payloadBytes-2) + `"`
	rawExtra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"source-0","recordIndex":0,"position":0}},"payload":` + rawLiteral + `}]}`
	escapedRecord, err := ingest.NewRetainedUnknown(schema.HarnessCodex, "record", "future&more", publicPosition(0), oversizedPayload(payloadBytes))
	if err != nil {
		t.Fatal(err)
	}
	escapedEntry, err := ingest.RetainedUnknownEntry(testutil.TestSessionUUID, 0, escapedRecord)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCases := loadNoMaterializationCases(t)
	entries := map[string]schema.SessionEntry{
		"payloadText": carrierEntry(t, publicPosition(0), oversizedPayload(payloadBytes)),
		"rawPayload":  {Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &rawExtra},
		"escapedKind": escapedEntry,
	}
	for _, c := range fixtureCases {
		t.Run(c.Name, func(t *testing.T) {
			entry, ok := entries[c.Encoding]
			if !ok {
				t.Fatalf("no-materialization case %q encodes %q, must be payloadText, rawPayload, or escapedKind", c.Name, c.Encoding)
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
		switch c.Encoding {
		case "payloadText", "rawPayload", "escapedKind":
		default:
			t.Fatalf("no-materialization case %q encodes %q, must be payloadText, rawPayload, or escapedKind", c.Name, c.Encoding)
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
// depend on member or entry order. Every integrity row carries an over-limit
// payload whose decoded text is itself a valid JSON document, so the
// authoritative path reaches the named shape refusal rather than stopping at
// payload content: a passing row proves the probe declined and the
// authoritative error surfaced, not merely that some integrity error appeared.
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
// shared scanner refuses (invalid syntax or depth), a legacy raw payload value
// with invalid JSON syntax (depth beyond the payload budget declines instead),
// and cross-record ordering or pointer uniqueness, which the authoritative path
// decides over the coordinate-sorted record set. Each size row also asserts
// its authoritative outcome: canonical rows are accepted by
// CollectRetainedUnknown apart from size, deliberate rows carry an integrity
// error there.
func TestProjectRetainedUnknownPrecedence(t *testing.T) {
	fixtures := loadRetainedProjectionPrecedenceFixtures(t)
	if fixtures.PayloadBytes <= 0 {
		t.Fatalf("precedence fixture %s needs a positive payloadBytes", retainedProjectionPrecedenceFixturePath)
	}
	over := `"` + strings.Repeat("1", fixtures.PayloadBytes-2) + `"`
	overInvalid := `"` + strings.Repeat("x", fixtures.PayloadBytes-2) + `"`
	overRawInvalid := `[` + strings.Repeat(",", fixtures.PayloadBytes-2) + `]`
	deep := strings.Repeat("[", 12001) + strings.Repeat("]", 12001)
	if len(over) != fixtures.PayloadBytes {
		t.Fatalf("built over-limit literal is %d bytes, want payloadBytes %d", len(over), fixtures.PayloadBytes)
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
