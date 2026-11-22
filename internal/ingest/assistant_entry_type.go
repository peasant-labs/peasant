package ingest

import (
	"github.com/peasant-labs/schema"
)

// assistantEntryType is the one typing rule for a top-level assistant record.
// hasProse means the record holds a non-blank text block or plain text; a
// preview filled from a tool result or from thinking is not prose.
// hasToolUse means the record holds a tool call; hasThinking means it holds a
// thinking block. Prose wins: a reply with prose is text and keeps its tool
// and thinking flags, while a prose-less reply is typed by what it holds.
// A record with none of the three stays text, so a blank text record counts
// and only tool-only and thinking-only replies leave the conversation count.
func assistantEntryType(hasProse, hasToolUse, hasThinking bool) schema.EntryType {
	switch {
	case hasProse:
		return schema.EntryTypeText
	case hasToolUse:
		return schema.EntryTypeToolUse
	case hasThinking:
		return schema.EntryTypeThinking
	default:
		return schema.EntryTypeText
	}
}

// toolOnlyCarrierEntryType types the carrier of an assistant message that
// holds tool calls and no prose or thinking. The carrier is a tool_use turn
// with no countable tool use of its own: no call key, no tool name, no
// content.
var toolOnlyCarrierEntryType = assistantEntryType(false, true, false)

// replyProseSeparator joins the text blocks of one agent reply into the
// reply's prose: each block is a paragraph of its own. Every harness that
// stores a reply as one entry joins its text with it.
const replyProseSeparator = "\n\n"
