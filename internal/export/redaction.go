package export

// Export egress redaction (PROPOSAL-3 section 5.5): stored retained evidence
// stays raw at rest; this package redacts at export time with the standard
// baseline engine and refuses fail-closed on any redaction or cap failure.
//
// Local-owner detail exits stay raw by design and are intentionally untouched:
// api.DetailPayloadWithReader / snapshot_detail.go, store_adapter.go,
// detail_navigation.go, websocket.go, and server.go serve the owner's own
// local data (display-to-owner on the owner-local API, consistent with
// raw-local source artifacts). Diagnostics and errors on those paths carry
// safe field paths only, never raw bytes or labels. Only this export egress
// (and the push upload path with configured rules) emits the redacted form.

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// NewBaselineRedactor builds the standard baseline engine export uses.
// It carries no custom patterns: export redacts with the same ruleset as the
// removed capture baseline, while push applies the configured rules.
func NewBaselineRedactor() (redact.JSONRedactor, error) {
	engine, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		return nil, fmt.Errorf("export retained evidence: build standard baseline redactor: %w; nothing exported; retry the export", err)
	}
	if engine == nil {
		return nil, fmt.Errorf("export retained evidence: standard baseline redactor unavailable; nothing exported; retry the export")
	}
	return engine, nil
}

// RedactExportRetained rewrites retained payload, kind, and namespace with the
// baseline engine. Stored bytes stay raw; only the egress projection is edited.
// A nil engine with retained records present refuses fail-closed: emitting raw
// stored evidence without redaction is never an export option. Errors name a
// safe module/operation/owned field only, never raw payload bytes, labels, or
// position strings.
func RedactExportRetained(records []schema.RetainedUnknownRecord, redactor redact.JSONRedactor) ([]schema.RetainedUnknownRecord, error) {
	if len(records) == 0 {
		return records, nil
	}
	if redactor == nil {
		return nil, fmt.Errorf("export retained evidence for publication: baseline redactor unavailable with %d retained record(s) present; nothing exported; rebuild the standard baseline engine and retry", len(records))
	}
	out := append([]schema.RetainedUnknownRecord(nil), records...)
	for i := range out {
		kind, namespace, err := redactExportLabels(out[i].Kind, out[i].Namespace, redactor)
		if err != nil {
			return nil, err
		}
		out[i].Kind = kind
		out[i].Namespace = namespace
		payload, err := ingest.RedactRetainedJSON(out[i].Payload, redactor, 0)
		if err != nil {
			return nil, fmt.Errorf("export retained evidence for publication: baseline redaction failed for retained payload; nothing exported; correct the baseline engine and retry")
		}
		out[i].Payload = payload
	}
	if err := schema.ValidateRetainedUnknown(schema.SessionDetailPayload{RetainedUnknown: out, Diagnostics: &schema.InterpretationDiagnostics{Partial: true}}); err != nil {
		return nil, fmt.Errorf("export retained evidence for publication: redacted evidence failed public validation: %w; nothing exported; re-index the source and retry", err)
	}
	return out, nil
}

func redactExportLabels(kind, namespace string, redactor redact.JSONRedactor) (string, string, error) {
	rewrite := func(text, field string) (string, error) {
		value := redactor.RedactJSON(text)
		result, ok := value.(string)
		if !ok || strings.TrimSpace(result) == "" || !utf8.ValidString(result) {
			return "", fmt.Errorf("export retained evidence for publication: baseline redaction of %s produced an empty, non-string, or invalid label; nothing exported; correct the baseline engine and retry", field)
		}
		return result, nil
	}
	kindOut, err := rewrite(kind, "retained kind")
	if err != nil {
		return "", "", err
	}
	namespaceOut, err := rewrite(namespace, "retained namespace")
	if err != nil {
		return "", "", err
	}
	return kindOut, namespaceOut, nil
}

// ValidateExportPayload checks public limits and the actual final serialized
// detail bytes before anything is exposed or written. Typed validation alone
// cannot catch redaction growth, escaping growth, or aggregate size: the
// emitted bytes are what the cap constrains. The payload is serialized with
// MarshalIndent because that is the exact shape the sessions command writes
// to the target file; validating the compact form would understate the
// emitted bytes.
func ValidateExportPayload(payload *schema.SessionDetailPayload) error {
	if payload == nil {
		return fmt.Errorf("export session: no detail payload was built; nothing exported; re-index the source and retry")
	}
	if err := schema.ValidateSessionDetailPayload(*payload); err != nil {
		return fmt.Errorf("export session: public validation failed: %w; nothing exported; re-index the source and retry", err)
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("export session: serialize validated detail: %w; nothing exported; re-index the source and retry", err)
	}
	if err := schema.ScanRawJSONDocument(encoded, schema.RawJSONPathPolicy{MaxDocumentBytes: defaults.SessionDetailDocumentCapBytes, MaxDocumentDepth: 64}); err != nil {
		return fmt.Errorf("export session: final serialized detail exceeds the public transfer limit: %w; local evidence is unchanged and nothing exported; use a receiver and contract supporting larger transfers when available", err)
	}
	if _, err := schema.DecodeSessionDetailPayloadRaw(encoded); err != nil {
		return fmt.Errorf("export session: final serialized detail failed contract decode: %w; nothing exported; re-index the source and retry", err)
	}
	return nil
}

// applyExportRedaction redacts the payload's retained evidence with the
// baseline engine and validates the redacted egress form. The stored bytes are
// never mutated; failure refuses the export with the existing target untouched
// and stdout silent (the caller writes only on nil error).
func applyExportRedaction(payload *schema.SessionDetailPayload, redactor redact.JSONRedactor) (*schema.SessionDetailPayload, error) {
	if payload == nil {
		return nil, fmt.Errorf("export session: no detail payload was built; nothing exported; re-index the source and retry")
	}
	if len(payload.RetainedUnknown) == 0 {
		if err := ValidateExportPayload(payload); err != nil {
			return nil, err
		}
		return payload, nil
	}
	redacted, err := RedactExportRetained(payload.RetainedUnknown, redactor)
	if err != nil {
		return nil, err
	}
	out := *payload
	out.RetainedUnknown = redacted
	if err := ValidateExportPayload(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
