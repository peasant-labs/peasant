package main

import (
	"github.com/peasant-labs/schema"
)

// TurnsToEntries projects the folded TurnDetail model (the turn-shaped data
// carried inside a pulled TranscriptContent envelope) back into a flat
// []schema.SessionEntry slice that the existing session-context renderers
// (renderSessionContextHuman / renderToolBox) consume UNCHANGED.
//
// DISPLAY-ONLY, ONE-WAY PROJECTION: the result of this function is the
// inverse of api.EntriesToTurns for RENDERING PURPOSES ONLY. It is NEVER
// persisted — pulled blobs are foreign and one-way, so they are not indexed in
// session_entries. The synthetic
// EntryIndex values it assigns to folded tool children are display ordinals, not
// DB row indices, and must not be written back to the store.
//
// It is deterministic: identical input always yields byte-identical entries, so
// it can be pinned by an output-equivalence golden test against the
// sessions-context rendering path.
//
// Mapping:
//   - each TurnDetail → one depth-0 SessionEntry (the message header + content
//     preview the renderer prints on the [N] role … depth=0 line);
//   - each folded ToolCallDetail → TWO depth-1 SessionEntry rows the renderer
//     draws as box-drawing blocks: a tool_use row (carrying the tool name +
//     arguments) followed by a tool_result row (carrying the result + error
//     flag). This matches the depth-1 tool_use/tool_result pair the indexer
//     originally produced before EntriesToTurns folded them into ToolCalls.
//
// Entry indices are assigned by a single monotonically increasing counter so the
// renderer's [N] ordinals are stable and the depth-1 children correctly point at
// their depth-0 parent via ParentIndex.
func TurnsToEntries(turns []schema.TurnDetail) []schema.SessionEntry {
	if len(turns) == 0 {
		return nil
	}

	// Pre-size: one entry per turn plus two per tool call.
	capHint := len(turns)
	for i := range turns {
		capHint += 2 * len(turns[i].ToolCalls)
	}
	entries := make([]schema.SessionEntry, 0, capHint)

	nextIndex := 0
	for i := range turns {
		t := &turns[i]

		parentIndex := nextIndex
		parent := schema.SessionEntry{
			EntryIndex:     parentIndex,
			EntryType:      depth0EntryType(t),
			Role:           t.Role,
			Depth:          0,
			TimestampMs:    timestampMsPtr(t),
			ContentPreview: nonEmptyStringPtr(t.Content),
			HasThinking:    t.HasThinking,
			HasToolUse:     len(t.ToolCalls) > 0,
			StopReason:     t.StopReason,
			TokensIn:       t.TokensIn,
			TokensOut:      t.TokensOut,
		}
		entries = append(entries, parent)
		nextIndex++

		for j := range t.ToolCalls {
			tc := &t.ToolCalls[j]

			// tool_use row: carries the tool name (ToolNamesCSV) and the call
			// arguments (ToolInput) the renderToolBox "┌ Tool: name(args)" line
			// reads.
			use := schema.SessionEntry{
				EntryIndex:   nextIndex,
				EntryType:    schema.EntryTypeToolUse,
				Role:         schema.RoleAssistant,
				Depth:        1,
				ParentIndex:  intPtr(parentIndex),
				TimestampMs:  timestampMsPtr(t),
				ToolNamesCSV: nonEmptyStringPtr(tc.Name),
				ToolInput:    nonEmptyStringPtr(tc.Arguments),
				ToolCallID:   nonEmptyStringPtr(tc.ID),
				HasToolUse:   true,
			}
			if tc.ToolKind != "" {
				kind := tc.ToolKind
				use.ToolKind = &kind
			}
			entries = append(entries, use)
			nextIndex++

			// tool_result row: carries the result body (ToolOutput) the
			// renderToolBox "(result)" block reads, plus the error flag.
			result := schema.SessionEntry{
				EntryIndex:   nextIndex,
				EntryType:    schema.EntryTypeToolResult,
				Role:         schema.RoleTool,
				Depth:        1,
				ParentIndex:  intPtr(parentIndex),
				TimestampMs:  timestampMsPtr(t),
				ToolNamesCSV: nonEmptyStringPtr(tc.Name),
				ToolOutput:   nonEmptyStringPtr(tc.Result),
				ToolCallID:   nonEmptyStringPtr(tc.ID),
				IsError:      tc.IsError,
			}
			entries = append(entries, result)
			nextIndex++
		}
	}

	return entries
}

// depth0EntryType returns the EntryType for a turn's depth-0 entry. The pulled
// turn carries the original EntryType when the envelope preserved it; otherwise
// it falls back to text (a plain message), which the depth-0 renderer treats
// identically (it keys on Role, not EntryType, for depth-0 lines).
func depth0EntryType(t *schema.TurnDetail) schema.EntryType {
	if t.EntryType != "" {
		return t.EntryType
	}
	return schema.EntryTypeText
}

// timestampMsPtr returns a pointer to the turn's timestamp in unix-millis, or
// nil when the turn has no timestamp (the zero time) so the renderer prints the
// "--:--:--" placeholder rather than the epoch.
func timestampMsPtr(t *schema.TurnDetail) *int64 {
	if t.Timestamp.IsZero() {
		return nil
	}
	ms := t.Timestamp.UnixMilli()
	return &ms
}

// nonEmptyStringPtr returns a pointer to s, or nil when s is empty, so absent
// content maps to a nil pointer (matching the session_entries convention where
// optional string fields distinguish absent from empty).
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// intPtr returns a pointer to i.
func intPtr(i int) *int {
	return &i
}
