package ingest

import (
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

// storedRetainedPayloadExceedsTransferLimit reports whether any entry stores a
// retained evidence payload whose public transfer size exceeds the published
// limit. It reads the stored JSON text in place, so a refusal allocates nothing
// proportional to the payload: an oversized record is refused before it is
// decoded or copied.
//
// The probe is deliberately conservative and sound in one direction only. It
// returns true only when it can positively measure a canonical stored record
// whose payload is over the limit. Any shape it does not recognize, any record
// that lacks public traversal coordinates, and any envelope outside the closed
// owned member set return false, so the authoritative read path
// (CollectRetainedUnknown) stays responsible for those refusals. That keeps
// payload-syntax corruption, envelope corruption, and the legacy missing-
// coordinate refusal on their existing error paths; only an oversized record
// that the authoritative path would otherwise accept is short-circuited here.
func storedRetainedPayloadExceedsTransferLimit(entries []schema.SessionEntry) bool {
	for i := range entries {
		extra := entries[i].Extra
		if extra == nil {
			continue
		}
		if storedRetainedExtraExceedsTransferLimit(*extra, retainedUnknownTransferLimitBytes) {
			return true
		}
	}
	return false
}

// storedRetainedExtraExceedsTransferLimit walks one stored Extra root object in
// place and probes only its retainedUnknown evidence array.
func storedRetainedExtraExceedsTransferLimit(extra string, limit int) bool {
	i := skipJSONSpace(extra, 0)
	if i >= len(extra) || extra[i] != '{' {
		return false
	}
	i++
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false
		}
		if extra[i] == '}' {
			return false
		}
		if extra[i] != '"' {
			return false
		}
		name, next, ok := canonicalJSONKey(extra, i)
		if !ok {
			return false
		}
		i = skipJSONSpace(extra, next)
		if i >= len(extra) || extra[i] != ':' {
			return false
		}
		i = skipJSONSpace(extra, i+1)
		if name == retainedUnknownKey {
			if i >= len(extra) || extra[i] != '[' {
				return false
			}
			return retainedArrayExceedsTransferLimit(extra, i, limit)
		}
		end, ok := skipJSONValue(extra, i)
		if !ok {
			return false
		}
		i = skipJSONSpace(extra, end)
		if i < len(extra) && extra[i] == ',' {
			i++
			continue
		}
		return false
	}
}

// retainedArrayExceedsTransferLimit walks the retainedUnknown array in place.
func retainedArrayExceedsTransferLimit(extra string, i, limit int) bool {
	i++ // consume '['
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false
		}
		if extra[i] == ']' {
			return false
		}
		if extra[i] != '{' {
			return false
		}
		over, end, ok := retainedEnvelopeExceedsTransferLimit(extra, i, limit)
		if !ok {
			return false
		}
		if over {
			return true
		}
		i = skipJSONSpace(extra, end)
		if i >= len(extra) {
			return false
		}
		if extra[i] == ',' {
			i++
			continue
		}
		return false
	}
}

// retainedEnvelopeExceedsTransferLimit measures one canonical stored record
// envelope in place. It requires the closed owned member set, at most one
// payload encoding, and public traversal coordinates; anything else is not
// measured and defers to the authoritative read path.
func retainedEnvelopeExceedsTransferLimit(extra string, i, limit int) (over bool, end int, ok bool) {
	var seen uint8
	sawPayload := false
	i++ // consume '{'
	for {
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, 0, false
		}
		if extra[i] == '}' {
			if !sawPayload {
				return false, 0, false
			}
			return false, i + 1, true
		}
		if extra[i] != '"' {
			return false, 0, false
		}
		name, next, ok := canonicalJSONKey(extra, i)
		if !ok {
			return false, 0, false
		}
		bit, owned := retainedEnvelopeMemberBit(name)
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
			_, stringEnd, exceeded, stringOK := scanJSONString(extra, i, limit)
			if !stringOK {
				return false, 0, false
			}
			if exceeded {
				return true, 0, true
			}
			i = stringEnd
		case "payload":
			if sawPayload {
				return false, 0, false
			}
			sawPayload = true
			valueEnd, exceeded, valueOK := skipJSONValueCapped(extra, i, limit)
			if !valueOK {
				return false, 0, false
			}
			if exceeded {
				return true, 0, true
			}
			i = valueEnd
		case "position":
			valueEnd, valueOK := skipJSONValue(extra, i)
			if !valueOK {
				return false, 0, false
			}
			// A legacy record without public coordinates must reach the
			// authoritative missing-coordinate refusal, never this one.
			if !strings.Contains(extra[i:valueEnd], `"public"`) {
				return false, 0, false
			}
			i = valueEnd
		default:
			valueEnd, valueOK := skipJSONValue(extra, i)
			if !valueOK {
				return false, 0, false
			}
			i = valueEnd
		}
		i = skipJSONSpace(extra, i)
		if i >= len(extra) {
			return false, 0, false
		}
		if extra[i] == ',' {
			i++
			continue
		}
		if extra[i] == '}' {
			if !sawPayload {
				return false, 0, false
			}
			return false, i + 1, true
		}
		return false, 0, false
	}
}

