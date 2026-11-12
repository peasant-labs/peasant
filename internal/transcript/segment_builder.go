package transcript

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// A transcript segment is a bounded run of whole turns: the transport unit of
// the transcript read path. This file holds the two facts that path needs from
// the one turn projection: how large a turn is before the served-text bound,
// and how a run of turns is cut into segments without splitting an entry, a
// turn, or a user turn.
//
// The cut and the read must agree on the measure, or the directory's declared
// range and the segment's turns would be different sets. Both use the
// contract's measure, schema.TranscriptTurnByteLength; the cut itself is the
// contract's schema.CutTranscriptSegments.

// ServedTurnBound reports what the served-text bound did to one turn, and the
// turn's size before the bound was applied. It is the per-turn sibling of
// ServedBoundReport: the aggregate report cannot say which turn was bounded or
// what the turn cost before the bound, and the segment row needs both.
type ServedTurnBound struct {
	// Truncated reports that at least one of the turn's own text fields was
	// shortened by the served-text bound.
	Truncated bool
	// ByteLength is the pre-bound encoded text and image metadata size. A segment's
	// ByteLength is the sum of its turns' ByteLength values.
	ByteLength int
}

// BoundServedTurns applies the served-text bound to every turn's own text and
// reports, per turn, whether it was shortened and what it measured before the
// bound. The caller passes the same ServedDocumentBudget the session detail
// path uses, so a segment and a session_detail row share one truncation story.
//
// An oversized turn can sit alone in a segment, so both the per-field and
// whole-document bounds apply just as they do for full detail.
func BoundServedTurns(turns []schema.TurnDetail, budget ServedDocumentBudget) []ServedTurnBound {
	bounds := make([]ServedTurnBound, len(turns))
	original := make([][]string, len(turns))
	for i := range turns {
		bounds[i].ByteLength = schema.TranscriptTurnByteLength(turns[i])
		for _, field := range turnTextFields(&turns[i]) {
			original[i] = append(original[i], *field.target)
		}
	}
	// Apply the same whole-document and per-field policy as detail, including
	// text parts and immutable image metadata, even for a single oversized turn.
	detail := schema.SessionDetailPayload{Turns: turns}
	BoundServedDetail(&detail, budget)
	for i := range turns {
		for j, field := range turnTextFields(&turns[i]) {
			if *field.target != original[i][j] {
				bounds[i].Truncated = true
			}
		}
	}
	return bounds
}

// turnTextField points at one bounded text field of a turn.
type turnTextField struct {
	target *string
	kind   ServedTextKind
}

// turnTextFields lists a turn's own text fields in document order. The order
// is irrelevant to the bound itself, which is per field, but keeping one list
// means every served turn bounds exactly the fields the detail path bounds.
func turnTextFields(turn *schema.TurnDetail) []turnTextField {
	fields := make([]turnTextField, 0, 1+2*len(turn.ToolCalls))
	fields = append(fields, turnTextField{target: &turn.Content, kind: ServedTextTurnContent})
	for i := range turn.ContentParts {
		part := &turn.ContentParts[i]
		if part.Type == schema.TurnPartTypeText {
			fields = append(fields, turnTextField{target: &part.Text, kind: ServedTextTurnContent})
		}
	}
	for i := range turn.ToolCalls {
		call := &turn.ToolCalls[i]
		fields = append(fields,
			turnTextField{target: &call.Arguments, kind: ServedTextToolArguments},
			turnTextField{target: &call.Result, kind: ServedTextToolResult},
		)
	}
	return fields
}

// SegmentTurnRows projects folded turns to served rows for one segment: the one
// TurnDetail projection plus the read's own truncated and byteLength facts. It
// bounds the content in place, so the returned rows are what the response
// carries.
func SegmentTurnRows(folded []ingest.Turn, budget ServedDocumentBudget) []schema.TranscriptTurn {
	turns := turnsToDetail(folded)
	bounds := BoundServedTurns(turns, budget)
	rows := make([]schema.TranscriptTurn, len(turns))
	for i := range turns {
		rows[i] = schema.TranscriptTurn{
			TurnDetail: turns[i],
			Truncated:  bounds[i].Truncated,
			ByteLength: bounds[i].ByteLength,
		}
	}
	return rows
}

// FoldedTurnByteLength returns the pre-bound JSON size of one folded turn. It is
// the measure the directory's refs carry and the measure a segment read must
// reproduce exactly, so a soft store-budget stop inside a turn is detectable.
func FoldedTurnByteLength(turn ingest.Turn) int {
	return schema.TranscriptTurnByteLength(turnsToDetail([]ingest.Turn{turn})[0])
}

