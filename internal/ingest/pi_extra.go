package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/schema"
)

// PiExtraKind identifies private persisted evidence, never a wire field.
type PiExtraKind string

const (
	PiExtraState          PiExtraKind = "pi.state"
	PiExtraUsage          PiExtraKind = "pi.usage"
	PiExtraNativeMetadata PiExtraKind = "pi.native_metadata"
	PiExtraCarrier        PiExtraKind = "pi.carrier"
	PiCarrierPartType                 = "pi.carrier"
)

// PiExtra is durable evidence on one row. SourceRef names its native message,
// not a block. Only the owning row carries Usage or Metadata; siblings must not
// repeat them. The shared projection resolves metadata attachments.
type PiExtra struct {
	Kind        PiExtraKind                   `json:"kind"`
	Harness     schema.Harness                `json:"harness"`
	SourceRef   string                        `json:"sourceRef,omitempty"`
	Usage       *schema.UsageDetail           `json:"usage,omitempty"`
	Metadata    []schema.NativeMetadataRecord `json:"metadata,omitempty"`
	ModelID     schema.ObservedModelID        `json:"model_id,omitempty"`
	Namespace   string                        `json:"namespace,omitempty"`
	SessionName *string                       `json:"sessionName,omitempty"`
}

// PiPublicRef domain-separates irreversible references. Never publish native IDs.
func PiPublicRef(sessionID, domain, nativeID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + domain + "\x00" + nativeID))
	return "pi-" + hex.EncodeToString(sum[:])
}

