package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// redactEntryExtra redacts an entry's Extra as the JSON document it is.
//
// Extra is a JSON document carried in a string. The whole-document redaction
// passes see that string as plain text, so a rule that matches next to a JSON
// escape cuts into it: the email rule reads "\u003cname@example.com" as the
// address "u003cname@example.com", and replacing it leaves "\<EMAIL>", which
// is no longer JSON. Every later reader of the entry then refuses it. Here
// each string token is decoded, redacted, and re-encoded only when a rule
// changed it, so the document stays valid and every other byte is kept. The
// result then goes through the rules as text once more, for the rules that
// match the JSON around a value, and fails closed if that pass breaks it.
//
// ok is false when Extra is not valid JSON: such a value is left for the text
// pass, which is all the redaction it can have. A recorded model id survives
// the redaction, as it does on the other seams.
func redactEntryExtra(redactor redact.JSONRedactor, extra *string) (_ *string, ok bool, err error) {
	if extra == nil || redactor == nil || !json.Valid([]byte(*extra)) {
		return extra, false, nil
	}
	redacted, err := ingest.RedactRetainedJSON(*extra, redactor, 0)
	if err != nil {
		return nil, false, fmt.Errorf("redact entry Extra for publication: %w", err)
	}
	// Some rules match the JSON around a value, not the value: go_import_path
	// needs the quotes of "github.com/org/repo", and npm_repo_url spans a
	// "repository" key and its URL. A token on its own cannot show them that,
	// so the serialized document is also passed through the rules as text, as
	// it always was. Every email, path and secret is already a placeholder
	// here, so that pass no longer cuts into an escape.
	//
	// Number literals are masked for that pass and put back after it. The
	// redactor never treats a JSON number as text (RedactJSON passes numbers
	// through), and a text rule would otherwise read the digits of a float
	// such as a cost as a card number and break the document.
	//
	// The pass's result is taken when it changed something and is still JSON
	// with every number in place; when it broke the document, the entry is
	// refused rather than published with what the rule matched.
	masked, numbers, err := maskJSONNumbers(redacted)
	if err != nil {
		return nil, false, fmt.Errorf("redact entry Extra for publication: %w", err)
	}
	text, isText := redactor.RedactJSON(masked).(string)
	if !isText {
		return nil, false, fmt.Errorf("redact entry Extra for publication: a rule changed the document into another type; nothing was uploaded; correct the rule so it rewrites text in place")
	}
	if text != masked {
		restored, restoreErr := restoreJSONNumbers(text, numbers)
		if restoreErr != nil || !json.Valid([]byte(restored)) {
			return nil, false, fmt.Errorf("redact entry Extra for publication: a rule that matches JSON context rewrote this entry's Extra into a document that is no longer JSON; nothing was uploaded, because publishing it unredacted would send what the rule matched; leave the session out of the push, or report the rule")
		}
		redacted = restored
	}
	modelID, present, err := observedModelFromExtra(extra)
	if err != nil {
		return nil, false, err
	}
	if !present {
		return &redacted, true, nil
	}
	restored, err := restoreObservedModelExtra(&redacted, modelID)
	if err != nil {
		return nil, false, err
	}
	return restored, true, nil
}

// redactEntryExtras redacts the Extra of every entry not in skip as a JSON
// document, returning the redacted values by entry index. The caller puts
// them back after its whole-document pass, so that pass never rewrites the
// serialized document as text.
func redactEntryExtras(redactor redact.JSONRedactor, entries []schema.SessionEntry, skip map[int]*string) (map[int]*string, error) {
	out := map[int]*string{}
	for i := range entries {
		if _, done := skip[i]; done {
			continue
		}
		redacted, ok, err := redactEntryExtra(redactor, entries[i].Extra)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i] = redacted
		}
	}
	return out, nil
}

// jsonNumberMask stands in for every number literal during the text pass. A
// single digit matches no rule.
const jsonNumberMask = "0"

// jsonNumberSpans returns the byte range of each number literal in doc, in
// order. doc must be valid JSON.
func jsonNumberSpans(doc string) ([][2]int, error) {
	decoder := json.NewDecoder(strings.NewReader(doc))
	decoder.UseNumber()
	var spans [][2]int
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return spans, nil
		}
		if err != nil {
			return nil, err
		}
		if number, ok := token.(json.Number); ok {
			end := int(decoder.InputOffset())
			spans = append(spans, [2]int{end - len(number), end})
		}
	}
}

// maskJSONNumbers replaces every number literal in doc with jsonNumberMask and
// returns the literals it replaced, in order.
func maskJSONNumbers(doc string) (string, []string, error) {
	spans, err := jsonNumberSpans(doc)
	if err != nil {
		return "", nil, err
	}
	var out strings.Builder
	numbers := make([]string, 0, len(spans))
	last := 0
	for _, span := range spans {
		out.WriteString(doc[last:span[0]])
		out.WriteString(jsonNumberMask)
		numbers = append(numbers, doc[span[0]:span[1]])
		last = span[1]
	}
	out.WriteString(doc[last:])
	return out.String(), numbers, nil
}

// restoreJSONNumbers puts the masked number literals back, in order. It
// refuses a document whose numbers a rule added, removed or rewrote.
func restoreJSONNumbers(doc string, numbers []string) (string, error) {
	spans, err := jsonNumberSpans(doc)
	if err != nil {
		return "", err
	}
	if len(spans) != len(numbers) {
		return "", fmt.Errorf("the document holds %d numbers after redaction, want %d", len(spans), len(numbers))
	}
	var out strings.Builder
	last := 0
	for i, span := range spans {
		if doc[span[0]:span[1]] != jsonNumberMask {
			return "", fmt.Errorf("a rule rewrote a masked number")
		}
		out.WriteString(doc[last:span[0]])
		out.WriteString(numbers[i])
		last = span[1]
	}
	out.WriteString(doc[last:])
	return out.String(), nil
}
