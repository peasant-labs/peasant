package ingest

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// Claude Code writes one API message as several JSONL records that share
// message.id: typically one record per content block (thinking, text,
// tool_use), each with its own uuid and a snapshot of the message usage. Tool
// results, progress records and attachments can sit between them, and the
// records of one message can resume after another message's records. The
// stored shape is one reply per message: a depth-0 assistant entry at the
// message's first record that holds the reply's prose, with its thinking and
// tool calls as depth-1 children at their own stream positions.

// claudeMessageUsage accumulates the token usage of a transcript counted once
// per API message. Every record of one message carries a snapshot of that
// message's usage, so summing the records would count it once per record.
// Within one message each field takes the last record that carries it (a
// present, non-zero count); a record without a message id is its own message.
// The stored reply entry merges its records by the same rule, so the session
// totals equal the sum of the reply entries.
type claudeMessageUsage struct {
	byMessage map[string]int
	in, out   []int
}

// observe records one record's input and output token counts.
func (u *claudeMessageUsage) observe(messageID string, tokensIn, tokensOut int) {
	slot := len(u.in)
	if messageID != "" {
		if u.byMessage == nil {
			u.byMessage = make(map[string]int)
		}
		if existing, seen := u.byMessage[messageID]; seen {
			slot = existing
		} else {
			u.byMessage[messageID] = slot
		}
	}
	if slot == len(u.in) {
		u.in, u.out = append(u.in, 0), append(u.out, 0)
	}
	if tokensIn > 0 {
		u.in[slot] = tokensIn
	}
	if tokensOut > 0 {
		u.out[slot] = tokensOut
	}
}

// totals is the session's input and output tokens, each message counted once.
func (u *claudeMessageUsage) totals() (int, int) {
	tokensIn, tokensOut := 0, 0
	for i := range u.in {
		tokensIn += u.in[i]
		tokensOut += u.out[i]
	}
	return tokensIn, tokensOut
}

// claudeEntrySource names where one grouped entry came from in the ungrouped
// record stream: the ordinal of its source record among the ungrouped depth-0
// entries, and its ordinal among that record's children (-1 for the record's
// own depth-0 entry). The managed producer keys native identities by it, so a
// block keeps the key it had when every record was its own entry.
type claudeEntrySource struct {
	record int
	block  int
}

// claudeReplyMessageID reports whether a depth-0 entry is a reply record and
// the API message id it belongs to. Only an assistant record that is not a
// control record, an omission placeholder or an opaque carrier is a reply
// record. A reply record without an id is a reply of its own.
func claudeReplyMessageID(entry schema.SessionEntry, line claudeIndexLine) (string, bool) {
	if entry.Depth != 0 || entry.Role != RoleAssistant || entry.PartType != nil || line.Type != string(claudeRecordTypeAssistant) {
		return "", false
	}
	return line.Message.ID, true
}

type claudeUngroupedRecord struct {
	position int   // position of the depth-0 entry in the ungrouped slice
	children []int // positions of its depth-1 children
}

