package transcript

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// A stored record may be far larger than the wire contract's document policy
// allows a served session_detail to be: the store keeps whole records, while the
// contract caps the document that carries them. Peasant never refuses a session
// for size. Instead the READ path bounds the oversized text it serves and says
// so in the text itself, so every consumer of this projection — the
// session_detail WebSocket, the kickstart preview, `peasant export` and the
// publication content — carries the same visible note. The stored entries are
// untouched; nothing here writes.

// ServedTextKind names the served text field a display bound applies to. It is
// closed: the bound is only ever applied to text this projection produces.
type ServedTextKind uint8

const (
	// ServedTextToolResult is a tool call's recorded result body.
	ServedTextToolResult ServedTextKind = iota
	// ServedTextToolArguments is a tool call's recorded argument document.
	ServedTextToolArguments
	// ServedTextTurnContent is a turn's own body text.
	ServedTextTurnContent
)

// String is the note wording for the kind. It is the ONLY source of that
// wording, so a note can never name a field the bound did not apply to.
func (k ServedTextKind) String() string {
	switch k {
	case ServedTextToolResult:
		return "tool result"
	case ServedTextToolArguments:
		return "tool arguments"
	case ServedTextTurnContent:
		return "turn content"
	}
	return "served text"
}

// IsValid reports whether k is one of the declared kinds.
func (k ServedTextKind) IsValid() bool {
	return k <= ServedTextTurnContent
}

// ServedDocumentBudget is the pair of sizes a served detail is held to: a
// per-field display bound and the encoded size of the whole document. Both are
// derived from one contract-cap constant in internal/defaults; neither is
// written as a literal here.
type ServedDocumentBudget struct {
	perField int
	document int
}

// PerField is the display bound one text field is held to before the document
// budget is considered.
func (b ServedDocumentBudget) PerField() int { return b.perField }

// Document is the encoded size the whole served document is held to.
func (b ServedDocumentBudget) Document() int { return b.document }

// NewServedDocumentBudget is the constructor boundary for a budget. Callers
// outside the defaults are rare (tests that must drive the bound with small
// sizes); the constructor exists so no caller can assemble an incoherent pair.
func NewServedDocumentBudget(perField, document int) (ServedDocumentBudget, error) {
	if perField < 0 || document <= 0 {
		return ServedDocumentBudget{}, fmt.Errorf(
			"served document budget rejected at transcript.NewServedDocumentBudget: per-field bound %d and document budget %d are not both usable sizes; "+
				"this ran while preparing a session detail for display, so nothing was served; a bound of zero bytes shows only the note and a negative or zero document budget cannot hold any document; "+
				"pass a per-field bound of zero or more and a document budget above zero, or use DefaultServedDocumentBudget",
			perField, document)
	}
	if perField > document {
		return ServedDocumentBudget{}, fmt.Errorf(
			"served document budget rejected at transcript.NewServedDocumentBudget: per-field bound %d exceeds document budget %d; "+
				"this ran while preparing a session detail for display, so nothing was served; one field would be allowed to fill more than the whole document, so the document bound could never hold; "+
				"lower the per-field bound to at most the document budget",
			perField, document)
	}
	return ServedDocumentBudget{perField: perField, document: document}, nil
}

// DefaultServedDocumentBudget is the budget every production served path uses.
// Its document size sits below the contract's document cap by a fixed margin, so
// a payload built at this budget decodes through schema.DecodeSessionDetailPayloadRaw.
func DefaultServedDocumentBudget() ServedDocumentBudget {
	return ServedDocumentBudget{
		perField: defaults.ServedTextFieldBudgetBytes,
		document: defaults.ServedDetailDocumentBudgetBytes,
	}
}

