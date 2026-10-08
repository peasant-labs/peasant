package store

import (
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// GenerationRecord is the session_generations row (design §3.3 table 3): the
// harmonized generation catalog that replaces session_projection_generations.
// The former metadata_json is flattened: UnifiedMetadata's 1:1 attributes are
// columns here, its 1:N collections are the GenerationChildren tables, and
// its relationships live in the relationship evidence rows. The measured stats
// are NOT here: they are mutable and live in CapturedStats; metadata_hash is
// the stats-excluded capture anchor. Closed sets reuse the schema module and
// indexformat; wire-external identifiers use their New* constructors at trust
// boundaries.
type GenerationRecord struct {
	SessionID                    schema.SessionID
	GenerationID                 string
	SchemaVersion                int
	Harness                      schema.Harness
	Model                        schema.ModelID
	Version                      string
	TimestampStartMs             int64
	TimestampEndMs               int64
	TimestampIngestedMs          *int64
	SourceFilePath               *string
	SourceFormat                 schema.SourceFormat
	GitBranch                    *string
	GitRemote                    *string
	GitWorktree                  *string
	GitTracking                  *string
	ProjectHash                  schema.ProjectHash
	ProjectFilePath              *string
	ProjectName                  string
	HostSlug                     schema.HostSlug
	RootSessionID                *schema.SessionID
	Purpose                      *schema.SessionPurpose
	CWD                          *string
	DerivedAtMs                  *int64
	ContentHash                  string
	MetadataHash                 string // stats-excluded capture anchor (never re-derived)
	RedactionApplied             bool
	RedactionLevel               *string
	RedactionRuleSetVersion      *string
	RedactionAtMs                *int64
	RedactionContentHashAtRedact *string
	AdapterVersion               *int
	DiagnosticsPartial           *bool
	Completeness                 indexformat.GenerationCompleteness
	SourceEvidenceDigest         string // 64 hex
	IndexFormatVersion           int    // always 2 for harmonized rows
	CandidateDigest              string // 64 hex, the §4.1 binding
	PriorEvidence                []byte // opaque harness evidence document
	InstalledAtMs                int64
	ActivatedAtMs                *int64
}

// GenerationSubagent is one session_generation_subagents row: the parent
// generation plus an ordinal, so identity and serialization order are exact.
type GenerationSubagent struct {
	Ordinal           int
	SubagentSessionID schema.SessionID
	ParentUUID        schema.SessionID
}

// GenerationCommit is one session_generation_commits row, in ordinal order.
type GenerationCommit struct {
	Ordinal     int
	Hash        string
	Message     string
	AuthorName  string
	AuthorEmail string
	CommitTime  int64
	AuthorTime  int64
}

// GenerationAssociation is one session_generation_associations row, in
// ordinal order.
type GenerationAssociation struct {
	Ordinal            int
	AssociationID      string
	ObservedCommitHash string
}

// GenerationDiagnostic is one session_generation_diagnostics row, in ordinal
// order.
type GenerationDiagnostic struct {
	Ordinal     int
	ErrorType   string
	Location    string
	Message     string
	Remediation string
}

// GenerationRelationship is one session_relationship_evidence row
// (metadata.relationships, structured; design §3.3 table 6).
type GenerationRelationship struct {
	Kind                    schema.SessionRelationshipKind
	TargetState             schema.RelationshipTargetState
	TargetLocalID           *schema.SessionID
	Evidence                *string
	AnchorKind              *schema.PublicSourceAnchorKind
	AnchorSourceEntryRef    *schema.SourceEntryRef
	AnchorSourceRevisionRef *string
}

// GenerationSegment is one session_context_segments row: one ordered captured
// context segment with its native coordinates.
type GenerationSegment struct {
	Ordinal                 int
	LogicalSessionID        *schema.SessionID
	PhysicalSourceID        string
	CoordinateKind          indexformat.CoordinateKind
	StartCoordinate         *int64
	EndExclusive            *int64
	DecodedByteStart        *int64
	DecodedByteEndExclusive *int64
	Inclusion               indexformat.SegmentInclusion
}

// GenerationSegmentRef is one session_context_segment_refs row
// (segment.CapturedRefs, ordered).
type GenerationSegmentRef struct {
	SegmentOrdinal int
	Ordinal        int
	SourceEntryRef schema.SourceEntryRef
}

// GenerationSection is one session_projection_sections row: one partition
// with its earlier-history state (no JSON column).
type GenerationSection struct {
	PartitionID  int
	EarlierState *schema.EarlierHistoryState
}

// GenerationNativeMetadata is one session_section_native_metadata row
// (partition.NativeMetadata, ordered). Data stays opaque: the native
// usage-metadata payload, schema-less by the harness's own design. It is
// text, not bytes: the STRICT data TEXT column refuses BLOB bindings, so the
// contract carries the JSON document as a string and the writer binds it
// directly with no conversion.
type GenerationNativeMetadata struct {
	PartitionID          int
	Ordinal              int
	NativeID             string
	Kind                 schema.NativeMetadataKind
	SourceEntryRef       schema.SourceEntryRef
	SourceType           schema.NativeMetadataSourceType
	SourceMessageRole    *schema.NativePiMessageRole
	AttachmentTurnIndex  *int
	AttachmentToolCallID *string
	CustomType           *string
	Data                 string // opaque native payload (JSON text, bound to the STRICT TEXT column)
}

// GenerationChildren is the generation's 1:N metadata children (design §3.3
// tables 3b and 6). Each collection is keyed by its parent plus an ordinal,
// so identity and serialization order are exact; each serializes back to its
// old JSON collection byte-for-byte for the migration's shadow verify (§7.2)
// and internal prior comparisons. Written with the generation row.
type GenerationChildren struct {
	Subagents      []GenerationSubagent
	Commits        []GenerationCommit
	Associations   []GenerationAssociation
	Diagnostics    []GenerationDiagnostic
	TitleRefs      []schema.SourceEntryRef
	Relationships  []GenerationRelationship
	Segments       []GenerationSegment
	SegmentRefs    []GenerationSegmentRef
	Sections       []GenerationSection
	NativeMetadata []GenerationNativeMetadata
}

// CapturedStats is the session_captured_stats row (design §3.3 table 3c): the
// session's mutable measurements. One row per native session, latest
// knowledge wins, no history. NULL (nil) = unknown; harness adoption is not
// uniform. Source records the latest update's origin — a row label, not
// per-field provenance. SeedJSON is the harness-only seed home: written only
// by the harness updates and the migration backfills, never by COMPUTE.
type CapturedStats struct {
	SessionID            schema.SessionID
	TurnCount            *int
	InputSubmissionCount *int64
	ToolCallCount        *int
	SubagentCount        *int
	DurationMs           *int64
	TokensIn             *int
	TokensOut            *int
	ThoughtTokens        *int
	CachedReadTokens     *int
	CachedWriteTokens    *int
	SeedJSON             *string // harness-only seed document; never written by COMPUTE
	Source               StatsSource
	UpdatedAtMs          int64
	Overflow             *string // opaque: stat kinds not yet promoted
}

// SeedWrite is one write to the harness-only seed home (design §3.3): the
// capture's stats document, replaced wholesale. Only harness origins
// (capture, resume, backfill, migration) may produce one; COMPUTE's derived
// upsert never writes it, so computed output can never be consumed as adapter
// evidence.
type SeedWrite struct {
	SessionID   schema.SessionID
	SeedJSON    string // the capture's stats document; must satisfy the json_valid guard
	UpdatedAtMs int64
}

// SeedWriteAllowed is the seed rule: it reports whether an origin may write
// the harness-only seed home. Only StatsSourceHarness origins are admitted.
// Stub: returns ErrHarmonizedNotImplemented until the stats writer lands it.
func SeedWriteAllowed(source StatsSource) (bool, error) {
	return false, ErrHarmonizedNotImplemented
}

// ContentBlob is the session_content header row (design §3.3 table 2): bytes
// with no body home. ChunkCount is derived from byte length, never stored.
type ContentBlob struct {
	SessionID  schema.SessionID
	Digest     ContentDigest
	ByteLength int64
}

// ContentChunk is one session_content_chunks row: a whole 64 KiB chunk of a
// ContentBlob, ordered by chunk index.
type ContentChunk struct {
	SessionID  schema.SessionID
	Digest     ContentDigest
	ChunkIndex int
	Data       []byte
}

// serializeMetadata rebuilds the captured UnifiedMetadata for the migration's
// shadow verify (§7.2) and for internal prior comparisons; it is not a wire
// surface — no wire payload carries the captured document, readers use the
// structured columns. Stub: panics with ErrHarmonizedNotImplemented until the
// migration lands it.
func serializeMetadata(gen GenerationRecord, children GenerationChildren, stats CapturedStats) []byte {
	panic(ErrHarmonizedNotImplemented)
}
