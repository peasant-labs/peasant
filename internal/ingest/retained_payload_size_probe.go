package ingest

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// retainedUnknownTransferLimitBytes is the published 8 MiB transfer limit for
// one retained evidence payload. The schema contract bounds each payload with
// the same 8 MiB raw-document cap it applies to a session_detail document
// (schema.ValidateRetainedUnknown), so peasant states the value once through
// defaults.SessionDetailDocumentCapBytes rather than repeating the literal.
const retainedUnknownTransferLimitBytes = defaults.SessionDetailDocumentCapBytes

// The in-place probe below must measure exactly the two quantities the
// authoritative read path measures, or its early refusal drifts from the
// projection-time backstop:
//
//   - payloadText (modern encoding): the DECODED text length. UnmarshalJSON
//     stores the decoded string as json.RawMessage, so the backstop compares
//     len(record.Payload) against the decoded length. TestScanJSONStringDecodedLength
//     cross-checks this quantity against encoding/json.
//   - payload (legacy raw encoding): the RAW value extent, including quotes and
//     escape bytes, because UnmarshalJSON stores payloadRaw verbatim so the
//     backstop compares len(record.Payload) against the byte extent of the
//     stored value. TestJSONValueRawExtentMatchesAuthoritativePayload cross-checks
//     this quantity against json.RawMessage.
//
// Delete the legacy raw `payload` branch when that stored encoding is retired.

// strictRetainedExtensionDepth bounds the strict re-scan of a skipped Extra
// root extension value. The authoritative root scan allows localRawEvidenceDepth
// levels from the document root (retained_raw_codec.go); an extension value
// sits one level below the root, so the same budget applies minus that level.
const strictRetainedExtensionDepth = 10000 - 1

// storedRetainedPayloadExceedsTransferLimit reports whether every retained
// evidence record in the entries is a canonical, well-formed envelope and at
// least one stores a payload whose public transfer size exceeds the published
// limit. It reads the stored JSON text in place, so a refusal allocates nothing
// proportional to the payload: an oversized record is refused before it is
// decoded or copied.
//
// The verdict is member-order independent. The probe walks every member of every
// envelope and every record of every array before it decides, so corruption that
// follows an over-limit payload is still seen; the over-limit result is
// accumulated and returned only once the whole set is known canonical. It is
// deliberately conservative and sound in one direction only: it returns true
// only when every record it examines is a canonical stored record and at least
// one is over the limit. The probe mirrors the authoritative decoder's cheap
// shape rules in place: whole-document strictness for the Extra root (trailing
// content, lax extension values, and trailing commas decline), the non-empty
// evidence array, the required envelope members with exactly one payload
// encoding, the harness match, the non-empty namespace and kind, the position
// locator rule (a non-zero line or sequence, a non-blank sourceId, or a valid
// sourceEntryRef alongside public), the RFC 6901 pointer grammar, safe-range
// coordinates with position >= recordIndex, and the non-empty decoded payload
// text. Anything else returns false, so the authoritative read path
// (CollectRetainedUnknown) stays responsible for those refusals.
//
// Three classes stay deliberately on the size refusal even though the
// authoritative path would name an integrity error, because deciding them needs
// the decode this probe exists to avoid or the global state it does not
// accumulate; each is named here and asserted in
// TestProjectRetainedUnknownPrecedence:
//   - a payloadText whose decoded text is not valid JSON: measuring the decoded
//     length cannot validate content without decoding;
//   - a legacy raw payload value with invalid JSON syntax: measuring the raw
//     extent cannot validate syntax without copying the value, which would
//     materialize exactly what the refusal must not materialize;
//   - cross-record ordering and pointer uniqueness, which the authoritative path
//     decides over the coordinate-sorted record set while the probe walks each
//     record once in stored order.
func storedRetainedPayloadExceedsTransferLimit(entries []schema.SessionEntry, harness Harness) bool {
	over := false
	for i := range entries {
		extra := entries[i].Extra
		if extra == nil {
			continue
		}
		entryOver, ok := storedRetainedExtraExceedsTransferLimit(*extra, harness, entries[i].Harness, retainedUnknownTransferLimitBytes)
		if !ok {
			return false
		}
		if entryOver {
			over = true
		}
	}
	return over
}