// groupClaudeReplies folds the ungrouped per-record entries into one reply
// per API message. messageIDs maps a depth-0 entry index to its message id;
// an assistant record without an id is a reply of its own. Every assistant
// reply, grouped or single, takes the same shape:
//
//   - the depth-0 entry stands at the first record's position and keeps that
//     record's identity, time, model and thinking level;
//   - its content is the reply's prose, every text block in source order;
//   - text blocks are not repeated as children unless the reply carries an
//     image, whose ordered parts need them;
//   - thinking, tool calls and every other block stay depth-1 children at
//     their own positions, re-parented to the reply;
//   - a reply that holds only thinking keeps the thinking as its content and
//     no thinking children, exactly like a standalone thinking turn;
//   - usage is the last record's snapshot, counted once; the stop reason is
//     the last one recorded; the source size is the sum of the records.
//
// Entry indexes are renumbered densely over the removed entries, keeping any
// gap the ungrouped stream already had, and every parent index follows. The
// renumbering walks the stream in order, so it requires ascending entry
// indexes and refuses any other input.
func groupClaudeReplies(entries []schema.SessionEntry, messageIDs map[int]string, fullContent bool) ([]schema.SessionEntry, []claudeEntrySource, error) {
	if len(entries) == 0 {
		return entries, nil, nil
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].EntryIndex <= entries[i-1].EntryIndex {
			return nil, nil, fmt.Errorf("ingest.groupClaudeReplies: entry index %d at position %d does not follow %d; replies are grouped and renumbered in stream order, so the parsed entries must be ascending; report this as a parser defect with the session id", entries[i].EntryIndex, i, entries[i-1].EntryIndex)
		}
	}
	records := make([]claudeUngroupedRecord, 0, len(entries))
	recordOf := make([]int, len(entries))
	blockOf := make([]int, len(entries))
	byEntryIndex := make(map[int]int, len(entries))
	for i, entry := range entries {
		if entry.Depth == 0 || entry.ParentIndex == nil {
			byEntryIndex[entry.EntryIndex] = len(records)
			recordOf[i], blockOf[i] = len(records), -1
			records = append(records, claudeUngroupedRecord{position: i})
			continue
		}
		owner, ok := byEntryIndex[*entry.ParentIndex]
		if !ok {
			// A child without a carrier in this stream cannot be grouped;
			// keep it where it is as a record of its own.
			recordOf[i], blockOf[i] = len(records), -1
			records = append(records, claudeUngroupedRecord{position: i})
			continue
		}
		recordOf[i], blockOf[i] = owner, len(records[owner].children)
		records[owner].children = append(records[owner].children, i)
	}

	// replyOf maps each assistant record to the first record of its message.
	replyOf := make([]int, len(records))
	members := make(map[int][]int)
	firstByID := make(map[string]int)
	for r := range records {
		replyOf[r] = -1
		carrier := entries[records[r].position]
		id, eligible := messageIDs[carrier.EntryIndex]
		if !eligible {
			continue
		}
		reply := r
		if id != "" {
			if first, seen := firstByID[id]; seen {
				reply = first
			} else {
				firstByID[id] = r
			}
		}
		replyOf[r] = reply
		members[reply] = append(members[reply], r)
	}

	drop := make([]bool, len(entries))
	replies := make(map[int]schema.SessionEntry, len(members))
	for reply, group := range members {
		merged, dropped, err := mergeClaudeReply(entries, records, group, fullContent)
		if err != nil {
			return nil, nil, err
		}
		replies[reply] = merged
		for _, position := range dropped {
			drop[position] = true
		}
	}

	newIndex := make(map[int]int, len(entries))
	removed := 0
	for i, entry := range entries {
		if drop[i] {
			removed++
			continue
		}
		newIndex[entry.EntryIndex] = entry.EntryIndex - removed
	}

	out := make([]schema.SessionEntry, 0, len(entries)-removed)
	sources := make([]claudeEntrySource, 0, len(entries)-removed)
	for i, entry := range entries {
		if drop[i] {
			continue
		}
		r := recordOf[i]
		if blockOf[i] < 0 {
			if merged, isReply := replies[r]; isReply {
				entry = merged
			}
		} else if reply := replyOf[r]; reply >= 0 {
			parent := entries[records[reply].position].EntryIndex
			entry.ParentIndex = &parent
		}
		entry.EntryIndex = newIndex[entry.EntryIndex]
		if entry.ParentIndex != nil {
			if parent, ok := newIndex[*entry.ParentIndex]; ok {
				entry.ParentIndex = &parent
			}
		}
		out = append(out, entry)
		sources = append(sources, claudeEntrySource{record: r, block: blockOf[i]})
	}
	return out, sources, nil
}

