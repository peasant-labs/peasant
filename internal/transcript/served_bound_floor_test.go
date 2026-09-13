package transcript

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
)

// servedBoundFloorDetail builds a detail whose tool results are exactly the
// given sizes, so a test can drive the shared bound down to any value.
func servedBoundFloorDetail(sizes []int) *schema.SessionDetailPayload {
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	detail := &schema.SessionDetailPayload{
		ID:        "served-bound-floor",
		Harness:   schema.HarnessClaudeCode,
		StartTime: at,
		EndTime:   at.Add(time.Minute),
	}
	for index, size := range sizes {
		detail.Turns = append(detail.Turns, schema.TurnDetail{
			Index:     index,
			Role:      schema.RoleAssistant,
			Timestamp: at,
			EntryType: schema.EntryTypeText,
			ToolCalls: []schema.ToolCallDetail{{ID: "call", Name: "Bash", Result: strings.Repeat("x", size)}},
		})
	}
	return detail
}

func servedBoundFloorEncodedLen(t *testing.T, detail *schema.SessionDetailPayload) int {
	t.Helper()
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("encode the served detail: %v", err)
	}
	return len(encoded)
}

// TestServedBoundLeavesAFieldWholeWhenBoundingWouldNotShrinkIt holds the floor
// under the shared bound.
//
// The bound is shared, so a session with very many recorded fields drives it
// toward zero. At a low bound the replacement note is longer than a short
// field's whole text: replacing it would GROW the document the bound exists to
// shrink, and it would cost the reader the only copy of a text they could have
// read in full. The reader-facing consequence is worst for text Peasant itself
// wrote — the note that stands where an oversized source record was omitted is
// about a hundred bytes, and a reader who loses it is not told that a record is
// missing at all.
func TestServedBoundLeavesAFieldWholeWhenBoundingWouldNotShrinkIt(t *testing.T) {
	t.Parallel()
	const shortField = "the whole recorded result"
	// Enough short fields that the document cannot fit at ANY bound, which is
	// what drives the search to its floor of zero. That floor is where the
	// replacement note is longer than a short field's whole text.
	const shortFields = 20
	sizes := make([]int, 0, shortFields+1)
	for range shortFields {
		sizes = append(sizes, len(shortField))
	}
	sizes = append(sizes, 64<<10)
	detail := servedBoundFloorDetail(sizes)
	for index := range shortFields {
		detail.Turns[index].ToolCalls[0].Result = shortField
	}
	before := servedBoundFloorEncodedLen(t, detail)

	// A document budget below even the empty document, so the search bottoms
	// out at zero and every oversized field is replaced by its note alone.
	budget, err := NewServedDocumentBudget(512, 1024)
	if err != nil {
		t.Fatal(err)
	}
	report := BoundServedDetail(detail, budget)
	if report.AppliedBytes != 0 {
		t.Fatalf("the applied bound is %d bytes; this case only reaches the floor at zero", report.AppliedBytes)
	}

	for index := range shortFields {
		if got := detail.Turns[index].ToolCalls[0].Result; got != shortField {
			t.Fatalf("the bound replaced short field %d with a longer note about it: %q", index, got)
		}
	}
	if long := detail.Turns[shortFields].ToolCalls[0].Result; !strings.Contains(long, "bounded for display") {
		t.Fatalf("the long field was not bounded, so this case proves nothing about the floor: %q", long)
	}
	if report.BoundedFields != 1 {
		t.Fatalf("the report counts %d bounded fields; exactly one field was replaced", report.BoundedFields)
	}
	if after := servedBoundFloorEncodedLen(t, detail); after > before {
		t.Fatalf("bounding GREW the document, from %d bytes to %d", before, after)
	}
}
