package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/redact"
)

// DiscoveryCodeClaudeAgentIDCollision is the DiscoveryDiagnostic code for a
// Claude subagent left out because another session's subagent has its id.
const DiscoveryCodeClaudeAgentIDCollision = "claude_agent_id_collision"

// ClaudeAdapter discovers and extracts metadata from Claude Code JSONL transcripts.
type ClaudeAdapter struct {
	diagnostics         []DiscoveryDiagnostic
	fs                  FileSystem
	git                 GitResolver
	salt                salt.Salt
	evidence            ClaudeEvidenceCache
	locationLookup      SessionLocationLookup
	logicalParentLookup LogicalParentReader
	// derivedLookup is the store's indexed identity reader. A scoped build uses
	// it to decide a teammate link without reading the whole evidence table.
	derivedLookup DerivedLookupReader
	// reminedCount is how many evidence records the most recent Discover had
	// to mine again. It describes that one call and is overwritten by the next.
	reminedCount int
}

var _ SourceAdapter = (*ClaudeAdapter)(nil)
var _ SessionLocator = (*ClaudeAdapter)(nil)
var _ ClaudeEvidenceCaching = (*ClaudeAdapter)(nil)
var _ ClaudeSessionLocationLookupCapable = (*ClaudeAdapter)(nil)
var _ ClaudeLogicalParentLookupCapable = (*ClaudeAdapter)(nil)
var _ DiscoveryStatistics = (*ClaudeAdapter)(nil)
var _ OriginEvidenceMiner = (*ClaudeAdapter)(nil)

// NewClaudeAdapter creates a ClaudeAdapter with injected dependencies.
func NewClaudeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *ClaudeAdapter {
	return &ClaudeAdapter{fs: fs, git: git, salt: s}
}

// SetClaudeEvidenceCache makes discovery reuse the evidence it mined during an
// earlier run. Without a cache the adapter mines every transcript again.
func (a *ClaudeAdapter) SetClaudeEvidenceCache(cache ClaudeEvidenceCache) {
	a.evidence = cache
}

// SetSessionLocationLookup gives the adapter a way to confirm whether a
// candidate cross-run spawner is already persisted, so re-parenting can trust
// data outside this run's own write batch. Without a lookup, cross-run linking
// never fires (see claudeSessionAlreadyStored).
func (a *ClaudeAdapter) SetSessionLocationLookup(lookup SessionLocationLookup) {
	a.locationLookup = lookup
}

// SetLogicalParentLookup gives the adapter the evidence-first accessor it walks
// an ancestor chain with, so the cycle guard follows a stored session's durable
// parent even while its availability cache is NULL. Without a lookup the walk
// stops at this run's own candidate edges (see claudeStoredParent).
func (a *ClaudeAdapter) SetLogicalParentLookup(lookup LogicalParentReader) {
	a.logicalParentLookup = lookup
}

// SetClaudeDerivedLookup gives a scoped Claude build the store's indexed
// identity reader. The adapter factory signature carries only the filesystem,
// the git resolver, and the salt, so a caller attaches this capability after it
// builds the adapter, the same optional shape the evidence cache uses.
func (a *ClaudeAdapter) SetClaudeDerivedLookup(lookup DerivedLookupReader) {
	a.derivedLookup = lookup
}

// ClaudeDerivedLookupCapable is implemented by an adapter that can decide a
// teammate link from the store's indexed identity counts.
type ClaudeDerivedLookupCapable interface {
	SetClaudeDerivedLookup(lookup DerivedLookupReader)
}

// AttachClaudeDerivedLookup gives adapter the indexed identity reader when the
// adapter can use one. Adapters that decide nothing from it ignore the reader.
func AttachClaudeDerivedLookup(adapter SourceAdapter, lookup DerivedLookupReader) {
	if adapter == nil || lookup == nil {
		return
	}
	if capable, ok := adapter.(ClaudeDerivedLookupCapable); ok {
		capable.SetClaudeDerivedLookup(lookup)
	}
}

func (a *ClaudeAdapter) Harness() Harness {
	return HarnessClaudeCode
}

// claudeJSONLLine is the raw shape of each line in a Claude Code JSONL transcript.
// Fields are parsed only as-needed; unmapped fields are silently ignored.
type claudeJSONLLine struct {
	SessionID string           `json:"sessionId"`
	Version   string           `json:"version"`
	Type      claudeRecordType `json:"type"`
	CWD       string           `json:"cwd"`
	GitBranch string           `json:"gitBranch"`
	// IsMeta is Claude Code's marker for a user-role record the harness
	// injected. The indexer reclassifies it as system (claudeIndexLine.IsMeta),
	// so discovery must not take a title from it either.
	IsMeta    bool   `json:"isMeta"`
	AgentID   string `json:"agentId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	// Effort is the assistant line's top-level resolved effort. Attachment
	// carries the nested attachment of an attachment record (the ultracode
	// mode enter/exit evidence). ThinkingMetadata is the legacy per-line
	// thinking configuration. All three are decoded tolerantly so a
	// malformed value never drops the line.
	Effort           json.RawMessage `json:"effort"`
	Attachment       json.RawMessage `json:"attachment"`
	ThinkingMetadata json.RawMessage `json:"thinkingMetadata"`
	Message          struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"` // string or []contentBlock
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

// claudeRecordType names the record kinds discovery reads from a transcript.
type claudeRecordType string

// String returns the wire form of the record type.
func (t claudeRecordType) String() string { return string(t) }

// isConversation reports whether the record type is one a reader can render as
// part of the conversation.
func (t claudeRecordType) isConversation() bool {
	return t == claudeRecordTypeUser || t == claudeRecordTypeAssistant
}

const (
	// claudeRecordTypeUser marks a user record.
	claudeRecordTypeUser claudeRecordType = "user"
	// claudeRecordTypeAssistant marks an assistant record.
	claudeRecordTypeAssistant claudeRecordType = "assistant"
)

// claudeContentBlockText names the content block that carries prose. A user
// record writes its prompt either as a bare string or as blocks of this type.
const claudeContentBlockText = "text"

// claudeHintLineLimit caps how many leading lines discovery reads for the
// display hints, so a large transcript never costs a full hint scan.
const claudeHintLineLimit = 10

// contentBlock is a typed block inside an assistant message content array.
type contentBlock struct {
	Type string `json:"type"`
}

type claudeFileOpener interface {
	Open(path string) (io.ReadCloser, error)
}

// Discover walks each configured source path looking for *.jsonl files.
// It identifies root sessions (UUID.jsonl at the project slug level) and links
// the subagent sessions under {uuid}/subagents/, both agent-*.jsonl directly
// there and agent-*.jsonl inside a workflow run, {uuid}/subagents/workflows/wf_*/.
//
// The walk only names files. Every root and subagent it names is handed to the
// same build-and-link function DiscoverSessionAt uses, with the full-walk mode,
// so a full discovery and a located source cannot describe one session two
// ways. That build keeps the two-pass order: roots first, then subagents, so
// parent sessions are registered before a subagent is linked to them.
//
// One session id holds one transcript, so under one source path the first
// transcript admitted for an agent id keeps it and a later one is left out and
// logged at debug level. Workflow transcripts are admitted before plain ones, in lexical path
// order: Claude Code has been seen to write the last turns of a workflow agent
// to the plain location under the same id after its run transcript, and
// admitting the run transcript first keeps the fuller transcript and makes the
// same choice on every harvest.
//
// Per RFC Section 6.4, symlinks are skipped silently.
func (a *ClaudeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	a.diagnostics = nil
	enumerated := make([]claudeEnumeratedRoot, 0, len(cfg.Paths))
	for _, basePath := range cfg.Paths {
		root, err := a.enumerateClaudeRoot(ctx, basePath)
		if err != nil {
			return nil, err
		}
		enumerated = append(enumerated, root)
	}
	sessions, _, err := a.buildClaudeSessions(ctx, cfg, enumerated, nil, nil, ClaudeBuildFullWalk)
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// claudeRootEntry is one root transcript the walk or a located source named,
// before the shared build reads it. info is a stat a caller already observed,
// so a located source never stats one path twice.
type claudeRootEntry struct {
	path      string
	sessionID string
	info      os.FileInfo
}

// claudeSubagentEntry is one subagent transcript (directly under a session's
// subagents directory, or inside one workflow run) a walk or a located source
// named.
type claudeSubagentEntry struct {
	path       string
	subagentID SessionID
	parentID   SessionID
	info       os.FileInfo
}

// claudeEnumeratedRoot is one configured source root's named transcripts. The
// walk fills it by walking; a located source fills it by name only.
type claudeEnumeratedRoot struct {
	root      ResolvedPath
	roots     []claudeRootEntry
	workflows []claudeSubagentEntry
	subagents []claudeSubagentEntry
}

// paths names every transcript this root's enumeration will build, so a scoped
// build reads the whole cache for exactly those paths and no others.
func (r claudeEnumeratedRoot) paths() []ResolvedPath {
	paths := make([]ResolvedPath, 0, len(r.roots)+len(r.workflows)+len(r.subagents))
	for _, entry := range r.roots {
		paths = append(paths, ResolvedPath(entry.path))
	}
	for _, entry := range r.workflows {
		paths = append(paths, ResolvedPath(entry.path))
	}
	for _, entry := range r.subagents {
		paths = append(paths, ResolvedPath(entry.path))
	}
	return paths
}

// enumerateClaudeRoot walks one configured root and returns the root and
// subagent transcripts it names. It opens no transcript: every named file is
// read once, by the shared build.
func (a *ClaudeAdapter) enumerateClaudeRoot(ctx context.Context, basePath ResolvedPath) (claudeEnumeratedRoot, error) {
	enumerated := claudeEnumeratedRoot{root: basePath}
	root := string(basePath)

	walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil {
			// Keep walking, but name a directory permissions hid so the
			// run does not just report fewer sessions.
			a.noteUnreadable(path, err)
			return nil
		}

		// Skip symlinks per RFC Section 6.4.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Only process .jsonl files.
		if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
			return nil
		}

		// Determine the session type by relative path structure.
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(rel, string(filepath.Separator))

		// Root session: {project-slug}/{uuid}.jsonl (depth 2)
		if len(parts) == 2 {
			sessionIDStr := strings.TrimSuffix(parts[1], defaults.ExtJSONL.String())
			if _, err := NewSessionID(sessionIDStr); err == nil {
				enumerated.roots = append(enumerated.roots, claudeRootEntry{path: path, sessionID: sessionIDStr})
			}
			return nil
		}

		// Subagent session, directly in the parent's subagents directory or
		// inside one of its workflow runs.
		if parentUUIDStr, file, workflow, ok := claudeSubagentLocation(parts); ok && strings.HasPrefix(file, defaults.ClaudeSubagentPrefix) {
			parentID, parentErr := NewSessionID(parentUUIDStr)
			subagentID, subagentErr := NewSessionID(strings.TrimSuffix(file, defaults.ExtJSONL.String()))
			if parentErr == nil && subagentErr == nil {
				entry := claudeSubagentEntry{path: path, subagentID: subagentID, parentID: parentID}
				if workflow {
					enumerated.workflows = append(enumerated.workflows, entry)
				} else {
					enumerated.subagents = append(enumerated.subagents, entry)
				}
			}
			return nil
		}

		return nil
	})

	if walkErr != nil {
		return claudeEnumeratedRoot{}, fmt.Errorf("Discover: walk %q: %w", root, walkErr)
	}
	return enumerated, nil
}

// ReminedCount reports how many cached evidence records the most recent Discover
// had to mine again. See DiscoveryStatistics for the scoping rule.
func (a *ClaudeAdapter) ReminedCount() int { return a.reminedCount }

