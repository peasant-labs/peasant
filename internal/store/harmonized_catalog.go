package store

import (
	"encoding/json"
	"fmt"

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
// the harness-only seed home. Only StatsSourceHarness origins are admitted;
// COMPUTE's derived upsert never writes it, so computed output can never be
// consumed as adapter evidence. An origin outside the closed set is refused
// with an error rather than a quiet false.
func SeedWriteAllowed(source StatsSource) (bool, error) {
	switch source {
	case StatsSourceHarness:
		return true, nil
	case StatsSourceDerived:
		return false, nil
	default:
		return false, fmt.Errorf("store.SeedWriteAllowed: origin %q is outside the closed stats-source set; no seed home exists for it; use harness or derived", string(source))
	}
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

// serializeMetadata rebuilds the captured UnifiedMetadata for internal
// prior comparisons and the migration's shadow verify (§7.2); it is not a
// wire surface — no wire payload carries the captured document, readers use
// the structured columns. It is the exact inverse of the catalog mapping:
// every stored column returns to its field, and the stats row supplies the
// measurements. Two values cannot round-trip: ParentUUID is not stored in
// the catalog row (the durable parent lives in sessions.parent_id, which
// the prior loader restores beside this function), and the seed document
// form of the stats is the harness's own JSON, not SessionStats.
func serializeMetadata(gen GenerationRecord, children GenerationChildren, stats CapturedStats) []byte {
	metadata := schema.UnifiedMetadata{
		SchemaVersion: gen.SchemaVersion,
		SessionID:     gen.SessionID,
		ModelHarness:  gen.Harness,
		Model:         gen.Model,
		Version:       gen.Version,
		Project: schema.ProjectContext{
			Hash: gen.ProjectHash,
			Name: gen.ProjectName,
		},
		HostSlug:    gen.HostSlug,
		ContentHash: gen.ContentHash,
	}
	metadata.Timestamp.Start = gen.TimestampStartMs
	metadata.Timestamp.End = gen.TimestampEndMs
	metadata.Timestamp.Ingested = gen.TimestampIngestedMs
	metadata.Source.Format = gen.SourceFormat
	if gen.SourceFilePath != nil {
		metadata.Source.FilePath = *gen.SourceFilePath
	}
	if gen.GitBranch != nil {
		metadata.Git.Branch = gen.GitBranch
	}
	if gen.GitRemote != nil {
		metadata.Git.Remote = gen.GitRemote
	}
	if gen.GitWorktree != nil {
		metadata.Git.Worktree = gen.GitWorktree
	}
	if gen.GitTracking != nil {
		metadata.Git.Tracking = gen.GitTracking
	}
	if gen.ProjectFilePath != nil {
		metadata.Project.FilePath = *gen.ProjectFilePath
	}
	if gen.RootSessionID != nil {
		metadata.RootSessionID = gen.RootSessionID
	}
	if gen.Purpose != nil {
		metadata.Purpose = *gen.Purpose
	}
	if gen.CWD != nil {
		metadata.CWD = *gen.CWD
	}
	metadata.DerivedAt = gen.DerivedAtMs
	metadata.Redaction.Applied = gen.RedactionApplied
	if gen.RedactionLevel != nil {
		metadata.Redaction.Level = *gen.RedactionLevel
	}
	if gen.RedactionRuleSetVersion != nil {
		metadata.Redaction.RuleSetVersion = *gen.RedactionRuleSetVersion
	}
	metadata.Redaction.RedactedAtMs = gen.RedactionAtMs
	if gen.RedactionContentHashAtRedact != nil {
		metadata.Redaction.ContentHashAtRedact = *gen.RedactionContentHashAtRedact
	}
	metadata.AdapterVersion = gen.AdapterVersion
	metadata.Stats = capturedStatsToSessionStats(stats)
	for _, subagent := range children.Subagents {
		metadata.Subagents = append(metadata.Subagents, schema.SubagentRef{
			SessionID: subagent.SubagentSessionID, ParentUUID: subagent.ParentUUID,
		})
	}
	for _, commit := range children.Commits {
		metadata.Git.Commits = append(metadata.Git.Commits, schema.CommitInfo{
			Hash: commit.Hash, Message: commit.Message,
			AuthorName: commit.AuthorName, AuthorEmail: commit.AuthorEmail,
			CommitTime: commit.CommitTime, AuthorTime: commit.AuthorTime,
		})
	}
	for _, association := range children.Associations {
		metadata.Git.Associations = append(metadata.Git.Associations, schema.PublishedAssociation{
			ID:                 schema.AssociationID(association.AssociationID),
			ObservedCommitHash: association.ObservedCommitHash,
		})
	}
	for _, diagnostic := range children.Diagnostics {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, schema.DiagnosticEntry{
			ErrorType: diagnostic.ErrorType, Location: diagnostic.Location,
			Message: diagnostic.Message, Remediation: diagnostic.Remediation,
		})
	}
	metadata.Diagnostics.Partial = gen.DiagnosticsPartial
	for _, relationship := range children.Relationships {
		restored := schema.SessionRelationship{
			Kind:        relationship.Kind,
			TargetState: relationship.TargetState,
		}
		if relationship.TargetLocalID != nil {
			restored.TargetLocalID = relationship.TargetLocalID
		}
		if relationship.Evidence != nil {
			restored.Evidence = schema.EvidenceKind(*relationship.Evidence)
		}
		if relationship.AnchorKind != nil {
			restored.Anchor = &schema.PublicSourceAnchor{Kind: *relationship.AnchorKind}
			if relationship.AnchorSourceEntryRef != nil {
				restored.Anchor.SourceEntryRef = *relationship.AnchorSourceEntryRef
			}
			if relationship.AnchorSourceRevisionRef != nil {
				restored.Anchor.SourceRevisionRef = schema.PublicRevisionRef(*relationship.AnchorSourceRevisionRef)
			}
		}
		metadata.Relationships = append(metadata.Relationships, restored)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

// capturedStatsToSessionStats maps one stats row to the wire stats shape:
// NULL columns read as the zero value for non-pointer fields (matching the
// NULL-to-wire mapping) and as absent for the optional pointers.
func capturedStatsToSessionStats(stats CapturedStats) schema.SessionStats {
	out := schema.SessionStats{}
	if stats.TurnCount != nil {
		out.TurnCount = *stats.TurnCount
	}
	out.InputSubmissionCount = stats.InputSubmissionCount
	if stats.ToolCallCount != nil {
		out.ToolCallCount = *stats.ToolCallCount
	}
	if stats.SubagentCount != nil {
		out.SubagentCount = *stats.SubagentCount
	}
	if stats.DurationMs != nil {
		out.DurationMs = *stats.DurationMs
	}
	if stats.TokensIn != nil {
		out.TokensIn = *stats.TokensIn
	}
	if stats.TokensOut != nil {
		out.TokensOut = *stats.TokensOut
	}
	out.ThoughtTokens = stats.ThoughtTokens
	out.CachedReadTokens = stats.CachedReadTokens
	out.CachedWriteTokens = stats.CachedWriteTokens
	return out
}
