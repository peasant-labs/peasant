package ingest

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode"
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
const strictRetainedExtensionDepth = localRawEvidenceDepth - 1

// retainedRawPayloadDepthBudget bounds the bracket nesting the probe measures
// for a legacy raw `payload` value. The authoritative document scan allows
// localRawEvidenceDepth levels from the Extra root; a payload value sits three
// levels below that root (root object, retainedUnknown array, envelope object),
// so the same budget applies minus those levels. The cap is conservative by
// one: it declines a payload nested exactly at the authoritative ceiling
// rather than risk reporting size over corruption.
const retainedRawPayloadDepthBudget = localRawEvidenceDepth - 4

// containsUnpairedSurrogateEscape mirrors the schema validateRawUnicodeEscapes
// rule byte-for-byte, including its quote tracking: a `\uXXXX` escape inside
// a JSON string that names a high surrogate must be immediately followed by a
// `\u` low surrogate in DC00-DFFF, and a lone low surrogate is refused. An
// invalid hex escape is ignored here and left to the structural walk, exactly
// as the authoritative pre-scan does. scanJSONString stays the measurement
// helper pinned by the measureCases fixture; this strict pass is a separate
// walk that declines.
func containsUnpairedSurrogateEscape(raw string) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' || i+4 >= len(raw) {
			continue
		}
		value, err := strconv.ParseUint(raw[i+1:i+5], 16, 16)
		if err != nil {
			continue
		}
		r := rune(value)
		if r >= 0xD800 && r <= 0xDFFF {
			if r < 0xD800 || r > 0xDBFF || i+10 >= len(raw) || raw[i+5] != '\\' || raw[i+6] != 'u' {
				return true
			}
			low, err := strconv.ParseUint(raw[i+7:i+11], 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 10
		} else {
			i += 4
		}
	}
	return false
}