// claudeSubagentLocation splits a root-relative Claude path into the parent
// session directory and the file name of the subagent transcript it holds. A
// subagent transcript sits in its parent's subagents directory, either directly
// or inside one workflow run directory:
//
//	{project-slug}/{uuid}/subagents/agent-{hex}.jsonl
//	{project-slug}/{uuid}/subagents/workflows/{run}/agent-{hex}.jsonl
//
// {run} is a directory whose name starts with wf_; the whole name is the run id.
// workflow reports the second layout. Any other path is not a subagent path.
func claudeSubagentLocation(parts []string) (parent, file string, workflow, ok bool) {
	if len(parts) < 4 || parts[2] != defaults.DirSubagents.String() {
		return "", "", false, false
	}
	switch {
	case len(parts) == 4:
		return parts[1], parts[3], false, true
	case len(parts) == 6 && parts[3] == defaults.ClaudeDirWorkflows.String() && isClaudeWorkflowRun(parts[4]):
		return parts[1], parts[5], true, true
	default:
		return "", "", false, false
	}
}

// isClaudeWorkflowRun reports whether a directory name is a workflow run id.
func isClaudeWorkflowRun(name string) bool {
	return len(name) > len(defaults.ClaudeWorkflowRunPrefix) && strings.HasPrefix(name, defaults.ClaudeWorkflowRunPrefix)
}

// MineOriginEvidence re-reads one Claude transcript that is still on disk and
// returns the origin its content decides, for the stored-row resolve pass.
//
// It is the ordinary root miner, not a second one: the same read, the same
// captures, and the same rule. A transcript that cannot be stat-ed or cannot be
// read reports false, which is the resolver's degraded case, because the same
// file may be readable on a later run.
//
// A child transcript never reaches here. The resolver decides a row that already
// carries a parent at rule step one, with no file read at all.
func (a *ClaudeAdapter) MineOriginEvidence(path ResolvedPath) (sessionorigin.Origin, bool) {
	info, err := a.fs.Stat(path.String())
	if err != nil {
		return "", false
	}
	evidence, readable := a.mineClaudeRootTranscript(path, info)
	if !readable {
		return "", false
	}
	return evidence.Origin, true
}

// loadCachedEvidence returns the evidence an earlier discovery mined and
// whether the load returned every stored row. A row this build cannot parse is
// skipped by the loader, so a successful load can be a strict subset of the
// store; the second result says whether the caller may take its identity counts
// from the store without adding the rows this map already covers. A missing or
// failing cache returns nil and false, so discovery mines everything again.
func (a *ClaudeAdapter) loadCachedEvidence(ctx context.Context) (map[ResolvedPath]ClaudeTranscriptEvidence, bool) {
	if a.evidence == nil {
		return nil, false
	}
	if completeness, ok := a.evidence.(ClaudeEvidenceCompleteness); ok {
		records, complete, err := completeness.LoadClaudeEvidenceCompletely(ctx)
		if err != nil {
			return nil, false
		}
		return records, complete
	}
	records, err := a.evidence.LoadClaudeEvidence(ctx)
	if err != nil {
		return nil, false
	}
	// A cache without the capability returns every record it holds, which is
	// the complete view by construction (the in-memory doubles).
	return records, true
}

// DiscoveryCodeClaudeEvidenceCacheWrite is the DiscoveryDiagnostic code for a
// full walk whose mined transcript evidence could not be written to the cache.
const DiscoveryCodeClaudeEvidenceCacheWrite = "claude_evidence_cache_write"

// saveMinedEvidence writes the records this run mined and removes the records
// whose transcripts are gone. Only the walked source paths are pruned, so a
// record left by a path this run did not visit stays in the cache.
//
// A failed write is reported, never swallowed: the caller records a diagnostic
// and the next discovery mines the transcripts again. No cache write is a
// correctness requirement — the cache only saves work — so the run continues.
func (a *ClaudeAdapter) saveMinedEvidence(
	ctx context.Context,
	cfg SourceConfig,
	cached map[ResolvedPath]ClaudeTranscriptEvidence,
	mined map[ResolvedPath]ClaudeTranscriptEvidence,
	remined []ClaudeTranscriptEvidence,
) error {
	if a.evidence == nil {
		return nil
	}
	var deletes []ResolvedPath
	for path := range cached {
		if _, seen := mined[path]; seen {
			continue
		}
		if pathUnderAnyRoot(path, cfg.Paths) {
			deletes = append(deletes, path)
		}
	}
	if len(remined) == 0 && len(deletes) == 0 {
		return nil
	}
	return a.evidence.SaveClaudeEvidence(ctx, remined, deletes)
}

// noteEvidenceCacheFailure records the full walk's cache-write failure on the
// run's diagnostics. The transcript evidence was still read this run, so the
// sessions are built normally; the next discovery simply mines the same
// transcripts again because nothing was cached.
func (a *ClaudeAdapter) noteEvidenceCacheFailure(err error) {
	a.diagnostics = append(a.diagnostics, DiscoveryDiagnostic{
		Provider: HarnessClaudeCode,
		Code:     DiscoveryCodeClaudeEvidenceCacheWrite,
		Summary:  "mined transcript evidence could not be cached",
		Detail:   fmt.Sprintf("the evidence cache write failed: %s; the sessions discovered this run are unaffected, and the next discovery mines the same transcripts again", firstLine(err.Error())),
	})
}

// claudeDeferredEvidence is the scoped build's evidence write. A scoped
// discovery writes nothing itself: it returns the mined and refreshed records
// here, and the core applies them only after every selected id is located, so a
// run refused for a miss leaves the cache untouched. Apply returns the cache's
// error instead of swallowing it, so the run fails before any session write.
type claudeDeferredEvidence struct {
	cache   ClaudeEvidenceCache
	upserts []ClaudeTranscriptEvidence
	deletes []ResolvedPath
}

var _ DeferredWrites = claudeDeferredEvidence{}

// Apply writes the deferred evidence records and reports a failed write.
func (e claudeDeferredEvidence) Apply(ctx context.Context) error {
	if e.cache == nil || len(e.upserts) == 0 && len(e.deletes) == 0 {
		return nil
	}
	if err := e.cache.SaveClaudeEvidence(ctx, e.upserts, e.deletes); err != nil {
		return fmt.Errorf("ingest: the located Claude build could not write its transcript evidence; the run stops before any session is written because a later run would otherwise decide teammate links from stale evidence; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: applying the scoped run's deferred discovery writes; impact: no session, index or audit row was written; fix: restore the evidence cache and retry: %w", err)
	}
	return nil
}

// linkClaudeTeammates links Claude root transcripts that declare a teammate
// identity to the one root that spawned it. The ambiguity rule is the same for
// a full walk and a located build: a link is created only when exactly one
// root path declares the identity and exactly one spawner path records
// spawning it, a child is never its own parent, and a pair that would close a
// cycle is refused.
//
// The counts come from the store's two indexed reads over the derived lookup
// tables — the partial identity index and claude_teammate_spawns — not from an
// in-memory pass over the whole evidence table. A build whose evidence view is
// only its own records (a store-free run, a dry run) answers the same two
// questions from that view through claudeEvidenceIdentityReader, so the rule
// itself still lives here once.
//
// `current` is the evidence every path this build touched carries now.
// `storedCounts` says whether the counts are the store's: the store describes
// the rows the last discovery saved, so a re-mined row replaces its stored
// contribution and a new row adds one, which claudeIdentityDeltas turns into
// the per-identity adjustment. When the counts come from the build's own view,
// that view already includes the adjustment and the delta is zero.
//
// A spawner found only in already-persisted evidence is pointed at ONLY when
// its session is stored: a parent identifier must never point at a session
// that is neither in this write batch nor already persisted, because the
// store's own FK-orphan guard would otherwise silently drop the child at write
// time, which is worse than leaving it parentless. The extension is
// one-directional — a later child may find an earlier spawner, never the
// reverse — and never creates a cycle: any assignment that would place a
// session among its own ancestors is refused rather than guessed.
func (a *ClaudeAdapter) linkClaudeTeammates(
	ctx context.Context,
	sessions []DiscoveredSession,
	cached, current map[ResolvedPath]ClaudeTranscriptEvidence,
	located *LocatedSource,
	storedCounts bool,
) error {
	evidence := mergeClaudeEvidence(cached, current)

	var reader DerivedLookupReader
	delta := claudeIdentityCountDelta{}
	if storedCounts {
		if a.derivedLookup == nil {
			return fmt.Errorf("ingest: the Claude build was asked to decide teammate links from the stored identity counts, but no indexed identity reader is attached; reading the whole evidence table instead is the pass this rewrite removes, and guessing would re-home a session under the wrong parent; where: ingest.ClaudeAdapter.linkClaudeTeammates in internal/ingest/claude.go; when: building discovered sessions; impact: no session was built and nothing was written; fix: attach the store's derived lookup reader to the adapter before the run (ingest.AttachClaudeDerivedLookup)")
		}
		reader = a.derivedLookup
		delta = claudeIdentityDeltas(cached, current)
	} else {
		reader = newClaudeEvidenceIdentityReader(evidence)
	}

	rootByID := make(map[SessionID]int, len(sessions))
	spawnersInBatch := make(map[ClaudeTeammateIdentity][]SessionID)
	for i := range sessions {
		if sessions[i].ParentUUID == nil {
			rootByID[sessions[i].SessionID] = i
		}
		record := evidence[sessions[i].SourcePath]
		if record.Scope != ClaudeEvidenceScopeRoot {
			continue
		}
		// One transcript's repeated spawn of the same identity is still one
		// spawner, exactly as the store's projection makes it.
		seen := make(map[ClaudeTeammateIdentity]struct{}, len(record.Spawns))
		for _, spawn := range record.Spawns {
			if _, duplicate := seen[spawn]; duplicate {
				continue
			}
			seen[spawn] = struct{}{}
			// Two is enough for the caller's zero/one/many decision.
			if len(spawnersInBatch[spawn]) < 2 {
				spawnersInBatch[spawn] = append(spawnersInBatch[spawn], sessions[i].SessionID)
			}
		}
	}

	candidates := make(map[SessionID]SessionID, len(sessions))
	for i := range sessions {
		session := sessions[i]
		if session.ParentUUID != nil {
			continue
		}
		record := evidence[session.SourcePath]
		if record.Scope != ClaudeEvidenceScopeRoot || record.Identity == nil {
			continue
		}
		identity := *record.Identity

		declarers, err := reader.CountClaudeIdentityDeclarers(ctx, identity)
		if err != nil {
			return fmt.Errorf("ingest: the Claude build could not count the root transcripts that declare teammate identity (%q, %q); without that count it cannot tell a unique identity from an ambiguous one, and guessing would re-home a session under the wrong parent; where: ingest.ClaudeAdapter.linkClaudeTeammates in internal/ingest/claude.go; when: deciding a teammate link for %q; impact: no session was built and nothing was written; fix: restore database access and retry: %w", identity.Team, identity.Name, session.SourcePath, err)
		}
		spawners, err := reader.CountClaudeIdentitySpawners(ctx, identity)
		if err != nil {
			return fmt.Errorf("ingest: the Claude build could not count the root transcripts that spawn teammate identity (%q, %q); without that count it cannot tell a unique spawner from an ambiguous one, and guessing would re-home a session under the wrong parent; where: ingest.ClaudeAdapter.linkClaudeTeammates in internal/ingest/claude.go; when: deciding a teammate link for %q; impact: no session was built and nothing was written; fix: restore database access and retry: %w", identity.Team, identity.Name, session.SourcePath, err)
		}
		declarers += delta.declarer(identity)
		spawners += delta.spawner(identity)
		if declarers != 1 || spawners != 1 {
			continue
		}

		parentID, haveParent, err := a.claudeTeammateParent(ctx, spawnersInBatch, evidence, reader, identity, session, located)
		if err != nil {
			return err
		}
		if !haveParent || parentID == session.SessionID {
			continue
		}
		candidates[session.SessionID] = parentID
	}

	cyclic := a.claudeCyclicChildren(ctx, candidates)
	for childID, parentID := range candidates {
		if cyclic[childID] {
			continue
		}
		childIdx, ok := rootByID[childID]
		if !ok {
			continue
		}
		parent := parentID
		sessions[childIdx].ParentUUID = &parent
		if parentIdx, inBatch := rootByID[parentID]; inBatch {
			sessions[parentIdx].SubagentPaths = append(sessions[parentIdx].SubagentPaths, sessions[childIdx].SourcePath)
		}
	}
	return nil
}