// SegmentEntryCounts returns, per segment ref, the number of stored entries
// that belong to the segment's whole turns. Entries are ordered by entry index,
// and a turn owns the entries from its depth-0 identity up to the next turn's
// depth-0 identity, so counting the span to the next segment's first turn gives
// the exact number a segment read may request. The count lets the read stop at
// lastTurn instead of overreading the continuation budget.
func SegmentEntryCounts(entries []schema.SessionEntry, refs []schema.TranscriptSegmentRef) []int {
	counts := make([]int, len(refs))
	if len(entries) == 0 || len(refs) == 0 {
		return counts
	}
	cursor := 0
	for i := range refs {
		for cursor < len(entries) && entries[cursor].EntryIndex < refs[i].FirstTurn {
			cursor++
		}
		start := cursor
		stop := int(^uint(0) >> 1)
		if i+1 < len(refs) {
			stop = refs[i+1].FirstTurn
		}
		for cursor < len(entries) && entries[cursor].EntryIndex < stop {
			cursor++
		}
		counts[i] = cursor - start
	}
	return counts
}

// CutSegments projects the folded turns to wire turns and cuts them with the
// contract's cut, schema.CutTranscriptSegments, so this server's segment
// boundaries are the ones every server produces. It returns one ref per segment
// plus the number of turns it cut. The measure, the whole-turn rule and the
// conversation turn spans are the contract's; this function owns only the
// projection.
func CutSegments(folded []ingest.Turn, budget int) ([]schema.TranscriptSegmentRef, int) {
	turns := turnsToDetail(folded)
	return schema.CutTranscriptSegments(turns, budget), len(turns)
}

// ConversationTurnsRule names the self-check rule a cut failed. Closed set.
type ConversationTurnsRule string

const (
	// ConversationTurnsRulePresence: a ref carries no span.
	ConversationTurnsRulePresence ConversationTurnsRule = "presence"
	// ConversationTurnsRuleContinuity: a span's before differs from the running
	// count, or its count is negative.
	ConversationTurnsRuleContinuity ConversationTurnsRule = "continuity"
	// ConversationTurnsRuleCanonical: the running count differs from the
	// session's canonical conversation turn count.
	ConversationTurnsRuleCanonical ConversationTurnsRule = "canonical"
)

// ConversationTurnsFault reports why a cut's position data is withheld.
type ConversationTurnsFault struct {
	Rule ConversationTurnsRule
	// Segment is the segment ordinal, or -1 for the canonical rule. It is a log
	// attribute and never reaches the wire.
	Segment   int
	Want, Got int
}

func (f *ConversationTurnsFault) Error() string {
	const where = "transcript.CheckConversationTurns: checking the cut's conversation turn positions before the directory is served: "
	const consequence = "; a reader would be shown a wrong position, so the directory is served without conversationTurns and totalConversationTurns"
	switch f.Rule {
	case ConversationTurnsRulePresence:
		return fmt.Sprintf("%ssegment %d carries no conversationTurns span (want 1 span, got %d)%s; make the cut open every segment with a span", where, f.Segment, f.Got, consequence)
	case ConversationTurnsRuleContinuity:
		if f.Got < 0 && f.Want == 0 {
			return fmt.Sprintf("%ssegment %d declares %d as a conversation turn count or start, and neither can be below %d%s; make the cut count only turns it appended", where, f.Segment, f.Got, f.Want, consequence)
		}
		return fmt.Sprintf("%ssegment %d declares %d conversation turns before it, but the earlier segments hold %d%s; make the cut start every span at the running count", where, f.Segment, f.Got, f.Want, consequence)
	case ConversationTurnsRuleCanonical:
		return fmt.Sprintf("%sthe segments hold %d conversation turns in total, but the session's canonical count is %d%s; make the cut and ingest.ConversationTurnCount apply the same rule to the same turns", where, f.Got, f.Want, consequence)
	}
	return fmt.Sprintf("%sunknown rule %q at segment %d (want %d, got %d)%s; add the rule to ConversationTurnsFault.Error", where, string(f.Rule), f.Segment, f.Want, f.Got, consequence)
}

// CheckConversationTurns walks the refs once. It returns nil when every ref
// carries a span, each span starts at the running count with a non-negative
// count, and the running count ends at canonical. Those three facts imply the
// wire rules for the whole list, and each wire rule is per ref or per adjacent
// pair, so no page of a checked directory can be refused over positions.
func CheckConversationTurns(refs []schema.TranscriptSegmentRef, canonical int) *ConversationTurnsFault {
	running := 0
	for i := range refs {
		span := refs[i].ConversationTurns
		if span == nil {
			return &ConversationTurnsFault{Rule: ConversationTurnsRulePresence, Segment: refs[i].Index, Want: 1, Got: 0}
		}
		if span.Before != running {
			return &ConversationTurnsFault{Rule: ConversationTurnsRuleContinuity, Segment: refs[i].Index, Want: running, Got: span.Before}
		}
		if span.Count < 0 {
			return &ConversationTurnsFault{Rule: ConversationTurnsRuleContinuity, Segment: refs[i].Index, Want: 0, Got: span.Count}
		}
		running += span.Count
	}
	if running != canonical {
		return &ConversationTurnsFault{Rule: ConversationTurnsRuleCanonical, Segment: -1, Want: canonical, Got: running}
	}
	return nil
}
