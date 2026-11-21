package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

var _ api.SegmentReader = (*SegmentReader)(nil)

// SegmentReader serves the transcript segment reads for the mock sessions. The
// detail page takes its turns from the segment directory and segment routes,
// which read stored entries, so a server whose sessions section is mocked needs
// entries for the same sessions its detail header counts. Each mock session's
// turns are flattened into the entries a store would hold: one depth-0 entry
// per turn and a tool_use and tool_result pair per tool call, which the
// segment producer folds back into the same turns.
type SegmentReader struct {
	entries map[ingest.SessionID][]schema.SessionEntry
}

// NewSegmentReader builds the stored entries for every session the provider
// serves.
func NewSegmentReader(p *Provider) *SegmentReader {
	entries := make(map[ingest.SessionID][]schema.SessionEntry, len(p.sessions))
	for i := range p.sessions {
		entries[p.sessions[i].ID] = sessionEntries(p.sessions[i])
	}
	return &SegmentReader{entries: entries}
}

func (r *SegmentReader) LoadAvailableSessionEntries(_ context.Context, id ingest.SessionID) ([]schema.SessionEntry, error) {
	entries, ok := r.entries[id]
	if !ok {
		return nil, fmt.Errorf("mock segment reader: session %s is not a mock session", id)
	}
	return entries, nil
}

func (r *SegmentReader) ReadSessionEntries(_ context.Context, id ingest.SessionID, opts ingest.SessionEntryReadOptions) (ingest.SessionEntryReadPage, error) {
	entries, ok := r.entries[id]
	if !ok {
		return ingest.SessionEntryReadPage{}, fmt.Errorf("mock segment reader: session %s is not a mock session", id)
	}
	page := ingest.SessionEntryReadPage{FromIndex: opts.FromIndex}
	for i := range entries {
		if entries[i].EntryIndex < opts.FromIndex {
			continue
		}
		if opts.Limit > 0 && len(page.Entries) == opts.Limit {
			next := entries[i].EntryIndex
			page.NextIndex = &next
			break
		}
		page.Entries = append(page.Entries, entries[i])
	}
	return page, nil
}

// Revision is the mock read revision, the same one the mock detail read
// carries, so a segment read pinned to the detail's revision matches.
func (r *SegmentReader) Revision(context.Context) (string, error) {
	return mockReadRevision, nil
}

func sessionEntries(session ingest.Session) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(session.Turns))
	parents := make(map[int]int, len(session.Turns))
	next := 0
	for i := range session.Turns {
		turn := &session.Turns[i]
		parentIndex := next
		parents[turn.Index] = parentIndex
		entryType := turn.EntryType
		if entryType == "" {
			entryType = schema.EntryTypeText
		}
		entry := schema.SessionEntry{
			SessionID:      session.ID,
			EntryIndex:     parentIndex,
			Harness:        session.Harness,
			EntryType:      entryType,
			Role:           turn.Role,
			TimestampMs:    timestampMs(turn.Timestamp),
			ContentPreview: optionalString(turn.Content),
			TokensIn:       turn.TokensIn,
			TokensOut:      turn.TokensOut,
			HasToolUse:     len(turn.ToolCalls) > 0,
			HasThinking:    turn.HasThinking,
			StopReason:     turn.StopReason,
			Depth:          turn.Depth,
			PartType:       turn.PartType,
			SourceEntryRef: schema.SourceEntryRef(turn.SourceEntryRef),
			Provenance:     turn.Provenance,
		}
		if turn.ParentIndex != nil {
			if mapped, ok := parents[*turn.ParentIndex]; ok {
				entry.ParentIndex = &mapped
			}
		}
		entries = append(entries, entry)
		next++
		for j := range turn.ToolCalls {
			call := &turn.ToolCalls[j]
			use := schema.SessionEntry{
				SessionID:      session.ID,
				EntryIndex:     next,
				Harness:        session.Harness,
				EntryType:      schema.EntryTypeToolUse,
				Role:           schema.RoleAssistant,
				TimestampMs:    entry.TimestampMs,
				HasToolUse:     true,
				ToolNamesCSV:   optionalString(call.Name),
				ToolInput:      optionalString(call.Arguments),
				ToolCallID:     optionalString(call.ID),
				Depth:          turn.Depth + 1,
				ParentIndex:    &parentIndex,
				SourceEntryRef: schema.SourceEntryRef(call.CallEntryRef),
				Provenance:     call.CallProvenance,
			}
			if call.ToolKind != "" {
				kind := call.ToolKind
				use.ToolKind = &kind
			}
			result := schema.SessionEntry{
				SessionID:      session.ID,
				EntryIndex:     next + 1,
				Harness:        session.Harness,
				EntryType:      schema.EntryTypeToolResult,
				Role:           schema.RoleTool,
				TimestampMs:    entry.TimestampMs,
				ToolNamesCSV:   optionalString(call.Name),
				ToolOutput:     optionalString(call.Result),
				ToolCallID:     optionalString(call.ID),
				IsError:        call.IsError,
				Depth:          turn.Depth + 1,
				ParentIndex:    &parentIndex,
				SourceEntryRef: schema.SourceEntryRef(call.ResultEntryRef),
				Provenance:     call.ResultProvenance,
			}
			entries = append(entries, use, result)
			next += 2
		}
	}
	return entries
}

func timestampMs(at time.Time) *int64 {
	if at.IsZero() {
		return nil
	}
	ms := at.UnixMilli()
	return &ms
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