// storedRetainedPayloadExceedsTransferLimit reports whether every retained
// evidence record in the entries is a canonical, well-formed envelope and at
// least one stores a payload whose public transfer size exceeds the published
// limit. It reads the stored JSON text in place, so a refusal allocates nothing
// proportional to the payload: an oversized payload is refused without decoding
// or copying the payload itself. Small owned members (harness, namespace,
// kind, source references, pointers) and member names are decoded one at a time
// to validate their canonical escaped spellings; each decode is bounded by its
// own member or key extent, never by a retained payload.
//
// The verdict is member-order independent. The probe walks every member of every
// envelope and every record of every array before it decides, so corruption that
// follows an over-limit payload is still seen; the over-limit result is
// accumulated and returned only once the whole set is known canonical. It is
// deliberately conservative and sound in one direction only: it returns true
// only when every record it examines is a canonical stored envelope apart from
// the three named content/global-state classes below, and at least one payload
// is over the limit. The probe mirrors the authoritative decoder's cheap
// shape rules in place: whole-document strictness for the Extra root (valid
// UTF-8, no unpaired surrogate escapes, trailing content, lax extension
// values, and trailing commas decline), the non-empty evidence array, the
// required envelope members with exactly one payload encoding, the harness
// match, the non-empty namespace and kind, the position locator rule (a
// non-zero line or sequence, a non-blank sourceId, or a valid sourceEntryRef
// alongside public), the RFC 6901 pointer grammar, safe-range coordinates with
// position >= recordIndex, the non-empty decoded payload text that does not
// trim to the null literal, and the legacy raw payload depth budget.
// Anything else returns false, so the authoritative read path
// (CollectRetainedUnknown) stays responsible for those refusals.
//
// Three classes stay deliberately on the size refusal even though the
// authoritative path would name an integrity error, because deciding them needs
// the decode this probe exists to avoid or the global state it does not
// accumulate; each is named here and asserted in
// TestProjectRetainedUnknownPrecedence:
//   - a payloadText whose decoded content the shared scanner refuses
//     (invalid JSON syntax, repeated object member names, unpaired escapes
//     already excluded above, or depth beyond the local budget): measuring
//     the decoded length cannot validate content without decoding;
//   - a legacy raw payload value with invalid JSON syntax, including repeated
//     object member names (depth beyond the payload budget is mirrored above
//     and declines): measuring the raw extent cannot validate syntax without
//     copying the value, which would materialize exactly what the refusal
//     must not materialize;
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
// evidence array, an unrecognized shape, or any malformed envelope or owned
// member returns ok=false so the authoritative path owns the integrity error.
// Payload and payloadText content are deliberately not validated here (see the
// three named precedences at the top of this file).
func storedRetainedExtraExceedsTransferLimit(extra string, harness, entryHarness Harness, limit int) (over, ok bool) {
	// The authoritative root scan refuses the whole document for invalid
	// UTF-8 and unpaired surrogate escapes before any structural walk
	// (schema.ScanRawJSONDocument via retained_raw_codec.go), so the probe
	// declines both without measuring. The UTF-8 check is one linear pass
	// with no allocation; the surrogate pre-scan mirrors the schema
	// validateRawUnicodeEscapes rule byte-for-byte, including its
	// quote-tracking, so the verdict cannot drift from the decoder.
	if !utf8.ValidString(extra) {
		return false, false
	}
	if containsUnpairedSurrogateEscape(extra) {
		return false, false
	}
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
			// (len(payload) == 0), so the probe declines it too. A decoded
			// text that trims to the null literal is refused the same way
			// (retained_unknown.go), so it declines as well rather than
			// reporting size over corruption.
			if decoded == 0 || decodedJSONStringIsNullLiteral(extra, i, valueEnd) {
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
			// A payload nested beyond its share of the authoritative depth
			// budget is refused for integrity, so it declines rather than
			// reporting size over corruption.
			if rawPayloadDepthExceedsBudget(extra, valueStart, valueEnd) {
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
// harness and the carrier entry's harness, either of which may be empty). The
// value is decoded in place so canonical escaped spellings measure; an
// absent, null, unknown, or mismatched harness returns ok=false so the
// authoritative path reports the integrity error.
func scanEnvelopeHarness(s string, i int, harness, entryHarness Harness) (end int, ok bool) {
	name, end, ok := decodeOwnedJSONString(s, i)
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

// scanNonEmptyJSONString reads one owned JSON string member, decoding its
// canonical escaped spellings in place, and requires its decoded text to be
// non-empty after trimming space, matching the authoritative namespace and
// kind rule.
func scanNonEmptyJSONString(s string, i int) (end int, ok bool) {
	text, end, ok := decodeOwnedJSONString(s, i)
	if !ok || strings.TrimSpace(text) == "" {
		return 0, false
	}
	if !utf8.ValidString(text) {
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
				value, _, valueOK := decodeOwnedJSONString(s, i)
				if !valueOK {
					return 0, false
				}
				if !utf8.ValidString(value) {
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
// integers with position >= recordIndex, mirroring the authoritative
// DecodeOwnedInt64 decode (which refuses leading zeros, fractions, and
// out-of-range spellings). Anything else returns ok=false so the authoritative
// decoder reports the integrity error.
func scanRetainedPublicPosition(s string, i int) (end int, ok bool) {
	i = skipJSONSpace(s, i)
	if i >= len(s) || s[i] != '{' {
		return 0, false
	}
	i++
	var seen uint64
	var recordIndex, position int64
	haveRecordIndex := false
	havePosition := false
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
			var coordinate int64
			coordinate, valueEnd, valueOK = scanRequiredJSONInteger(s, i)
			if valueOK {
				if name == "recordIndex" {
					recordIndex = coordinate
					haveRecordIndex = true
				} else {
					position = coordinate
					havePosition = true
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
	if !haveRecordIndex || !havePosition || position < recordIndex {
		return 0, false
	}
	return i + 1, true
}

// scanRequiredJSONString reads a non-empty owned JSON string member, decoding
// its canonical escaped spellings in place.
func scanRequiredJSONString(s string, i int) (end int, ok bool) {
	text, end, ok := decodeOwnedJSONString(s, i)
	if !ok || text == "" {
		return 0, false
	}
	if !utf8.ValidString(text) {
		return 0, false
	}
	return end, true
}

// scanRequiredJSONInteger reads a non-null owned JSON integer member through
// the shared DecodeOwnedInt64 helper plus the JSON-safe coordinate bound, so a
// leading-zero spelling the authoritative decoder refuses declines here too.
func scanRequiredJSONInteger(s string, i int) (value int64, end int, ok bool) {
	valueEnd, valueOK := skipJSONValue(s, i)
	if !valueOK {
		return 0, 0, false
	}
	literal := s[i:valueEnd]
	if literal == "null" {
		return 0, 0, false
	}
	coordinate, err := DecodeOwnedInt64(json.RawMessage(literal), "position.public")
	if err != nil || coordinate < 0 || coordinate > maxSafeCoordinate {
		return 0, 0, false
	}
	return coordinate, valueEnd, true
}

// scanOptionalJSONString reads an optional position string member. A `null`
// value is allowed (the authoritative decoder ignores it); any other value must
// be a JSON string with a canonical escaped spelling decoded in place.
func scanOptionalJSONString(s string, i int) (end int, ok bool) {
	valueEnd, valueOK := skipJSONValue(s, i)
	if !valueOK {
		return 0, false
	}
	if s[i:valueEnd] == "null" {
		return valueEnd, true
	}
	_, end, ok = decodeOwnedJSONString(s, i)
	return end, ok
}

// scanOptionalJSONInteger reads an optional position integer member. A `null`
// value is allowed; any other value must be a bare digit run so the caller can
// run it through the authoritative DecodeOwnedInt64, which refuses leading
// zeros, fractions, and out-of-range spellings.
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
// the pre-filter for the optional coordinate fields. The authoritative
// DecodeOwnedInt64 run by the caller refuses leading-zero spellings among
// them, so this helper accepts a superset that the caller narrows.
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

// canonicalJSONKey returns the decoded member name at s[i]. A canonical
// (unescaped) spelling returns a slice of the stored text without allocating;
// an escaped spelling is decoded in place with decodeOwnedJSONString, bounded
// by the key extent, so escaped spellings measure and duplicate/alias checks
// run on decoded names exactly as the authoritative decoder normalizes them
// (CheckCanonicalObjectKeys / ExtractExtraRetainedArray via encoding/json).
// An undecodable key declines so the authoritative path owns the refusal.
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
		case c == '\\':
			name, end, ok := decodeOwnedJSONString(s, i)
			if !ok {
				return "", 0, false
			}
			return name, end, true
		case c < 0x20:
			return "", 0, false
		default:
			j++
		}
	}
	return "", 0, false
}

// decodeOwnedJSONString decodes one small owned JSON string member in place.
// The literal extent is bounded by the member itself, never by a retained
// payload, so the allocation cannot scale with the payload the refusal must
// not materialize. It accepts the canonical escaped spellings the capture
// constructor stores (encoding/json escapes `&`, `<`, `>`, `"`, `\` and
// controls), which canonicalJSONKey declines. A lone surrogate escape never
// reaches here: the whole-document pre-scan declines it first.
func decodeOwnedJSONString(s string, i int) (string, int, bool) {
	end, ok := skipJSONValue(s, i)
	if !ok || i >= len(s) || s[i] != '"' {
		return "", 0, false
	}
	var decoded string
	if err := json.Unmarshal([]byte(s[i:end]), &decoded); err != nil {
		return "", 0, false
	}
	return decoded, end, true
}

// decodedJSONStringIsNullLiteral reports whether the JSON string literal
// s[start:end] (including quotes) decodes to text that trims to the null
// literal, mirroring the authoritative isNullJSON(payload) refusal for a
// payloadText whose decoded text is empty-adjacent to null. The walk decodes
// escapes on the fly without materializing the string, so a whitespace-padded
// null of any length declines without allocating proportionally to it.
func decodedJSONStringIsNullLiteral(s string, start, end int) bool {
	// State: 0 leading whitespace, 1-4 matching "null", 5 trailing whitespace.
	state := 0
	j := start + 1
	limit := end - 1
	for j < limit {
		var r rune
		c := s[j]
		if c == '\\' {
			j++
			if j >= limit {
				return false
			}
			switch s[j] {
			case '"', '\\', '/':
				r = rune(s[j])
				j++
			case 'b':
				r = '\b'
				j++
			case 'f':
				r = '\f'
				j++
			case 'n':
				r = '\n'
				j++
			case 'r':
				r = '\r'
				j++
			case 't':
				r = '\t'
				j++
			case 'u':
				if j+4 >= len(s) {
					return false
				}
				// j points at 'u'; hex follows inside the literal, which
				// scanJSONString already proved well-formed.
				r1, hexOK := parseHex4(s[j+1 : j+5])
				if !hexOK {
					return false
				}
				j += 5
				if r1 >= 0xD800 && r1 <= 0xDBFF {
					low, pairOK := parseHex4Pair(s, j)
					if !pairOK {
						return false
					}
					r = rune(0x10000 + (r1-0xD800)*0x400 + (low - 0xDC00))
					j += 6
				} else if r1 >= 0xDC00 && r1 <= 0xDFFF {
					return false
				} else {
					r = rune(r1)
				}
			default:
				return false
			}
		} else if c < 0x80 {
			r = rune(c)
			j++
		} else {
			decoded, size := utf8.DecodeRuneInString(s[j:limit])
			if decoded == utf8.RuneError && size <= 1 {
				return false
			}
			r = decoded
			j += size
		}
		// Feed the decoded rune to the null-trim state machine, matching the
		// authoritative bytes.TrimSpace rule via unicode.IsSpace.
		isSpace := unicode.IsSpace(r)
		switch state {
		case 0:
			if isSpace {
				continue
			}
			if r == 'n' {
				state = 1
			} else {
				return false
			}
		case 1:
			if r == 'u' {
				state = 2
			} else {
				return false
			}
		case 2:
			if r == 'l' {
				state = 3
			} else {
				return false
			}
		case 3:
			if r == 'l' {
				state = 4
			} else {
				return false
			}
		case 4:
			if isSpace {
				state = 5
			} else {
				return false
			}
		case 5:
			if !isSpace {
				return false
			}
		}
	}
	// Empty decoded text is handled by the decoded==0 rule; here null means
	// exactly the four letters with only surrounding whitespace.
	return state == 4 || state == 5
}

// scanJSONString measures the decoded byte length of the JSON string literal at
// s[i] without decoding it into a string. The count matches encoding/json's
// string decoding: each escape yields the UTF-8 length of its decoded rune, and
// raw bytes count one each. Probe-level callers reach here only after the
// whole-document valid-UTF-8 and unpaired-surrogate pre-scans pass, so raw
// bytes are valid UTF-8 on that path; the unpaired-surrogate arms below count
// the replacement length only to keep this measurement helper pinned by the
// measureCases fixture, never to accept the shape. This is the quantity the
// probe compares to the transfer limit for the payloadText encoding; see the
// two-quantity contract at the top of this file.
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
// Nesting depth is not capped here: the legacy raw payload branch enforces its
// own share of the authoritative depth budget via rawPayloadDepthExceedsBudget,
// and extension values enforce theirs via the strict re-scan, so this walk
// stays unbounded and cannot pre-decline a shape the authoritative check
// would accept.
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

// rawPayloadDepthExceedsBudget reports whether the JSON value s[start:end]
// nests brackets deeper than the payload's share of the authoritative depth
// budget. The walk is structural and allocation-free: strings are consumed
// exactly, and only the bracket depth of the payload value itself is measured.
// A deeper payload declines so corruption surfaces as integrity rather than
// size; extension values keep their own budget via the strict re-scan.
func rawPayloadDepthExceedsBudget(s string, start, end int) bool {
	depth := 0
	maxDepth := 0
	for j := start; j < end; {
		switch s[j] {
		case '"':
			_, stringEnd, stringOK := scanJSONString(s, j)
			if !stringOK || stringEnd > end {
				return true
			}
			j = stringEnd
		case '{', '[':
			depth++
			if depth > maxDepth {
				maxDepth = depth
				if maxDepth > retainedRawPayloadDepthBudget {
					return true
				}
			}
			j++
		case '}', ']':
			depth--
			j++
		default:
			j++
		}
	}
	return false
}
