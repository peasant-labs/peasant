package mock

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// The mock store serves the stored entries of its sessions, so the transcript
// segment reads open a mock session the way they open a stored one. The server
// cuts, folds and serves those entries with the same segment producer it runs
// against the analytics store; the mock only supplies the entries.
var _ api.SegmentReader = (*Provider)(nil)

// Revision is the one store revision the mock serves. The mock has no writes,
// so every segment directory and segment is cut under the same revision the
// mock detail read reports.
func (p *Provider) Revision(context.Context) (string, error) {
	return mockReadRevision, nil
}

// LoadAvailableSessionEntries returns every stored entry of one mock session,
// whole: the mock holds full content, so the available read is the full read.
// An identifier the mock does not hold has no stored entries, exactly as the
// analytics store answers for a session it never indexed.
func (p *Provider) LoadAvailableSessionEntries(_ context.Context, id ingest.SessionID) ([]schema.SessionEntry, error) {
	session := p.storedSession(id)
	if session == nil {
		return nil, nil
	}
	return storedEntries(session)
}

// ReadSessionEntries pages one mock session's stored entries the way the
// analytics store pages them: entries at or after FromIndex in entry order, at
// most Limit of them, stopping before an entry that would carry the page past
// SoftMaxBytes once it holds at least one entry. A zero Limit reads the
// store's default page of 100 entries; a zero SoftMaxBytes sets no byte bound,
// because the mock holds a handful of short turns.
func (p *Provider) ReadSessionEntries(_ context.Context, id ingest.SessionID, opts ingest.SessionEntryReadOptions) (ingest.SessionEntryReadPage, error) {
	page := ingest.SessionEntryReadPage{FromIndex: opts.FromIndex}
	if _, err := ingest.NewSessionEntryReadMode(string(opts.Mode)); err != nil {
		return page, fmt.Errorf("mock store entry page for session %s: %w; no entries were read; request one of the read modes ingest.SessionEntryReadMode names", id, err)
	}
	if opts.FromIndex < 0 || opts.Limit < 0 || opts.SoftMaxBytes < 0 {
		return page, fmt.Errorf("mock store entry page for session %s: from index %d, limit %d or byte budget %d is negative; no entries were read; send non-negative read options", id, opts.FromIndex, opts.Limit, opts.SoftMaxBytes)
	}
	session := p.storedSession(id)
	if session == nil {
		return page, nil
	}
	entries, err := storedEntries(session)
	if err != nil {
		return page, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = 100
	}
	for _, entry := range entries {
		if entry.EntryIndex < opts.FromIndex {
			continue
		}
		size := storedEntryBytes(entry)
		full := len(page.Entries) >= limit
		overBudget := opts.SoftMaxBytes > 0 && page.BytesRead+size > opts.SoftMaxBytes
		if full || (overBudget && len(page.Entries) > 0) {
			next := entry.EntryIndex
			page.NextIndex = &next
			break
		}
		page.Entries = append(page.Entries, entry)
		page.BytesRead += size
	}
	return page, nil
}

func (p *Provider) storedSession(id ingest.SessionID) *ingest.Session {
	for i := range p.sessions {
		if p.sessions[i].ID == id {
			return &p.sessions[i]
		}
	}
	return nil
}

// storedEntries lays one mock session out as the entries a harvest stores for
// it: each turn is one top-level entry, followed by one tool_use child entry per
// tool call that carries the call's input and its result. Entry indexes run in
// that stored order from 0, so a session without tool calls keeps its turn
// identities and a session with them leaves room for its children, as a stored
// session does. A nested turn's parent index follows its parent to the new
// index. A turn that names no entry type is a plain text message.
func storedEntries(session *ingest.Session) ([]schema.SessionEntry, error) {
	entries := make([]schema.SessionEntry, 0, len(session.Turns))
	indexOf := make(map[int]int, len(session.Turns))
	next := 0
	for _, turn := range session.Turns {
		if _, duplicate := indexOf[turn.Index]; duplicate {
			return nil, fmt.Errorf("mock store entries for session %s: turn identity %d appears twice in the mock session; no entries were served, because two turns cannot share one stored entry; give every mock turn its own index", session.ID, turn.Index)
		}
		index := next
		indexOf[turn.Index] = index
		next++

		entryType := turn.EntryType
		if entryType == "" {
			entryType = schema.EntryTypeText
		}
		content := turn.Content
		entry := schema.SessionEntry{
			SessionID:      session.ID,
			EntryIndex:     index,
			Harness:        session.Harness,
			EntryType:      entryType,
			Role:           turn.Role,
			TimestampMs:    storedTimestamp(turn),
			ContentPreview: &content,
			TokensIn:       turn.TokensIn,
			TokensOut:      turn.TokensOut,
			HasToolUse:     len(turn.ToolCalls) > 0,
			HasThinking:    turn.HasThinking,
			StopReason:     turn.StopReason,
			Depth:          turn.Depth,
			SourceEntryRef: schema.SourceEntryRef(turn.SourceEntryRef),
		}
		if turn.ParentIndex != nil {
			parent, ok := indexOf[*turn.ParentIndex]
			if !ok {
				return nil, fmt.Errorf("mock store entries for session %s: turn %d names parent turn %d, which does not precede it; no entries were served, because a nested turn folds into an earlier parent; point the mock turn at an earlier turn", session.ID, turn.Index, *turn.ParentIndex)
			}
			entry.ParentIndex = &parent
		}
		entries = append(entries, entry)

		for _, call := range turn.ToolCalls {
			child := toolCallEntry(session, turn, call, next, index)
			entries = append(entries, child)
			next++
		}
	}
	return entries, nil
}

func toolCallEntry(session *ingest.Session, turn ingest.Turn, call ingest.ToolCall, index, parent int) schema.SessionEntry {
	id, name, input := call.ID, call.Name, call.Arguments
	entry := schema.SessionEntry{
		SessionID:    session.ID,
		EntryIndex:   index,
		Harness:      session.Harness,
		EntryType:    schema.EntryTypeToolUse,
		Role:         turn.Role,
		TimestampMs:  storedTimestamp(turn),
		HasToolUse:   true,
		IsError:      call.IsError,
		ToolCallID:   &id,
		ToolNamesCSV: &name,
		ToolInput:    &input,
		// The fold joins tool calls from depth-1 children to the entry their
		// parent index names, whatever that entry's own depth.
		Depth:       1,
		ParentIndex: &parent,
	}
	if call.Result != "" {
		result := call.Result
		entry.ToolOutput = &result
	}
	return entry
}

func storedTimestamp(turn ingest.Turn) *int64 {
	if turn.Timestamp.IsZero() {
		return nil
	}
	ms := turn.Timestamp.UnixMilli()
	return &ms
}

func storedEntryBytes(entry schema.SessionEntry) int64 {
	var size int64
	for _, field := range []*string{entry.ContentPreview, entry.ToolInput, entry.ToolOutput} {
		if field != nil {
			size += int64(len(*field))
		}
	}
	return size
}
