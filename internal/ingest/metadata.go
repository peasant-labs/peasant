package ingest

import "github.com/peasant-labs/schema"

// CurrentSchemaVersion is the schema version written by this build of the ingest tool.
const CurrentSchemaVersion = schema.MetadataSchemaVersion

// UnifiedMetadata is the on-disk JSON stored alongside each raw transcript.
type UnifiedMetadata = schema.UnifiedMetadata

type TimestampInfo = schema.TimestampInfo
type SourceInfo = schema.SourceInfo
type GitContext = schema.GitContext
type ProjectInfo = schema.ProjectContext
type StatsInfo = schema.SessionStats
type SubagentRef = schema.SubagentRef
type DiagnosticsInfo = schema.DiagnosticsInfo
type DiagnosticEntry = schema.DiagnosticEntry
type SourceFormat = schema.SourceFormat
type CommitInfo = schema.CommitInfo
type RedactionInfo = schema.RedactionInfo

const (
	SourceFormatJSONL = schema.SourceFormatJSONL
	SourceFormatJSON  = schema.SourceFormatJSON
)

// NewUnifiedMetadata creates a UnifiedMetadata with SchemaVersion set to CurrentSchemaVersion.
func NewUnifiedMetadata() UnifiedMetadata {
	return schema.NewUnifiedMetadata()
}
