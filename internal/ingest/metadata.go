package ingest

import (
	"fmt"

	"github.com/peasant-labs/schema"
)

// CurrentSchemaVersion is the schema version written by this build of the ingest tool.
const CurrentSchemaVersion = schema.MetadataSchemaVersion

// metadataSchemaVersion is a recorded metadata schema version: the number a
// stored sidecar carries, or the one this build writes. It is a distinct type
// so a version cannot be confused with any other count at the rule below, and
// so an absent or impossible number is rejected where it enters rather than
// quietly comparing as smaller than everything.
type metadataSchemaVersion int

// newMetadataSchemaVersion admits a recorded version. Zero is the absent
// version a sidecar written before versioning carries, and it is admitted: it
// is genuinely older than every named version and must refresh. A negative
// number is not a version any build ever wrote.
func newMetadataSchemaVersion(value int) (metadataSchemaVersion, error) {
	if value < 0 {
		return 0, fmt.Errorf("read metadata schema version %d: a recorded version is never negative; no session was refreshed or re-harvested; restore valid version metadata in the managed sidecar and retry harvest", value)
	}
	return metadataSchemaVersion(value), nil
}

// refreshFreeMetadataVersions is the statically defined set of recorded
// metadata schema versions whose difference from the version this build writes
// is OPTIONAL FIELDS ONLY. A sidecar at one of these versions is readable as
// it stands, so re-harvesting it would cost the user a full pass over the
// corpus and change nothing they can see.
//
// The set is declared, never derived from a comparison with the current
// version: writing the rule as "version 9 while the current version is 10"
// silently stops matching at the next re-pin, and every stored sidecar at 9 AND
// 10 would then be marked stale by one contract bump that added nothing
// required. Version 11 is listed for exactly that reason: it is the next
// expected current version and it adds no required field, so the re-pin is a
// one-line change here rather than a corpus-wide re-harvest.
//
// Adding a version here is a claim that nothing REQUIRED was added between it
// and the current version. A version that adds a required field must be left
// out, and every older sidecar then refreshes as usual.
var refreshFreeMetadataVersions = map[metadataSchemaVersion]bool{9: true, 10: true, 11: true}

// metadataNeedsRefresh reports whether a sidecar recorded at version must be
// rebuilt before this build can rely on it. current is a parameter, not the
// package constant, so the rule can be stated and tested against a version
// this build does not yet write without any global being changed.
func metadataNeedsRefresh(version, current metadataSchemaVersion) bool {
	if refreshFreeMetadataVersions[version] {
		return false
	}
	return version < current
}

// MetadataSchemaVersionIsCurrent reports whether a recorded metadata schema
// version is one this build reads as it stands: at or below the version this
// build writes, and not requiring a rebuild. It is the one place the readable
// set is stated, so a store-side predicate that compares a stored version
// against this build's current version shares the refresh-free policy instead
// of re-stating an exact equality. A version above the current one is not
// current: only a build that writes it can read it.
func MetadataSchemaVersionIsCurrent(version int) bool {
	if version < 1 || version > int(CurrentSchemaVersion) {
		return false
	}
	recorded, err := newMetadataSchemaVersion(version)
	return err == nil && !metadataNeedsRefresh(recorded, CurrentSchemaVersion)
}

// ReadableMetadataSchemaVersions returns every recorded metadata schema version
// this build reads as current, in ascending order. It is the complement, within
// the versions this build writes, of the versions that require a native
// refresh. A store-side predicate binds this set instead of comparing a stored
// version to the current version exactly, so a declared refresh-free schema
// bump does not make an otherwise current capture read as needing a re-ingest.
func ReadableMetadataSchemaVersions() []int {
	versions := make([]int, 0, int(CurrentSchemaVersion))
	for version := 1; version <= int(CurrentSchemaVersion); version++ {
		if MetadataSchemaVersionIsCurrent(version) {
			versions = append(versions, version)
		}
	}
	return versions
}

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
