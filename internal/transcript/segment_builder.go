package transcript

import (
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
// range and the segment's turns would be different sets. Both use
// TurnByteLength here.

// ServedTurnBound reports what the served-text bound did to one turn, and the
// turn's size before the bound was applied. It is the per-turn sibling of
// ServedBoundReport: the aggregate report cannot say which turn was bounded or
// what the turn cost before the bound, and the segment row needs both.
type ServedTurnBound struct {
	// Truncated reports that at least one of the turn's own text fields was
	// shortened by the served-text bound.
	Truncated bool
	// ByteLength is the pre-bound JSON size of the turn's own content plus
	// every toolCalls[].arguments and toolCalls[].result. A segment's
	// ByteLength is the sum of its turns' ByteLength values.
	ByteLength int
}

// TurnByteLength returns the pre-bound JSON byte length of a turn's own text:
// its content plus every tool call's arguments and result. It is measured with
// the JSON encoder, so escaping is counted exactly as the wire will carry it.
// It is intentionally the size BEFORE the served-text bound, so an oversized
// turn stays detectable in the cut after its text has been shortened.
func TurnByteLength(turn schema.TurnDetail) int {
	total := jsonEncodedStringLen(turn.Content)
	for i := range turn.ToolCalls {
		total += jsonEncodedStringLen(turn.ToolCalls[i].Arguments)
		total += jsonEncodedStringLen(turn.ToolCalls[i].Result)
	}
	return total
}

// BoundServedTurns applies the served-text bound to every turn's own text and
// reports, per turn, whether it was shortened and what it measured before the
// bound. The caller passes the same ServedDocumentBudget the session detail
// path uses, so a segment and a session_detail row share one truncation story.
//
// The document budget's per-field bound is the one that applies here. A segment
// is at most the segment budget by construction (or one oversized turn alone),
// and the response's own total is bounded by the segment cut rather than by the
// session document budget.
func BoundServedTurns(turns []schema.TurnDetail, budget ServedDocumentBudget) []ServedTurnBound {
	bounds := make([]ServedTurnBound, len(turns))
	for i := range turns {
		turn := &turns[i]
		bound := ServedTurnBound{ByteLength: TurnByteLength(*turn)}
		for _, field := range turnTextFields(turn) {
			bounded := boundServedText(field.kind, *field.target, budget.perField)
			if bounded != *field.target {
				bound.Truncated = true
				*field.target = bounded
			}
		}
		bounds[i] = bound
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
	return TurnByteLength(turnsToDetail([]ingest.Turn{turn})[0])
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

// CutSegments fills segments with whole turns up to budget bytes and returns
// one ref per segment plus the number of turns it cut. A turn is appended whole
// or deferred; a turn larger than the budget is alone in its segment. The
// function never splits a turn, so it never splits an entry or a user turn.
//
// The segment's ByteLength is the sum of its turns' TurnByteLength values,
// which is the measure the cut used, so the directory states the same cost the
// segment read will serve.
func CutSegments(folded []ingest.Turn, budget int) ([]schema.TranscriptSegmentRef, int) {
	return CutTurnRows(turnsToDetail(folded), budget)
}

// CutTurnRows cuts already-projected wire turns into segment refs. It exists so
// the cut can be exercised without rebuilding a session's entries and so the
// directory build and the segment read use one implementation.
func CutTurnRows(turns []schema.TurnDetail, budget int) ([]schema.TranscriptSegmentRef, int) {
	if len(turns) == 0 {
		return []schema.TranscriptSegmentRef{}, 0
	}
	refs := make([]schema.TranscriptSegmentRef, 0, len(turns))
	var current *schema.TranscriptSegmentRef
	bytes := 0
	for i := range turns {
		size := TurnByteLength(turns[i])
		if current != nil && bytes+size > budget {
			refs = append(refs, *current)
			current = nil
			bytes = 0
		}
		if current == nil {
			current = &schema.TranscriptSegmentRef{
				Index:      len(refs),
				FirstTurn:  turns[i].Index,
				LastTurn:   turns[i].Index,
				ByteLength: 0,
			}
		}
		current.LastTurn = turns[i].Index
		current.ByteLength += size
		bytes += size
	}
	if current != nil {
		refs = append(refs, *current)
	}
	return refs, len(turns)
}