// ServedBoundReport says what a bound actually did, so a caller or a test can
// assert the outcome rather than infer it from the text.
type ServedBoundReport struct {
	// AppliedBytes is the display bound that ended up in force. It equals the
	// budget's per-field bound unless the document budget forced a lower one.
	AppliedBytes int
	// BoundedFields counts the text fields that were shortened.
	BoundedFields int
	// WithinDocumentBudget reports that the encoded document fits the budget.
	// It is false only when text this projection does not bound already fills
	// the budget by itself.
	WithinDocumentBudget bool
}

// servedTextField is one bounded text field: where it lives and what it held.
type servedTextField struct {
	target   *string
	original string
	encoded  int
	kind     ServedTextKind
}

// BoundServedDetail bounds the oversized served text of detail in place and
// returns what it did.
//
// It is applied ONCE, by the canonical producer in sessionToDetail, so a served
// field carries exactly one note. It is not written to be applied twice: a
// bounded value carries its note inside itself, so a second application would
// treat the note as part of the record and bound it again. Every served path
// reaches the producer, so there is no second application to guard against.
func BoundServedDetail(detail *schema.SessionDetailPayload, budget ServedDocumentBudget) ServedBoundReport {
	report := ServedBoundReport{AppliedBytes: budget.perField, WithinDocumentBudget: true}
	if detail == nil {
		return report
	}
	fields := collectServedTextFields(detail)
	if len(fields) == 0 {
		return report
	}
	applyServedBound(fields, budget.perField)
	bound := budget.perField
	if base, ok := servedDocumentBaseSize(detail, fields); ok {
		bound, report.WithinDocumentBudget = servedFittingBound(fields, base, budget)
		applyServedBound(fields, bound)
	}
	report.AppliedBytes = bound
	for _, field := range fields {
		if len(field.original) > bound {
			report.BoundedFields++
		}
	}
	return report
}

// collectServedTextFields walks the payload once, in document order, so the
// bound is deterministic for a given payload.
func collectServedTextFields(detail *schema.SessionDetailPayload) []servedTextField {
	fields := make([]servedTextField, 0, len(detail.Turns))
	for turnIndex := range detail.Turns {
		turn := &detail.Turns[turnIndex]
		fields = append(fields, servedTextField{target: &turn.Content, original: turn.Content, kind: ServedTextTurnContent})
		for callIndex := range turn.ToolCalls {
			call := &turn.ToolCalls[callIndex]
			fields = append(fields, servedTextField{target: &call.Arguments, original: call.Arguments, kind: ServedTextToolArguments})
			fields = append(fields, servedTextField{target: &call.Result, original: call.Result, kind: ServedTextToolResult})
		}
	}
	for index := range fields {
		fields[index].encoded = jsonEncodedStringLen(fields[index].original)
	}
	return fields
}

// servedDocumentBaseSize is the encoded size of the document with every bounded
// field emptied. The bounded fields all carry a non-omitted JSON key, so the
// size at a given bound is this base plus each field's encoded value, and the
// document size can be evaluated without re-encoding the whole payload.
func servedDocumentBaseSize(detail *schema.SessionDetailPayload, fields []servedTextField) (int, bool) {
	held := make([]string, len(fields))
	for index, field := range fields {
		held[index] = *field.target
		*field.target = ""
	}
	encoded, err := json.Marshal(detail)
	for index, field := range fields {
		*field.target = held[index]
	}
	if err != nil {
		// The payload does not encode at all. That is not a size problem and it
		// is not this function's to report: the caller's own encode, on the
		// validated producer boundary, raises it with the failing value.
		return 0, false
	}
	// Each emptied field contributed the two quote bytes of "".
	return len(encoded) - 2*len(fields), true
}

