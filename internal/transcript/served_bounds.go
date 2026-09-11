package transcript

import (
	"encoding/json"
	"fmt"
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
		if servedTextWasBounded(field, bound) {
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

// servedBoundSearchDipBytes is how far the search looks above a bound that fits
// before it accepts that bound as the largest one. The document size rises with
// the bound by one byte per field still being shortened, and falls only when a
// note's human-readable size crosses a unit boundary and renders a byte or two
// shorter. That dip is smaller than a single step for any field count, so a
// handful of bytes of look-ahead covers it.
const servedBoundSearchDipBytes = 8

// servedFittingBound is the largest display bound at or below the budget's
// per-field bound whose document still fits. Because the bound is shared, the
// largest fields give up bytes first and equally sized fields give up equally.
//
// The bound is searched as an INTEGER number of bytes, not picked from the
// recorded field sizes. The largest bound that fits normally lies BETWEEN two
// field sizes, so a search over the field sizes alone collapses under document
// pressure: with 64 recorded results of 256 KiB under a 7 MiB document budget
// the only sizes on offer are 0 and 262144, the second does not fit, and the
// first serves every result as its note alone. A bound near 110 KiB fits and
// shows the reader the leading part of every result.
func servedFittingBound(fields []servedTextField, base int, budget ServedDocumentBudget) (int, bool) {
	if servedDocumentSize(fields, base, budget.perField) <= budget.document {
		return budget.perField, true
	}
	if servedDocumentSize(fields, base, 0) > budget.document {
		// Not even a note-only document fits: text this projection does not
		// bound already fills the budget. Bound everything to the note and say
		// the document did not fit, rather than pretend it did.
		return 0, false
	}
	low, high, best := 1, budget.perField, 0
	for low <= high {
		middle := low + (high-low)/2
		if servedDocumentSize(fields, base, middle) <= budget.document {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	// The note names the shown size in human units, so the document size is not
	// strictly increasing in the bound and the halving search can stop on either
	// side of a dip. The answer is verified rather than assumed: step down until
	// the chosen bound really fits, then step up over any dip so the bound that
	// is served is the largest that fits.
	for best > 0 && servedDocumentSize(fields, base, best) > budget.document {
		best--
	}
	for step := 0; step < servedBoundSearchDipBytes && best < budget.perField; step++ {
		if servedDocumentSize(fields, base, best+1) > budget.document {
			break
		}
		best++
	}
	return best, true
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
//
// A field is left WHOLE when its note would be no shorter than the text it
// replaces. Bounding such a field buys the document nothing — at a low shared
// bound a 50-byte result would be replaced by a 90-byte note, growing the very
// document the bound is trying to fit — and it costs the reader the only copy of
// a text they could have read in full. Because the replacement is therefore
// never longer than the original, the document size stays non-decreasing in the
// bound, which is what lets the search below find a fitting bound at all.
func boundServedText(kind ServedTextKind, text string, showBytes int) string {
	if len(text) <= showBytes {
		return text
	}
	shown := text[:cutServedTextAt(text, showBytes)]
	bounded := shown + servedBoundNote(kind, len(shown), len(text))
	if len(bounded) >= len(text) {
		return text
	}
	return bounded
}

// servedTextWasBounded reports whether the bound actually replaced this field's
// text, which is what the report counts. Asking the same function that does the
// work keeps the counter from claiming a field the bound left whole.
func servedTextWasBounded(field servedTextField, showBytes int) bool {
	return boundServedText(field.kind, field.original, showBytes) != field.original
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
// The note says WHERE the whole record is without saying "locally". This exact
// text travels into the publication body, so a reader on another machine reads
// it too, and for them nothing about the record is local; naming the store that
// recorded the session is true for both readers.
func servedBoundNote(kind ServedTextKind, shown, recorded int) string {
	return fmt.Sprintf("\n[%s bounded for display: showing %s of %s; the full record is kept by the peasant store that recorded this session]",
		kind, defaults.HumanByteSize(int64(shown)), defaults.HumanByteSize(int64(recorded)))
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