// EncodePiExtra validates and serializes evidence for SessionEntry.Extra.
func EncodePiExtra(value PiExtra) (*string, error) {
	if !utf8.ValidString(value.Namespace) || !utf8.ValidString(string(value.ModelID)) {
		return nil, piEvidenceError(fmt.Errorf("typed Pi evidence contains invalid UTF-8"))
	}
	total := 0
	for _, record := range value.Metadata {
		if !utf8.ValidString(record.CustomType) || len(record.CustomType) > 128 {
			return nil, piEvidenceError(fmt.Errorf("customType must be valid UTF-8 within 128 bytes"))
		}
		if _, err := schema.DecodeNativeMetadataDataRaw(record.Data); err != nil {
			return nil, piEvidenceError(err)
		}
		total += len(record.Data)
	}
	if total > 1<<20 {
		return nil, piEvidenceError(fmt.Errorf("aggregate metadata data exceeds 1 MiB before encoding"))
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if _, _, err := DecodePiExtra(&text); err != nil {
		return nil, err
	}
	return &text, nil
}

// DecodePiExtra scans before decoding, including selected metadata budgets.
func DecodePiExtra(extra *string) (PiExtra, bool, error) {
	var value PiExtra
	if extra == nil {
		return value, false, nil
	}
	raw := []byte(*extra)
	if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: 8 << 20, MaxDocumentDepth: 64, OpaqueMetadataPointers: []string{"/metadata/*/data"}}); err != nil {
		return value, false, piEvidenceError(err)
	}
	var marker struct {
		Kind    string `json:"kind"`
		Harness string `json:"harness"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return value, false, piEvidenceError(err)
	}
	if marker.Harness != string(schema.HarnessPi) && !bytes.HasPrefix([]byte(marker.Kind), []byte("pi.")) {
		return value, false, nil
	}
	if err := validatePiTypedPresence(raw, ""); err != nil {
		return value, true, piEvidenceError(err)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return value, true, piEvidenceError(err)
	}
	if value.Harness != schema.HarnessPi {
		return value, true, piEvidenceError(fmt.Errorf("carrier harness must be pi"))
	}
	switch value.Kind {
	case PiExtraState, PiExtraUsage, PiExtraNativeMetadata, PiExtraCarrier:
	default:
		return value, true, piEvidenceError(fmt.Errorf("unknown Pi evidence kind"))
	}
	if value.SourceRef != "" && !validPiRef(value.SourceRef) {
		return value, true, piEvidenceError(fmt.Errorf("source reference is not a generated Pi public reference"))
	}
	if value.Usage != nil {
		if err := schema.ValidateUsageDetail(*value.Usage); err != nil {
			return value, true, piEvidenceError(err)
		}
		if value.Usage.SourceEntryRef != value.SourceRef || !validPiRef(string(value.Usage.OwnerID)) {
			return value, true, piEvidenceError(fmt.Errorf("usage owner/source reference mismatch"))
		}
	}
	if len(value.Metadata) > 256 {
		return value, true, piEvidenceError(fmt.Errorf("metadata record budget exceeded"))
	}
	for _, record := range value.Metadata {
		if record.Source.EntryRef != value.SourceRef || !validPiRef(record.ID) || record.Attachment != nil {
			return value, true, piEvidenceError(fmt.Errorf("metadata must name its owning source and defer attachment to projection"))
		}
		if _, err := schema.DecodeNativeMetadataDataRaw(record.Data); err != nil {
			return value, true, piEvidenceError(err)
		}
	}
	return value, true, nil
}

// DecodePiEntryExtra also verifies the row's harness context. A Pi row without
// its typed evidence must not be silently treated as a legacy usage-less row.
func DecodePiEntryExtra(entry schema.SessionEntry) (PiExtra, bool, error) {
	extra, pi, err := DecodePiExtra(entry.Extra)
	if err != nil {
		return extra, pi, err
	}
	if (entry.Harness == schema.HarnessPi || IsPiCarrier(entry)) && !pi {
		return extra, pi, piEvidenceError(fmt.Errorf("Pi indexed row is missing its typed evidence marker"))
	}
	if pi && entry.Harness != "" && entry.Harness != schema.HarnessPi {
		return extra, pi, piEvidenceError(fmt.Errorf("indexed harness disagrees with Pi evidence"))
	}
	return extra, pi, nil
}

func validPiRef(value string) bool {
	if len(value) != 67 || value[:3] != "pi-" {
		return false
	}
	_, err := hex.DecodeString(value[3:])
	return err == nil
}

// ValidatePiPublicRef rejects persisted native IDs at the public-ref boundary.
func ValidatePiPublicRef(value string) error {
	if !validPiRef(value) {
		return piEvidenceError(fmt.Errorf("reference must be a nonreversible PiPublicRef value"))
	}
	return nil
}

func piEvidenceError(err error) error {
	return fmt.Errorf("Pi evidence validation failed in ingest.DecodePiExtra during indexed-row decoding: %w; no projection or publication was produced; repair the source and re-index this session before retrying", err)
}

// NewPiCarrier creates an ordinary nonconversational system row. Header-only
// imports write one so the existing replacement writer removes stale rows.
func NewPiCarrier(sessionID schema.SessionID, index int, extra PiExtra) (schema.SessionEntry, error) {
	encoded, err := EncodePiExtra(extra)
	part := PiCarrierPartType
	return schema.SessionEntry{SessionID: sessionID, Harness: schema.HarnessPi, EntryIndex: index, Role: schema.RoleSystem, EntryType: schema.EntryTypeSystem, PartType: &part, Extra: encoded}, err
}

// IsPiCarrier excludes private evidence from conversation, search and metrics.
func IsPiCarrier(entry schema.SessionEntry) bool {
	return entry.PartType != nil && *entry.PartType == PiCarrierPartType
}

// ConversationalEntries excludes private carrier rows without mutating input.
func ConversationalEntries(entries []schema.SessionEntry) []schema.SessionEntry {
	out := make([]schema.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if !IsPiCarrier(entry) {
			out = append(out, entry)
		}
	}
	return out
}

// PiUsageFromRaw preserves native cost spelling and validates present tokens.
// Absent usage still creates an unknown owner for an eligible source.
func PiUsageFromRaw(sessionID, nativeID string, scope schema.UsageScope, raw json.RawMessage) (schema.UsageDetail, error) {
	usage := schema.UsageDetail{OwnerID: schema.UsageOwnerID(PiPublicRef(sessionID, "owner", nativeID)), SourceEntryRef: PiPublicRef(sessionID, "entry", nativeID), Scope: scope, Completeness: schema.UsageUnknown}
	if len(raw) != 0 {
		if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: 8 << 20, MaxDocumentDepth: 64}); err != nil {
			return usage, piEvidenceError(err)
		}
		if err := validatePiTypedPresence(raw, "usage"); err != nil {
			return usage, piEvidenceError(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return usage, piEvidenceError(err)
		}
		if costRaw, ok := fields["cost"]; ok {
			var costs map[string]json.RawMessage
			if err := json.Unmarshal(costRaw, &costs); err != nil {
				return usage, piEvidenceError(err)
			}
			for _, amount := range costs {
				if len(amount) == 0 || bytes.TrimSpace(amount)[0] == '"' {
					return usage, piEvidenceError(fmt.Errorf("recorded native cost must be a JSON number, not a string"))
				}
			}
		}
		var tokens schema.TokenUsageDetail
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return usage, piEvidenceError(err)
		}
		var costs struct {
			Cost map[string]json.Number `json:"cost"`
		}
		if err := json.Unmarshal(raw, &costs); err != nil {
			return usage, piEvidenceError(err)
		}
		usage.Tokens = &tokens
		present := 0
		for _, p := range []*int64{tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheWrite, tokens.TotalTokens} {
			if p != nil {
				present++
			}
		}
		if present == 5 {
			usage.Completeness = schema.UsageComplete
		} else if present > 0 {
			usage.Completeness = schema.UsagePartial
		}
		if costs.Cost != nil {
			cost := &schema.RecordedCostDetail{Source: schema.RecordedCostSourceHarnessEstimate}
			for key, dest := range map[string]**schema.RecordedCostAmount{"input": &cost.Input, "output": &cost.Output, "cacheRead": &cost.CacheRead, "cacheWrite": &cost.CacheWrite, "total": &cost.Total} {
				if number, ok := costs.Cost[key]; ok {
					value := schema.RecordedCostAmount(number.String())
					*dest = &value
				}
			}
			usage.Cost = cost
		}
	}
	return usage, schema.ValidateUsageDetail(usage)
}

// Presence checks occur while raw fields still distinguish null from omission.
// Arbitrary metadata data is validated by its own scanner, not this typed walk.
func validatePiTypedPresence(raw json.RawMessage, path string) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("null is not a typed Pi evidence value at %s", path)
	}
	if len(raw) == 0 {
		return fmt.Errorf("missing Pi evidence at %s", path)
	}
	if raw[0] == '{' {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for key, value := range fields {
			if key == "data" && strings.Contains(path, "metadata/") {
				continue
			}
			if key == "attachment" && strings.Contains(path, "metadata/") {
				return fmt.Errorf("stored metadata must not preassign an attachment")
			}
			if err := validatePiTypedPresence(value, path+"/"+key); err != nil {
				return err
			}
		}
	} else if raw[0] == '[' {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for i, value := range values {
			if err := validatePiTypedPresence(value, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