// claudeTeammateParent names the one spawner of an identity whose uniqueness
// the two counts already proved. The cheapest place that can name it wins: a
// located session's stored logical parent, then a spawner inside this build's
// own batch, then the single stored spawner path through the indexed read. A
// read that returns more than one usable path is refused rather than resolved
// to whichever path sorted first.
func (a *ClaudeAdapter) claudeTeammateParent(
	ctx context.Context,
	spawnersInBatch map[ClaudeTeammateIdentity][]SessionID,
	evidence map[ResolvedPath]ClaudeTranscriptEvidence,
	reader DerivedLookupReader,
	identity ClaudeTeammateIdentity,
	child DiscoveredSession,
	located *LocatedSource,
) (SessionID, bool, error) {
	if located != nil && child.SessionID == located.ID && located.Stored.Parent != nil {
		return *located.Stored.Parent, true, nil
	}

	inBatch := spawnersInBatch[identity]
	if len(inBatch) > 1 {
		return "", false, nil // two spawners: the identity is ambiguous
	}
	if len(inBatch) == 1 {
		return inBatch[0], true, nil
	}

	paths, err := reader.ClaudeIdentitySpawnerPaths(ctx, identity.Team, identity.Name)
	if err != nil {
		return "", false, fmt.Errorf("ingest: the Claude build could not read the stored spawner paths for teammate identity (%q, %q); without them it cannot name the spawner a full run would link to, and guessing would re-home %s under the wrong parent; where: ingest.ClaudeAdapter.claudeTeammateParent in internal/ingest/claude.go; when: deciding a teammate link for %q; impact: no session was built and nothing was written; fix: restore database access and retry: %w", identity.Team, identity.Name, child.SessionID, child.SourcePath, err)
	}
	// A stored path this build re-mined to no longer spawn is not a spawner any
	// more, even though its old row is still in the store until this run's
	// evidence write. The counts already exclude it, so the path list must too.
	usable := make([]string, 0, len(paths))
	for _, raw := range paths {
		path, pathErr := NewResolvedPath(raw)
		if pathErr != nil {
			return "", false, fmt.Errorf("ingest: the stored spawner path %q for teammate identity (%q, %q) is not a resolvable path; the link step cannot point %s at a source it cannot name; where: ingest.ClaudeAdapter.claudeTeammateParent in internal/ingest/claude.go; when: deciding a teammate link for %q; impact: no session was built and nothing was written; fix: repair the stored evidence path or re-run a full discovery: %w", raw, identity.Team, identity.Name, child.SessionID, child.SourcePath, pathErr)
		}
		if record, isCurrent := evidence[path]; isCurrent && !claudeSpawnsIdentity(record, identity) {
			continue
		}
		usable = append(usable, raw)
	}
	if len(usable) != 1 {
		return "", false, nil
	}
	spawnerPath, pathErr := NewResolvedPath(usable[0])
	if pathErr != nil {
		return "", false, fmt.Errorf("ingest: the stored spawner path %q for teammate identity (%q, %q) is not a resolvable path; the link step cannot point %s at a source it cannot name; where: ingest.ClaudeAdapter.claudeTeammateParent in internal/ingest/claude.go; when: deciding a teammate link for %q; impact: no session was built and nothing was written; fix: repair the stored evidence path or re-run a full discovery: %w", usable[0], identity.Team, identity.Name, child.SessionID, child.SourcePath, pathErr)
	}
	spawnID, ok := claudeSessionIDFromRootPath(spawnerPath)
	if !ok || spawnID == child.SessionID {
		return "", false, nil
	}
	// The evidence cache keeps a transcript for every mined root, so a spawner
	// path can name a session that has no stored row: a session outside the
	// run's filters, or one pruned while its file survived. A full run never
	// points at such a parent, and a located build must make the same decision.
	if !a.claudeSessionAlreadyStored(ctx, spawnID) {
		return "", false, nil
	}
	return spawnID, true, nil
}

// mineClaudeRootTranscript reads one root transcript ONCE and derives every
// fact discovery takes from its content: whether it holds a conversation, the
// teammate identity it declares, the teammates it spawned, and the display
// hints. One read replaces the three separate passes over the same bytes.
//
// The second result reports whether the record may be cached. An unreadable
// file produces no durable fact, so it is mined again on the next discovery.
func (a *ClaudeAdapter) mineClaudeRootTranscript(path ResolvedPath, info os.FileInfo) (ClaudeTranscriptEvidence, bool) {
	evidence := newClaudeEvidence(path, ClaudeEvidenceScopeRoot, info)

	data, err := a.fs.ReadFile(path.String())
	if err != nil {
		// An unreadable file fails open, exactly as the conversation check does,
		// so discovery can surface it instead of discarding it silently. It
		// carries no evidence, so the rule declares it unknown, which is the
		// visible answer.
		evidence.HasConversationRecord = true
		evidence.Origin, evidence.Signal = sessionorigin.Classify(sessionorigin.Evidence{})
		return evidence, false
	}
	beginClaudeIdentity(data, &evidence)

	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)

	var identity *ClaudeTeammateIdentity
	var invalidIdentity, malformed, conversation bool
	var validRecords, lineNumber int
	spawnCalls := make(map[string]bool)
	var hints claudeSessionHints
	// The origin captures ride this same scan. Like the identity capture above
	// them they read every line, because a harness is free to record the launch
	// fields on any record; the hint limit below bounds the display hints alone
	// and has never bounded identity.
	var entrypoint, promptSource, firstUserText string
	var haveFirstUserText bool

	for scanner.Scan() {
		raw := scanner.Bytes()
		if lineNumber < claudeHintLineLimit && !hints.complete() {
			applyClaudeHints(&hints, raw)
		}
		lineNumber++

		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			// A record this build cannot parse fails open.
			malformed = true
			evidence.NativeAgentValid = false
			continue
		}
		addClaudeIdentityRecord(value, &evidence, spawnCalls)
		validRecords++
		if recordType, present := value["type"]; present {
			text, isText := recordType.(string)
			switch {
			case !isText:
				malformed = true
			case claudeRecordType(text).isConversation():
				conversation = true
			}
		}

		_, hasTeam := value["teamName"]
		_, hasName := value["agentName"]
		if hasTeam || hasName {
			team, teamOK := value["teamName"].(string)
			name, nameOK := value["agentName"].(string)
			if !teamOK || !nameOK || team == "" || name == "" {
				invalidIdentity = true
			} else {
				candidate := ClaudeTeammateIdentity{Team: team, Name: name}
				if identity == nil {
					identity = &candidate
				} else if *identity != candidate {
					invalidIdentity = true
				}
			}
		}
		if entrypoint == "" {
			entrypoint, _ = value["entrypoint"].(string)
		}
		if promptSource == "" {
			promptSource, _ = value["promptSource"].(string)
		}
		if !haveFirstUserText {
			// The first REAL user record decides, not the first user record.
			// Claude Code opens a locally run slash command with a caveat
			// record, and that record would otherwise become the transcript's
			// first user text and carry no signal at all. Leading scaffolding
			// is read past; the first record that is not scaffolding stops the
			// search, so nothing after it is skipped.
			if text, ok := claudeUserRecordText(value); ok && !skipClaudeInjectedOnlyUserRecord(text) {
				firstUserText = text
				haveFirstUserText = true
			}
		}
		if spawn, ok := claudeTeammateSpawn(value); ok {
			evidence.Spawns = append(evidence.Spawns, spawn)
		}
	}

	if !invalidIdentity {
		evidence.Identity = identity
	}
	complete := scanner.Err() == nil && len(scanner.Oversized()) == 0
	evidence.NativeAgentValid = evidence.NativeAgentValid && evidence.NativeAgentID != "" && complete
	evidence.HasConversationRecord = conversation || malformed || scanner.Err() != nil || validRecords == 0
	evidence.Title = hints.title
	evidence.Branch = hints.branch
	evidence.CWD = hints.cwd
	// Mining runs before linking, so a root has no known parent yet. A root that
	// linking later attaches to a spawner already declares the identity that
	// makes it agent-driven, so nothing is lost by asking the rule here.
	evidence.Origin, evidence.Signal = sessionorigin.Classify(
		ClaudeOriginEvidence(evidence.Identity, entrypoint, promptSource, firstUserText, false),
	)
	return evidence, true
}

// ClaudeOriginEvidence assembles the origin evidence for one mined transcript.
// identity is the value the scan already collected; entrypoint, promptSource and
// firstUserText are captured in that same pass. hasParent is supplied by the
// caller because it is Peasant's discovery state rather than harness content.
func ClaudeOriginEvidence(
	identity *ClaudeTeammateIdentity,
	entrypoint, promptSource, firstUserText string,
	hasParent bool,
) sessionorigin.Evidence {
	evidence := sessionorigin.Evidence{
		HasParent:     hasParent,
		Entrypoint:    entrypoint,
		PromptSource:  promptSource,
		FirstUserText: firstUserText,
	}
	if identity != nil {
		evidence.TeamName = identity.Team
		evidence.AgentName = identity.Name
	}
	return evidence
}

// claudeInjectedOnlyUserWrappers is the closed set of Claude Code wrappers that
// open a user-role record the HARNESS wrote, containing nothing a person typed
// and nothing an agent authored. A run of these at the head of a transcript is
// scaffolding in front of the real opening record, so the origin capture reads
// past them (see skipClaudeInjectedOnlyUserRecord).
//
// The names come from the redact wrapper catalog. No markup literal is spelled
// here, so a harness renaming a wrapper stays one upstream change.
//
// Why each member is in the set:
//
//   - LocalCommandCaveat: the fixed notice Claude Code emits before a locally
//     run slash command ("the messages below were generated by the user while
//     running local commands"). It is boilerplate the harness composes; the
//     person's actual action is the command wrapper that follows it. This is
//     the record that motivated reading past the head at all.
//   - LocalCommandStdout and LocalCommandStderr: captured output of a local
//     command. Program output, echoed into the turn by the harness.
//   - SystemReminder: harness-composed reminders and notes injected into a user
//     turn. Never typed, never authored by an agent.
//   - TaskNotification: the harness's own report that a background task changed
//     state. Machine-generated status, not authorship by anyone.
//   - EnvironmentContext: a harness dump of the working environment.
//
// Why the near neighbours are NOT in the set, since a wrong member here is a
// misclassification with no other symptom:
//
//   - CommandName, CommandMessage and CommandArgs are the person's action. A
//     command wrapper IS the evidence the rule is looking for; skipping one
//     would delete the very signal this change exists to reach.
//   - TeammateMessage and AgentMessage carry agent authorship. Skipping either
//     would let a later, human-looking record decide a session an agent in fact
//     opened -- the permissive failure, which hides a person's view of who
//     started what.
//   - UserQuery wraps the person's own prose.
//   - SystemContext, RecommendedPlugins and UserAction belong to other
//     harnesses in the same catalog. This skip runs only inside the Claude
//     adapter, so including them would add members no Claude transcript can
//     produce and no test can exercise.
var claudeInjectedOnlyUserWrappers = []string{
	redact.WrapperLocalCommandCaveat,
	redact.WrapperLocalCommandStdout,
	redact.WrapperLocalCommandStderr,
	redact.WrapperSystemReminder,
	redact.WrapperTaskNotification,
	redact.WrapperEnvironmentContext,
}

// skipClaudeInjectedOnlyUserRecord reports whether one user-record text is
// harness scaffolding that the origin capture should read past rather than hand
// to the rule.
//
// Only a LEADING run is ever skipped: the caller stops at the first record this
// returns false for, so an injected record appearing AFTER the real opening
// record is never reached, let alone skipped. A transcript whose user records
// are all injected yields no first user text at all, which is the honest answer
// -- the rule then reaches no content signal and declares the origin unknown.
func skipClaudeInjectedOnlyUserRecord(text string) bool {
	opening := strings.TrimLeft(text, " \t\r\n")
	for _, name := range claudeInjectedOnlyUserWrappers {
		if sessionorigin.OpensWithTag(opening, name) {
			return true
		}
	}
	return false
}

