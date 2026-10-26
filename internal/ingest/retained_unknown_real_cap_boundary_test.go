//go:build !race

package ingest_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestProjectRetainedUnknownRealCapBoundary is the one real-size proof for the
// retained-unknown document family. It is build-tagged !race and serial so the
// cap-sized materialization never runs under the race detector; every other
// boundary case in this family drives the same production decision path with a
// small injected limit. A canonical raw payload of exactly the published
// per-payload cap is accepted through the production entry, and one byte more is
// refused by the in-place transfer probe.
func TestProjectRetainedUnknownRealCapBoundary(t *testing.T) {
	limit := defaults.RetainedUnknownPayloadCapBytes
	rawExtra := func(payload string) string {
		return `{"retainedUnknown":[{"harness":"codex","namespace":"record","kind":"future","position":{"line":1,"public":{"sourceRef":"s","recordIndex":0,"position":0}},"payload":` + payload + `}]}`
	}
	atCapExtra := rawExtra(`"` + strings.Repeat("x", limit-2) + `"`)
	atCapEntry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &atCapExtra}
	projected, err := ingest.ProjectRetainedUnknown([]schema.SessionEntry{atCapEntry}, schema.HarnessCodex)
	if err != nil || len(projected) != 1 {
		t.Fatalf("payload at the %d-byte cap was not accepted: records=%d err=%v", limit, len(projected), err)
	}
	if len(projected[0].Payload) != limit {
		t.Fatalf("at-cap projected payload is %d bytes, want exactly %d", len(projected[0].Payload), limit)
	}
	oneOverExtra := rawExtra(`"` + strings.Repeat("x", limit-1) + `"`)
	oneOverEntry := schema.SessionEntry{Harness: schema.HarnessCodex, EntryIndex: 0, Extra: &oneOverExtra}
	_, err = ingest.ProjectRetainedUnknown([]schema.SessionEntry{oneOverEntry}, schema.HarnessCodex)
	if err == nil || !strings.Contains(err.Error(), defaults.HumanByteSize(int64(limit))+" transfer limit") {
		t.Fatalf("one-over payload was not refused by the transfer limit: %v", err)
	}
}
