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
//
// Calibration: forcing the in-place probe off makes this same 64 MiB payload
// allocate ~3.83 GB before the refusal, so the payload/4 bound sits four orders
// of magnitude below the allocation the probe removes. The bound is therefore
// reproducible from this committed test alone, not from an unrecorded local
// measurement.
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
// outrank the transfer refusal after the early decision, and that the outcome
// does not depend on member or entry order. Missing traversal coordinates and
// every envelope the in-place probe declines return false from the probe, so the
// authoritative path keeps their existing errors. A payload that is well-formed
// JSON text at the envelope level but oversized is refused for size even when
// its decoded content is not valid JSON: the early decision cannot know
// payload-content syntax without decoding, which is the materialization this
// change removes. That one case is asserted as the deliberate new behaviour.
func TestProjectRetainedUnknownPrecedence(t *testing.T) {
	overPayload := string(oversizedPayload(8<<20 + 64))
	publicPosition := `"position":{"public":{"sourceRef":"s","recordIndex":0,"position":0}}`
	over := `"payloadText":` + overPayload
	carrier := func(index int, extra string) schema.SessionEntry {
		return schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: index, Extra: &extra}
	}
	// assertIntegrity projects one or more stored Extras through the public entry
	// point and requires the integrity refusal, not the transfer refusal.
	assertIntegrity := func(t *testing.T, extras ...string) {
		t.Helper()
		entries := make([]schema.SessionEntry, 0, len(extras))
		for index, extra := range extras {
			entries = append(entries, carrier(index, extra))
		}
		_, err := ingest.ProjectRetainedUnknown(entries, schema.HarnessCodex)
		var target *ingest.EvidenceIntegrityError
		if !errors.As(err, &target) {
			t.Fatalf("integrity refusal was masked by the transfer refusal: %v", err)
		}
	}

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

	// Member-order matrix: the over-limit payload must not mask corruption that
	// appears before it or after it.
	t.Run("oversized_unknown_member_before_payload_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","bogus":1,`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_unknown_member_after_payload_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,`+over+`,"bogus":1}]}`)
	})
	t.Run("oversized_both_encodings_raw_then_text_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,"payload":"x",`+over+`}]}`)
	})
	t.Run("oversized_both_encodings_text_then_raw_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,`+over+`,"payload":"x"}]}`)
	})

	// Integrity classes the probe now declines before measuring.
	t.Run("oversized_unknown_harness_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"bogus","namespace":"record","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_harness_mismatch_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"claude-code","namespace":"record","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_public_null_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":null},`+over+`}]}`)
	})
	t.Run("oversized_missing_namespace_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_null_namespace_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":null,"kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_empty_namespace_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_missing_kind_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_null_kind_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":null,`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_empty_kind_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_missing_harness_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"namespace":"record","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_missing_position_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+over+`}]}`)
	})
	t.Run("oversized_missing_payload_encoding_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`}]}`)
	})
	t.Run("oversized_position_unknown_member_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","recordIndex":0,"position":0},"bogus":1},`+over+`}]}`)
	})
	t.Run("oversized_public_unknown_member_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","recordIndex":0,"position":0,"bogus":1}},`+over+`}]}`)
	})
	t.Run("oversized_public_string_coordinate_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","recordIndex":"0","position":0}},`+over+`}]}`)
	})
	t.Run("oversized_public_float_coordinate_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","recordIndex":1.5,"position":1}},`+over+`}]}`)
	})
	t.Run("oversized_duplicate_public_member_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s","sourceRef":"t","recordIndex":0,"position":0}},`+over+`}]}`)
	})
	t.Run("oversized_public_missing_member_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"public":{"sourceRef":"s"}},`+over+`}]}`)
	})
	t.Run("oversized_duplicate_envelope_member_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","kind":"future",`+publicPosition+`,`+over+`}]}`)
	})
	t.Run("oversized_duplicate_root_key_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,`+over+`}],"retainedUnknown":[]}`)
	})
	t.Run("oversized_duplicate_root_case_alias_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,`+over+`}],"RetainedUnknown":[]}`)
	})
	t.Run("oversized_corrupt_envelope_after_over_envelope_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",`+publicPosition+`,`+over+`},{"harness":"codex","namespace":"record","kind":"future","bogus":1,`+publicPosition+`,`+over+`}]}`)
	})

	// Entry-order matrix: an over-limit well-formed entry must not mask a corrupt
	// entry in either direction.
	goodExtra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future",` + publicPosition + `,` + over + `}]}`
	badExtra := `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","bogus":1,` + publicPosition + `,` + over + `}]}`
	t.Run("oversized_entry_before_corrupt_entry_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, goodExtra, badExtra)
	})
	t.Run("corrupt_entry_before_oversized_entry_keeps_integrity_refusal", func(t *testing.T) {
		assertIntegrity(t, badExtra, goodExtra)
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