// claudeUserRecordText returns the text of one user-role record. The second
// result says whether the record was a user-role record at all, so the caller
// can stop at the first one even when that first one carries no text.
func claudeUserRecordText(value map[string]any) (string, bool) {
	if recordType, ok := value["type"].(string); !ok || claudeRecordType(recordType) != claudeRecordTypeUser {
		return "", false
	}
	message, ok := value["message"].(map[string]any)
	if !ok {
		return "", false
	}
	if role, ok := message["role"].(string); !ok || role != "user" {
		return "", false
	}
	switch content := message["content"].(type) {
	case string:
		return content, true
	case []any:
		var builder strings.Builder
		for _, block := range content {
			fields, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if kind, ok := fields["type"].(string); !ok || kind != claudeContentBlockText {
				continue
			}
			text, ok := fields["text"].(string)
			if !ok {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(text)
		}
		return builder.String(), true
	default:
		return "", true
	}
}

// mineClaudeSubagentTranscript mines the whole structural distribution, just
// like root mining, so native owner proof cannot depend on the first record.
func (a *ClaudeAdapter) mineClaudeSubagentTranscript(path ResolvedPath, info os.FileInfo) ClaudeTranscriptEvidence {
	evidence, _ := a.mineClaudeRootTranscript(path, info)
	evidence.Scope = ClaudeEvidenceScopeSubagent
	// A subagent transcript is by definition the child of a root, which is the
	// first step of the rule. The rule is asked rather than answered for it, so
	// that step 1 keeps exactly one definition and a change to it reaches this
	// path too.
	evidence.Origin, evidence.Signal = sessionorigin.Classify(sessionorigin.Evidence{HasParent: true})
	return evidence
}

// newClaudeEvidence starts a record keyed on the file that produced it.
func newClaudeEvidence(path ResolvedPath, scope ClaudeEvidenceScope, info os.FileInfo) ClaudeTranscriptEvidence {
	return ClaudeTranscriptEvidence{
		SourcePath:      path,
		Scope:           scope,
		ModTimeUnixNano: info.ModTime().UnixNano(),
		SizeBytes:       info.Size(),
	}
}

// hasClaudeConversationRecord reports whether path contains at least one native
// user or assistant record. Claude stores summary and file-history records in
// session-shaped JSONL files, but neither represents a renderable conversation.
// Unreadable, empty, or malformed files fail open so discovery can surface them
// for diagnostics rather than silently discarding a future transcript format.
func (a *ClaudeAdapter) hasClaudeConversationRecord(path string) bool {
	var reader io.Reader
	var closeFile func() error
	if opener, ok := a.fs.(claudeFileOpener); ok {
		file, err := opener.Open(path)
		if err != nil {
			return true
		}
		reader = file
		closeFile = file.Close
	} else {
		data, err := a.fs.ReadFile(path)
		if err != nil {
			return true
		}
		reader = bytes.NewReader(data)
	}
	if closeFile != nil {
		defer closeFile()
	}

	scanner := newJSONLRecordStreamScanner(reader, defaults.MaxJSONLRecordBytes)
	var validRecords int
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var value struct {
			Type claudeRecordType `json:"type"`
		}
		if json.Unmarshal(line, &value) != nil {
			return true
		}
		validRecords++
		if value.Type.isConversation() {
			return true
		}
	}
	return scanner.Err() != nil || validRecords == 0
}

// claudeTeammateSpawn recognizes only the top-level native tool result. Tool
// output may contain arbitrary JSON-shaped text, so recursive matching would
// let an unrelated tool payload forge discovery evidence.
func claudeTeammateSpawn(value map[string]any) (ClaudeTeammateIdentity, bool) {
	result, ok := value["toolUseResult"].(map[string]any)
	if !ok {
		return ClaudeTeammateIdentity{}, false
	}
	if status, _ := result["status"].(string); status != "teammate_spawned" {
		return ClaudeTeammateIdentity{}, false
	}
	team, teamOK := result["team_name"].(string)
	name, nameOK := result["name"].(string)
	if !teamOK || !nameOK || team == "" || name == "" {
		return ClaudeTeammateIdentity{}, false
	}
	return ClaudeTeammateIdentity{Team: team, Name: name}, true
}

// claudeSessionHints holds metadata extracted from the first few JSONL lines.
type claudeSessionHints struct {
	title  string
	branch string
	cwd    string
}

// complete reports whether every hint already has a value, so the caller can
// stop looking at further lines.
func (h claudeSessionHints) complete() bool {
	return h.title != "" && h.branch != "" && h.cwd != ""
}

// applyClaudeHints takes the session title, the git branch, and the working
// directory from one raw JSONL line. Each hint keeps the first value it finds.
// A line this build cannot parse contributes nothing.
func applyClaudeHints(hints *claudeSessionHints, raw []byte) {
	var line claudeJSONLLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return
	}
	// Grab branch and CWD from the first line that has them.
	if hints.branch == "" && line.GitBranch != "" {
		hints.branch = line.GitBranch
	}
	if hints.cwd == "" && line.CWD != "" {
		hints.cwd = line.CWD
	}
	// Derive the display title from the first user message that carries real
	// user prose. The shared redaction-free pipeline strips Claude's own markup
	// (system-reminder blocks, command and local-command wrappers, skill
	// bodies) and caps the length, so the raw markup no longer leaks into the
	// title. A turn that cleans to nothing, or that cannot be cleaned safely,
	// leaves the hint empty and the next user line becomes the candidate. An
	// isMeta record is never a candidate: the harness wrote it, whatever its
	// text, and the indexer stores it as a system entry.
	if hints.title == "" && line.Type == claudeRecordTypeUser && line.Message.Role == "user" && !line.IsMeta {
		if text := extractTextFromContent(line.Message.Content); text != "" {
			hints.title = simpleTitle(text, defaults.HarnessClaudeCode)
		}
	}
}

// extractTextFromContent extracts plain text from a Claude message content field.
// Content can be a JSON string or an array of content blocks.
func extractTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

// ExtractMetadata reads the JSONL transcript and builds UnifiedMetadata.
func (a *ClaudeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("ExtractMetadata: read %q: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessClaudeCode
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}
	allowed, relationships, identityErr := a.verifyClaudeIdentity(data, session)
	if identityErr != nil {
		return nil, identityErr
	}
	meta.Relationships = relationships

	firstLine, parseErr := parseClaudeTranscriptMetadataWithIdentity(data, &meta, allowed)
	if parseErr != nil {
		return nil, parseErr
	}
	if firstLine != nil {
		// Resolve git metadata using the cwd from the first line.
		cwd := firstLine.CWD
		if cwd == "" {
			cwd = filepath.Dir(string(session.SourcePath))
		}

		// If cwd is under ~/.claude/projects/, try to decode the actual project path.
		// Claude project dirs encode the real path by replacing "/" with "-".
		if decoded := a.decodeClaudeProjectDir(cwd); decoded != "" {
			cwd = decoded
		}

		remoteURL, trackingStr := ResolveGitRemote(ctx, a.git, cwd, firstLine.GitBranch, "")
		remoteErr := error(nil)
		// If direct remote check fails, walk up parent directories to find one.
		// This ensures sessions from decoded slug paths (which may point to a
		// subdirectory) still resolve the correct git remote for project grouping.
		if (remoteErr != nil || remoteURL == "") && a.git != nil && firstLine.GitBranch == "" {
			if walkedRemote, _, walkErr := a.git.WalkUpRemoteURL(ctx, cwd); walkErr == nil && walkedRemote != "" {
				remoteURL = walkedRemote
				remoteErr = nil
			}
		}
		branchStr, branchErr := a.git.Branch(ctx, cwd)
		worktreeStr, worktreeErr := a.git.Worktree(ctx, cwd)

		// Build GitContext — all fields are nullable.
		gitInfo := GitContext{}

		// Prefer gitBranch from JSONL over resolved branch.
		branch := firstLine.GitBranch
		if branch == "" && branchErr == nil && branchStr != "" {
			branch = branchStr
		}
		if branch != "" {
			b := branch
			gitInfo.Branch = &b
		}

		if remoteErr == nil && remoteURL != "" {
			r := remoteURL
			gitInfo.Remote = &r
		}

		if worktreeErr == nil && worktreeStr != "" {
			w := worktreeStr
			gitInfo.Worktree = &w
		}

		if trackingStr != "" {
			tr := trackingStr
			gitInfo.Tracking = &tr
		}

		meta.Git = gitInfo

		// Store the real working directory for context-aware slug redaction.

		// ProjectInfo.
		projectPath := worktreeStr
		if projectPath == "" {
			projectPath = cwd
		}

		projectHash, hostSlug, err := DeriveProjectIdentifiers(a.salt, remoteURL, projectPath)
		if err != nil {
			// DeriveProjectIdentifiersWithGit should not fail for valid paths,
			// but fall back to zero-value hash if it does.
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "derive_identity_error",
				Location:    fmt.Sprintf("session %s", session.SessionID),
				Message:     fmt.Sprintf("failed to derive project identifiers: %v", err),
				Remediation: "Check git remote URL or working directory path.",
			})
		} else {
			meta.HostSlug = hostSlug
		}

		// Derive project name from git remote (repo name) when available,
		// so that worktrees and subdirectories show the repository name
		// (e.g., "widget-service") rather than the worktree/branch
		// name (e.g., "feature-push").
		projectName := RepoNameFromRemote(remoteURL)
		if projectName == "" {
			projectName = filepath.Base(projectPath)
		}

		meta.Project = ProjectInfo{
			Hash:     projectHash,
			FilePath: projectPath,
			Name:     projectName,
		}
	}

	meta.Stats.SubagentCount = len(session.SubagentPaths)

	// Build subagent refs from SubagentPaths.
	for _, sp := range session.SubagentPaths {
		filename := filepath.Base(string(sp))
		subIDStr := strings.TrimSuffix(filename, defaults.ExtJSONL.String())
		subSID, err := NewSessionID(subIDStr)
		if err != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{
			SessionID:  subSID,
			ParentUUID: session.SessionID,
		})
	}

	return &meta, nil
}

func parseClaudeTranscriptMetadata(data []byte, meta *UnifiedMetadata) (*claudeJSONLLine, error) {
	return parseClaudeTranscriptMetadataWithIdentity(data, meta, nil)
}