// servedFittingBound is the largest display bound at or below the budget's
// per-field bound whose document still fits. Because the bound is shared, the
// largest fields give up bytes first and equally sized fields give up equally.
func servedFittingBound(fields []servedTextField, base int, budget ServedDocumentBudget) (int, bool) {
	if servedDocumentSize(fields, base, budget.perField) <= budget.document {
		return budget.perField, true
	}
	// Candidate bounds are the field sizes plus zero: between two adjacent field
	// sizes the document size only grows with the bound, so the answer is one of
	// them.
	candidates := make([]int, 0, len(fields)+1)
	candidates = append(candidates, 0)
	for _, field := range fields {
		candidates = append(candidates, len(field.original))
	}
	sort.Ints(candidates)
	low, high := 0, len(candidates)-1
	best := -1
	for low <= high {
		middle := (low + high) / 2
		if servedDocumentSize(fields, base, candidates[middle]) <= budget.document {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	// The note names the shown size in human units, so a one-byte rise in the
	// bound can shorten the note by a byte or two. The search is verified rather
	// than assumed: step down until the chosen bound really fits.
	for best >= 0 && servedDocumentSize(fields, base, candidates[best]) > budget.document {
		best--
	}
	if best < 0 {
		// Not even a note-only document fits: text this projection does not
		// bound already fills the budget. Bound everything to the note and say
		// the document did not fit, rather than pretend it did.
		return 0, false
	}
	return candidates[best], true
}

// servedDocumentSize is the encoded document size when every field is bound to
// showBytes.
func servedDocumentSize(fields []servedTextField, base int, showBytes int) int {
	size := base
	for _, field := range fields {
		if len(field.original) <= showBytes {
			size += field.encoded
			continue
		}
		size += jsonEncodedStringLen(boundServedText(field.kind, field.original, showBytes))
	}
	return size
}

// applyServedBound rewrites every field from its ORIGINAL value, so the bound can
// be lowered and raised while searching without compounding notes.
func applyServedBound(fields []servedTextField, showBytes int) {
	for _, field := range fields {
		*field.target = boundServedText(field.kind, field.original, showBytes)
	}
}

// boundServedText returns text unchanged when it fits, and otherwise the leading
// showBytes of it (cut on a rune boundary) followed by a visible note naming what
// was shown, what was recorded, and where the whole record still lives.
func boundServedText(kind ServedTextKind, text string, showBytes int) string {
	if len(text) <= showBytes {
		return text
	}
	shown := text[:cutServedTextAt(text, showBytes)]
	return shown + servedBoundNote(kind, len(shown), len(text))
}

// cutServedTextAt backs a byte cut off the middle of a rune, so a bounded field
// stays valid UTF-8 and the contract's raw scanner still accepts the document.
func cutServedTextAt(text string, showBytes int) int {
	if showBytes <= 0 {
		return 0
	}
	if showBytes >= len(text) {
		return len(text)
	}
	cut := showBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}

// servedBoundNote is the visible note. Its wording follows the partial-preview
// line style: plain sentence text, because it is content a reader sees, not UI
// chrome.
func servedBoundNote(kind ServedTextKind, shown, recorded int) string {
	return fmt.Sprintf("\n[%s bounded for display: showing %s of %s; the full record is stored locally]",
		kind, formatServedBytes(shown), formatServedBytes(recorded))
}

// formatServedBytes renders a size the way the note reports it.
func formatServedBytes(size int) string {
	if size < 1<<10 {
		return fmt.Sprintf("%d B", size)
	}
	// A size that would render as "1024.0 KiB" is reported in the next unit
	// instead, so a bound landing one byte under a mebibyte does not read as more
	// than a mebibyte.
	if kib := float64(size) / float64(1<<10); kib < 1023.95 {
		return fmt.Sprintf("%.1f KiB", kib)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/float64(1<<20))
}

// jsonEncodedStringLen is the number of bytes the JSON encoder writes for s,
// quotes included. It is measured with the encoder itself so escaping,
// HTML-escaping and invalid-UTF-8 replacement are counted exactly as the
// document will carry them.
func jsonEncodedStringLen(s string) int {
	encoded, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string does not fail; a future change that made it
		// fail must not silently under-count the document.
		return len(s) + 2
	}
	return len(encoded)
}