// storedRetainedExtraExceedsTransferLimit walks one stored Extra root in place
// and probes only its retainedUnknown evidence array. It returns (over, ok):
// over reports an over-limit payload in a canonical record, and ok reports that
// the whole root and every retained record in it were recognized as canonical
// stored shapes. A duplicate or case-aliased root member, trailing content
// after the root object, a trailing comma, a lax extension value, an empty
// evidence array, an unrecognized shape, or any malformed record returns
// ok=false so the authoritative path owns the integrity error.
func storedRetainedExtraExceedsTransferLimit(extra string, harness, entryHarness Harness, limit int) (over, ok bool) {
	i := skipJSONSpace(extra, 0)
	if i >= len(extra) || extra[i] != '{' {
		return false, false
	}
	i++
	seen := make(map[string]struct{})
	folded := make(map[string]string)
	arrayStart := -1
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, false
		}
		if extra[i] == '}' {
			break
		}
		if extra[i] != '"' {
			return false, false
		}
		name, next, keyOK := canonicalJSONKey(extra, i)
		if !keyOK {
			return false, false
		}
		if _, duplicate := seen[name]; duplicate {
			return false, false
		}
		seen[name] = struct{}{}
		fold := strings.ToLower(name)
		if prev, exists := folded[fold]; exists && prev != name {
			return false, false
		}
		folded[fold] = name
		i = skipJSONSpace(extra, next)
		if i >= len(extra) || extra[i] != ':' {
			return false, false
		}
		i = skipJSONSpace(extra, i+1)
		if name == retainedUnknownKey {
			if i >= len(extra) || extra[i] != '[' {
				return false, false
			}
			arrayStart = i
		}
		end, valueOK := skipJSONValue(extra, i)
		if !valueOK {
			return false, false
		}
		if name != retainedUnknownKey {
			// Extension members are preserved verbatim by the authoritative
			// path, which still scans the whole root strictly
			// (ExtractExtraRetainedArray), so the probe strict-checks each
			// skipped value with the same helper. The copy is bounded by the
			// extension value's own extent, never by a retained payload.
			if err := schema.ScanRawJSONDocument([]byte(extra[i:end]), schema.RawJSONPathPolicy{MaxDocumentBytes: end - i, MaxDocumentDepth: strictRetainedExtensionDepth}); err != nil {
				return false, false
			}
		}
		i = skipJSONSpace(extra, end)
		if i >= len(extra) {
			return false, false
		}
		if extra[i] == ',' {
			i = skipJSONSpace(extra, i+1)
			if i >= len(extra) || extra[i] == '}' {
				return false, false
			}
			continue
		}
		if extra[i] == '}' {
			break
		}
		return false, false
	}
	// The authoritative root scan refuses trailing content after the document;
	// the probe must decline it too rather than report size over corruption.
	if skipJSONSpace(extra, i+1) != len(extra) {
		return false, false
	}
	if arrayStart < 0 {
		// A root without retained evidence stores nothing to measure.
		return false, true
	}
	return retainedArrayExceedsTransferLimit(extra, arrayStart, harness, entryHarness, limit)
}

// retainedArrayExceedsTransferLimit walks the retainedUnknown array in place.
// It keeps walking every envelope after an over-limit one so a later malformed
// record still declines the probe (ok=false), which keeps the verdict
// independent of envelope order.
func retainedArrayExceedsTransferLimit(extra string, i int, harness, entryHarness Harness, limit int) (over, ok bool) {
	i++ // consume '['
	count := 0
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, false
		}
		if extra[i] == ']' {
			// An empty evidence array is refused by the authoritative path
			// (RetainedUnknownOf), so the probe declines it too rather than
			// letting an over-limit record in another entry mask it.
			if count == 0 {
				return false, false
			}
			return over, true
		}
		if extra[i] != '{' {
			return false, false
		}
		envelopeOver, end, envelopeOK := retainedEnvelopeExceedsTransferLimit(extra, i, harness, entryHarness, limit)
		if !envelopeOK {
			return false, false
		}
		if envelopeOver {
			over = true
		}
		count++
		i = skipJSONSpace(extra, end)
		if i >= len(extra) {
			return false, false
		}
		if extra[i] == ',' {
			i = skipJSONSpace(extra, i+1)
			if i >= len(extra) || extra[i] == ']' {
				return false, false
			}
			continue
		}
		if extra[i] == ']' {
			return over, true
		}
		return false, false
	}
}