func parseClaudeTranscriptMetadataWithIdentity(data []byte, meta *UnifiedMetadata, allowed map[string]bool) (*claudeJSONLLine, error) {
	var (
		firstLine      claudeJSONLLine
		literalCWD     string
		harnessVersion string // first non-empty version across all lines
		hasFirst       bool
		firstTimestamp string // earliest non-empty timestamp across all lines
		lastTimestamp  string // latest non-empty timestamp across all lines
		turnCount      int
		toolCount      int
		tokensIn       int
		tokensOut      int
		// levelState tracks the ultracode session mode in source order.
		levelState claudeThinkingLevelState
		// modernEffortSeen records any non-empty top-level effort anywhere;
		// the legacy level is consulted only when none exists.
		modernEffortSeen bool
		legacyLevel      string
	)

	// The shared record reader handles a record of any size: a record up to
	// defaults.MaxJSONLRecordBytes is read whole, and a longer one is skipped
	// and listed by Oversized() rather than failing the scan.
	scanner := newJSONLRecordScanner(data, defaults.MaxJSONLRecordBytes)

	for scanner.Scan() {
		// The reader is the one source of the line number. A counter kept here
		// would count only the records handed back, so every diagnostic after a
		// record the reader passed over would name an earlier line than the one
		// at fault.
		lineNum := scanner.Line()
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var line claudeJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse JSONL line: %v", err),
				Remediation: "Check the transcript file for corruption or truncation at the indicated line.",
			})
			continue
		}

		foreignSession := line.SessionID != "" && line.SessionID != meta.SessionID.String()
		foreignParent := meta.ParentUUID == nil || line.SessionID != meta.ParentUUID.String()
		identityMismatch := foreignSession && foreignParent && !allowed[line.SessionID]
		if identityMismatch {
			return nil, fmt.Errorf("Claude metadata capture for %s: transcript sessionId disagrees with the discovered session or parent identity; no capture was written; restore the matching source and rerun peasant ingest", meta.SessionID)
		}
		if literalCWD == "" {
			literalCWD = line.CWD
		}
		// Bookkeeping records such as file-history-snapshot and last-prompt can
		// lead a transcript and carry no version, so the first line cannot
		// supply it.
		if harnessVersion == "" {
			harnessVersion = line.Version
		}
		if !hasFirst {
			firstLine = line
			hasFirst = true
		}

		// Track earliest and latest non-empty timestamps across all lines.
		if line.Timestamp != "" {
			if firstTimestamp == "" {
				firstTimestamp = line.Timestamp
			}
			lastTimestamp = line.Timestamp
		}

		levelState.observe(string(line.Type), line.Attachment)
		effort := claudeEffortValue(line.Effort)
		if effort != "" {
			modernEffortSeen = true
		}
		if legacyLevel == "" {
			legacyLevel = claudeLegacyThinkingLevel(line.ThinkingMetadata)
		}
		if line.Type == claudeRecordTypeAssistant {
			// The governing level of the earliest assistant line that has one
			// seeds the session: the ultracode mode while on, otherwise the
			// line's own effort.
			mapping, diagnostic := levelState.governing(effort)
			appendThinkingLevelDiagnostic(meta, diagnostic, lineNum)
			applyThinkingLevelObservation(meta, mapping)
		}

		// Count turns.
		if line.Type.isConversation() {
			turnCount++
		}

		// Count tool calls and accumulate tokens from assistant messages.
		if line.Type == claudeRecordTypeAssistant && len(line.Message.Content) > 0 {
			// content may be a string or an array of blocks.
			// Only count tool_use blocks from arrays.
			var blocks []contentBlock
			if err := json.Unmarshal(line.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						toolCount++
					}
				}
			}

			// Capture model from first assistant message seen.
			if line.Message.Model != "" && meta.Model == "" {
				mid, merr := NewModelID(line.Message.Model)
				if merr == nil {
					meta.Model = mid
				}
			}

			// Accumulate token usage from assistant messages.
			// input_tokens in Claude's JSONL is only the non-cached portion;
			// the bulk of input tokens are in cache_creation and cache_read fields.
			if line.Message.Usage != nil {
				tokensIn += line.Message.Usage.InputTokens +
					line.Message.Usage.CacheCreationInputTokens +
					line.Message.Usage.CacheReadInputTokens
				tokensOut += line.Message.Usage.OutputTokens
			}
		}
	}

	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", scanner.Line()),
			Message:     fmt.Sprintf("scanner error reading transcript: %v", err),
			Remediation: "Verify the transcript file is not corrupted or truncated.",
		})
	}

	// Populate fields from parsed lines.
	if hasFirst {
		meta.Version = harnessVersion

		// Timestamps: use earliest/latest non-empty timestamps found across
		// all lines, not just the first/last line positions. Some sessions
		// have a first line (e.g. init/system) without a timestamp field.
		startMs := parseTimestampMillis(firstTimestamp)
		endMs := parseTimestampMillis(lastTimestamp)
		ingested := time.Now().UnixMilli()
		meta.Timestamp = TimestampInfo{
			Start:    startMs,
			End:      endMs,
			Ingested: &ingested,
		}

		// Duration.
		if startMs > 0 && endMs >= startMs {
			meta.Stats.DurationMs = endMs - startMs
		}

	}

	if !modernEffortSeen && meta.ThinkingLevel == "" && meta.ThinkingLevelRaw == "" {
		// Weak fallback: the legacy level only when no modern effort exists
		// anywhere in the transcript. Budgets are never consulted.
		mapping, diagnostic := mapClaudeLegacyLevel(legacyLevel)
		appendThinkingLevelDiagnostic(meta, diagnostic, 0)
		applyThinkingLevelSeed(meta, mapping)
	}

	meta.CWD = literalCWD
	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut

	if hasFirst {
		return &firstLine, nil
	}
	return nil, nil
}

// claudeProjectsDirSegment is the path segment that identifies a directory as a
// Claude project memory directory. Used to detect when the CWD fallback points
// at a Claude internal directory rather than the actual project.
//
// The one supported Claude Code session root, on every platform, is the CLI's
// own per-project memory tree: "~/.claude/projects" (unix / macOS) and, on
// Windows, "%USERPROFILE%\.claude\projects" — os.UserHomeDir() resolves
// USERPROFILE there, so the same "~/.claude/projects" default in
// defaults.DefaultClaudePath expands correctly with no Windows-specific
// default needed (see internal/defaults/config.go and
// internal/ingest/types.go's NewResolvedPath). This constant is compared
// against a slash-normalized cwd (see decodeClaudeProjectDir) so detection
// works whether cwd arrived with "/" or "\" separators.
//
// Explicitly OUT OF SCOPE for this release: the Claude *desktop* app's
// per-session sandbox roots under "%APPDATA%\Claude\local-agent-mode-sessions\...".
// Those are a different product surface with their own ad-hoc layout; see the
// public report anthropics/claude-code#84552. Nothing in this package scans
// %APPDATA%, and no code should be added here to do so.
const claudeProjectsDirSegment = "/.claude/projects/"

// decodeClaudeProjectDir checks if cwd is under a Claude project memory directory
// and attempts to decode the encoded directory name back to the real filesystem path.
//
// Claude encodes project paths by replacing "/" with "-" and prepending "-":
//
//	/home/user/dev/my-project → -home-user-dev-my-project
//
// The encoding is ambiguous (dashes in directory names vs path separators), so this
// method uses a greedy filesystem-based decoder: starting from root, it tries each
// segment and checks if a directory exists. If not, it merges segments with dashes.
//
// cwd may use either "/" or "\" as its separator — on Windows the JSONL "cwd"
// field and the session file's own directory both use native backslash paths
// (e.g. "C:\Users\alice\.claude\projects\C--Users-alice-project"), so detection
// normalizes a copy of cwd with filepath.ToSlash before matching against the
// forward-slash claudeProjectsDirSegment constant and splitting out the encoded
// segment. filepath.ToSlash is a no-op on unix (Separator is already "/"), so
// unix behavior is byte-identical. The decoded path returned by DecodeClaudeSlug
// is unaffected by this normalization: it is built by decodeProjectSlug via
// filepath.Join/filepath.Clean, which already render in the build's native
// separator regardless of the "/"-based root they start from — so callers that
// hand this value to filesystem/git operations (ResolveGitRemote, a.git.Branch,
// a.git.Worktree, a.git.WalkUpRemoteURL, filepath.Base) receive a native-form
// path with no further conversion needed here.
//
// Returns the decoded path if it exists on the filesystem, or empty string if
// decoding fails or the path does not exist.
func (a *ClaudeAdapter) decodeClaudeProjectDir(cwd string) string {
	normalized := filepath.ToSlash(cwd)
	idx := strings.Index(normalized, claudeProjectsDirSegment)
	if idx < 0 {
		return ""
	}

	// Extract the portion after "/.claude/projects/".
	// cwd may be the project dir itself or a parent of the JSONL file.
	after := normalized[idx+len(claudeProjectsDirSegment):]
	if after == "" {
		return ""
	}

	// The basename may have a trailing slash or additional path components
	// (e.g., if cwd is the dir containing the JSONL). Take only the first
	// path component after "/.claude/projects/".
	encoded := strings.SplitN(after, "/", 2)[0]

	return DecodeClaudeSlug(encoded, a.dirExists)
}

