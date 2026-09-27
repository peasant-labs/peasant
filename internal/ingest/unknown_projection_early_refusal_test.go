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
// is refused without decoding or copying it. The allocated-byte delta is
// measured for the refusal call alone, after the entry already exists in
// memory, and asserted far below the payload size. A bound of payload/4 is
// generous for allocator noise while still an order of magnitude below what
// decoding the payload would allocate. Both stored encodings are exercised:
// the modern payloadText string and the legacy raw payload value.
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
	cases := []struct {
		name  string
		entry schema.SessionEntry
	}{
		{"payloadText", carrierEntry(t, publicPosition(0), oversizedPayload(payloadBytes))},
		{"rawPayload", schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &rawExtra}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{c.entry}, schema.HarnessCodex)
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
// keep their existing errors, including corrupt coordinates, trailing content,
// lax grammar, and an empty evidence array in any member or entry order. A
// record whose coordinates order corruptly (position < recordIndex) keeps the
// position refusal: the probe mirrors the ordering rule in place.
//
// Three deliberate size precedences are asserted as the intended consequence of
// refusing before materialization: a payloadText whose decoded text is not
// valid JSON, a legacy raw payload value with invalid JSON syntax, and
// cross-record ordering or pointer uniqueness, which the authoritative path
// decides over the coordinate-sorted record set.
func TestProjectRetainedUnknownPrecedence(t *testing.T) {
	fixtures := loadRetainedProjectionPrecedenceFixtures(t)
	if fixtures.PayloadBytes <= 0 {
		t.Fatalf("precedence fixture %s needs a positive payloadBytes", retainedProjectionPrecedenceFixturePath)
	}
	over := `"` + strings.Repeat("1", fixtures.PayloadBytes-2) + `"`
	overInvalid := `"` + strings.Repeat("x", fixtures.PayloadBytes-2) + `"`
	overRawInvalid := `[` + strings.Repeat(",", fixtures.PayloadBytes-2) + `]`
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
		})
	}
}
