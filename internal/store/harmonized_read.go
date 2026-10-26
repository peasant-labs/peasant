package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/peasant-labs/schema"
)

// The harmonized read direction (design section 3.4, section 6.2 readers 1
// and 3): structured body rows back to the wire struct, the canonical text
// derived from them, and the routing shim that shapes a body row exactly as
// the legacy mirror would have stored it.
//
// entryFromRow is the ONLY row-to-struct reconstruction: every column maps to
// one SessionEntry field, preserving NULL against empty and pointer nil-ness
// exactly. Storage identity (BodyID, BodyDigest) stays out of the wire
// struct. The four promoted extra keys (ModelID, TokensReasoning, CacheRead,
// CacheWrite) fold back into Extra beside the unknown remainder, with
// ExtraVerbatim winning whenever the canonical rebuild would not be
// byte-identical to the original document.
func entryFromRow(r EntryRecord) schema.SessionEntry {
	entry := schema.SessionEntry{
		SessionID:      r.SessionID,
		EntryIndex:     r.EntryIndex,
		Harness:        r.Harness,
		EntryType:      r.EntryType,
		Role:           r.Role,
		TimestampMs:    r.TimestampMs,
		ContentPreview: r.ContentPreview,
		TokensIn:       r.TokensIn,
		TokensOut:      r.TokensOut,
		HasToolUse:     r.HasToolUse,
		ToolKind:       r.ToolKind,
		ToolNamesCSV:   r.ToolNamesCSV,
		HasThinking:    r.HasThinking,
		IsError:        r.IsError,
		StopReason:     r.StopReason,
		RawByteLength:  r.RawByteLength,
		ToolCallID:     r.ToolCallID,
		EntryID:        r.EntryID,
		ParentEntryID:  r.ParentEntryID,
		Depth:          r.Depth,
		ParentIndex:    r.ParentIndex,
		ToolInput:      r.ToolInput,
		ToolOutput:     r.ToolOutput,
		PartType:       r.PartType,
		SourceEntryRef: r.SourceEntryRef,
		Provenance:     r.Provenance,
	}
	entry.Extra = rebuildExtra(r)
	return entry
}

// rebuildExtra folds the promoted columns back into the entry's Extra
// document. ExtraVerbatim wins outright: it holds the original string
// whenever the canonical rebuild would not be byte-identical. Otherwise the
// promoted keys merge over the unknown remainder with sorted-key encoding.
// This is the raw full/generation shape; routing applies mergeExtIntoExtra
// separately to preserve the mirror's extension-read semantics.
//
// A remainder that no longer parses is passed through verbatim without the
// merge. Corruption of that shape is caught by the digest check on full reads
// (serializeEntry against the stored body_digest refuses); routing reads
// surface it at JSON decode or show the stored bytes in a non-authoritative
// view, per the read-verification rule.
func rebuildExtra(r EntryRecord) *string {
	if r.ExtraVerbatim != nil {
		return r.ExtraVerbatim
	}
	merged := make(map[string]json.RawMessage)
	if r.Extra != nil {
		if err := json.Unmarshal([]byte(*r.Extra), &merged); err != nil {
			return r.Extra
		}
	}
	mergeExtraValue(merged, "model_id", r.ModelID)
	mergeExtraInt(merged, "tokens_reasoning", r.TokensReasoning)
	mergeExtraInt(merged, "cache_read", r.CacheRead)
	mergeExtraInt(merged, "cache_write", r.CacheWrite)
	if len(merged) == 0 {
		return nil
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return r.Extra
	}
	out := string(encoded)
	return &out
}

// mergeExtraValue encodes one promoted text key into the rebuild map. A
// marshal failure keeps the previous document: the digest check on full reads
// stays the corruption gate, and routing reads keep showing stored bytes.
func mergeExtraValue(merged map[string]json.RawMessage, key string, value *string) {
	if value == nil {
		return
	}
	encoded, err := json.Marshal(*value)
	if err != nil {
		return
	}
	merged[key] = encoded
}

// mergeExtraInt encodes one promoted integer key into the rebuild map. JSON
// numbers marshal without a fraction, matching the mirror's value_int merge.
func mergeExtraInt(merged map[string]json.RawMessage, key string, value *int) {
	if value == nil {
		return
	}
	encoded, err := json.Marshal(*value)
	if err != nil {
		return
	}
	merged[key] = encoded
}

// serializeEntry is the canonical text: the same json.Marshal the writer
// applied to the in-memory entry. body_digest is the sha256 of exactly these
// bytes; the wire builders use entryFromRow directly, so byte-parity with the
// retired entry_json is this one function's contract.
func serializeEntry(r EntryRecord) []byte {
	encoded, err := json.Marshal(entryFromRow(r))
	if err != nil {
		panic(err)
	}
	return encoded
}

// SerializeEntryDigest returns the integrity anchor for one body row: the hex
// sha256 of its canonical text. Full reads recompute it and compare it with
// the stored body_digest; a mismatch refuses the whole read.
func SerializeEntryDigest(r EntryRecord) string {
	sum := sha256.Sum256(serializeEntry(r))
	return hex.EncodeToString(sum[:])
}

// legacyShape preserves the historical hash domains. Bounded shapes omit
// provenance and limit ContentPreview; full shapes retain raw fields.
// Output callers use mirrorShape separately, after full-read verification.
func legacyShape(row EntryRecord, bounded bool) schema.SessionEntry {
	entry := entryFromRow(row)
	if !bounded {
		return entry
	}
	if entry.ContentPreview != nil {
		preview := contentPreview(*entry.ContentPreview)
		entry.ContentPreview = &preview
	}
	entry.SourceEntryRef = ""
	entry.Provenance = nil
	return entry
}

// mirrorShape projects the columns the retired mirror actually carried.
// Full capture verification and hashing must use legacyShape before this
// output-only projection removes generation provenance.
func mirrorShape(row EntryRecord, bounded bool) schema.SessionEntry {
	entry := legacyShape(row, bounded)
	entry.SourceEntryRef = ""
	entry.Provenance = nil
	return entry
}

func promotedExtKVs(row EntryRecord) map[string]any {
	ext := make(map[string]any)
	if row.ModelID != nil {
		ext["model_id"] = *row.ModelID
	}
	if row.TokensReasoning != nil {
		ext["tokens_reasoning"] = *row.TokensReasoning
	}
	if row.CacheRead != nil {
		ext["cache_read"] = *row.CacheRead
	}
	if row.CacheWrite != nil {
		ext["cache_write"] = *row.CacheWrite
	}
	return ext
}
