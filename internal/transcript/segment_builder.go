package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// PartitionDisplayNumbers computes the wire displayNumber for every turn in a
// partition using schema.CookTranscriptRows over the whole partition, never
// over a single segment's turns. Returns nil, false when the partition has no
// valid harness, no kept turns, or CookTranscriptRows refuses the partition.
// Unkept turns (empty, short system banners, or adjacent duplicates folded by
// CookTranscriptRows) inherit their kept neighbor's label so every served
// segment satisfies the contract's all-or-none segment numbering invariant.
func PartitionDisplayNumbers(folded []ingest.Turn, harness schema.Harness) ([]string, bool) {
	if len(folded) == 0 || !slices.Contains(schema.AllHarnesses, harness) {
		return nil, false
	}
	verdicts, err := cookTurnVerdicts(folded, harness)
	if err != nil {
		return nil, false
	}
	keptLabels := make([]string, len(verdicts))
	for i, verdict := range verdicts {
		if verdict.Keep {
			keptLabels[i] = verdict.DisplayNumber
		}
	}
	return fillDisplayNumbers(folded, verdicts, keptLabels)
}

// PartitionNumberState tracks incremental displayNumber assignment across
// consecutive streamed segments without retaining earlier turn text. Calling
// NextSegment for each contiguous cut produces the same labels as calling
// PartitionDisplayNumbers over the full partition at once.
type PartitionNumberState struct {
	hasKept  bool
	disabled bool
	task     int
	sub      int
	lastKept ingest.Turn
}

// NextSegment computes the displayNumber for each turn in the next segment of
// a partition, continuing task and sub counters across segment boundaries.
func (s *PartitionNumberState) NextSegment(turns []ingest.Turn, harness schema.Harness) ([]string, bool) {
	if s.disabled || len(turns) == 0 || !slices.Contains(schema.AllHarnesses, harness) {
		return nil, false
	}
	if !s.hasKept {
		verdicts, err := cookTurnVerdicts(turns, harness)
		if err != nil {
			s.disabled = true
			return nil, false
		}
		keptLabels := make([]string, len(verdicts))
		lastKeptIdx := -1
		for i, verdict := range verdicts {
			if !verdict.Keep || verdict.DisplayNumber == "" {
				continue
			}
			task, sub, ok := parseDisplayNumber(verdict.DisplayNumber)
			if !ok {
				s.disabled = true
				return nil, false
			}
			keptLabels[i] = verdict.DisplayNumber
			s.task, s.sub = task, sub
			lastKeptIdx = i
		}
		if lastKeptIdx < 0 {
			return nil, false
		}
		s.hasKept = true
		s.lastKept = stripTurnForCook(turns[lastKeptIdx])
		return fillDisplayNumbers(turns, verdicts, keptLabels)
	}

	batch := make([]ingest.Turn, 1+len(turns))
	batch[0] = s.lastKept
	copy(batch[1:], turns)
	verdicts, err := cookTurnVerdicts(batch, harness)
	if err != nil {
		s.disabled = true
		return nil, false
	}
	prevLabel := formatDisplayNumber(s.task, s.sub)
	keptLabels := make([]string, len(verdicts))
	havePrevCook := false
	prevCookTask := 0
	lastKeptIdx := 0
	if verdicts[0].Keep && verdicts[0].DisplayNumber != "" {
		cookTask, _, ok := parseDisplayNumber(verdicts[0].DisplayNumber)
		if !ok {
			s.disabled = true
			return nil, false
		}
		keptLabels[0] = prevLabel
		prevCookTask = cookTask
		havePrevCook = true
	}
	for i := 1; i < len(verdicts); i++ {
		if !verdicts[i].Keep || verdicts[i].DisplayNumber == "" {
			continue
		}
		cookTask, _, ok := parseDisplayNumber(verdicts[i].DisplayNumber)
		if !ok {
			s.disabled = true
			return nil, false
		}
		if !havePrevCook {
			havePrevCook = true
			prevCookTask = cookTask
			keptLabels[i] = prevLabel
		} else {
			if cookTask > prevCookTask {
				s.task++
				s.sub = 0
			} else {
				s.sub++
			}
			prevCookTask = cookTask
			keptLabels[i] = formatDisplayNumber(s.task, s.sub)
		}
		lastKeptIdx = i
	}
	if lastKeptIdx > 0 {
		s.lastKept = stripTurnForCook(batch[lastKeptIdx])
	}
	numbers, ok := fillDisplayNumbers(batch, verdicts, keptLabels)
	if !ok {
		return nil, false
	}
	return numbers[1:], true
}

func cookTurnVerdicts(folded []ingest.Turn, harness schema.Harness) ([]schema.CookedTurn, error) {
	turns := stripTurnsForCookDetail(folded)
	raw, err := json.Marshal(struct {
		Turns []schema.TurnDetail `json:"turns"`
	}{Turns: turns})
	if err != nil {
		return nil, err
	}
	cookTurns, err := schema.NewTranscriptCookTurns(raw)
	if err != nil {
		return nil, err
	}
	return schema.CookTranscriptRows(cookTurns, harness)
}