// retainedEnvelopeExceedsTransferLimit measures one canonical stored record
// envelope in place. It validates the whole envelope before deciding: every
// owned member must be present and non-null, no member may be unknown or
// duplicated, exactly one payload encoding may be present, the harness must be
// a known harness matching every harness the caller expects, the decoded
// payload text must be non-empty, and position must carry public coordinates
// plus a non-public locator with safe-range ordering. The over-limit verdict is
// accumulated across members and returned last, so member order cannot let an
// over-limit payload mask corruption later in the envelope. Anything
// non-canonical is not measured and defers to the authoritative read path
// (ok=false), which owns the integrity error.
func retainedEnvelopeExceedsTransferLimit(extra string, i int, harness, entryHarness Harness, limit int) (over bool, end int, ok bool) {
	var seen uint64
	sawPayload := false
	over = false
	i++ // consume '{'
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, 0, false
		}
		if extra[i] == '}' {
			break
		}
		if extra[i] != '"' {
			return false, 0, false
		}
		name, next, keyOK := canonicalJSONKey(extra, i)
		if !keyOK {
			return false, 0, false
		}
		bit, owned := jsonMemberBit(retainedEnvelopeKeys, name)
		if !owned || seen&bit != 0 {
			return false, 0, false
		}
		seen |= bit
		i = skipJSONSpace(extra, next)
		if i >= len(extra) || extra[i] != ':' {
			return false, 0, false
		}
		i = skipJSONSpace(extra, i+1)
		switch name {
		case "payloadText":
			// At most one payload encoding: the authoritative decode refuses a
			// record that carries both, so this probe must not measure either.
			if sawPayload || i >= len(extra) || extra[i] != '"' {
				return false, 0, false
			}
			sawPayload = true
			decoded, valueEnd, stringOK := scanJSONString(extra, i)
			if !stringOK {
				return false, 0, false
			}
			// An empty decoded payload is refused by the authoritative path
			// (len(payload) == 0), so the probe declines it too.
			if decoded == 0 {
				return false, 0, false
			}
			if decoded > limit {
				over = true
			}
			i = valueEnd
		case "payload":
			if sawPayload {
				return false, 0, false
			}
			sawPayload = true
			valueStart := i
			valueEnd, valueOK := skipJSONValue(extra, i)
			if !valueOK || extra[valueStart:valueEnd] == "null" {
				return false, 0, false
			}
			// Match the backstop, which measures the raw stored extent
			// (len(record.Payload)) for the legacy encoding, whatever JSON kind
			// the value is: a string's quotes and escape bytes count too.
			if valueEnd-valueStart > limit {
				over = true
			}
			i = valueEnd
		case "position":
			valueEnd, positionOK := scanRetainedPosition(extra, i)
			if !positionOK {
				return false, 0, false
			}
			i = valueEnd
		case "harness":
			valueEnd, harnessOK := scanEnvelopeHarness(extra, i, harness, entryHarness)
			if !harnessOK {
				return false, 0, false
			}
			i = valueEnd
		case "namespace", "kind":
			valueEnd, textOK := scanNonEmptyJSONString(extra, i)
			if !textOK {
				return false, 0, false
			}
			i = valueEnd
		default:
			return false, 0, false
		}
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, 0, false
		}
		if extra[i] == ',' {
			i = skipJSONSpace(extra, i+1)
			if i >= len(extra) || extra[i] == '}' {
				return false, 0, false
			}
			continue
		}
		if extra[i] == '}' {
			break
		}
		return false, 0, false
	}
	required := jsonMemberMask(retainedEnvelopeKeys, "harness", "namespace", "kind", "position")
	if seen&required != required || !sawPayload {
		return false, 0, false
	}
	return over, i + 1, true
}

// jsonMemberBit maps a canonical object member name to its duplicate-detection
// bit within one closed key set. Masks are 64 bits wide so a growing key set
// cannot silently wrap around and disable duplicate detection.
func jsonMemberBit(keys []string, name string) (uint64, bool) {
	for index, key := range keys {
		if key == name {
			return 1 << uint(index), true
		}
	}
	return 0, false
}

// jsonMemberMask returns the combined bits of the named members within a closed
// key set.
func jsonMemberMask(keys []string, names ...string) uint64 {
	var mask uint64
	for _, name := range names {
		if bit, ok := jsonMemberBit(keys, name); ok {
			mask |= bit
		}
	}
	return mask
}