// dirExists checks if path exists as a directory using the adapter's FileSystem.
func (a *ClaudeAdapter) dirExists(path string) bool {
	info, err := a.fs.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// parseTimestampMillis parses an ISO 8601 / RFC 3339 timestamp and returns Unix milliseconds.
// Returns 0 on parse failure.
func parseTimestampMillis(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try RFC3339Nano as fallback.
		t, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

var _ DiscoveryDiagnosticReporter = (*ClaudeAdapter)(nil)

// DiscoveryDiagnostics names the source locations the last Discover could not read.
func (a *ClaudeAdapter) DiscoveryDiagnostics() []DiscoveryDiagnostic {
	return append([]DiscoveryDiagnostic(nil), a.diagnostics...)
}

func (a *ClaudeAdapter) noteUnreadable(path string, err error) {
	if d, ok := unreadableSourceDiagnostic(HarnessClaudeCode, path, err); ok {
		a.diagnostics = append(a.diagnostics, d)
	}
}

// claudeAncestorProbeKind is the one ancestor-probe kind the Claude adapter
// declares. A Claude subagent lives under its parent's session directory, and
// the parent root transcript sits directly above that directory, so an unstored
// parent costs one stat of a path derived from the child alone: never a
// listing, never a walk.
const claudeAncestorProbeKind = ProbePath

// claudeBuildEvidence is the evidence a build may reuse, keyed by source path,
// plus the records it had to mine again.
type claudeBuildEvidence struct {
	cached  map[ResolvedPath]ClaudeTranscriptEvidence
	mined   map[ResolvedPath]ClaudeTranscriptEvidence
	remined []ClaudeTranscriptEvidence
	missing map[ResolvedPath]struct{}
}

// buildClaudeSessions is the one construction path for Discover and
// DiscoverSessionAt. The mode alone decides the evidence source, the teammate
// step, and the evidence step; there is no second switch, so no invalid pair of
// choices can be written.
//
// It owns, in this order: subagent admission (workflow transcripts first, then
// plain ones, first admitted wins, collision diagnostic), native-agent linking,
// the teammate step, and the evidence step.
//
// A full walk reads the whole evidence cache and prunes the records of every
// path it walked and did not see. A scoped build reads only the paths it names,
// writes nothing, and returns its evidence records as deferred writes. A scoped
// build also treats the located subagent's native-agent owner and layout parent
// as context sources: their evidence is read and refreshed so the child's link
// equals a full run's, but their own session rows are never returned as
// members.
func (a *ClaudeAdapter) buildClaudeSessions(
	ctx context.Context,
	cfg SourceConfig,
	enumerated []claudeEnumeratedRoot,
	located *LocatedSource,
	locatedMissing []ResolvedPath,
	mode ClaudeBuildMode,
) ([]DiscoveredSession, DeferredWrites, error) {
	if !mode.IsValid() {
		return nil, nil, fmt.Errorf("ingest: the Claude build was called with an unset mode; the mode alone decides whether the evidence cache is read whole or by path and whether a teammate link reads the whole cache or the store's identity counts, so the zero value would silently pick one; where: ingest.ClaudeAdapter.buildClaudeSessions in internal/ingest/claude.go; when: building discovered sessions; impact: no session was built and nothing was read; fix: pass ClaudeBuildFullWalk for Discover or ClaudeBuildScoped for DiscoverSessionAt")
	}

	memberPaths := make([]ResolvedPath, 0, len(enumerated))
	for _, root := range enumerated {
		memberPaths = append(memberPaths, root.paths()...)
	}
	if mode == ClaudeBuildScoped {
		// A stored child whose file is gone is not a member path, but its
		// cached evidence row is still this run's to prune.
		memberPaths = append(memberPaths, locatedMissing...)
	}

	state := claudeBuildEvidence{mined: make(map[ResolvedPath]ClaudeTranscriptEvidence), missing: make(map[ResolvedPath]struct{})}
	cacheComplete := false
	if mode == ClaudeBuildFullWalk {
		state.cached, cacheComplete = a.loadCachedEvidence(ctx)
	} else {
		cached, err := a.loadEvidenceByPath(ctx, memberPaths)
		if err != nil {
			return nil, nil, err
		}
		state.cached = cached
		for _, path := range locatedMissing {
			state.missing[path] = struct{}{}
		}
	}

	sessions := make([]DiscoveredSession, 0, len(memberPaths))
	for _, root := range enumerated {
		rootSessions := a.buildClaudeRootEntries(root, mode, &state)
		sessions = append(sessions, rootSessions...)
	}

	var contextSessions []DiscoveredSession
	contextPaths := make(map[ResolvedPath]struct{})
	if mode == ClaudeBuildScoped && located != nil {
		loaded, err := a.loadClaudeLocatedContext(ctx, located, &state, contextPaths)
		if err != nil {
			return nil, nil, err
		}
		contextSessions = loaded
	}

	linking := sessions
	if len(contextSessions) > 0 {
		linking = make([]DiscoveredSession, 0, len(sessions)+len(contextSessions))
		linking = append(linking, sessions...)
		linking = append(linking, contextSessions...)
	}
	linking = a.linkClaudeNativeAgents(linking, state.mined)
	if len(contextPaths) > 0 {
		linking = claudeWithoutContextSessions(linking, contextPaths)
	}
	sessions = linking

	// The full walk uses the store's indexed identity counts only when its
	// whole-table evidence load returned every stored row. A load that skipped a
	// row (an unreadable record, or a failed load) is a strict subset of the
	// store, so adding this run's records to the store count would double-book
	// the paths the subset covers and drop a link the evidence view forms. In
	// that case the build decides from the evidence view, exactly as it did
	// before the indexed rewrite. A scoped build reads its named paths and
	// always uses the stored counts; a store-free or dry run has no store to
	// count from and uses its own view.
	storedCounts := mode == ClaudeBuildScoped ||
		(a.derivedLookup != nil && a.evidence != nil && cacheComplete)
	if err := a.linkClaudeTeammates(ctx, sessions, state.cached, state.mined, located, storedCounts); err != nil {
		return nil, nil, err
	}

	a.reminedCount = len(state.remined)
	if mode == ClaudeBuildFullWalk {
		if err := a.saveMinedEvidence(ctx, cfg, state.cached, state.mined, state.remined); err != nil {
			a.noteEvidenceCacheFailure(err)
		}
		return sessions, NoDeferredWrites{}, nil
	}

	deletes := make([]ResolvedPath, 0, len(state.missing))
	for path := range state.missing {
		if _, cached := state.cached[path]; cached {
			deletes = append(deletes, path)
		}
	}
	return sessions, claudeDeferredEvidence{cache: a.evidence, upserts: state.remined, deletes: deletes}, nil
}

// buildClaudeRootEntries builds one enumerated source root's roots and then
// its subagents, so a parent is registered before a child is linked to it. The
// admission order is workflow transcripts first, then plain ones, in
// enumeration order; a later transcript for an id already admitted under the
// same source root is left out, and a distinct parent makes that collision
// visible.
func (a *ClaudeAdapter) buildClaudeRootEntries(root claudeEnumeratedRoot, mode ClaudeBuildMode, state *claudeBuildEvidence) []DiscoveredSession {
	var sessions []DiscoveredSession
	rootIndex := make(map[string]int, len(root.roots))

	for _, entry := range root.roots {
		info := entry.info
		if info == nil {
			observed, err := a.fs.Stat(entry.path)
			if err != nil {
				if mode == ClaudeBuildScoped && errors.Is(err, fs.ErrNotExist) {
					state.missing[ResolvedPath(entry.path)] = struct{}{}
				}
				continue
			}
			info = observed
		}
		rp := ResolvedPath(entry.path)

		evidence, ok := state.cached[rp]
		if !ok || !evidence.Fresh(ClaudeEvidenceScopeRoot, info) {
			var cacheable bool
			evidence, cacheable = a.mineClaudeRootTranscript(rp, info)
			if cacheable {
				state.remined = append(state.remined, evidence)
			}
		}
		state.mined[rp] = evidence

		if !evidence.HasConversationRecord {
			continue
		}
		sid, _ := NewSessionID(entry.sessionID) // already validated at enumeration
		ds := DiscoveredSession{
			SessionID:     sid,
			Harness:       HarnessClaudeCode,
			SourcePath:    rp,
			SourceFormat:  SourceFormatJSONL,
			OriginalRoot:  root.root,
			SubagentPaths: []ResolvedPath{},
			DebugPaths:    []ResolvedPath{},
			ModTime:       info.ModTime(),
			Title:         evidence.Title,
			Branch:        evidence.Branch,
			CWD:           evidence.CWD,
			Origin:        evidence.Origin,
			Signal:        evidence.Signal,
		}
		rootIndex[entry.sessionID] = len(sessions)
		sessions = append(sessions, ds)
	}

	// admitted maps each subagent id admitted under this source root to its
	// transcript. Workflow transcripts come first, and a workflow child keeps
	// its run directory in its source path, which is where the run id is
	// recorded.
	admitted := make(map[SessionID]ResolvedPath)
	admittedParent := make(map[SessionID]SessionID)
	ordered := make([]claudeSubagentEntry, 0, len(root.workflows)+len(root.subagents))
	ordered = append(ordered, root.workflows...)
	ordered = append(ordered, root.subagents...)
	for _, entry := range ordered {
		if kept, taken := admitted[entry.subagentID]; taken {
			slog.Debug("claude discovery: transcript left out, another transcript holds its agent id",
				"session_id", entry.subagentID.String(), "left_out", entry.path, "kept", kept.String())
			// The same agent under the same parent (a workflow copy) is
			// expected. Under a different parent it is a distinct subagent
			// whose transcript is not stored, so say so.
			if admittedParent[entry.subagentID] != entry.parentID {
				a.diagnostics = append(a.diagnostics, DiscoveryDiagnostic{
					Provider: HarnessClaudeCode,
					Code:     DiscoveryCodeClaudeAgentIDCollision,
					Location: entry.path,
					Summary:  "subagent left out: another session's subagent has the same agent id",
					Detail: fmt.Sprintf("subagent %s of session %s was not ingested because %s (session %s) already holds that id; "+
						"discovery keeps whichever transcript it reaches first, and subagent ids are not scoped to their parent yet", entry.subagentID, entry.parentID, kept, admittedParent[entry.subagentID]),
				})
			}
			continue
		}
		info := entry.info
		if info == nil {
			observed, err := a.fs.Stat(entry.path)
			if err != nil {
				if mode == ClaudeBuildScoped && errors.Is(err, fs.ErrNotExist) {
					state.missing[ResolvedPath(entry.path)] = struct{}{}
				}
				continue
			}
			info = observed
		}
		rp := ResolvedPath(entry.path)

		evidence, ok := state.cached[rp]
		if !ok || !evidence.Fresh(ClaudeEvidenceScopeSubagent, info) {
			evidence = a.mineClaudeSubagentTranscript(rp, info)
			state.remined = append(state.remined, evidence)
		}
		state.mined[rp] = evidence

		if !evidence.HasConversationRecord {
			continue
		}
		parentSID := entry.parentID

		// Link to parent's SubagentPaths — guaranteed parent is already registered.
		if idx, ok := rootIndex[parentSID.String()]; ok {
			sessions[idx].SubagentPaths = append(sessions[idx].SubagentPaths, rp)
		}

		ds := DiscoveredSession{
			SessionID:     entry.subagentID,
			Harness:       HarnessClaudeCode,
			SourcePath:    rp,
			SourceFormat:  SourceFormatJSONL,
			OriginalRoot:  root.root,
			ParentUUID:    &parentSID,
			SubagentPaths: []ResolvedPath{},
			DebugPaths:    []ResolvedPath{},
			ModTime:       info.ModTime(),
			Origin:        evidence.Origin,
			Signal:        evidence.Signal,
		}
		admitted[entry.subagentID] = rp
		admittedParent[entry.subagentID] = parentSID
		sessions = append(sessions, ds)
	}
	return sessions
}

// loadEvidenceByPath reads the cached evidence for exactly the named paths. A
// scoped build never reads the whole table: the paths it names are its members
// and the context sources it reads for them.
func (a *ClaudeAdapter) loadEvidenceByPath(ctx context.Context, paths []ResolvedPath) (map[ResolvedPath]ClaudeTranscriptEvidence, error) {
	if a.evidence == nil || len(paths) == 0 {
		return nil, nil
	}
	records, err := a.evidence.LoadClaudeEvidenceFor(ctx, paths)
	if err != nil {
		return nil, fmt.Errorf("ingest: the located Claude build could not read the cached evidence for the %d path(s) it names; a link decided without that evidence would disagree with a full discovery; where: ingest.ClaudeAdapter.loadEvidenceByPath in internal/ingest/claude.go; when: reading the evidence cache by path; impact: no member was built and nothing was written; fix: restore the evidence cache and retry: %w", len(paths), err)
	}
	return records, nil
}

// loadClaudeLocatedContext reads the evidence of the located subagent's
// native-agent owner and layout parent. Both are context sources: their
// evidence decides the child's link exactly as a full run decides it, and a
// stale row is mined and refreshed through the deferred write. Neither is
// returned as a member, so neither session row is written.
func (a *ClaudeAdapter) loadClaudeLocatedContext(
	ctx context.Context,
	located *LocatedSource,
	state *claudeBuildEvidence,
	contextPaths map[ResolvedPath]struct{},
) ([]DiscoveredSession, error) {
	layout, hasLayout := claudeLayoutParent(located.Path)
	if !hasLayout {
		return nil, nil // a root transcript names no owner and lays no parent out beside itself
	}
	evidence, ok := state.mined[located.Path]
	if !ok {
		return nil, nil
	}

	paths := make([]ResolvedPath, 0, 2)
	if owner, known := claudeRecordOwner(evidence); known {
		paths = append(paths, claudeSourcePath(located.Path, owner))
	}
	layoutPath := claudeSourcePath(located.Path, layout)
	if len(paths) == 0 || paths[0] != layoutPath {
		paths = append(paths, layoutPath)
	}

	loaded, err := a.loadEvidenceByPath(ctx, paths)
	if err != nil {
		return nil, err
	}
	var sessions []DiscoveredSession
	for _, path := range paths {
		if _, isMember := state.mined[path]; isMember {
			continue // already built and linked as a member
		}
		contextPaths[path] = struct{}{}
		info, statErr := a.fs.Stat(path.String())
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				state.missing[path] = struct{}{}
			}
			continue
		}
		record, cached := loaded[path]
		if !cached || !record.Fresh(ClaudeEvidenceScopeRoot, info) {
			var cacheable bool
			record, cacheable = a.mineClaudeRootTranscript(path, info)
			if cacheable {
				state.remined = append(state.remined, record)
			}
		}
		state.mined[path] = record
		id, ok := claudeSessionIDFromRootPath(path)
		if !ok {
			continue
		}
		sessions = append(sessions, claudeContextSession(id, path, record, info))
	}
	return sessions, nil
}

// claudeContextSession is the in-memory session a context source contributes to
// the build. It carries only what linking reads; it is removed from the built
// members before they are returned, so it is never diffed, extracted or
// written.
func claudeContextSession(id SessionID, path ResolvedPath, evidence ClaudeTranscriptEvidence, info os.FileInfo) DiscoveredSession {
	return DiscoveredSession{
		SessionID:     id,
		Harness:       HarnessClaudeCode,
		SourcePath:    path,
		SourceFormat:  SourceFormatJSONL,
		SubagentPaths: []ResolvedPath{},
		DebugPaths:    []ResolvedPath{},
		ModTime:       info.ModTime(),
		Title:         evidence.Title,
		Branch:        evidence.Branch,
		CWD:           evidence.CWD,
		Origin:        evidence.Origin,
		Signal:        evidence.Signal,
	}
}

// claudeWithoutContextSessions drops the context sources from the built
// members. Their evidence was refreshed, but their own session rows are not
// this run's to write.
func claudeWithoutContextSessions(sessions []DiscoveredSession, context map[ResolvedPath]struct{}) []DiscoveredSession {
	kept := sessions[:0]
	for _, session := range sessions {
		if _, isContext := context[session.SourcePath]; isContext {
			continue
		}
		kept = append(kept, session)
	}
	return kept
}

// claudeSpawnsIdentity reports whether one root transcript recorded spawning a
// teammate identity.
func claudeSpawnsIdentity(record ClaudeTranscriptEvidence, identity ClaudeTeammateIdentity) bool {
	for _, spawn := range record.Spawns {
		if spawn == identity {
			return true
		}
	}
	return false
}

// claudeTranscriptPath is where one Claude transcript sits: the configured root
// above its project directory, the project directory's name, the session id the
// file name holds, and, for a subagent, the session directory that lays it out.
type claudeTranscriptPath struct {
	root     ResolvedPath
	project  string
	id       SessionID
	parent   SessionID
	workflow bool
}

