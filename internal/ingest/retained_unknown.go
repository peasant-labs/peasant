package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// UnknownSourcePosition identifies evidence in the source, not in a projection.
// Line and Sequence are one-based when present; native sources may instead use
// their stable ID or opaque source reference. JSONPointer addresses a nested value.
type UnknownSourcePosition struct {
	Public         *UnknownPublicPosition `json:"public,omitempty"`
	SourceEntryRef schema.SourceEntryRef  `json:"sourceEntryRef,omitempty"`
	SourceID       string                 `json:"sourceId,omitempty"`
	Line           int                    `json:"line,omitempty"`
	Sequence       int                    `json:"sequence,omitempty"`
	JSONPointer    string                 `json:"jsonPointer,omitempty"`
}

// UnknownPublicPosition is assigned while traversing the complete source, before
// known records or blocks are folded away. SourceRef identifies the opaque source
// stream, not a filesystem path or an individual native entry. Both coordinates
// are zero-based; absence must never be confused with the first source record.
type UnknownPublicPosition struct {
	SourceRef   string `json:"sourceRef"`
	RecordIndex int64  `json:"recordIndex"`
	Position    int64  `json:"position"`
}

// RetainedUnknown is private source evidence with an explicit redaction boundary. It is not a
// conversation turn or an outbound wire contract. Payload is never truncated.
type RetainedUnknown struct {
	Harness   Harness               `json:"harness"`
	Namespace string                `json:"namespace"`
	Kind      string                `json:"kind"`
	Position  UnknownSourcePosition `json:"position"`
	Payload   json.RawMessage       `json:"payload"`
}

// MarshalJSON stores the payload as JSON text rather than as an embedded value.
// encoding/json compacts RawMessage values; that would silently normalize the
// original whitespace and string escaping every time Extra is written. This is
// a private storage encoding, not the public retained-evidence contract.
func (r RetainedUnknown) MarshalJSON() ([]byte, error) {
	type envelope RetainedUnknown
	fields := envelope(r)
	fields.Payload = nil
	return json.Marshal(struct {
		*envelope
		Payload     json.RawMessage `json:"payload,omitempty"`
		PayloadText string          `json:"payloadText"`
	}{envelope: &fields, PayloadText: string(r.Payload)})
}

// UnmarshalJSON keeps older private captures readable without inventing their
// absent public coordinates. A legacy payload value and a new payloadText may
// not coexist: choosing one would discard potentially different evidence.
func (r *RetainedUnknown) UnmarshalJSON(data []byte) error {
	type envelope RetainedUnknown
	var fields envelope
	encoded := struct {
		*envelope
		PayloadText json.RawMessage `json:"payloadText"`
	}{envelope: &fields}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return fmt.Errorf("read retained evidence: invalid private envelope; no evidence was certified; restore a supported capture or re-index the source")
	}
	if encoded.PayloadText != nil {
		if len(fields.Payload) != 0 {
			return fmt.Errorf("read retained evidence: conflicting private payload encodings; no evidence was certified; restore an unambiguous capture or re-index the source")
		}
		var text string
		if err := json.Unmarshal(encoded.PayloadText, &text); err != nil || bytes.Equal(bytes.TrimSpace(encoded.PayloadText), []byte("null")) {
			return fmt.Errorf("read retained evidence: payloadText must contain JSON text as a string; no evidence was certified; restore an intact capture or re-index the source")
		}
		fields.Payload = json.RawMessage(text)
	}
	*r = RetainedUnknown(fields)
	return nil
}

const retainedUnknownKey = "retainedUnknown"

var unknownEvidenceRedactor = sync.OnceValues(func() (redact.Redactor, error) {
	return redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
})

// NewRetainedUnknownFromSource applies the canonical standard redaction rules
// before constructing private evidence. Ordinary local ingest may retain raw
// source artifacts; that policy is not proof that index evidence is redacted.
// A caller with configured custom rules applies those first, as for known data.
func NewRetainedUnknownFromSource(harness Harness, namespace, kind string, position UnknownSourcePosition, payload json.RawMessage) (RetainedUnknown, error) {
	engine, err := unknownEvidenceRedactor()
	if err != nil {
		return RetainedUnknown{}, fmt.Errorf("initialize unknown-evidence redaction: %w; evidence was not stored; repair redaction configuration before retrying", err)
	}
	encoded, err := RedactRetainedJSON(string(payload), engine, 0)
	if err != nil {
		return RetainedUnknown{}, err
	}
	position.SourceID = engine.RedactText(position.SourceID)
	return NewRetainedUnknown(harness, namespace, engine.RedactText(kind), position, json.RawMessage(encoded))
}

// NewRetainedUnknown requires the caller to apply the ordinary capture redaction
// boundary first. It does not redact itself.
func NewRetainedUnknown(harness Harness, namespace, kind string, position UnknownSourcePosition, redactedPayload json.RawMessage) (RetainedUnknown, error) {
	if position.SourceEntryRef != "" {
		if err := position.SourceEntryRef.Validate(); err != nil {
			return RetainedUnknown{}, fmt.Errorf("retain unknown source evidence: invalid opaque source reference; preserve the previous capture and correct the native reference allocator: %w", err)
		}
	}
	if !slices.Contains(schema.Harnesses(), harness) || strings.TrimSpace(namespace) == "" || strings.TrimSpace(kind) == "" ||
		position.Line < 0 || position.Sequence < 0 ||
		(position.Line == 0 && position.Sequence == 0 && strings.TrimSpace(position.SourceID) == "" && position.SourceEntryRef == "") ||
		!validUnknownPointer(position.JSONPointer) || !json.Valid(redactedPayload) {
		return RetainedUnknown{}, fmt.Errorf("retain unknown source evidence in ingest: invalid harness, discriminator, source coordinate or JSON payload; no evidence was certified; apply baseline redaction to valid JSON and supply the actual source position")
	}
	return RetainedUnknown{Harness: harness, Namespace: namespace, Kind: kind, Position: position, Payload: append(json.RawMessage(nil), redactedPayload...)}, nil
}

