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
// root transcript is mined for the teammate identity, the spawn records, and the
// display hints. A subagent transcript is only checked for a conversation
// record, so its cached row must never answer a root question.
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
	SourcePath            ResolvedPath
	Scope                 ClaudeEvidenceScope
	ModTimeUnixNano       int64
	SizeBytes             int64
	HasConversationRecord bool
	Identity              *ClaudeTeammateIdentity
	Spawns                []ClaudeTeammateIdentity
	Title                 string
	Branch                string
	CWD                   string
	// Origin is who drove the session, decided by the one rule at mine time from
	// the evidence this record was read for. Every mined record carries a menu
	// value: a root from its content, a subagent because it is a child.
	Origin sessionorigin.Origin
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
// Scope-independent on purpose: EVERY mined record now carries a menu value —
// a root from its content, a subagent because it is classified Agent at mine
// time as the child of a root. If a future field ever needs this same
// treatment, do NOT add a second clause here: two clauses would mean this
// predicate has silently become "the record is complete", which deserves its
// own name. Add a record schema-version column instead, which covers every
// future field at once.
func (e ClaudeTranscriptEvidence) Fresh(scope ClaudeEvidenceScope, info os.FileInfo) bool {
	if info == nil || !scope.IsValid() || e.Scope != scope || e.Origin == "" {
		return false
	}
	return e.SizeBytes == info.Size() && e.ModTimeUnixNano == info.ModTime().UnixNano()
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