// parseClaudeTranscriptPath reads Claude's two transcript layouts from a path
// alone:
//
//	{root}/{project}/{id}.jsonl
//	{root}/{project}/{parent}/subagents/agent-{native}.jsonl
//	{root}/{project}/{parent}/subagents/workflows/{run}/agent-{native}.jsonl
//
// It reads no directory and stats nothing.
func parseClaudeTranscriptPath(path ResolvedPath) (claudeTranscriptPath, bool) {
	base := filepath.Base(path.String())
	if !strings.HasSuffix(base, defaults.ExtJSONL.String()) {
		return claudeTranscriptPath{}, false
	}
	id, err := NewSessionID(strings.TrimSuffix(base, defaults.ExtJSONL.String()))
	if err != nil {
		return claudeTranscriptPath{}, false
	}

	dir := filepath.Dir(path.String())
	workflow := false
	switch {
	case filepath.Base(dir) == defaults.DirSubagents.String():
		// direct subagent: {parent}/subagents/agent-{native}.jsonl
	case filepath.Base(filepath.Dir(dir)) == defaults.ClaudeDirWorkflows.String() && isClaudeWorkflowRun(filepath.Base(dir)) &&
		filepath.Base(filepath.Dir(filepath.Dir(dir))) == defaults.DirSubagents.String():
		// workflow subagent: {parent}/subagents/workflows/{run}/agent-{native}.jsonl
		dir = filepath.Dir(filepath.Dir(dir))
		workflow = true
	default:
		// root: {project}/{id}.jsonl
		projectDir := dir
		root, rootErr := NewResolvedPath(filepath.Dir(projectDir))
		if rootErr != nil {
			return claudeTranscriptPath{}, false
		}
		return claudeTranscriptPath{root: root, project: filepath.Base(projectDir), id: id}, true
	}

	parentDir := filepath.Dir(dir)
	parent, parentErr := NewSessionID(filepath.Base(parentDir))
	if parentErr != nil {
		return claudeTranscriptPath{}, false
	}
	projectDir := filepath.Dir(parentDir)
	root, rootErr := NewResolvedPath(filepath.Dir(projectDir))
	if rootErr != nil {
		return claudeTranscriptPath{}, false
	}
	return claudeTranscriptPath{root: root, project: filepath.Base(projectDir), id: id, parent: parent, workflow: workflow}, true
}

// claudeEnumeratedRootFor returns the configured source root a parsed path hangs
// from. The root the layout names must be configured exactly; that is the only
// shape a full walk admits, and therefore the only OriginalRoot it records.
func claudeEnumeratedRootFor(cfg SourceConfig, root ResolvedPath) (ResolvedPath, bool) {
	for _, configured := range cfg.Paths {
		if filepath.Clean(configured.String()) == filepath.Clean(root.String()) {
			return configured, true
		}
	}
	return "", false
}

// claudeSourceGoneError is the one statement of "this recorded path no longer
// holds the session it was recorded for". It wraps ErrSourceGone, so the core
// takes the by-name lookup path for the id.
func claudeSourceGoneError(path ResolvedPath, id SessionID, because string) error {
	return fmt.Errorf("ingest: the located Claude path %q no longer holds session %s because %s; a Claude transcript is named by the file it lives in and the id its content records, so this path is not that session's source; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path; fix: let the by-name lookup find the source the session is in now: %w", path, id, because, ErrSourceGone)
}

// claudeGoneSourceBuild is the build a gone located path returns alongside
// ErrSourceGone. The path holds no transcript any more, so the evidence record
// mined from it is stale; the returned build carries its prune as a deferred
// write, which the core applies only after every selected id is located, so a
// refused run still writes nothing. The stored session row is untouched: the
// core records the stored-but-unreadable outcome.
func (a *ClaudeAdapter) claudeGoneSourceBuild(path ResolvedPath) LocatedBuild {
	return LocatedBuild{Deferred: claudeDeferredEvidence{cache: a.evidence, deletes: []ResolvedPath{path}}}
}

// DiscoverSessionAt builds the members one located Claude source carries: the
// source itself, the subagents its layout names, the stored children the core
// passed in, and the evidence of the native-agent owner and layout parent its
// own transcript proves. It reads only what it builds and writes nothing: its
// evidence records are returned as deferred writes.
//
// A path that no longer holds its transcript returns ErrSourceGone together
// with a build whose only deferred write is that path's evidence prune, so the
// scoped run leaves the same evidence table a full walk does: the row for a
// transcript that no longer exists goes, while the stored session stays.
//
// The role and the outcome of every member are the core's: only the core knows
// whether a member was requested, related-only, or an ancestor, so the adapter
// leaves both at their zero value.
func (a *ClaudeAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	if at.ID == "" {
		return LocatedBuild{}, fmt.Errorf("ingest: ClaudeAdapter.DiscoverSessionAt was given a located source with no session id at %q; a located source must name the session it holds so the member can be matched to its store row; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved", at.Path)
	}
	parsed, ok := parseClaudeTranscriptPath(at.Path)
	if !ok || parsed.id != at.ID {
		return a.claudeGoneSourceBuild(at.Path), claudeSourceGoneError(at.Path, at.ID, "the path is not a root or subagent Claude transcript that holds this session id")
	}
	root, ok := claudeEnumeratedRootFor(cfg, parsed.root)
	if !ok {
		return LocatedBuild{}, fmt.Errorf("ingest: the located Claude path %q lies outside every configured Claude source root; a Claude project sits directly below its configured root, so a source no root contains would carry an OriginalRoot no full walk gives it; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: resolving the source root of the located path; impact: nothing was read and no member was built; fix: restore the transcript under a configured Claude source path, or add the path's root to the configured source paths", at.Path)
	}
	parsed.root = root

	info, err := a.fs.Stat(at.Path.String())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return a.claudeGoneSourceBuild(at.Path), claudeSourceGoneError(at.Path, at.ID, "the file no longer exists")
		}
		return LocatedBuild{}, fmt.Errorf("ingest: the located Claude transcript %q for session %s could not be observed; the path exists but its state cannot be read, so the member cannot be built from it; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path, and the core records the read failure; fix: make the file readable and retry: %w", at.Path, at.ID, err)
	}
	if info.IsDir() {
		return a.claudeGoneSourceBuild(at.Path), claudeSourceGoneError(at.Path, at.ID, "the path is a directory, not a transcript file")
	}

	enumerated, unreadable, missing, err := a.enumerateClaudeLocatedSource(ctx, cfg, parsed, at, info)
	if err != nil {
		return LocatedBuild{}, err
	}
	sessions, deferred, err := a.buildClaudeSessions(ctx, cfg, enumerated, &at, missing, ClaudeBuildScoped)
	if err != nil {
		return LocatedBuild{}, err
	}

	members := make([]LocatedMember, 0, len(sessions)+len(unreadable))
	for i := range sessions {
		members = append(members, LocatedMember{ID: sessions[i].SessionID, Harness: HarnessClaudeCode, Source: &sessions[i]})
	}
	members = append(members, unreadable...)
	claudeSetLocatedLinkAndProbe(members, at)
	return LocatedBuild{Members: members, Deferred: deferred}, nil
}

// enumerateClaudeLocatedSource names the members a located source carries
// without walking: the source, the subagents its own layout names, and the
// stored children the core passed in. A stored child whose file is gone or
// unreadable is returned as a member with no source and its reason, because the
// store still records it and the run's upkeep set must see it.
//
// One group per configured root: a stored child may live under another
// configured root, and its member must carry that root as its OriginalRoot
// exactly as a full walk would.
func (a *ClaudeAdapter) enumerateClaudeLocatedSource(
	ctx context.Context,
	cfg SourceConfig,
	parsed claudeTranscriptPath,
	at LocatedSource,
	info os.FileInfo,
) ([]claudeEnumeratedRoot, []LocatedMember, []ResolvedPath, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	groups := []claudeEnumeratedRoot{{root: parsed.root}}
	groupByRoot := map[ResolvedPath]int{parsed.root: 0}
	groupAt := func(root ResolvedPath) *claudeEnumeratedRoot {
		if index, ok := groupByRoot[root]; ok {
			return &groups[index]
		}
		groupByRoot[root] = len(groups)
		groups = append(groups, claudeEnumeratedRoot{root: root})
		return &groups[len(groups)-1]
	}
	seenPaths := map[ResolvedPath]struct{}{at.Path: {}}
	seenIDs := map[SessionID]struct{}{at.ID: {}}

	if parsed.parent != "" {
		entry := claudeSubagentEntry{path: at.Path.String(), subagentID: at.ID, parentID: parsed.parent, info: info}
		if parsed.workflow {
			groupAt(parsed.root).workflows = append(groupAt(parsed.root).workflows, entry)
		} else {
			groupAt(parsed.root).subagents = append(groupAt(parsed.root).subagents, entry)
		}
	} else {
		groupAt(parsed.root).roots = append(groupAt(parsed.root).roots, claudeRootEntry{path: at.Path.String(), sessionID: at.ID.String(), info: info})
		workflows, subagents, err := a.enumerateClaudeLayoutRelatives(ctx, parsed)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, entry := range workflows {
			if _, duplicate := seenIDs[entry.subagentID]; duplicate {
				continue
			}
			seenIDs[entry.subagentID] = struct{}{}
			seenPaths[ResolvedPath(entry.path)] = struct{}{}
			groupAt(parsed.root).workflows = append(groupAt(parsed.root).workflows, entry)
		}
		for _, entry := range subagents {
			if _, duplicate := seenIDs[entry.subagentID]; duplicate {
				continue
			}
			seenIDs[entry.subagentID] = struct{}{}
			seenPaths[ResolvedPath(entry.path)] = struct{}{}
			groupAt(parsed.root).subagents = append(groupAt(parsed.root).subagents, entry)
		}
	}

	var unreadable []LocatedMember
	var missing []ResolvedPath
	for _, child := range at.Stored.Children {
		childParsed, childInfo, member, readable, childMissing := a.claudeStoredChild(cfg, child)
		if !readable {
			unreadable = append(unreadable, member)
			if childMissing {
				missing = append(missing, child.Path)
			}
			continue
		}
		if _, duplicate := seenPaths[child.Path]; duplicate {
			continue
		}
		seenPaths[child.Path] = struct{}{}
		entry := claudeSubagentEntry{path: child.Path.String(), subagentID: childParsed.id, parentID: childParsed.parent, info: childInfo}
		if childParsed.parent != "" {
			if childParsed.workflow {
				groupAt(childParsed.root).workflows = append(groupAt(childParsed.root).workflows, entry)
			} else {
				groupAt(childParsed.root).subagents = append(groupAt(childParsed.root).subagents, entry)
			}
			continue
		}
		groupAt(childParsed.root).roots = append(groupAt(childParsed.root).roots, claudeRootEntry{path: child.Path.String(), sessionID: childParsed.id.String(), info: childInfo})
	}
	return groups, unreadable, missing, nil
}