func validUnknownPointer(pointer string) bool {
	if pointer != "" && !strings.HasPrefix(pointer, "/") {
		return false
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] == '~' {
			i++
			if i == len(pointer) || (pointer[i] != '0' && pointer[i] != '1') {
				return false
			}
		}
	}
	return true
}

// AttachRetainedUnknown merges evidence without replacing other private fields.
func AttachRetainedUnknown(entry *schema.SessionEntry, records []RetainedUnknown) error {
	if len(records) == 0 {
		return nil
	}
	prior, err := RetainedUnknownOf(*entry)
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, err := NewRetainedUnknown(record.Harness, record.Namespace, record.Kind, record.Position, record.Payload); err != nil {
			return err
		}
	}
	fields := map[string]json.RawMessage{}
	if entry.Extra != nil {
		if err := json.Unmarshal([]byte(*entry.Extra), &fields); err != nil || fields == nil {
			return fmt.Errorf("retain source evidence: entry extra is not a JSON object; no capture was certified; repair the entry producer")
		}
	}
	fields[retainedUnknownKey], err = json.Marshal(append(prior, records...))
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	text := string(encoded)
	entry.Extra = &text
	return nil
}

// RetainedUnknownOf validates the read-side boundary before accounting evidence.
func RetainedUnknownOf(entry schema.SessionEntry) ([]RetainedUnknown, error) {
	if entry.Extra == nil {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*entry.Extra), &fields); err != nil {
		return nil, fmt.Errorf("read retained unknown evidence: malformed entry extra; preserve prior capture and repair its producer: %w", err)
	}
	raw, exists := fields[retainedUnknownKey]
	if !exists {
		return nil, nil
	}
	var records []RetainedUnknown
	if err := json.Unmarshal(raw, &records); err != nil || len(records) == 0 {
		return nil, fmt.Errorf("read retained unknown evidence: expected a nonempty evidence array; preserve prior capture and repair its producer")
	}
	for _, record := range records {
		if _, err := NewRetainedUnknown(record.Harness, record.Namespace, record.Kind, record.Position, record.Payload); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// RetainedUnknownEntry carries evidence without inventing conversation text.
func RetainedUnknownEntry(sessionID SessionID, entryIndex int, record RetainedUnknown) (schema.SessionEntry, error) {
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: entryIndex, Harness: record.Harness, Role: RoleSystem, EntryType: EntryTypeSystem}
	err := AttachRetainedUnknown(&entry, []RetainedUnknown{record})
	return entry, err
}

// IsRetainedUnknownCarrier identifies a private evidence-only row. Such a row
// must not be rendered as an empty or fabricated conversation turn.
func IsRetainedUnknownCarrier(entry schema.SessionEntry) bool {
	if entry.Role != RoleSystem || entry.EntryType != EntryTypeSystem || entry.ContentPreview != nil || entry.ToolInput != nil || entry.ToolOutput != nil {
		return false
	}
	records, err := RetainedUnknownOf(entry)
	return err == nil && len(records) > 0
}

func retainedUnknownEntries(entries []schema.SessionEntry) ([]RetainedUnknown, error) {
	var records []RetainedUnknown
	for _, entry := range entries {
		found, err := RetainedUnknownOf(entry)
		if err != nil {
			return nil, err
		}
		records = append(records, found...)
	}
	return records, nil
}

// RetainedUnknownKindCount distinguishes occurrences from affected sessions.
type RetainedUnknownKindCount struct {
	Harness     Harness `json:"harness"`
	Namespace   string  `json:"namespace"`
	Kind        string  `json:"kind"`
	Occurrences int     `json:"occurrences"`
	Sessions    int     `json:"sessions"`
}

func retainedUnknownCounts(records []RetainedUnknown) []RetainedUnknownKindCount {
	counts := map[string]RetainedUnknownKindCount{}
	for _, record := range records {
		key := string(record.Harness) + "\x00" + record.Namespace + "\x00" + record.Kind
		count := counts[key]
		count.Harness, count.Namespace, count.Kind = record.Harness, record.Namespace, record.Kind
		count.Occurrences++
		count.Sessions = 1
		counts[key] = count
	}
	return aggregateRetainedUnknownCountsMap(counts)
}

func aggregateRetainedUnknownCounts(counts []RetainedUnknownKindCount) []RetainedUnknownKindCount {
	merged := map[string]RetainedUnknownKindCount{}
	for _, count := range counts {
		key := string(count.Harness) + "\x00" + count.Namespace + "\x00" + count.Kind
		prior := merged[key]
		count.Occurrences += prior.Occurrences
		count.Sessions += prior.Sessions
		merged[key] = count
	}
	return aggregateRetainedUnknownCountsMap(merged)
}

func aggregateRetainedUnknownCountsMap(counts map[string]RetainedUnknownKindCount) []RetainedUnknownKindCount {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []RetainedUnknownKindCount
	for _, key := range keys {
		out = append(out, counts[key])
	}
	return out
}
