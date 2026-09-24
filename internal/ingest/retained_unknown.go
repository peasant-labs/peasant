package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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

// UnmarshalJSON decodes one owned evidence object with presence awareness.
// The raw object is scanned before any map or struct decode, then checked
// against the exact canonical member names and closed member sets: owned
// envelope, position, and public coordinate objects. Required members must be
// explicitly present and non-null; absent private coordinates are never
// confused with the first source record. Public traversal coordinates decode
// directly as int64, never float64.
//
// `public` may be wholly absent only for the legacy-read policy. An explicit
// `public:null` is malformed, not legacy absence. A legacy raw `payload`,
// including a JSON string value, is preserved verbatim and never confused
// with the modern `payloadText` string encoding.
func (r *RetainedUnknown) UnmarshalJSON(data []byte) error {
	if err := ScanRawEvidenceDocument(data, "envelope"); err != nil {
		return err
	}
	if err := CheckCanonicalObjectKeys(data, retainedEnvelopeKeys, "envelope"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return &EvidenceIntegrityError{Field: "envelope"}
	}
	harnessRaw, ok := fields["harness"]
	if !ok || isNullJSON(harnessRaw) {
		return &EvidenceIntegrityError{Field: "envelope.harness"}
	}
	namespaceRaw, ok := fields["namespace"]
	if !ok || isNullJSON(namespaceRaw) {
		return &EvidenceIntegrityError{Field: "envelope.namespace"}
	}
	kindRaw, ok := fields["kind"]
	if !ok || isNullJSON(kindRaw) {
		return &EvidenceIntegrityError{Field: "envelope.kind"}
	}
	positionRaw, ok := fields["position"]
	if !ok || isNullJSON(positionRaw) {
		return &EvidenceIntegrityError{Field: "position"}
	}
	payloadRaw, hasPayload := fields["payload"]
	payloadTextRaw, hasText := fields["payloadText"]
	if hasPayload && hasText {
		return &EvidenceIntegrityError{Field: "envelope.payload"}
	}
	if !hasPayload && !hasText {
		return &EvidenceIntegrityError{Field: "envelope.payload"}
	}
	var harness Harness
	if err := json.Unmarshal(harnessRaw, &harness); err != nil {
		return &EvidenceIntegrityError{Field: "envelope.harness"}
	}
	var namespace, kind string
	if err := json.Unmarshal(namespaceRaw, &namespace); err != nil {
		return &EvidenceIntegrityError{Field: "envelope.namespace"}
	}
	if err := json.Unmarshal(kindRaw, &kind); err != nil {
		return &EvidenceIntegrityError{Field: "envelope.kind"}
	}
	position, err := decodeUnknownSourcePosition(positionRaw)
	if err != nil {
		return err
	}
	var payload json.RawMessage
	if hasPayload {
		if err := ScanRawEvidenceDocument(payloadRaw, "envelope.payload"); err != nil {
			return err
		}
		payload = append(json.RawMessage(nil), payloadRaw...)
	} else {
		if isNullJSON(payloadTextRaw) {
			return &EvidenceIntegrityError{Field: "envelope.payloadText"}
		}
		var text string
		if err := json.Unmarshal(payloadTextRaw, &text); err != nil {
			return &EvidenceIntegrityError{Field: "envelope.payloadText"}
		}
		if !utf8.ValidString(text) {
			return &EvidenceIntegrityError{Field: "envelope.payloadText"}
		}
		if err := ScanRawEvidenceDocument([]byte(text), "envelope.payloadText"); err != nil {
			return err
		}
		payload = json.RawMessage(text)
	}
	record, err := checkRetainedUnknownShape(harness, namespace, kind, position, payload)
	if err != nil {
		return err
	}
	*r = record
	return nil
}

// Canonical owned member names. Matching is exact: Go case folding is not a
// compatibility feature, and escaped spellings of the same decoded key obey
// duplicate detection in CheckCanonicalObjectKeys.
var (
	retainedEnvelopeKeys = []string{"harness", "namespace", "kind", "position", "payload", "payloadText"}
	retainedPositionKeys = []string{"public", "sourceEntryRef", "sourceId", "line", "sequence", "jsonPointer"}
	retainedPublicKeys   = []string{"sourceRef", "recordIndex", "position"}
)

// maxSafeCoordinate bounds owned traversal coordinates to the JSON-safe
// integer range shared with the local evidence validator.
const maxSafeCoordinate = int64(9007199254740991)

func isNullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeUnknownSourcePosition decodes one owned position object. The public
// coordinate object may be wholly absent (legacy reads only); when present it
// must be a non-null object carrying exactly sourceRef, recordIndex, and
// position, each explicitly present and non-null with integers decoded
// directly as int64.
func decodeUnknownSourcePosition(raw json.RawMessage) (UnknownSourcePosition, error) {
	var position UnknownSourcePosition
	if err := CheckCanonicalObjectKeys(raw, retainedPositionKeys, "position"); err != nil {
		return position, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return position, &EvidenceIntegrityError{Field: "position"}
	}
	if publicRaw, ok := fields["public"]; ok {
		if isNullJSON(publicRaw) {
			return position, &EvidenceIntegrityError{Field: "position.public"}
		}
		public, err := decodeUnknownPublicPosition(publicRaw)
		if err != nil {
			return position, err
		}
		position.Public = public
	}
	if refRaw, ok := fields["sourceEntryRef"]; ok && !isNullJSON(refRaw) {
		var ref string
		if err := json.Unmarshal(refRaw, &ref); err != nil {
			return position, &EvidenceIntegrityError{Field: "position.sourceEntryRef"}
		}
		position.SourceEntryRef = schema.SourceEntryRef(ref)
	}
	if idRaw, ok := fields["sourceId"]; ok && !isNullJSON(idRaw) {
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return position, &EvidenceIntegrityError{Field: "position.sourceId"}
		}
		position.SourceID = id
	}
	if lineRaw, ok := fields["line"]; ok && !isNullJSON(lineRaw) {
		line, err := DecodeOwnedInt64(lineRaw, "position.line")
		if err != nil {
			return position, err
		}
		if line < 0 || line > int64(maxIntCoordinate()) {
			return position, &EvidenceIntegrityError{Field: "position.line"}
		}
		position.Line = int(line)
	}
	if sequenceRaw, ok := fields["sequence"]; ok && !isNullJSON(sequenceRaw) {
		sequence, err := DecodeOwnedInt64(sequenceRaw, "position.sequence")
		if err != nil {
			return position, err
		}
		if sequence < 0 || sequence > int64(maxIntCoordinate()) {
			return position, &EvidenceIntegrityError{Field: "position.sequence"}
		}
		position.Sequence = int(sequence)
	}
	if pointerRaw, ok := fields["jsonPointer"]; ok && !isNullJSON(pointerRaw) {
		var pointer string
		if err := json.Unmarshal(pointerRaw, &pointer); err != nil {
			return position, &EvidenceIntegrityError{Field: "position.jsonPointer"}
		}
		position.JSONPointer = pointer
	}
	return position, nil
}

func maxIntCoordinate() int {
	return int(^uint(0) >> 1)
}

// decodeUnknownPublicPosition decodes one owned public coordinate object with
// all three members explicitly present and non-null.
func decodeUnknownPublicPosition(raw json.RawMessage) (*UnknownPublicPosition, error) {
	if err := CheckCanonicalObjectKeys(raw, retainedPublicKeys, "position.public"); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, &EvidenceIntegrityError{Field: "position.public"}
	}
	sourceRaw, ok := fields["sourceRef"]
	if !ok || isNullJSON(sourceRaw) {
		return nil, &EvidenceIntegrityError{Field: "position.public.sourceRef"}
	}
	recordRaw, ok := fields["recordIndex"]
	if !ok || isNullJSON(recordRaw) {
		return nil, &EvidenceIntegrityError{Field: "position.public.recordIndex"}
	}
	positionRaw, ok := fields["position"]
	if !ok || isNullJSON(positionRaw) {
		return nil, &EvidenceIntegrityError{Field: "position.public.position"}
	}
	var sourceRef string
	if err := json.Unmarshal(sourceRaw, &sourceRef); err != nil {
		return nil, &EvidenceIntegrityError{Field: "position.public.sourceRef"}
	}
	recordIndex, err := DecodeOwnedInt64(recordRaw, "position.public.recordIndex")
	if err != nil {
		return nil, err
	}
	pos, err := DecodeOwnedInt64(positionRaw, "position.public.position")
	if err != nil {
		return nil, err
	}
	if recordIndex < 0 || recordIndex > maxSafeCoordinate || pos < recordIndex || pos > maxSafeCoordinate {
		return nil, &EvidenceIntegrityError{Field: "position.public.position"}
	}
	return &UnknownPublicPosition{SourceRef: sourceRef, RecordIndex: recordIndex, Position: pos}, nil
}

