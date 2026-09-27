package ingest_test

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
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
// decoding the payload would allocate.
func TestProjectRetainedUnknownRefusesOversizedPayloadBeforeMaterializing(t *testing.T) {
	const payloadBytes = 64 << 20
	entry := carrierEntry(t, publicPosition(0), oversizedPayload(payloadBytes))

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

// TestProjectRetainedUnknownPrecedence documents which integrity refusals still
// outrank the transfer refusal after the early decision. Missing traversal
// coordinates and envelopes outside the owned member set return false from the
// in-place probe, so the authoritative path keeps their existing errors. A
// payload that is well-formed JSON text at the envelope level but oversized is
// refused for size even when its decoded content is not valid JSON: the early
// decision cannot know payload-content syntax without decoding, which is the
// materialization this change removes. That one case is asserted as the
// deliberate new behaviour.
func TestProjectRetainedUnknownPrecedence(t *testing.T) {
	t.Run("oversized_with_missing_coordinates_keeps_legacy_refusal", func(t *testing.T) {
		position := ingest.UnknownSourcePosition{Line: 1}
		entry := carrierEntry(t, position, oversizedPayload(8<<20+64))
		_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
		if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
			t.Fatalf("missing coordinates did not outrank the transfer refusal: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "8 MiB transfer limit") {
			t.Fatalf("transfer refusal replaced the legacy coordinate refusal: %v", err)
		}
	})

	t.Run("oversized_with_unknown_envelope_member_keeps_integrity_refusal", func(t *testing.T) {
		extra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","bogus":1,"position":{"public":{"sourceRef":"s","recordIndex":0,"position":0}},"payloadText":` + string(oversizedPayload(8<<20+64)) + `}]}`
		entry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &extra}
		_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
		var integrity *ingest.EvidenceIntegrityError
		if !errors.As(err, &integrity) {
			t.Fatalf("envelope corruption did not outrank the transfer refusal: %v", err)
		}
	})

	t.Run("oversized_malformed_payload_content_is_refused_for_size", func(t *testing.T) {
		// The envelope and its payloadText are well-formed JSON, so the stored
		// shape is measurable without decoding; the decoded payload text is not
		// valid JSON. The early decision refuses for size before the payload is
		// decoded, which is the intended consequence of refusing before
		// materialization.
		extra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","recordIndex":0,"position":0}},"payloadText":"` + strings.Repeat("{", 8<<20+64) + `"}]}`
		entry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &extra}
		_, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{entry}, schema.HarnessCodex)
		if err == nil || !strings.Contains(err.Error(), "8 MiB transfer limit") {
			t.Fatalf("oversized malformed payload was not refused for size: %v", err)
		}
	})
}