// retainedEnvelopeMemberBit maps an owned envelope member to its duplicate-
// detection bit. Membership comes from the canonical member set the decode path
// enforces, so the probe cannot drift onto a different closed set.
func retainedEnvelopeMemberBit(name string) (uint8, bool) {
	for index, key := range retainedEnvelopeKeys {
		if key == name {
			return 1 << uint(index), true
		}
	}
	return 0, false
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
// s[i] without decoding it into a string. When limit is positive it stops as
// soon as the decoded length exceeds limit and reports exceeded, so an
// oversized payload is measured from its prefix only. The count matches
// encoding/json's string decoding: each escape yields the UTF-8 length of its
// decoded rune, and raw bytes count one each (the stored document is valid
// UTF-8, and a malformed byte only ever under-counts, which defers the refusal
// to the authoritative path rather than fabricating one).
func scanJSONString(s string, i, limit int) (decoded, end int, exceeded, ok bool) {
	j := i + 1
	for j < len(s) {
		switch c := s[j]; {
		case c == '"':
			return decoded, j + 1, false, true
		case c == '\\':
			j++
			if j >= len(s) {
				return 0, 0, false, false
			}
			switch s[j] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				decoded++
				j++
			case 'u':
				if j+4 >= len(s) {
					return 0, 0, false, false
				}
				r1, hexOK := parseHex4(s[j+1 : j+5])
				if !hexOK {
					return 0, 0, false, false
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
				return 0, 0, false, false
			}
		case c < 0x20:
			return 0, 0, false, false
		default:
			decoded++
			j++
		}
		if limit > 0 && decoded > limit {
			return decoded, 0, true, true
		}
	}
	return 0, 0, false, false
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

// skipJSONValue advances past one JSON value without decoding it.
func skipJSONValue(s string, i int) (int, bool) {
	end, _, ok := skipJSONValueCapped(s, i, 0)
	return end, ok
}

// skipJSONValueCapped advances past one JSON value without decoding it. When
// limit is positive it stops early once the raw value extent exceeds limit and
// reports exceeded, so a legacy raw payload is rejected from its prefix.
func skipJSONValueCapped(s string, i, limit int) (end int, exceeded bool, ok bool) {
	i = skipJSONSpace(s, i)
	if i >= len(s) {
		return 0, false, false
	}
	switch c := s[i]; {
	case c == '"':
		_, end, exceeded, stringOK := scanJSONString(s, i, limit)
		return end, exceeded, stringOK
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
				_, stringEnd, _, stringOK := scanJSONString(s, j, 0)
				if !stringOK {
					return 0, false, false
				}
				j = stringEnd
			case open:
				depth++
				j++
			case closeBracket:
				depth--
				j++
				if depth == 0 {
					if limit > 0 && j-i > limit {
						return j, true, true
					}
					return j, false, true
				}
			default:
				j++
			}
			if limit > 0 && j-i > limit {
				return j, true, true
			}
		}
		return 0, false, false
	default:
		j := i
		for j < len(s) && !isJSONValueDelimiter(s[j]) {
			j++
		}
		if j == i {
			return 0, false, false
		}
		if limit > 0 && j-i > limit {
			return j, true, true
		}
		return j, false, true
	}
}

func isJSONValueDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', '}', ']':
		return true
	}
	return false
}