const retainedUnknownKey = "retainedUnknown"

// NewRetainedUnknown stores raw source payload and labels verbatim. Capture
// applies no redaction: stored index Extra holds the raw unredacted payload,
// kind, namespace, and position strings, and redaction applies at egress only
// (push with configured rules, file/stdout export with the standard baseline
// engine). There is no redactor dependency and no redactor-init failure mode
// on this path. It validates shape, integrity, ownership, and coordinates
// only. The registry source reference for retained-unknown evidence lives
// here (see record_kinds.yaml).
func NewRetainedUnknown(harness Harness, namespace, kind string, position UnknownSourcePosition, payload json.RawMessage) (RetainedUnknown, error) {
	return checkRetainedUnknownShape(harness, namespace, kind, position, payload)
}

// checkRetainedUnknownShape is the single shape, integrity, ownership, and
// coordinate validator shared by the capture constructor and the strict
// presence-aware decode path. Failures name the owned field only and carry no
// source bytes.
func checkRetainedUnknownShape(harness Harness, namespace, kind string, position UnknownSourcePosition, payload json.RawMessage) (RetainedUnknown, error) {
	fail := func(field string) (RetainedUnknown, error) {
		return RetainedUnknown{}, &EvidenceIntegrityError{Field: field}
	}
	if position.SourceEntryRef != "" {
		if err := position.SourceEntryRef.Validate(); err != nil {
			return fail("position.sourceEntryRef")
		}
	}
	if !slices.Contains(schema.Harnesses(), harness) {
		return fail("envelope.harness")
	}
	if strings.TrimSpace(namespace) == "" {
		return fail("envelope.namespace")
	}
	if strings.TrimSpace(kind) == "" {
		return fail("envelope.kind")
	}
	if !utf8.ValidString(namespace) {
		return fail("envelope.namespace")
	}
	if !utf8.ValidString(kind) {
		return fail("envelope.kind")
	}
	if !utf8.ValidString(position.SourceID) {
		return fail("position.sourceId")
	}
	if !utf8.ValidString(position.JSONPointer) {
		return fail("position.jsonPointer")
	}
	if !utf8.ValidString(string(position.SourceEntryRef)) {
		return fail("position.sourceEntryRef")
	}
	if position.Line < 0 {
		return fail("position.line")
	}
	if position.Sequence < 0 {
		return fail("position.sequence")
	}
	if position.Line == 0 && position.Sequence == 0 && strings.TrimSpace(position.SourceID) == "" && position.SourceEntryRef == "" {
		return fail("position")
	}
	if !validUnknownPointer(position.JSONPointer) {
		return fail("position.jsonPointer")
	}
	if len(payload) == 0 {
		return fail("envelope.payload")
	}
	if err := ScanRawEvidenceDocument(payload, "envelope.payload"); err != nil {
		return fail("envelope.payload")
	}
	return RetainedUnknown{Harness: harness, Namespace: namespace, Kind: kind, Position: position, Payload: append(json.RawMessage(nil), payload...)}, nil
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
// The Extra root is checked before any map decode: duplicate members and case
// aliases of the reserved retainedUnknown key refuse, while unrelated
// extension fields pass through untouched. Every retained payload must be
// complete lexical JSON before a valid evidence set returns.
func RetainedUnknownOf(entry schema.SessionEntry) ([]RetainedUnknown, error) {
	if entry.Extra == nil {
		return nil, nil
	}
	raw, present, err := ExtractExtraRetainedArray(*entry.Extra)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var raws []json.RawMessage
	if err := decoder.Decode(&raws); err != nil {
		return nil, &EvidenceIntegrityError{Field: "extra.retainedUnknown"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !isJSONEOF(err) {
		return nil, &EvidenceIntegrityError{Field: "extra.retainedUnknown"}
	}
	if len(raws) == 0 {
		return nil, &EvidenceIntegrityError{Field: "extra.retainedUnknown"}
	}
	records := make([]RetainedUnknown, 0, len(raws))
	for _, item := range raws {
		var record RetainedUnknown
		if err := json.Unmarshal(item, &record); err != nil {
			return nil, err
		}
		if _, err := checkRetainedUnknownShape(record.Harness, record.Namespace, record.Kind, record.Position, record.Payload); err != nil {
			return nil, err
		}
		records = append(records, record)
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
