package main

import (
	"testing"
	"time"

	"github.com/peasant-labs/schema"
)

// TestTurnsToEntries_DepthAndParentMapping verifies the core projection
// contract: each turn becomes a depth-0 entry and each folded tool call becomes
// a depth-1 tool_use + tool_result pair pointing back at the parent.
func TestTurnsToEntries_DepthAndParentMapping(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1_700_000_000, 0).UTC()
	turns := []schema.TurnDetail{
		{
			Index:     0,
			Role:      schema.RoleUser,
			Content:   "please build the thing",
			Timestamp: ts,
			EntryType: schema.EntryTypeText,
		},
		{
			Index:     1,
			Role:      schema.RoleAssistant,
			Content:   "on it",
			Timestamp: ts,
			EntryType: schema.EntryTypeText,
			ToolCalls: []schema.ToolCallDetail{
				{
					ID:        "tool-1",
					Name:      "Bash",
					Arguments: `{"command":"ls"}`,
					Result:    "file.go",
					ToolKind:  schema.ToolCallKindExecute,
				},
			},
		},
	}

	entries := TurnsToEntries(turns)

	// 2 turns + (1 tool call x 2 rows) = 4 entries.
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Entry 0: depth-0 user message.
	if entries[0].Depth != 0 || entries[0].Role != schema.RoleUser {
		t.Errorf("entry 0: want depth=0 role=user, got depth=%d role=%s", entries[0].Depth, entries[0].Role)
	}
	if entries[0].ParentIndex != nil {
		t.Errorf("entry 0: depth-0 must have nil ParentIndex, got %v", *entries[0].ParentIndex)
	}

	// Entry 1: depth-0 assistant message.
	if entries[1].Depth != 0 || entries[1].EntryIndex != 1 {
		t.Errorf("entry 1: want depth=0 index=1, got depth=%d index=%d", entries[1].Depth, entries[1].EntryIndex)
	}
	if !entries[1].HasToolUse {
		t.Errorf("entry 1: assistant turn with tool calls must set HasToolUse")
	}

	// Entry 2: depth-1 tool_use, parent = entry 1.
	if entries[2].Depth != 1 || entries[2].EntryType != schema.EntryTypeToolUse {
		t.Errorf("entry 2: want depth=1 tool_use, got depth=%d type=%s", entries[2].Depth, entries[2].EntryType)
	}
	if entries[2].ParentIndex == nil || *entries[2].ParentIndex != 1 {
		t.Errorf("entry 2: want ParentIndex=1, got %v", entries[2].ParentIndex)
	}
	if entries[2].ToolNamesCSV == nil || *entries[2].ToolNamesCSV != "Bash" {
		t.Errorf("entry 2: want ToolNamesCSV=Bash, got %v", entries[2].ToolNamesCSV)
	}
	if entries[2].ToolInput == nil || *entries[2].ToolInput != `{"command":"ls"}` {
		t.Errorf("entry 2: want ToolInput preserved, got %v", entries[2].ToolInput)
	}
	if entries[2].ToolKind == nil || *entries[2].ToolKind != schema.ToolCallKindExecute {
		t.Errorf("entry 2: want ToolKind=execute, got %v", entries[2].ToolKind)
	}

	// Entry 3: depth-1 tool_result, parent = entry 1.
	if entries[3].Depth != 1 || entries[3].EntryType != schema.EntryTypeToolResult {
		t.Errorf("entry 3: want depth=1 tool_result, got depth=%d type=%s", entries[3].Depth, entries[3].EntryType)
	}
	if entries[3].ParentIndex == nil || *entries[3].ParentIndex != 1 {
		t.Errorf("entry 3: want ParentIndex=1, got %v", entries[3].ParentIndex)
	}
	if entries[3].ToolOutput == nil || *entries[3].ToolOutput != "file.go" {
		t.Errorf("entry 3: want ToolOutput=file.go, got %v", entries[3].ToolOutput)
	}
	if entries[3].Role != schema.RoleTool {
		t.Errorf("entry 3: tool_result role must be tool, got %s", entries[3].Role)
	}
}

// TestTurnsToEntries_IndicesMonotonic verifies entry indices increase by one
// across turns AND their folded children, so the renderer's [N] ordinals are
// stable and contiguous (windowing relies on this).
func TestTurnsToEntries_IndicesMonotonic(t *testing.T) {
	t.Parallel()

	turns := []schema.TurnDetail{
		{Index: 0, Role: schema.RoleAssistant, Content: "a", ToolCalls: []schema.ToolCallDetail{{ID: "t1", Name: "Read", Arguments: "{}", Result: "x"}}},
		{Index: 1, Role: schema.RoleAssistant, Content: "b"},
		{Index: 2, Role: schema.RoleAssistant, Content: "c", ToolCalls: []schema.ToolCallDetail{{ID: "t2", Name: "Edit", Arguments: "{}", Result: "y"}}},
	}

	entries := TurnsToEntries(turns)
	for i := range entries {
		if entries[i].EntryIndex != i {
			t.Errorf("entry at slot %d has EntryIndex %d; indices must be 0..n-1 contiguous", i, entries[i].EntryIndex)
		}
	}
}

// TestTurnsToEntries_Empty verifies the nil/empty input contract.
func TestTurnsToEntries_Empty(t *testing.T) {
	t.Parallel()
	if got := TurnsToEntries(nil); got != nil {
		t.Errorf("TurnsToEntries(nil) = %v, want nil", got)
	}
	if got := TurnsToEntries([]schema.TurnDetail{}); got != nil {
		t.Errorf("TurnsToEntries(empty) = %v, want nil", got)
	}
}

// TestTurnsToEntries_AbsentFieldsAreNil verifies empty content/args/result map to
// nil pointers (absent), not empty-string pointers, matching the session_entries
// convention the renderer relies on (it tests *ptr != "" guarding).
func TestTurnsToEntries_AbsentFieldsAreNil(t *testing.T) {
	t.Parallel()

	turns := []schema.TurnDetail{
		{Index: 0, Role: schema.RoleAssistant, Content: "", ToolCalls: []schema.ToolCallDetail{
			{ID: "", Name: "", Arguments: "", Result: ""},
		}},
	}
	entries := TurnsToEntries(turns)
	if entries[0].ContentPreview != nil {
		t.Errorf("empty turn content must map to nil ContentPreview, got %v", *entries[0].ContentPreview)
	}
	if entries[1].ToolNamesCSV != nil || entries[1].ToolInput != nil || entries[1].ToolCallID != nil {
		t.Errorf("empty tool_use fields must be nil, got name=%v input=%v id=%v",
			entries[1].ToolNamesCSV, entries[1].ToolInput, entries[1].ToolCallID)
	}
	if entries[2].ToolOutput != nil {
		t.Errorf("empty tool result must map to nil ToolOutput, got %v", *entries[2].ToolOutput)
	}
}

// TestTurnsToEntries_ZeroTimestampIsNil verifies the zero time maps to a nil
// TimestampMs so the renderer prints the "--:--:--" placeholder, not the epoch.
func TestTurnsToEntries_ZeroTimestampIsNil(t *testing.T) {
	t.Parallel()
	entries := TurnsToEntries([]schema.TurnDetail{{Index: 0, Role: schema.RoleUser, Content: "hi"}})
	if entries[0].TimestampMs != nil {
		t.Errorf("zero timestamp must map to nil TimestampMs, got %v", *entries[0].TimestampMs)
	}
}