// scanEnvelopeHarness reads the envelope harness member and requires it to name
// a known harness that matches every harness the caller expects (the export
// harness and the carrier entry's harness, either of which may be empty). An
// absent, null, escaped, unknown, or mismatched harness returns ok=false so the
// authoritative path reports the integrity error.
func scanEnvelopeHarness(s string, i int, harness, entryHarness Harness) (end int, ok bool) {
	name, end, ok := canonicalJSONKey(s, i)
	if !ok {
		return 0, false
	}
	harnessValue := Harness(name)
	if !slices.Contains(schema.Harnesses(), harnessValue) {
		return 0, false
	}
	for _, expected := range []Harness{harness, entryHarness} {
		if expected != "" && harnessValue != expected {
			return 0, false
		}
	}
	return end, true
}

// scanNonEmptyJSONString reads a canonical (unescaped) JSON string and requires
// its decoded text to be non-empty after trimming space, matching the
// authoritative namespace and kind rule. An escaped spelling defers to the
// authoritative path rather than being measured.
func scanNonEmptyJSONString(s string, i int) (end int, ok bool) {
	text, end, ok := canonicalJSONKey(s, i)
	if !ok || strings.TrimSpace(text) == "" {
		return 0, false
	}
	return end, true
}

// scanRetainedPosition validates one owned position object in place. It
// requires the closed position member set with no duplicates, the optional
// members to carry the right scalar kind, and a canonical, non-null public
// coordinate object carrying all three members. It mirrors the authoritative
// locator rule (checkRetainedUnknownShape): besides public, the position must
// carry a non-zero line or sequence, a non-blank sourceId, or a valid
// sourceEntryRef; a public-only position declines. Line and sequence spellings
// are checked with the authoritative DecodeOwnedInt64 plus the int range, the
// pointer must satisfy the shared validUnknownPointer grammar, and a non-empty
// sourceEntryRef must satisfy schema.SourceEntryRef.Validate. A legacy position
// with no public member returns ok=false so the authoritative
// missing-coordinate refusal stays on its existing path.
func scanRetainedPosition(s string, i int) (end int, ok bool) {
	i = skipJSONSpace(s, i)
	if i >= len(s) || s[i] != '{' {
		return 0, false
	}
	i++
	var seen uint64
	hasPublic := false
	hasLocator := false
	for {
		i = skipJSONSpace(s, i)
		if i >= len(s) {
			return 0, false
		}
		if s[i] == '}' {
			break
		}
		if s[i] != '"' {
			return 0, false
		}
		name, next, keyOK := canonicalJSONKey(s, i)
		if !keyOK {
			return 0, false
		}
		bit, owned := jsonMemberBit(retainedPositionKeys, name)
		if !owned || seen&bit != 0 {
			return 0, false
		}
		seen |= bit
		i = skipJSONSpace(s, next)
		if i >= len(s) || s[i] != ':' {
			return 0, false
		}
		i = skipJSONSpace(s, i+1)
		switch name {
		case "public":
			publicEnd, publicOK := scanRetainedPublicPosition(s, i)
			if !publicOK {
				return 0, false
			}
			hasPublic = true
			i = publicEnd
		case "line", "sequence":
			valueEnd, valueOK := scanOptionalJSONInteger(s, i)
			if !valueOK {
				return 0, false
			}
			if literal := s[i:valueEnd]; literal != "null" {
				coordinate, err := DecodeOwnedInt64(json.RawMessage(literal), "position."+name)
				if err != nil || coordinate < 0 || coordinate > int64(maxIntCoordinate()) {
					return 0, false
				}
				if coordinate != 0 {
					hasLocator = true
				}
			}
			i = valueEnd
		case "sourceEntryRef", "sourceId", "jsonPointer":
			valueEnd, valueOK := scanOptionalJSONString(s, i)
			if !valueOK {
				return 0, false
			}
			if s[i:valueEnd] != "null" {
				value, _, valueOK := canonicalJSONKey(s, i)
				if !valueOK {
					return 0, false
				}
				switch name {
				case "sourceEntryRef":
					if value != "" {
						if err := schema.SourceEntryRef(value).Validate(); err != nil {
							return 0, false
						}
						hasLocator = true
					}
				case "sourceId":
					if strings.TrimSpace(value) != "" {
						hasLocator = true
					}
				default: // jsonPointer
					if !validUnknownPointer(value) {
						return 0, false
					}
				}
			}
			i = valueEnd
		default:
			return 0, false
		}
		i = skipJSONSpace(s, i)
		if i >= len(s) {
			return 0, false
		}
		if s[i] == ',' {
			i = skipJSONSpace(s, i+1)
			if i >= len(s) || s[i] == '}' {
				return 0, false
			}
			continue
		}
		if s[i] == '}' {
			break
		}
		return 0, false
	}
	if !hasPublic || !hasLocator {
		return 0, false
	}
	return i + 1, true
}

