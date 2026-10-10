package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// EntryRecord is the session_entry_bodies row (design §3.3 table 1): every
// schema.SessionEntry field is a column, with the schema's named types — no
// raw strings for closed sets (harness, entry type, role, tool kind, stop
// reason) — and the same pointer nil-ness the wire struct uses
// (schema/content.go). Raw strings enter only through the New* constructors
// at the trust boundaries. The four extra keys (model_id, tokens_reasoning,
// cache_read, cache_write) are promoted entry columns; extra keeps only the
// unknown remainder, canonical; extra_verbatim holds the original string
// whenever the canonical rebuild would not be byte-identical.
type EntryRecord struct {
	BodyID          int64 // >= BodyRowIDBase (1 << 50): the shared FTS rowid base, spelled as a literal in the DDL CHECK
	SessionID       schema.SessionID
	BodyDigest      string // hex sha256(serializeEntry(row)) — a digest, not a closed set
	EntryIndex      int
	Harness         schema.Harness // = bestiary.Harness
	EntryType       schema.EntryType
	Role            schema.Role
	TimestampMs     *int64
	ContentPreview  *string
	TokensIn        *int
	TokensOut       *int
	HasToolUse      bool
	ToolKind        *schema.ToolCallKind
	ToolNamesCSV    *string
	HasThinking     bool
	IsError         bool
	StopReason      *schema.StopReason
	RawByteLength   *int
	ToolCallID      *string
	EntryID         *string
	ParentEntryID   *string
	Depth           int
	ParentIndex     *int
	ToolInput       *string
	ToolOutput      *string
	ModelID         *string // promoted from extra
	TokensReasoning *int    // promoted from extra
	CacheRead       *int    // promoted from extra
	CacheWrite      *int    // promoted from extra
	Extra           *string // the unknown remainder, canonical
	ExtraVerbatim   *string // the non-reconstructible residue
	PartType        *string // the provider's original label (open, not a closed set)
	SourceEntryRef  schema.SourceEntryRef
	Provenance      *schema.ContentProvenance
}

// bodyDigestForRecord hashes the canonical text: the entry row's integrity
// anchor, stored in session_entry_bodies.body_digest and recomputed on every
// full read.
func bodyDigestForRecord(r EntryRecord) BodyDigest {
	sum := sha256.Sum256(serializeEntry(r))
	return BodyDigest(hex.EncodeToString(sum[:]))
}

// contentOf returns the emitted ref's content bytes: the one byte string the
// entry contributes for its source ref. A tool result emits its tool output,
// a tool call its tool input, and every other entry its content preview; the
// tool-row contentPreview echo is the duplicate the kept-echo decision stores
// as-is, never the home. Entries with no content emit nothing.
func contentOf(r EntryRecord) []byte {
	if r.ToolOutput != nil {
		return []byte(*r.ToolOutput)
	}
	if r.ToolInput != nil {
		return []byte(*r.ToolInput)
	}
	if r.ContentPreview != nil {
		return []byte(*r.ContentPreview)
	}
	return nil
}

// The four extra keys promoted to entry columns (the legacy writeExtKeys
// list): model_id as text, the three token counts as integers.
const (
	extraKeyModelID         = "model_id"
	extraKeyTokensReasoning = "tokens_reasoning"
	extraKeyCacheRead       = "cache_read"
	extraKeyCacheWrite      = "cache_write"
)

