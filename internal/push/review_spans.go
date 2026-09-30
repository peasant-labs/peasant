package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/peasant-labs/schema"
)

// ReviewTurnSpan is where one turn lies in the review text PublicationReviewText
// returns, as byte offsets from the start of that text, End exclusive.
// EntryIndex is the turn's TurnDetail.index, the index the transcript viewer
// opens a turn by.
type ReviewTurnSpan struct {
	Start, End int
	EntryIndex int
	ToolCalls  []ReviewToolCallSpan
}

// ReviewToolCallSpan is where one tool call of a turn lies in the review text.
type ReviewToolCallSpan struct {
	Start, End int
	ID         string
}

// PublicationReviewSpans locates every turn and tool call of the content in the review
// text, so a redaction match found in that text can name the turn that shows
// it. The review text starts with the content's JSON encoding, and the encoding
// of each turn inside it is the turn's own encoding, so each is found in order.
// A turn the encoding does not contain is an error: a caller must not report a
// match as lying outside every turn when it could not tell.
func PublicationReviewSpans(content schema.TranscriptContent) (ReviewSpans, error) {
	if content.SessionDetail == nil {
		return ReviewSpans{}, nil
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return ReviewSpans{}, fmt.Errorf("locate turns in the review text: encode the content: %w", err)
	}
	turns := make([]ReviewTurnSpan, 0, len(content.SessionDetail.Turns))
	cursor := 0
	for _, turn := range content.SessionDetail.Turns {
		raw, err := json.Marshal(turn)
		if err != nil {
			return ReviewSpans{}, fmt.Errorf("locate turn %d in the review text: encode it: %w", turn.Index, err)
		}
		at := bytes.Index(encoded[cursor:], raw)
		if at < 0 {
			return ReviewSpans{}, fmt.Errorf("locate turn %d in the review text: its encoding is not in the content's encoding, so no match can be placed in a turn", turn.Index)
		}
		span := ReviewTurnSpan{Start: cursor + at, End: cursor + at + len(raw), EntryIndex: turn.Index}
		callCursor := span.Start
		for _, call := range turn.ToolCalls {
			rawCall, err := json.Marshal(call)
			if err != nil {
				return ReviewSpans{}, fmt.Errorf("locate tool call %q of turn %d in the review text: encode it: %w", call.ID, turn.Index, err)
			}
			at := bytes.Index(encoded[callCursor:span.End], rawCall)
			if at < 0 {
				return ReviewSpans{}, fmt.Errorf("locate tool call %q of turn %d in the review text: its encoding is not in the turn's encoding", call.ID, turn.Index)
			}
			start := callCursor + at
			span.ToolCalls = append(span.ToolCalls, ReviewToolCallSpan{Start: start, End: start + len(rawCall), ID: call.ID})
			callCursor = start + len(rawCall)
		}
		turns = append(turns, span)
		cursor = span.End
	}
	return ReviewSpans{turns: turns}, nil
}

// ReviewSpans holds the turn spans of one review text in order.
type ReviewSpans struct {
	turns []ReviewTurnSpan
}

// Locate names the turn, and the tool call when there is one, that holds the
// byte at offset in the review text. ok is false when the offset lies outside
// every turn, for example in session metadata.
func (s ReviewSpans) Locate(offset int) (entryIndex int, toolCallID string, ok bool) {
	i := sort.Search(len(s.turns), func(i int) bool { return s.turns[i].End > offset })
	if i == len(s.turns) || offset < s.turns[i].Start {
		return 0, "", false
	}
	turn := s.turns[i]
	for _, call := range turn.ToolCalls {
		if offset >= call.Start && offset < call.End {
			return turn.EntryIndex, call.ID, true
		}
	}
	return turn.EntryIndex, "", true
}