// scanRetainedPublicPosition validates the owned public coordinate object in
// place: the closed member set, no duplicates, all three members present and
// non-null, sourceRef a non-empty string, and the two coordinates safe-range
// unsigned integers with position >= recordIndex, mirroring the authoritative
// decode. Anything else returns ok=false so the authoritative
// decoder reports the integrity error.
func scanRetainedPublicPosition(s string, i int) (end int, ok bool) {
	i = skipJSONSpace(s, i)
	if i >= len(s) || s[i] != '{' {
		return 0, false
	}
	i++
	var seen uint64
	var recordIndex, position uint64
	for {
		i = skipJSONSpace(s, i)
		if i >= len(s) {
			return 0, false
		}
		if s[i] == '}' {
			break
		}
		if s[i] != '"' {
			return 0, false
		}
		name, next, keyOK := canonicalJSONKey(s, i)
		if !keyOK {
			return 0, false
		}
		bit, owned := jsonMemberBit(retainedPublicKeys, name)
		if !owned || seen&bit != 0 {
			return 0, false
		}
		seen |= bit
		i = skipJSONSpace(s, next)
		if i >= len(s) || s[i] != ':' {
			return 0, false
		}
		i = skipJSONSpace(s, i+1)
		var valueEnd int
		var valueOK bool
		if name == "sourceRef" {
			valueEnd, valueOK = scanRequiredJSONString(s, i)
		} else {
			valueEnd, valueOK = scanRequiredJSONInteger(s, i)
			if valueOK {
				coordinate, err := strconv.ParseUint(s[i:valueEnd], 10, 64)
				if err != nil {
					return 0, false
				}
				if name == "recordIndex" {
					recordIndex = coordinate
				} else {
					position = coordinate
				}
			}
		}
		if !valueOK {
			return 0, false
		}
		i = valueEnd
		i = skipJSONSpace(s, i)
		if i >= len(s) {
			return 0, false
		}
		if s[i] == ',' {
			i = skipJSONSpace(s, i+1)
			if i >= len(s) || s[i] == '}' {
				return 0, false
			}
			continue
		}
		if s[i] == '}' {
			break
		}
		return 0, false
	}
	if seen != jsonMemberMask(retainedPublicKeys, retainedPublicKeys...) {
		return 0, false
	}
	if position < recordIndex {
		return 0, false
	}
	return i + 1, true
}

// scanRequiredJSONString reads a non-empty canonical JSON string member.
func scanRequiredJSONString(s string, i int) (end int, ok bool) {
	text, end, ok := canonicalJSONKey(s, i)
	if !ok || text == "" {
		return 0, false
	}
	return end, true
}

// scanRequiredJSONInteger reads a non-null unsigned JSON integer member within
// the shared JSON-safe coordinate range.
func scanRequiredJSONInteger(s string, i int) (end int, ok bool) {
	valueEnd, ok := skipJSONValue(s, i)
	if !ok {
		return 0, false
	}
	literal := s[i:valueEnd]
	if !isUnsignedJSONInteger(literal) {
		return 0, false
	}
	value, err := strconv.ParseUint(literal, 10, 64)
	if err != nil || value > uint64(maxSafeCoordinate) {
		return 0, false
	}
	return valueEnd, true
}

// scanOptionalJSONString reads an optional position string member. A `null`
// value is allowed (the authoritative decoder ignores it); any other value must
// be a canonical JSON string.
func scanOptionalJSONString(s string, i int) (end int, ok bool) {
	valueEnd, valueOK := skipJSONValue(s, i)
	if !valueOK {
		return 0, false
	}
	if s[i:valueEnd] == "null" {
		return valueEnd, true
	}
	_, end, ok = canonicalJSONKey(s, i)
	return end, ok
}

// scanOptionalJSONInteger reads an optional position integer member. A `null`
// value is allowed; any other value must be an unsigned JSON integer literal.
func scanOptionalJSONInteger(s string, i int) (end int, ok bool) {
	valueEnd, valueOK := skipJSONValue(s, i)
	if !valueOK {
		return 0, false
	}
	literal := s[i:valueEnd]
	if literal == "null" {
		return valueEnd, true
	}
	if !isUnsignedJSONInteger(literal) {
		return 0, false
	}
	return valueEnd, true
}

