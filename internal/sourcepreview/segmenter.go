package sourcepreview

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// Segment is one contract transcript segment cut from a capture, with the
// records that belong beside its turns and the ref the directory appends.
type Segment struct {
	Segment         schema.TranscriptSegment
	Ref             schema.TranscriptSegmentRef
	NativeMetadata  []schema.NativeMetadataRecord
	RetainedUnknown []schema.RetainedUnknownRecord
	// EncodedBytes is the JSON size of Segment, NativeMetadata and
	// RetainedUnknown together: what the segment adds to a frame.
	EncodedBytes int
}

// ErrSerializedBudget reports that the next segment would pass the serialized
// cap. Nothing more is handed out; the caller ends the capture with a cut.
var ErrSerializedBudget = errors.New("the preview holds as many turns as one preview may; nothing more is sent")

// ErrFrameExceedsCap reports a segment that still does not fit the frame cap
// after its oversized text was bounded to the frame.
var ErrFrameExceedsCap = errors.New("one segment of this session is larger than one preview frame may carry; nothing was sent")

// Segmenter turns a capture's sealed turns into contract segments for a
// surface that pages by segment. It cuts with the contract's rule: a segment
// fills with whole turns until the next would pass the budget, and a turn
// larger than the budget is alone in its segment with its text bounded.
//
// It hands out one segment per Next. When the buffered turns run out before a
// segment is full, Next hands out the short segment rather than waiting for
// another source read, so first paint never waits on a second read. Preview
// segment boundaries are therefore local to the capture.
type Segmenter struct {
	revision    string
	opts        SegmenterOptions
	bound       transcript.ServedDocumentBudget
	harness     schema.Harness
	numberState transcript.PartitionNumberState
	turns       []ingest.Turn
	sizes       []int
	metadata    []schema.NativeMetadataRecord
	retained    []RetainedRecord
	refs        []schema.TranscriptSegmentRef
	loaded      int
	conv        int
	serialized  int64
	done        bool
}

// NewSegmenter builds a segmenter for one capture. revision is the capture's
// decimal revision, so no segment of one capture can sit beside another's.
func NewSegmenter(revision string, opts SegmenterOptions) (*Segmenter, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if !decimalRevision(revision) {
		return nil, fmt.Errorf("segmenter refused at sourcepreview.NewSegmenter: revision %q is not a decimal string; a segment's revision keeps one capture's segments apart from another's; pass the capture revision as decimal digits", revision)
	}
	bound := transcript.DefaultServedDocumentBudget()
	if opts.FrameCapBytes > 0 {
		// One oversized turn is bounded to fit the frame, with room for the
		// envelope, rather than to the whole-document bound.
		perField := min(defaults.ServedTextFieldBudgetBytes, opts.FrameCapBytes/4)
		budget, err := transcript.NewServedDocumentBudget(perField, opts.FrameCapBytes/2)
		if err != nil {
			return nil, fmt.Errorf("segmenter refused at sourcepreview.NewSegmenter: the frame cap %d bytes gives no valid served-text bound: %w; pass a larger frame cap", opts.FrameCapBytes, err)
		}
		bound = budget
	}
	return &Segmenter{revision: revision, opts: opts, bound: bound}, nil
}

// Push buffers a page's sealed turns and records.
func (s *Segmenter) Push(page Page) {
	if s.harness == "" && page.Session.Harness != "" {
		s.harness = page.Session.Harness
	}
	for _, turn := range page.Turns {
		s.turns = append(s.turns, turn)
		s.sizes = append(s.sizes, transcript.FoldedTurnByteLength(turn))
	}
	s.metadata = append(s.metadata, page.NativeMetadata...)
	s.retained = append(s.retained, page.RetainedUnknown...)
}

// Pending reports whether buffered turns wait to be cut.
func (s *Segmenter) Pending() bool { return len(s.turns) > 0 }