// enumerateClaudeLayoutRelatives reads the subagents directory a located
// root's own id names, so the child transcripts a full walk finds under it are
// built the same way. A missing directory is no children, not a failure; every
// other read error is returned so the core can name it.
func (a *ClaudeAdapter) enumerateClaudeLayoutRelatives(ctx context.Context, parsed claudeTranscriptPath) ([]claudeSubagentEntry, []claudeSubagentEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(parsed.root.String(), parsed.project, parsed.id.String(), defaults.DirSubagents.String())
	entries, err := a.fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("ingest: the located Claude session's subagents directory %q could not be listed; a directory that cannot be read may hold child transcripts, so the build cannot claim this session has none; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: enumerating a located source's layout relatives; impact: nothing was built for this located source; fix: make the directory readable and retry: %w", dir, err)
	}
	ext := defaults.ExtJSONL.String()
	var subagents []claudeSubagentEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ext) || !strings.HasPrefix(entry.Name(), defaults.ClaudeSubagentPrefix) {
			continue
		}
		id, idErr := NewSessionID(strings.TrimSuffix(entry.Name(), ext))
		if idErr != nil {
			continue
		}
		subagents = append(subagents, claudeSubagentEntry{path: filepath.Join(dir, entry.Name()), subagentID: id, parentID: parsed.id})
	}

	workflowsDir := filepath.Join(dir, defaults.ClaudeDirWorkflows.String())
	runs, err := a.fs.ReadDir(workflowsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, subagents, nil
		}
		return nil, nil, fmt.Errorf("ingest: the located Claude session's workflow directory %q could not be listed; a directory that cannot be read may hold child transcripts, so the build cannot claim this session has none; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: enumerating a located source's layout relatives; impact: nothing was built for this located source; fix: make the directory readable and retry: %w", workflowsDir, err)
	}
	var workflows []claudeSubagentEntry
	for _, run := range runs {
		if !run.IsDir() || !isClaudeWorkflowRun(run.Name()) {
			continue
		}
		runDir := filepath.Join(workflowsDir, run.Name())
		files, readErr := a.fs.ReadDir(runDir)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				continue
			}
			return nil, nil, fmt.Errorf("ingest: the located Claude session's workflow run directory %q could not be listed; a directory that cannot be read may hold child transcripts, so the build cannot claim this session has none; where: ingest.ClaudeAdapter.DiscoverSessionAt in internal/ingest/claude.go; when: enumerating a located source's layout relatives; impact: nothing was built for this located source; fix: make the directory readable and retry: %w", runDir, readErr)
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ext) || !strings.HasPrefix(file.Name(), defaults.ClaudeSubagentPrefix) {
				continue
			}
			id, idErr := NewSessionID(strings.TrimSuffix(file.Name(), ext))
			if idErr != nil {
				continue
			}
			workflows = append(workflows, claudeSubagentEntry{path: filepath.Join(runDir, file.Name()), subagentID: id, parentID: parsed.id})
		}
	}
	return workflows, subagents, nil
}

// claudeStoredChild resolves one stored child either to a source the shared
// build reads or to an unreadable member that names why. A readable child keeps
// the parent the store recorded, so its own fields equal a full run's.
func (a *ClaudeAdapter) claudeStoredChild(cfg SourceConfig, child StoredSessionSource) (claudeTranscriptPath, os.FileInfo, LocatedMember, bool, bool) {
	member := LocatedMember{ID: child.ID, Harness: child.Harness}
	if child.Harness != HarnessClaudeCode {
		member.Unreadable = UnreadableSourceFailed
		if child.Harness == "" {
			member.Detail = "the store records no harness for this child, so the claude adapter cannot attribute its source"
		} else {
			member.Detail = "a " + string(child.Harness) + " source cannot be read by the claude adapter"
		}
		return claudeTranscriptPath{}, nil, member, false, false
	}
	parsed, ok := parseClaudeTranscriptPath(child.Path)
	if !ok || parsed.id != child.ID {
		member.Unreadable = UnreadableSourceGone
		return claudeTranscriptPath{}, nil, member, false, true
	}
	root, ok := claudeEnumeratedRootFor(cfg, parsed.root)
	if !ok {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded path is outside every configured Claude source root"
		return claudeTranscriptPath{}, nil, member, false, false
	}
	parsed.root = root
	info, err := a.fs.Stat(child.Path.String())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			member.Unreadable = UnreadableSourceGone
			return claudeTranscriptPath{}, nil, member, false, true
		}
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLine(err.Error())
		return claudeTranscriptPath{}, nil, member, false, false
	}
	if info.IsDir() {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded path is a directory, not a Claude transcript file"
		return claudeTranscriptPath{}, nil, member, false, false
	}
	return parsed, info, LocatedMember{}, true, false
}

// claudeSetLocatedLinkAndProbe gives the located member the one thing the core
// cannot derive: the probe for an unstored ancestor, or the parent the store
// already records. It writes no link away. The probe is a pure value derived
// from the child's own path; nothing is read here. The adapter does not ask
// whether the parent it names is recorded: the core asks the store once per
// probe hit and stops at a recorded ancestor, so a stored parent never becomes a
// located member.
func claudeSetLocatedLinkAndProbe(members []LocatedMember, at LocatedSource) {
	for i := range members {
		if members[i].ID != at.ID || members[i].Source == nil {
			continue
		}
		source := members[i].Source
		if at.Stored.Parent != nil {
			if source.ParentUUID == nil {
				parent := *at.Stored.Parent
				source.ParentUUID = &parent
			}
			return
		}
		if source.ParentUUID == nil || *source.ParentUUID == at.ID {
			return
		}
		members[i].ParentProbe = &AncestorProbe{
			Kind:    claudeAncestorProbeKind,
			Harness: HarnessClaudeCode,
			Path:    claudeSourcePath(source.SourcePath, *source.ParentUUID),
		}
		return
	}
}

// claudeSearchPattern names one of the three transcript shapes a by-name lookup
// searches under each configured root.
type claudeSearchPattern uint8

const (
	claudeSearchRootPattern claudeSearchPattern = iota
	claudeSearchSubagentPattern
	claudeSearchWorkflowPattern
)

// claudeSearchLocation renders the pattern one configured root was searched
// with, for the miss error's searched block. It carries no resolved folder or
// file that happens to exist.
func claudeSearchLocation(root ResolvedPath, id SessionID, pattern claudeSearchPattern) string {
	display := claudeDisplayPath(root)
	ext := defaults.ExtJSONL.String()
	switch pattern {
	case claudeSearchSubagentPattern:
		return filepath.Join(display, "*", "*", defaults.DirSubagents.String(), string(id)+ext)
	case claudeSearchWorkflowPattern:
		return filepath.Join(display, "*", "*", defaults.DirSubagents.String(), defaults.ClaudeDirWorkflows.String(), defaults.ClaudeWorkflowRunPrefix+"*", string(id)+ext)
	default:
		return filepath.Join(display, "*", string(id)+ext)
	}
}

// claudeDisplayPath renders a source path with the home directory shortened,
// the way the searched block shows every location.
func claudeDisplayPath(path ResolvedPath) string {
	raw := string(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(raw, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(raw, home)
	}
	return raw
}

// LookupSessionSources finds the Claude transcripts that can hold id BY NAME:
// the root pattern, the subagent pattern, and the workflow pattern under each
// configured root. It never walks: one directory read names the projects, one
// names each project's sessions, one names each session's workflow runs, and a
// stat per candidate confirms the file. Named ids only, never a transcript
// read.
//
// A failed search is recorded ONCE, by the core: on a returned error the core
// appends the SearchFailed entry and carries the miss, so this method never
// appends one itself. It returns the entries it gathered for the roots it
// completed, so the miss still names where else the id was searched. Every
// adapter's lookup obeys the same convention.
func (a *ClaudeAdapter) LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if _, err := NewSessionID(string(id)); err != nil {
		return SourceLookup{}, fmt.Errorf("ingest: ClaudeAdapter.LookupSessionSources was given %q, which is not a session id; no Claude source can be searched under a name no session can hold; where: ingest.ClaudeAdapter.LookupSessionSources in internal/ingest/claude.go; when: locating a named session by name; impact: no Claude source is searched; fix: pass the session id the user named: %w", id, err)
	}

	ext := defaults.ExtJSONL.String()
	var lookup SourceLookup
	seenRoots := make(map[ResolvedPath]struct{}, len(cfg.Paths))
	seenSources := make(map[ResolvedPath]struct{}, len(cfg.Paths))
	addSource := func(path string) {
		resolved, err := NewResolvedPath(path)
		if err != nil {
			return
		}
		if _, duplicate := seenSources[resolved]; duplicate {
			return
		}
		seenSources[resolved] = struct{}{}
		lookup.Sources = append(lookup.Sources, resolved)
	}

	for _, basePath := range cfg.Paths {
		if _, duplicate := seenRoots[basePath]; duplicate {
			// One root named twice would claim the same transcript twice, read
			// downstream as a duplicate session, and report the same search
			// twice.
			continue
		}
		seenRoots[basePath] = struct{}{}
		if err := ctx.Err(); err != nil {
			return lookup, err
		}
		root := basePath.String()
		rootSearched := SearchedSource{Harness: HarnessClaudeCode, Method: SearchNamePattern, Location: claudeSearchLocation(basePath, id, claudeSearchRootPattern)}
		subagentSearched := SearchedSource{Harness: HarnessClaudeCode, Method: SearchNamePattern, Location: claudeSearchLocation(basePath, id, claudeSearchSubagentPattern)}
		workflowSearched := SearchedSource{Harness: HarnessClaudeCode, Method: SearchNamePattern, Location: claudeSearchLocation(basePath, id, claudeSearchWorkflowPattern)}

		projects, err := a.fs.ReadDir(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				lookup.Searched = append(lookup.Searched, rootSearched, subagentSearched, workflowSearched)
				continue
			}
			return lookup, claudeLookupFailure(id, "list", root, err)
		}

		type projectScan struct {
			dir      string
			sessions []os.DirEntry
		}
		scans := make([]projectScan, 0, len(projects))
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			dir := filepath.Join(root, project.Name())
			candidate := filepath.Join(dir, string(id)+ext)
			hit, statErr := a.claudeSourceExists(candidate)
			if statErr != nil {
				return lookup, claudeLookupFailure(id, "stat", candidate, statErr)
			}
			if hit {
				addSource(candidate)
			}
			sessions, readErr := a.fs.ReadDir(dir)
			if readErr != nil {
				if errors.Is(readErr, fs.ErrNotExist) {
					continue
				}
				return lookup, claudeLookupFailure(id, "list", dir, readErr)
			}
			scans = append(scans, projectScan{dir: dir, sessions: sessions})
		}
		lookup.Searched = append(lookup.Searched, rootSearched)

		for _, scan := range scans {
			for _, session := range scan.sessions {
				if !session.IsDir() {
					continue
				}
				subagentsDir := filepath.Join(scan.dir, session.Name(), defaults.DirSubagents.String())
				candidate := filepath.Join(subagentsDir, string(id)+ext)
				hit, statErr := a.claudeSourceExists(candidate)
				if statErr != nil {
					return lookup, claudeLookupFailure(id, "stat", candidate, statErr)
				}
				if hit {
					addSource(candidate)
				}
			}
		}
		lookup.Searched = append(lookup.Searched, subagentSearched)

		for _, scan := range scans {
			for _, session := range scan.sessions {
				if !session.IsDir() {
					continue
				}
				workflowsDir := filepath.Join(scan.dir, session.Name(), defaults.DirSubagents.String(), defaults.ClaudeDirWorkflows.String())
				runs, readErr := a.fs.ReadDir(workflowsDir)
				if readErr != nil {
					if errors.Is(readErr, fs.ErrNotExist) {
						continue
					}
					return lookup, claudeLookupFailure(id, "list", workflowsDir, readErr)
				}
				for _, run := range runs {
					if !run.IsDir() || !isClaudeWorkflowRun(run.Name()) {
						continue
					}
					candidate := filepath.Join(workflowsDir, run.Name(), string(id)+ext)
					hit, statErr := a.claudeSourceExists(candidate)
					if statErr != nil {
						return lookup, claudeLookupFailure(id, "stat", candidate, statErr)
					}
					if hit {
						addSource(candidate)
					}
				}
			}
		}
		lookup.Searched = append(lookup.Searched, workflowSearched)
	}
	return lookup, nil
}

// claudeLookupFailure is the one statement of "the by-name lookup could not
// cover this source". The core records the failure from the returned error, and
// the detail says why the id cannot be reported absent from this source.
func claudeLookupFailure(id SessionID, action, path string, err error) error {
	return fmt.Errorf("ingest: the Claude lookup for session %s could not %s %q; the search stopped before this source was fully covered, so the id cannot be reported absent from it; where: ingest.ClaudeAdapter.LookupSessionSources in internal/ingest/claude.go; when: locating the named session by name; impact: this source was not fully searched, and the miss text names the failure; fix: make the path readable and retry: %w", id, action, path, err)
}

// claudeSourceExists reports whether path is a readable file. A missing path is
// not a failure: it is simply not a hit.
func (a *ClaudeAdapter) claudeSourceExists(path string) (bool, error) {
	info, err := a.fs.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}