// isUnsignedJSONInteger reports whether literal is a bare run of decimal digits,
// the only spelling encoding/json accepts for the int64 coordinate fields.
func isUnsignedJSONInteger(literal string) bool {
	if literal == "" {
		return false
	}
	for k := 0; k < len(literal); k++ {
		if literal[k] < '0' || literal[k] > '9' {
			return false
		}
	}
	return true
}

// canonicalJSONKey returns the unescaped member name at s[i] when it is a
// canonical (unescaped) JSON key. An escaped key can never name an owned member,
// which is exactly the closed-member rule, so it is not a candidate.
func canonicalJSONKey(s string, i int) (string, int, bool) {
	if i >= len(s) || s[i] != '"' {
		return "", 0, false
	}
	start := i + 1
	j := start
	for j < len(s) {
		switch c := s[j]; {
		case c == '"':
			return s[start:j], j + 1, true
		case c == '\\' || c < 0x20:
			return "", 0, false
		default:
			j++
		}
	}
	return "", 0, false
}

// scanJSONString measures the decoded byte length of the JSON string literal at
// s[i] without decoding it into a string. The count matches encoding/json's
// string decoding: each escape yields the UTF-8 length of its decoded rune, and
// raw bytes count one each (the stored document is valid UTF-8, and a malformed
// byte only ever under-counts, which defers the refusal to the authoritative
// path rather than fabricating one). This is the quantity the probe compares to
// the transfer limit for the payloadText encoding; see the two-quantity
// contract at the top of this file.
func scanJSONString(s string, i int) (decoded, end int, ok bool) {
	j := i + 1
	for j < len(s) {
		switch c := s[j]; {
		case c == '"':
			return decoded, j + 1, true
		case c == '\\':
			j++
			if j >= len(s) {
				return 0, 0, false
			}
			switch s[j] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				decoded++
				j++
			case 'u':
				if j+4 >= len(s) {
					return 0, 0, false
				}
				r1, hexOK := parseHex4(s[j+1 : j+5])
				if !hexOK {
					return 0, 0, false
				}
				j += 5
				switch {
				case r1 >= 0xD800 && r1 <= 0xDBFF:
					if _, pairOK := parseHex4Pair(s, j); pairOK {
						decoded += 4
						j += 6
					} else {
						decoded += 3
					}
				case r1 >= 0xDC00 && r1 <= 0xDFFF:
					decoded += 3
				default:
					decoded += utf8.RuneLen(rune(r1))
				}
			default:
				return 0, 0, false
			}
		case c < 0x20:
			return 0, 0, false
		default:
			decoded++
			j++
		}
	}
	return 0, 0, false
}

// parseHex4Pair consumes a low-surrogate escape at s[i] when it directly
// follows a high surrogate, so the pair decodes to one four-byte rune.
func parseHex4Pair(s string, i int) (int, bool) {
	if i+5 >= len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	r, ok := parseHex4(s[i+2 : i+6])
	if !ok || r < 0xDC00 || r > 0xDFFF {
		return 0, false
	}
	return r, true
}

func parseHex4(s string) (int, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var v int
	for k := 0; k < 4; k++ {
		switch c := s[k]; {
		case c >= '0' && c <= '9':
			v = v<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | int(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

func skipJSONSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONValue advances past one JSON value without decoding it. Bracket
// balancing is structural, not strict grammar: strings are consumed exactly,
// but lax interiors (a trailing comma, a leading zero) inside a skipped value
// are the strict re-scan's job for extension members, and a deliberately named
// size precedence for the legacy raw payload value (see the probe header).
func skipJSONValue(s string, i int) (int, bool) {
	i = skipJSONSpace(s, i)
	if i >= len(s) {
		return 0, false
	}
	switch c := s[i]; {
	case c == '"':
		_, end, stringOK := scanJSONString(s, i)
		return end, stringOK
	case c == '{' || c == '[':
		open := c
		closeBracket := byte('}')
		if open == '[' {
			closeBracket = ']'
		}
		depth := 0
		j := i
		for j < len(s) {
			switch s[j] {
			case '"':
				_, stringEnd, stringOK := scanJSONString(s, j)
				if !stringOK {
					return 0, false
				}
				j = stringEnd
			case open:
				depth++
				j++
			case closeBracket:
				depth--
				j++
				if depth == 0 {
					return j, true
				}
			default:
				j++
			}
		}
		return 0, false
	default:
		j := i
		for j < len(s) && !isJSONValueDelimiter(s[j]) {
			j++
		}
		if j == i {
			return 0, false
		}
		return j, true
	}
}

func isJSONValueDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', '}', ']':
		return true
	}
	return false
}
