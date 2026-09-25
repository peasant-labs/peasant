package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/schema"
)

// This file owns the shared raw-safety helper API for retained-evidence
// decoding (PROPOSAL-3 section 5.1). SLICE-2 (capture assessment / V1
// admission) and SLICE-4 (Codex parse-once indexing) consume these helpers;
// keep their signatures stable so the lanes integrate without churn.
//
// Shared API surface:
//   - ScanRawEvidenceDocument(raw, ownedField)
//   - CheckCanonicalObjectKeys(raw, allowed, ownedPath)
//   - ExtractExtraRetainedArray(extra)
//   - DecodeOwnedInt64(raw, ownedField)
//   - EvidenceIntegrityError / LegacyPositionUnavailableError
//
// All helpers fail closed with safe messages: they name the owned field only
// and never carry source bytes, labels, pointers, or scanner cause chains.

// localRawEvidenceDepth is the existing local syntax depth for private
// retained evidence. It bounds structure nesting only; byte budgets always
// come from the actual document length, never a transport cap.
const localRawEvidenceDepth = 10000

// EvidenceIntegrityError reports corrupt private evidence: missing, null,
// malformed, duplicated, or aliased owned members, payload-encoding ambiguity,
// invalid Unicode, or trailing values. It names the owned field only.
type EvidenceIntegrityError struct {
	// Field is the owned field path (for example,
	// "position.public.recordIndex"). It never holds source bytes.
	Field string
}

func (e *EvidenceIntegrityError) Error() string {
	field := e.Field
	if field == "" {
		field = "envelope"
	}
	return fmt.Sprintf("retain unknown source evidence in ingest: %s is missing, null, malformed, duplicated, or unrecognized; no evidence was certified; restore an intact capture or re-index the source", field)
}

// LegacyPositionUnavailableError reports an old private record whose public
// traversal coordinates are wholly absent. It is compatible with the
// long-standing ErrUnknownPositionUnavailable sentinel: errors.Is against
// that sentinel keeps working for bounded-preview callers.
type LegacyPositionUnavailableError struct{}

func (e *LegacyPositionUnavailableError) Error() string {
	return "retain unknown source evidence in ingest: position.public is wholly absent; this stored evidence predates traversal coordinates and no full-data certificate was issued; re-index the original source with a position-aware adapter"
}

// Is preserves errors.Is(err, ErrUnknownPositionUnavailable) compatibility.
func (e *LegacyPositionUnavailableError) Is(target error) bool {
	return target == ErrUnknownPositionUnavailable
}

// ScanRawEvidenceDocument validates one complete JSON value with the actual
// local byte length and the existing local syntax depth. The scanner's unsafe
// error text is discarded: scanner failures may quote private source text, so
// callers only learn the owned field that failed.
func ScanRawEvidenceDocument(raw []byte, ownedField string) error {
	if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{
		MaxDocumentBytes: len(raw),
		MaxDocumentDepth: localRawEvidenceDepth,
	}); err != nil {
		return &EvidenceIntegrityError{Field: ownedField}
	}
	return nil
}

// CheckCanonicalObjectKeys enforces exact canonical member names and a closed
// member set for one owned object. Go case folding is not a compatibility
// feature here: any member that matches an allowed name or a present sibling
// case-insensitively without matching exactly is a case alias and is refused.
// Exact duplicates (including escaped spellings of the same decoded key, which
// the streaming decoder normalizes before comparison) are refused. Unknown
// members are refused. Unrelated extensions are never passed through this
// check: apply it only to owned envelope, position, and public objects, never
// inside native payload or payloadText.
func CheckCanonicalObjectKeys(raw json.RawMessage, allowed []string, ownedPath string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return &EvidenceIntegrityError{Field: ownedPath}
	}
	seen := make(map[string]struct{})
	folded := make(map[string]string)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return &EvidenceIntegrityError{Field: ownedPath}
		}
		key, ok := keyToken.(string)
		if !ok {
			return &EvidenceIntegrityError{Field: ownedPath}
		}
		if _, duplicate := seen[key]; duplicate {
			return &EvidenceIntegrityError{Field: ownedPath + "." + key}
		}
		seen[key] = struct{}{}
		fold := strings.ToLower(key)
		if prev, exists := folded[fold]; exists && prev != key {
			return &EvidenceIntegrityError{Field: ownedPath + "." + key}
		}
		folded[fold] = key
		if _, ok := allow[key]; !ok {
			// A differently-cased spelling of an allowed name is an alias,
			// not an extension; anything else is outside the closed set.
			return &EvidenceIntegrityError{Field: ownedPath + "." + key}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return &EvidenceIntegrityError{Field: ownedPath + "." + key}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return &EvidenceIntegrityError{Field: ownedPath}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !isJSONEOF(err) {
		return &EvidenceIntegrityError{Field: ownedPath}
	}
	return nil
}

// ExtractExtraRetainedArray validates the extensible Extra root and returns
// the raw retainedUnknown evidence array when present. Unrelated extension
// fields are permitted and preserved for the caller. Duplicate members and
// case aliases of the reserved retainedUnknown key are refused before any map
// decode can discard them.
func ExtractExtraRetainedArray(extra string) (array json.RawMessage, present bool, err error) {
	if err := ScanRawEvidenceDocument([]byte(extra), "extra"); err != nil {
		return nil, false, err
	}
	decoder := json.NewDecoder(strings.NewReader(extra))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false, &EvidenceIntegrityError{Field: "extra"}
	}
	seen := make(map[string]struct{})
	folded := make(map[string]string)
	var retained json.RawMessage
	found := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, &EvidenceIntegrityError{Field: "extra"}
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, &EvidenceIntegrityError{Field: "extra"}
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, false, &EvidenceIntegrityError{Field: "extra." + key}
		}
		seen[key] = struct{}{}
		fold := strings.ToLower(key)
		if prev, exists := folded[fold]; exists && prev != key {
			return nil, false, &EvidenceIntegrityError{Field: "extra." + key}
		}
		folded[fold] = key
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, &EvidenceIntegrityError{Field: "extra." + key}
		}
		if fold == strings.ToLower(retainedUnknownKey) {
			if key != retainedUnknownKey {
				return nil, false, &EvidenceIntegrityError{Field: "extra." + key}
			}
			retained, found = value, true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, &EvidenceIntegrityError{Field: "extra"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !isJSONEOF(err) {
		return nil, false, &EvidenceIntegrityError{Field: "extra"}
	}
	return retained, found, nil
}

// DecodeOwnedInt64 decodes an owned integer directly as int64, never float64,
// so large coordinates keep full precision and fractional values refuse
// instead of truncating. Range and sign limits are applied by the caller.
func DecodeOwnedInt64(raw json.RawMessage, ownedField string) (int64, error) {
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, &EvidenceIntegrityError{Field: ownedField}
	}
	return value, nil
}

func isJSONEOF(err error) bool {
	return err == io.EOF
}
