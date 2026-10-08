package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
)

// TestEntryFromRowPreservesPresence builds one body row with every optional
// column set beside empty-string sentinels and asserts the reconstruction
// keeps NULL apart from empty and pointer nil-ness exactly.
func TestEntryFromRowPreservesPresence(t *testing.T) {
	empty := ""
	ts := int64(1720000000000)
	row := EntryRecord{
		SessionID:      schema.SessionID("ses_01JABC"),
		EntryIndex:     7,
		Harness:        schema.HarnessOpenCode,
		EntryType:      schema.EntryTypeToolResult,
		Role:           schema.Role("assistant"),
		TimestampMs:    &ts,
		ContentPreview: &empty,
		HasToolUse:     false,
		HasThinking:    false,
		IsError:        false,
		Depth:          0,
		SourceEntryRef: schema.SourceEntryRef("opencode:7"),
	}
	entry := entryFromRow(row)
	if entry.TimestampMs == nil || *entry.TimestampMs != ts {
		t.Fatalf("TimestampMs = %v, want %d", entry.TimestampMs, ts)
	}
	if entry.ContentPreview == nil || *entry.ContentPreview != "" {
		t.Fatalf("ContentPreview = %v, want empty non-nil", entry.ContentPreview)
	}
	if entry.TokensIn != nil || entry.ToolInput != nil || entry.Extra != nil {
		t.Fatalf("unset columns must read nil: TokensIn=%v ToolInput=%v Extra=%v",
			entry.TokensIn, entry.ToolInput, entry.Extra)
	}
	if entry.SourceEntryRef != row.SourceEntryRef {
		t.Fatalf("SourceEntryRef = %q, want %q", entry.SourceEntryRef, row.SourceEntryRef)
	}
}

// TestSerializeEntryIsPlainMarshal asserts the canonical text is exactly what
// encoding/json produces for the reconstructed struct: no custom marshaler,
// no key reordering outside struct order.
func TestSerializeEntryIsPlainMarshal(t *testing.T) {
	preview := "total 4\n-rw-r--r-- 1 minttea minttea 1234 main.go"
	output := "total 4\n-rw-r--r-- 1 minttea minttea 1234 main.go"
	row := EntryRecord{
		SessionID:      schema.SessionID("ses_01JABC"),
		EntryIndex:     7,
		Harness:        schema.HarnessOpenCode,
		EntryType:      schema.EntryTypeToolResult,
		Role:           schema.Role("assistant"),
		ContentPreview: &preview,
		ToolOutput:     &output,
		Depth:          0,
		SourceEntryRef: schema.SourceEntryRef("opencode:7"),
	}
	want, err := json.Marshal(entryFromRow(row))
	if err != nil {
		t.Fatalf("marshal reconstructed entry: %v", err)
	}
	if got := serializeEntry(row); string(got) != string(want) {
		t.Fatalf("serializeEntry = %s, want %s", got, want)
	}
	if digest := SerializeEntryDigest(row); len(digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", digest)
	}
}

// TestRebuildExtraMergeOrder pins the canonical rebuild: promoted keys merge
// over the unknown remainder with sorted-key encoding, matching the mirror's
// ext merge byte for byte.
func TestRebuildExtraMergeOrder(t *testing.T) {
	remainder := `{"zeta":1,"alpha":"x"}`
	model := "test-model"
	reasoning := 12
	row := EntryRecord{Extra: &remainder, ModelID: &model, TokensReasoning: &reasoning}
	extra := rebuildExtra(row)
	if extra == nil {
		t.Fatal("rebuildExtra = nil, want merged document")
	}
	// Sorted keys: alpha, cache_read absent, model_id, tokens_reasoning, zeta.
	want := `{"alpha":"x","model_id":"test-model","tokens_reasoning":12,"zeta":1}`
	if *extra != want {
		t.Fatalf("rebuildExtra = %s, want %s", *extra, want)
	}
}

// TestRebuildExtraVerbatimWins pins the residue rule: a stored verbatim
// original passes through untouched, promoted columns included or not.
func TestRebuildExtraVerbatimWins(t *testing.T) {
	verbatim := `{"model_id":"m","custom":[1,2]}`
	remainder := `{"custom":[1,2]}`
	model := "m"
	row := EntryRecord{Extra: &remainder, ExtraVerbatim: &verbatim, ModelID: &model}
	if extra := rebuildExtra(row); extra == nil || *extra != verbatim {
		t.Fatalf("rebuildExtra = %v, want verbatim %s", extra, verbatim)
	}
}

// TestRebuildExtraEmptyReadsNil pins the absent case: no remainder and no
// promoted keys means no Extra document, matching the wire omitempty.
func TestRebuildExtraEmptyReadsNil(t *testing.T) {
	if extra := rebuildExtra(EntryRecord{}); extra != nil {
		t.Fatalf("rebuildExtra = %s, want nil", *extra)
	}
}

// TestLegacyShapeBoundsLikeMirror pins the shim contract in both shapes:
// bounded reconstructs the row exactly as the mirror stored it (bounded
// preview, no ref, no provenance); unbounded returns the full entry the
// publication path hydrates.
func TestLegacyShapeBoundsLikeMirror(t *testing.T) {
	long := strings.Repeat("あ", 800)
	row := EntryRecord{
		SessionID:      schema.SessionID("ses_01JABC"),
		EntryIndex:     3,
		Harness:        schema.HarnessOpenCode,
		EntryType:      schema.EntryTypeText,
		Role:           schema.Role("user"),
		ContentPreview: &long,
		Depth:          0,
		SourceEntryRef: schema.SourceEntryRef("opencode:3"),
	}
	bounded := legacyShape(row, true)
	if bounded.ContentPreview == nil || *bounded.ContentPreview != contentPreview(long) {
		t.Fatalf("bounded preview = %v, want mirror bound", bounded.ContentPreview)
	}
	if bounded.SourceEntryRef != "" || bounded.Provenance != nil {
		t.Fatalf("bounded shim keeps ref/provenance: %q %v", bounded.SourceEntryRef, bounded.Provenance)
	}
	full := legacyShape(row, false)
	if full.ContentPreview == nil || *full.ContentPreview != long {
		t.Fatalf("full preview truncated: %d bytes of %d", len(*full.ContentPreview), len(long))
	}
	if full.SourceEntryRef != row.SourceEntryRef {
		t.Fatalf("full shim drops ref: %q, want %q", full.SourceEntryRef, row.SourceEntryRef)
	}
}
