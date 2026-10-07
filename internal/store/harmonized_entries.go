package store

import (
	"errors"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// bodyRowIDBase is the FTS rowid base (design §3.3, §3.5): every
// session_entry_bodies.body_id is allocated at or above it, and every
// session_entries rowid stays below it, so the two external-content FTS rowid
// spaces never meet. SQLite DDL cannot reference a Go constant, so the schema
// spells the literal 1125899906842624 in the CHECK and the allocation
// subquery; Go allocation, harvest verify --content, and fixtures use this
// constant. Pinned by the rowid-space-guard fixture.
const bodyRowIDBase int64 = 1 << 50 // 1125899906842624

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
	BodyID          int64 // >= bodyRowIDBase
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

// entryFromRow is the ONLY row -> struct reconstruction: every column maps to
// one SessionEntry field, preserving NULL vs empty and pointer nil-ness
// exactly (design §3.4). Stub: panics with ErrHarmonizedNotImplemented until
// the reader slice lands it.
func entryFromRow(r EntryRecord) schema.SessionEntry {
	panic(ErrHarmonizedNotImplemented)
}

// serializeEntry is the canonical text: the same json.Marshal the old writer
// applied to the in-memory entry. body_digest = sha256(serializeEntry(row));
// the wire builders use entryFromRow directly, so byte-parity is this one
// function's contract (golden-tested; design §3.4). Stub: panics with
// ErrHarmonizedNotImplemented until the reader slice lands it.
func serializeEntry(r EntryRecord) []byte {
	panic(ErrHarmonizedNotImplemented)
}

// legacyShape is the routing shim (design §6.2 reader 3): the entry row
// reconstructed and bounded exactly as the mirror would have stored it.
// contentPreview is bounded by contentPreview() only where the mirror bounded
// it (full-content captures, not preview-only ones); ext keys come from the
// promoted columns merged by mergeExtIntoExtra exactly as for the mirror.
// Callers (metrics, classifier inputs, sessions context, TUI, code map) are
// unchanged. Stub: panics with ErrHarmonizedNotImplemented until the reader
// slice lands it.
func legacyShape(row EntryRecord, bounded bool) schema.SessionEntry {
	panic(ErrHarmonizedNotImplemented)
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