// entryRecordFromEntry maps one in-memory entry to its body row: every field
// becomes a column, the four known extra keys promote to their columns, and
// the unknown remainder stays canonical with the verbatim gate. A malformed
// extra document refuses the mapping, so hostile input stops in prepare
// before any object is staged. Pi evidence documents stay whole: their keys
// are never promoted, only carried.
func entryRecordFromEntry(entry schema.SessionEntry) (EntryRecord, error) {
	record := EntryRecord{
		SessionID:      entry.SessionID,
		EntryIndex:     entry.EntryIndex,
		Harness:        entry.Harness,
		EntryType:      entry.EntryType,
		Role:           entry.Role,
		TimestampMs:    entry.TimestampMs,
		ContentPreview: entry.ContentPreview,
		TokensIn:       entry.TokensIn,
		TokensOut:      entry.TokensOut,
		HasToolUse:     entry.HasToolUse,
		ToolKind:       entry.ToolKind,
		ToolNamesCSV:   entry.ToolNamesCSV,
		HasThinking:    entry.HasThinking,
		IsError:        entry.IsError,
		StopReason:     entry.StopReason,
		RawByteLength:  entry.RawByteLength,
		ToolCallID:     entry.ToolCallID,
		EntryID:        entry.EntryID,
		ParentEntryID:  entry.ParentEntryID,
		Depth:          entry.Depth,
		ParentIndex:    entry.ParentIndex,
		ToolInput:      entry.ToolInput,
		ToolOutput:     entry.ToolOutput,
		PartType:       entry.PartType,
		SourceEntryRef: entry.SourceEntryRef,
	}
	if entry.Provenance != nil {
		provenance := *entry.Provenance
		record.Provenance = &provenance
	}
	extra, err := splitEntryExtra(entry.SessionID, entry.EntryIndex, entry.Extra)
	if err != nil {
		return EntryRecord{}, err
	}
	record.ModelID = extra.ModelID
	record.TokensReasoning = extra.TokensReasoning
	record.CacheRead = extra.CacheRead
	record.CacheWrite = extra.CacheWrite
	record.Extra = extra.Canonical
	record.ExtraVerbatim = extra.Verbatim
	return record, nil
}

// promotedExtra is the split of one entry's extra document: the four known
// keys as column values plus the canonical remainder (or the verbatim
// original when the canonical rebuild would not be byte-identical). Only one
// of Canonical and Verbatim is ever non-nil, matching the table CHECK.
type promotedExtra struct {
	ModelID         *string
	TokensReasoning *int
	CacheRead       *int
	CacheWrite      *int
	Canonical       *string
	Verbatim        *string
}

// splitEntryExtra promotes the four known keys to column values and returns
// the canonical remainder (or the verbatim original when the canonical
// rebuild would not be byte-identical). Pi evidence documents bypass
// promotion: their keys are evidence, not provider overflow. A malformed
// document refuses the split, so hostile input stops in prepare before any
// object is staged.
func splitEntryExtra(sessionID schema.SessionID, entryIndex int, extra *string) (promotedExtra, error) {
	if extra == nil {
		return promotedExtra{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(*extra)))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return promotedExtra{}, fmt.Errorf("store: entry %d of session %s carries a malformed extra document: %w; no object was staged; re-index the source with a valid provider overflow document", entryIndex, sessionID, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return promotedExtra{}, fmt.Errorf("store: entry %d of session %s carries an extra document with trailing bytes; no object was staged; re-index the source with a valid provider overflow document", entryIndex, sessionID)
	}
	if _, pi, decodeErr := ingest.DecodePiExtra(extra); decodeErr != nil {
		return promotedExtra{}, decodeErr
	} else if pi {
		return finishExtraSplit(*extra, raw, promotedExtra{}), nil
	}
	var split promotedExtra
	remainder := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		switch key {
		case extraKeyModelID:
			var text string
			if json.Unmarshal(value, &text) == nil && text != "" {
				split.ModelID = &text
				continue
			}
		case extraKeyTokensReasoning, extraKeyCacheRead, extraKeyCacheWrite:
			var number json.Number
			if json.Unmarshal(value, &number) == nil {
				if asInt, ok := jsonNumberToInt(number); ok {
					switch key {
					case extraKeyTokensReasoning:
						split.TokensReasoning = &asInt
					case extraKeyCacheRead:
						split.CacheRead = &asInt
					case extraKeyCacheWrite:
						split.CacheWrite = &asInt
					}
					continue
				}
			}
		}
		remainder[key] = value
	}
	return finishExtraSplit(*extra, remainder, split), nil
}