// mergeClaudeReply builds one reply's depth-0 entry from its member records
// and reports the positions of the entries the reply absorbs: the depth-0
// entries of every member after the first, and the children whose content the
// reply's own entry now holds.
func mergeClaudeReply(entries []schema.SessionEntry, records []claudeUngroupedRecord, group []int, fullContent bool) (schema.SessionEntry, []int, error) {
	reply := entries[records[group[0]].position]
	var dropped []int
	var prose, thinking []string
	var toolNames []string
	var thinkingChildren []int
	hasImage := false
	totalBytes, sized := 0, false
	for n, r := range group {
		record := records[r]
		carrier := entries[record.position]
		if n > 0 {
			dropped = append(dropped, record.position)
			retained, err := RetainedUnknownOf(carrier)
			if err == nil {
				err = AttachRetainedUnknown(&reply, retained)
			}
			if err != nil {
				return schema.SessionEntry{}, nil, fmt.Errorf("ingest.mergeClaudeReply: the retained evidence of record entry %d could not move to its reply at entry %d; the evidence would be lost; report this with the session id: %w", carrier.EntryIndex, reply.EntryIndex, err)
			}
			// Each usage field takes the last record that carries it, the
			// rule claudeMessageUsage applies to the session totals.
			if carrier.TokensIn != nil {
				reply.TokensIn = carrier.TokensIn
			}
			if carrier.TokensOut != nil {
				reply.TokensOut = carrier.TokensOut
			}
			if carrier.StopReason != nil {
				reply.StopReason = carrier.StopReason
			}
			reply.IsError = reply.IsError || carrier.IsError
		}
		if carrier.RawByteLength != nil {
			totalBytes += *carrier.RawByteLength
			sized = true
		}
		reply.HasToolUse = reply.HasToolUse || carrier.HasToolUse
		reply.HasThinking = reply.HasThinking || carrier.HasThinking
		if carrier.ToolNamesCSV != nil {
			toolNames = append(toolNames, *carrier.ToolNamesCSV)
		}
		recordProse := false
		for _, position := range record.children {
			child := entries[position]
			switch {
			case child.EntryType == schema.EntryTypeImage:
				hasImage = true
			case claudeProseChild(child):
				recordProse = true
				prose = append(prose, *child.ContentPreview)
			case child.EntryType == schema.EntryTypeThinking:
				thinkingChildren = append(thinkingChildren, position)
				if child.ContentPreview != nil && *child.ContentPreview != "" {
					thinking = append(thinking, *child.ContentPreview)
				}
			}
		}
		if !recordProse && len(record.children) == 0 && carrier.EntryType == schema.EntryTypeText && carrier.ContentPreview != nil && strings.TrimSpace(*carrier.ContentPreview) != "" {
			// A record decomposed without children (full depth off) keeps
			// its first text block on its own entry.
			prose = append(prose, *carrier.ContentPreview)
		}
		if len(record.children) == 0 && carrier.EntryType == schema.EntryTypeThinking && carrier.ContentPreview != nil && *carrier.ContentPreview != "" {
			thinking = append(thinking, *carrier.ContentPreview)
		}
	}
	if !hasImage {
		for _, r := range group {
			for _, position := range records[r].children {
				if claudeProseChild(entries[position]) {
					dropped = append(dropped, position)
				}
			}
		}
	}
	if sized {
		reply.RawByteLength = &totalBytes
	}
	if len(toolNames) > 0 {
		csv := strings.Join(toolNames, ",")
		reply.ToolNamesCSV = &csv
	} else {
		reply.ToolNamesCSV = nil
	}
	joined := strings.Join(prose, replyProseSeparator)
	hasProse := strings.TrimSpace(joined) != ""
	reply.EntryType = assistantEntryType(hasProse, reply.HasToolUse, reply.HasThinking)
	switch {
	case hasProse:
		reply.ContentPreview = claudeReplyPreview(joined, fullContent)
	case !reply.HasToolUse && len(thinking) > 0:
		// A thinking-only reply is a standalone thinking turn: the thinking
		// is its content, and its thinking blocks are not repeated below it.
		reply.ContentPreview = claudeReplyPreview(strings.Join(thinking, replyProseSeparator), fullContent)
		dropped = append(dropped, thinkingChildren...)
	default:
		reply.ContentPreview = nil
	}
	return reply, dropped, nil
}

// claudeProseChild reports whether a decomposed child is one of the reply's
// own text blocks: an assistant text block with words.
func claudeProseChild(child schema.SessionEntry) bool {
	return child.Role == RoleAssistant && child.EntryType == schema.EntryTypeText && child.PartType != nil && *child.PartType == claudeContentBlockText &&
		child.ContentPreview != nil && strings.TrimSpace(*child.ContentPreview) != ""
}

func claudeReplyPreview(text string, fullContent bool) *string {
	if !fullContent {
		text = truncateString(text, defaults.ContentPreviewLimit)
	}
	return &text
}
