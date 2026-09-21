package push

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// redactRetainedJSON rewrites JSON string tokens, not arbitrary JSON numbers.
// Unchanged tokens, whitespace and number literals survive byte-for-byte. Decode
// string escapes before applying rules so \u escapes cannot hide credentials.
// Embedded JSON strings use the same path, bounded by the public depth limit.
func redactRetainedJSON(payload string, redactor redact.JSONRedactor, depth int) (string, error) {
	return ingest.RedactRetainedJSON(payload, redactor, depth)
}

func redactRetainedRecords(records []schema.RetainedUnknownRecord, redactor redact.JSONRedactor) ([]schema.RetainedUnknownRecord, error) {
	out := append([]schema.RetainedUnknownRecord(nil), records...)
	for i := range out {
		payload, err := redactRetainedJSON(out[i].Payload, redactor, 0)
		if err != nil {
			return nil, err
		}
		out[i].Payload = payload
	}
	if len(out) > 0 {
		if err := schema.ValidateRetainedUnknown(schema.SessionDetailPayload{RetainedUnknown: out, Diagnostics: &schema.InterpretationDiagnostics{Partial: true}}); err != nil {
			return nil, fmt.Errorf("redact retained evidence before publication: %w; nothing uploaded; correct rules that make JSON keys ambiguous", err)
		}
	}
	return out, nil
}

// PublicationReviewText validates the upload's actual redaction path, then
// exposes unredacted text to the local consent scanner. Retained JSON string
// escapes are decoded by the same visitor upload uses; scanning just the outer
// envelope would miss escaped credentials and give misleading consent feedback.
func PublicationReviewText(content schema.TranscriptContent, redactor redact.JSONRedactor) (string, error) {
	if _, err := marshalBuiltTranscriptContent(content, redactor); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	collector := &reviewStringCollector{}
	if content.SessionDetail != nil {
		if _, err := redactRetainedRecords(content.SessionDetail.RetainedUnknown, collector); err != nil {
			return "", err
		}
	}
	if len(collector.values) == 0 {
		return string(encoded), nil
	}
	return string(encoded) + "\n" + strings.Join(collector.values, "\n"), nil
}

type reviewStringCollector struct{ values []string }

var _ redact.JSONRedactor = (*reviewStringCollector)(nil)

func (c *reviewStringCollector) RedactJSON(value any) any {
	if text, ok := value.(string); ok {
		c.values = append(c.values, text)
	}
	return value
}