// finishExtraSplit rebuilds the canonical remainder beside the promoted
// columns and applies the verbatim gate: the gate compares the full
// rebuild (promoted values plus remainder, canonical key order) against
// the original bytes. When the rebuild is byte-identical the unknown
// remainder alone is stored (NULL when nothing remains); otherwise the
// original is kept verbatim and the canonical side stays NULL.
func finishExtraSplit(original string, remainder map[string]json.RawMessage, split promotedExtra) promotedExtra {
	merged := make(map[string]json.RawMessage, len(remainder)+4)
	for key, value := range remainder {
		merged[key] = value
	}
	if split.ModelID != nil {
		merged[extraKeyModelID] = json.RawMessage(quoteJSONString(*split.ModelID))
	}
	if split.TokensReasoning != nil {
		merged[extraKeyTokensReasoning] = json.RawMessage(jsonNumber(*split.TokensReasoning))
	}
	if split.CacheRead != nil {
		merged[extraKeyCacheRead] = json.RawMessage(jsonNumber(*split.CacheRead))
	}
	if split.CacheWrite != nil {
		merged[extraKeyCacheWrite] = json.RawMessage(jsonNumber(*split.CacheWrite))
	}
	rebuilt, err := json.Marshal(merged)
	if err != nil || string(rebuilt) != original {
		out := original
		split.Verbatim = &out
		return split
	}
	if len(remainder) == 0 && original != "{}" {
		// The original held only promoted keys in canonical order: the
		// columns reproduce it, so no remainder is stored.
		return split
	}
	encoded, err := json.Marshal(remainder)
	if err != nil {
		out := original
		split.Verbatim = &out
		return split
	}
	out := string(encoded)
	split.Canonical = &out
	return split
}

// jsonNumberToInt accepts only integral JSON numbers that fit an int without
// changing spelling: token counts are whole values, and a fractional,
// exponent, or out-of-range spelling stays in the unknown remainder instead
// of losing precision.
func jsonNumberToInt(number json.Number) (int, bool) {
	asInt64, err := number.Int64()
	if err != nil {
		return 0, false
	}
	if asInt64 != int64(int(asInt64)) {
		return 0, false
	}
	if json.Number(fmt.Sprintf("%d", asInt64)).String() != number.String() {
		return 0, false
	}
	return int(asInt64), true
}

// quoteJSONString encodes one string as its JSON spelling for the canonical
// remainder rebuild.
func quoteJSONString(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// jsonNumber encodes one int as its canonical JSON spelling.
func jsonNumber(value int) string {
	return fmt.Sprintf("%d", value)
}

// The one read router is a single store-level selection shim. The routing
// vocabulary is closed and unchanged: a file-backed session reads through the
// legacy representation, a harmonized session through the body rows.
var (
	// ErrLegacySnapshot marks a read that must use the legacy representation:
	// the session's active generation row is file-backed (Release N only).
	ErrLegacySnapshot = errors.New("store: the session reads through the legacy representation; its active generation is file-backed, so body rows do not exist; read the mirror and chunks instead")
	// ErrSnapshotIncomplete marks a preview-only capture: the stored text is
	// unbounded and the full-capture verification cannot run against it.
	ErrSnapshotIncomplete = errors.New("store: the session holds a preview-only capture; full-capture verification and digest binding do not apply; read the bounded preview path instead")
)

// SnapshotReader is the store-level read-router boundary: one immutable read
// snapshot per session, held under the session's shared lock for the whole
// callback. It aliases the indexformat seam so store consumers import only
// this package's contracts.
type SnapshotReader = indexformat.SnapshotReader

// The production Store is the concrete SnapshotReader consumed through this
// seam; the guard below pins the alias to the implementation the transcript
// hydration and export layers already use.
var _ SnapshotReader = (*Store)(nil)