// stripTurnsForCookDetail retains only the fields schema.CookTranscriptRows inspects
// so 32 MiB tool payloads are neither copied nor marshaled during numbering.
func stripTurnsForCookDetail(folded []ingest.Turn) []schema.TurnDetail {
	turns := make([]schema.TurnDetail, len(folded))
	for i, t := range folded {
		var toolCalls []schema.ToolCallDetail
		if len(t.ToolCalls) > 0 {
			toolCalls = make([]schema.ToolCallDetail, len(t.ToolCalls))
			for j, tc := range t.ToolCalls {
				toolCalls[j] = schema.ToolCallDetail{
					CallEntryRef:     schema.SourceEntryRef(tc.CallEntryRef),
					ResultEntryRef:   schema.SourceEntryRef(tc.ResultEntryRef),
					CallProvenance:   tc.CallProvenance,
					ResultProvenance: tc.ResultProvenance,
					Usage:            tc.Usage,
					ID:               tc.ID,
					Name:             tc.Name,
				}
			}
		}
		var parts []schema.TurnContentPart
		for _, part := range contentPartsFromTurn(t) {
			if part.Type == schema.TurnPartTypeImage {
				parts = append(parts, part)
			}
		}
		turns[i] = schema.TurnDetail{
			ContentParts:   parts,
			ControlKind:    t.ControlKind,
			SourceEntryRef: schema.SourceEntryRef(t.SourceEntryRef),
			Provenance:     t.Provenance,
			Usage:          t.Usage,
			Index:          t.Index,
			Role:           t.Role,
			Content:        compactCookContent(t.Content),
			ToolCalls:      toolCalls,
			Depth:          t.Depth,
		}
	}
	return turns
}

func stripTurnForCook(t ingest.Turn) ingest.Turn {
	stripped := ingest.Turn{
		Index:          t.Index,
		Role:           t.Role,
		Content:        compactCookContent(t.Content),
		Depth:          t.Depth,
		ControlKind:    t.ControlKind,
		SourceEntryRef: t.SourceEntryRef,
		Provenance:     t.Provenance,
		Usage:          t.Usage,
	}
	for _, part := range t.Parts {
		if part.Kind == ingest.TurnPartImage {
			stripped.Parts = append(stripped.Parts, part)
		}
	}
	if len(t.ToolCalls) > 0 {
		stripped.ToolCalls = make([]ingest.ToolCall, len(t.ToolCalls))
		for i, tc := range t.ToolCalls {
			stripped.ToolCalls[i] = ingest.ToolCall{
				ID:               tc.ID,
				Name:             tc.Name,
				CallEntryRef:     tc.CallEntryRef,
				ResultEntryRef:   tc.ResultEntryRef,
				CallProvenance:   tc.CallProvenance,
				ResultProvenance: tc.ResultProvenance,
				Usage:            tc.Usage,
			}
		}
	}
	return stripped
}

// compactCookContent replaces long non-blank content with a deterministic
// printable ASCII digest so CookTranscriptRows avoids escaping and scanning
// megabytes of repeated text while preserving non-empty, >=8-unit, and
// exact-duplicate semantics.
func compactCookContent(content string) string {
	const minCompactBytes = 96
	if len(content) <= minCompactBytes || !hasVisibleASCIIBlock(content, 8) {
		return content
	}
	sum := sha256.Sum256([]byte(content))
	return "peasant-cook-sha256:" + hex.EncodeToString(sum[:])
}

func hasVisibleASCIIBlock(s string, minRun int) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= '!' && b <= '~' {
			run++
			if run >= minRun {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func fillDisplayNumbers(turns []ingest.Turn, verdicts []schema.CookedTurn, keptLabels []string) ([]string, bool) {
	firstKept := ""
	for i, verdict := range verdicts {
		if verdict.Keep && keptLabels[i] != "" {
			firstKept = keptLabels[i]
			break
		}
	}
	if firstKept == "" {
		return nil, false
	}
	numbers := make([]string, len(verdicts))
	lastKept := ""
	for i, verdict := range verdicts {
		if verdict.Keep && keptLabels[i] != "" {
			numbers[i] = keptLabels[i]
			lastKept = keptLabels[i]
			continue
		}
		if inheritsNextKeptDisplayNumber(turns, verdicts, keptLabels, i) {
			numbers[i] = keptLabels[i+1]
			continue
		}
		if lastKept != "" {
			numbers[i] = lastKept
		} else {
			numbers[i] = firstKept
		}
	}
	return numbers, true
}

func inheritsNextKeptDisplayNumber(turns []ingest.Turn, verdicts []schema.CookedTurn, keptLabels []string, i int) bool {
	if i+1 >= len(verdicts) {
		return false
	}
	if !verdicts[i+1].Keep || keptLabels[i+1] == "" {
		return false
	}
	if turns[i].Role != turns[i+1].Role {
		return false
	}
	return turns[i].Content == turns[i+1].Content && strings.TrimSpace(turns[i].Content) != ""
}

func formatDisplayNumber(task, sub int) string {
	label := strconv.Itoa(task)
	if sub <= 0 {
		return label
	}
	var buf [32]byte
	i := len(buf)
	for n := sub; n > 0; {
		n--
		i--
		buf[i] = byte('a' + n%26)
		n /= 26
	}
	return label + string(buf[i:])
}

func parseDisplayNumber(label string) (task, sub int, ok bool) {
	split := 0
	for split < len(label) && label[split] >= '0' && label[split] <= '9' {
		split++
	}
	if split == 0 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(label[:split])
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	for i := split; i < len(label); i++ {
		c := label[i]
		if c < 'a' || c > 'z' {
			return 0, 0, false
		}
		sub = sub*26 + int(c-'a'+1)
	}
	return n, sub, true
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
