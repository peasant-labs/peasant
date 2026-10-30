package push

import (
	"encoding/json"
	"fmt"

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
// changed it, so the document stays valid and every other byte is kept.
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