// Next cuts one segment from the buffered turns. final reports that the
// capture has ended, so the segment that empties the buffer is the last and
// carries hasNewer false. ok is false when nothing is buffered.
func (s *Segmenter) Next(final bool) (segment Segment, ok bool, err error) {
	if s.done {
		return Segment{}, false, ErrSerializedBudget
	}
	if len(s.turns) == 0 {
		return Segment{}, false, nil
	}
	count, total := 0, 0
	for count < len(s.turns) {
		if count > 0 && total+s.sizes[count] > s.opts.BudgetBytes {
			break
		}
		total += s.sizes[count]
		count++
	}
	turns := s.turns[:count]
	rows := transcript.SegmentTurnRows(turns, s.bound)
	if displayNumbers, ok := s.numberState.NextSegment(turns, s.harness); ok && len(displayNumbers) == len(rows) {
		for i := range rows {
			rows[i].DisplayNumber = displayNumbers[i]
		}
	}
	index := len(s.refs)
	ref := schema.TranscriptSegmentRef{Index: index, FirstTurn: turns[0].Index, LastTurn: turns[count-1].Index, ByteLength: total,
		ConversationTurns: &schema.ConversationTurnSpan{Before: s.conv}}
	for _, row := range rows {
		if schema.IsConversationTurn(row.Role, row.EntryType, row.Depth) {
			ref.ConversationTurns.Count++
		}
	}
	segment = Segment{
		Segment: schema.TranscriptSegment{Index: index, Revision: s.revision, FirstTurn: ref.FirstTurn, LastTurn: ref.LastTurn,
			ByteLength: total, HasOlder: index > 0, HasNewer: !(final && count == len(s.turns)), Turns: rows},
		Ref: ref,
	}
	segment.NativeMetadata = attachedMetadata(s.metadata, turns)
	boundary := math.MaxInt
	if count < len(s.turns) {
		boundary = s.turns[count].Index
	}
	keep := s.retained[:0:0]
	for _, record := range s.retained {
		if record.EntryIndex < boundary {
			segment.RetainedUnknown = append(segment.RetainedUnknown, record.Record)
		} else {
			keep = append(keep, record)
		}
	}
	encoded, err := json.Marshal(struct {
		Segment         schema.TranscriptSegment       `json:"segment"`
		NativeMetadata  []schema.NativeMetadataRecord  `json:"nativeMetadata,omitempty"`
		RetainedUnknown []schema.RetainedUnknownRecord `json:"retainedUnknown,omitempty"`
	}{segment.Segment, segment.NativeMetadata, segment.RetainedUnknown})
	if err != nil {
		return Segment{}, false, fmt.Errorf("cut segment %d at sourcepreview.Segmenter.Next: encoding it failed: %w; nothing was handed out; report this as a defect", index, err)
	}
	segment.EncodedBytes = len(encoded)
	if s.opts.FrameCapBytes > 0 && segment.EncodedBytes > s.opts.FrameCapBytes {
		return Segment{}, false, fmt.Errorf("%w (segment %d encodes to %d bytes, over the %d-byte frame cap)", ErrFrameExceedsCap, index, segment.EncodedBytes, s.opts.FrameCapBytes)
	}
	if s.opts.SerializedCapBytes > 0 && s.serialized+int64(segment.EncodedBytes) > s.opts.SerializedCapBytes {
		s.done = true
		s.turns, s.sizes, s.metadata, s.retained = nil, nil, nil, nil
		return Segment{}, false, ErrSerializedBudget
	}
	if err := segment.Segment.Validate(); err != nil {
		return Segment{}, false, fmt.Errorf("cut segment %d at sourcepreview.Segmenter.Next: the segment does not satisfy the contract: %w; nothing was handed out; report this as a defect", index, err)
	}
	s.serialized += int64(segment.EncodedBytes)
	s.conv += ref.ConversationTurns.Count
	s.loaded += count
	s.refs = append(s.refs, ref)
	s.turns, s.sizes = s.turns[count:], s.sizes[count:]
	s.metadata = keepUnattached(s.metadata, turns)
	s.retained = keep
	return segment, true, nil
}

// SerializedCutNotice is the sentence for a capture the serialized cap ended.
func (s *Segmenter) SerializedCutNotice() string {
	return serializedBudgetNotice(s.opts.SerializedCapBytes)
}

// Refs returns every ref handed out so far, in order.
func (s *Segmenter) Refs() []schema.TranscriptSegmentRef {
	return append([]schema.TranscriptSegmentRef(nil), s.refs...)
}

// LoadedTurns counts the turns of every segment handed out.
func (s *Segmenter) LoadedTurns() int { return s.loaded }

// LoadedConversationTurns counts the conversation turns of every segment
// handed out.
func (s *Segmenter) LoadedConversationTurns() int { return s.conv }

// SerializedBytes is the encoded size of every segment handed out.
func (s *Segmenter) SerializedBytes() int64 { return s.serialized }

func keepUnattached(records []schema.NativeMetadataRecord, sent []ingest.Turn) []schema.NativeMetadataRecord {
	if len(records) == 0 {
		return records
	}
	indexes := make(map[int]struct{}, len(sent))
	for _, turn := range sent {
		indexes[turn.Index] = struct{}{}
	}
	keep := records[:0:0]
	for _, record := range records {
		if record.Attachment != nil && record.Attachment.TurnIndex != nil {
			if _, ok := indexes[*record.Attachment.TurnIndex]; ok {
				continue
			}
		}
		keep = append(keep, record)
	}
	return keep
}

func decimalRevision(revision string) bool {
	if revision == "" {
		return false
	}
	for _, r := range revision {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
