package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// ClaudeTeammateIdentity names one Claude teammate. Claude does not record which
// child session belongs to which parent, so discovery mines the team and agent
// name from the transcripts and pairs the two sides itself.
type ClaudeTeammateIdentity struct {
	Team string
	Name string
}

// ClaudeEvidenceScope says which facts a cached transcript record carries. A
// root transcript is mined for teammate identity, spawns, and display hints.
// Both scopes carry full structural identity proof; the scope still prevents
// reusing a child row as a root after a layout change.
type ClaudeEvidenceScope string

const (
	// ClaudeEvidenceScopeRoot marks a record mined from a root transcript.
	ClaudeEvidenceScopeRoot ClaudeEvidenceScope = "root"
	// ClaudeEvidenceScopeSubagent marks a record mined from a subagent transcript.
	ClaudeEvidenceScopeSubagent ClaudeEvidenceScope = "subagent"
)

// String returns the stored form of the scope.
func (s ClaudeEvidenceScope) String() string { return string(s) }

// IsValid reports whether the scope is one this build knows.
func (s ClaudeEvidenceScope) IsValid() bool {
	switch s {
	case ClaudeEvidenceScopeRoot, ClaudeEvidenceScopeSubagent:
		return true
	default:
		return false
	}
}

// ClaudeTranscriptEvidence is everything Claude discovery derives from the
// CONTENT of one transcript file. Reading that content costs a full file read
// and a JSON parse of every line, so the result is cached against the size and
// the modification time of the file that produced it.
type ClaudeTranscriptEvidence struct {
	ProofVersion          int
	Records               []ClaudeIdentityRecord
	NativeAgentID         string
	NativeAgentValid      bool
	ContentDigest         string
	SourcePath            ResolvedPath
	Scope                 ClaudeEvidenceScope
	ModTimeUnixNano       int64
	SizeBytes             int64
	HasConversationRecord bool
	// ControlRecordSeen marks a parsed record whose kind is a summary,
	// file-history, or recognized control record but which carries no session
	// id. It distinguishes a rotated owner transcript from an empty, blank, or
	// junk file. It is derived at mine time and never persisted.
	ControlRecordSeen bool `json:"-"`
	Identity          *ClaudeTeammateIdentity
	Spawns            []ClaudeTeammateIdentity
	Title             string
	Branch            string
	CWD               string
	// Origin is who drove the session, decided by the one rule at mine time from
	// the evidence this record was read for. Every mined record carries a menu
	// value: a root from its content, a subagent because it is a child.
	Origin sessionorigin.Origin
	// Signal names which rule step decided Origin. It is not persisted to the
	// evidence cache — only carried on the in-memory record produced by a fresh
	// mine — because nothing in production reads it back from storage; its sole
	// consumer is explanatory reporting (the origin audit harness and the
	// fixtures), which always mines fresh rather than reading a cached row.
	Signal sessionorigin.Signal
}

// Fresh reports whether this record still describes the file that info names.
// A record is fresh only when the scope, the size, and the modification time all
// agree, so any edit to a transcript makes discovery mine it again.
//
// A record mined before this build knew about origin carries the empty origin
// (the storage-layer marker described on ClaudeTranscriptEvidence.Origin). It
// is missing a field this build needs, so it is not fresh whatever its size and
// mod time. Re-mining is work discovery already does for a changed file, so the
// upgrade needs no separate pass.
//
// ProofVersion marks the schema of the structural ownership evidence. Legacy
// rows re-mine once even when their file stat is unchanged; future evidence
// additions advance this marker rather than adding field-by-field predicates.
func (e ClaudeTranscriptEvidence) Fresh(scope ClaudeEvidenceScope, info os.FileInfo) bool {
	if info == nil || e.Origin == "" || e.ProofVersion != ClaudeIdentityProofVersion {
		return false
	}
	if !scope.IsValid() || e.Scope != scope {
		return false
	}
	return e.SizeBytes == info.Size() && e.ModTimeUnixNano == info.ModTime().UnixNano()
}

// ClaudeIdentityProofVersion is stored on every cached evidence row. A row is
// fresh only when its version equals this constant exactly, so any build with
// a different version (older or newer) re-mines the row and rewrites it with
// its own version. Unchanged rows at the current version need no transcript
// read. Version 3 re-mines rows whose cached Title came from an isMeta record.
const ClaudeIdentityProofVersion = 3

// ClaudeIdentityRecord retains only native structural coordinates, never message
// text. Repeated session IDs describe the distribution and chain, not ownership.
type ClaudeIdentityRecord struct {
	SessionID    string `json:"sessionId"`
	UUID         string `json:"uuid"`
	ParentUUID   string `json:"parentUuid"`
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	SpawnAgentID string `json:"spawnAgentId,omitempty"`
	AgentID      string `json:"agentId,omitempty"`
	Sidechain    *bool  `json:"isSidechain,omitempty"`
}

// ClaudeEvidenceCache persists mined Claude transcript evidence between
// discoveries. An unchanged transcript is then never read or parsed again.
// Implementations are best effort: a cache that fails simply makes discovery do
// the full work, which is always correct.
type ClaudeEvidenceCache interface {
	// LoadClaudeEvidence returns every cached record, keyed by source path.
	LoadClaudeEvidence(ctx context.Context) (map[ResolvedPath]ClaudeTranscriptEvidence, error)
	// SaveClaudeEvidence writes the records mined during one discovery and
	// deletes the records whose transcripts are gone.
	SaveClaudeEvidence(ctx context.Context, upserts []ClaudeTranscriptEvidence, deletes []ResolvedPath) error
}

// ClaudeEvidenceCaching is implemented by an adapter that can reuse mined
// transcript evidence. The adapter factory signature carries only the
// filesystem, the git resolver, and the salt, so a caller attaches the cache
// after it builds the adapter.
type ClaudeEvidenceCaching interface {
	SetClaudeEvidenceCache(cache ClaudeEvidenceCache)
}

// AttachClaudeEvidenceCache gives adapter the evidence cache when the adapter
// can use one. Adapters that mine nothing ignore the cache.
func AttachClaudeEvidenceCache(adapter SourceAdapter, cache ClaudeEvidenceCache) {
	if adapter == nil || cache == nil {
		return
	}
	if caching, ok := adapter.(ClaudeEvidenceCaching); ok {
		caching.SetClaudeEvidenceCache(cache)
	}
}

// pathUnderAnyRoot reports whether path sits inside one of the roots. Discovery
// prunes only the cached records it is responsible for, so a record left by a
// source path that this run did not walk stays in the cache.
func pathUnderAnyRoot(path ResolvedPath, roots []ResolvedPath) bool {
	candidate := filepath.Clean(path.String())
	for _, root := range roots {
		cleaned := filepath.Clean(root.String())
		if cleaned == "." || cleaned == "" {
			continue
		}
		prefix := cleaned
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}
