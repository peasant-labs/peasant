package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/schema"
)

// --- SessionID ---

// SessionID is a validated session identifier from a source provider.
type SessionID = schema.SessionID

// NewSessionID validates and constructs a SessionID.
func NewSessionID(raw string) (SessionID, error) {
	return schema.NewSessionID(raw)
}

// --- ModelID ---

// ModelID identifies a specific model version (e.g. "claude-opus-4-6").
type ModelID = schema.ModelID

// NewModelID validates and constructs a ModelID.
func NewModelID(raw string) (ModelID, error) {
	return schema.NewModelID(raw)
}

// --- ProjectHash ---

// ProjectHash is a SHA-256 hex digest of the project's origin URL or local path.
type ProjectHash = schema.ProjectHash

// NewProjectHash validates and constructs a ProjectHash.
func NewProjectHash(raw string) (ProjectHash, error) {
	return schema.NewProjectHash(raw)
}

// --- HostSlug ---

// HostSlug is a sanitized, filesystem-safe identifier derived from git remote.
type HostSlug = schema.HostSlug

// NewHostSlug validates and constructs a HostSlug.
func NewHostSlug(raw string) (HostSlug, error) {
	return schema.NewHostSlug(raw)
}

// --- ResolvedPath ---

// ResolvedPath is a tilde-expanded, cleaned absolute path.
// Construction via NewResolvedPath guarantees expansion has occurred.
type ResolvedPath string

// NewResolvedPath expands tildes, cleans, and validates that the path is absolute.
func NewResolvedPath(raw string) (ResolvedPath, error) {
	if raw == "" {
		return "", fmt.Errorf("invalid path: empty string")
	}

	expanded := raw
	if strings.HasPrefix(expanded, "~/") || expanded == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand tilde in %q: %w", raw, err)
		}
		if expanded == "~" {
			expanded = home
		} else {
			expanded = filepath.Join(home, expanded[2:])
		}
	}

	expanded = filepath.Clean(expanded)

	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("invalid path %q: must be absolute (resolved to %q)", raw, expanded)
	}

	return ResolvedPath(expanded), nil
}

func (r ResolvedPath) String() string { return string(r) }

// --- Role ---

// Role represents the sender of a message turn.
type Role = schema.Role

const (
	RoleUser      = schema.RoleUser
	RoleAssistant = schema.RoleAssistant
	RoleTool      = schema.RoleTool
	RoleSystem    = schema.RoleSystem
)

// --- SessionOutcome ---

// SessionOutcome represents the resolution status of a session.
type SessionOutcome = schema.SessionOutcome

const (
	OutcomeResolved = schema.OutcomeResolved
	OutcomePartial  = schema.OutcomePartial
	OutcomeFailed   = schema.OutcomeFailed
)

// AllOutcomes is the canonical list of all known session outcomes.
var AllOutcomes = schema.AllOutcomes

// --- EntryType ---

// EntryType classifies a single entry within an agent session transcript.
type EntryType = schema.EntryType

const (
	EntryTypeText       = schema.EntryTypeText
	EntryTypeToolUse    = schema.EntryTypeToolUse
	EntryTypeToolResult = schema.EntryTypeToolResult
	EntryTypeThinking   = schema.EntryTypeThinking
	EntryTypeSystem     = schema.EntryTypeSystem
	EntryTypeError      = schema.EntryTypeError
	EntryTypeResult     = schema.EntryTypeResult
)

// AllEntryTypes is the canonical list of all known entry types.
var AllEntryTypes = schema.AllEntryTypes

// --- Harness ---

// Harness identifies the coding tool or AI-assisted development environment.
type Harness = schema.Harness

const (
	HarnessClaudeCode  = schema.HarnessClaudeCode
	HarnessGeminiCLI   = schema.HarnessGeminiCLI
	HarnessCodex       = schema.HarnessCodex
	HarnessOpenCode    = schema.HarnessOpenCode
	HarnessCursor      = schema.HarnessCursor
	HarnessAntigravity = schema.HarnessAntigravity
	HarnessStrike      = schema.HarnessStrike
)

// AllHarnesses is the canonical list of harnesses peasant supports for ingestion.
var AllHarnesses = schema.AllHarnesses

// --- SessionMetrics ---

// SessionMetrics represents the unified metrics for a session.
// v1-migrated rows have ComputeVersion=0 and only retained v1 fields populated.
// v2-computed rows have ComputeVersion>=1 and all applicable fields populated.
// It maps 1:1 to a row in the session_metrics table.
//
// schema.QualityMetrics is embedded and provides TurnCount, SubagentCount,
// ComputedAt, ComputeVersion, and all quality/cost signal fields.
// Fields below are SQLite-specific bookkeeping not present in the wire format.
type SessionMetrics struct {
	SessionID SessionID
	schema.QualityMetrics
}

// --- IndexOutcome ---

// IndexOutcome classifies the result of indexing a session.
type IndexOutcome string

const (
	IndexOutcomeIndexed   IndexOutcome = "indexed"
	IndexOutcomeReindexed IndexOutcome = "reindexed"
	IndexOutcomeFallback  IndexOutcome = "fallback"
	IndexOutcomeSkipped   IndexOutcome = "skipped"
	IndexOutcomeError     IndexOutcome = "error"
)

// AllIndexOutcomes is the canonical list of all known index outcomes.
var AllIndexOutcomes = []IndexOutcome{
	IndexOutcomeIndexed, IndexOutcomeReindexed, IndexOutcomeFallback,
	IndexOutcomeSkipped, IndexOutcomeError,
}

// String returns the string representation of an IndexOutcome.
func (o IndexOutcome) String() string { return string(o) }

// IsValid returns true if the IndexOutcome is one of the known values.
func (o IndexOutcome) IsValid() bool {
	switch o {
	case IndexOutcomeIndexed, IndexOutcomeReindexed, IndexOutcomeFallback, IndexOutcomeSkipped, IndexOutcomeError:
		return true
	}
	return false
}

// --- IndexLogEntry ---

// IndexLogEntry records one session indexing attempt for audit purposes.
// Written during the INDEX stage of pipeline.Run(). Maps 1:1 to a row
// in the index_log table.
type IndexLogEntry struct {
	SessionID    SessionID
	Harness      Harness
	Outcome      IndexOutcome
	IndexVersion int
	EntriesCount int
	SourcePath   *string
	OriginalRoot *string
	Reason       *string
	StartedAt    int64  // Unix millis
	FinishedAt   *int64 // Unix millis — nil if indexing did not complete
	ErrorMessage *string
}

// --- CurrentIndexVersion ---

// CurrentIndexVersion tracks indexer evolution separately from metadata schema.
// Bump when indexer logic changes and sessions need re-indexing.
// v1: initial indexing support with index_log tracking.
// v2: full-depth content block decomposition enabled in production.
// v3: tool_kind and stop_reason columns populated (push-v2).
// v4: project walk-up via WalkUpRemoteURL; provider roles reclassified during indexing.
// v5: content-block detection for skill bodies; depth=1 role propagation; direct/progress/queue-operation types.
// v6: suppress duplicate user text echo entries in OpenCode indexParts; inherit parent role for non-echo parts.
// v7: skip empty text/thinking/default parts at index time (both OpenCode and Claude); remove read-time filters from EntriesToTurns.
// v8: restore empty-part storage at index time; restore read-time empty suppression and consecutive dedup in EntriesToTurns; add part_type column.
// v9: reclassify Claude compaction/context-continuation messages from role=user to role=system.
// v10: Claude indexer writes canonical roles at index time: depth-1 tool_result → role=tool,
//
//	AskUserQuestion tool_results stay role=user; depth-0 tool_result wrappers → role=tool;
//	content_preview migrated from depth-0 wrappers to depth-1 tool_result children (R1–R3, R6).
//
// v11: reclassify empty progress/direct/queue-operation entries to role=system;
//
//	reclassify "Tool loaded." wrappers with tool_result siblings to role=system.
const CurrentIndexVersion = 11

// --- PruneFilter ---

// PruneFilter constrains which sessions are eligible for pruning.
type PruneFilter struct {
	SessionIDs  []SessionID // specific session IDs (empty = not filtered by ID)
	ProjectHash *string     // filter by project hash
	Harness     *Harness    // filter by provider
	Before      *int64      // unix ms — sessions started before this time
	After       *int64      // unix ms — sessions started after this time
	All         bool        // prune all sessions (ignores other filters)
}

// PruneSessionRow is a session eligible for pruning.
type PruneSessionRow struct {
	SessionID   SessionID
	Harness     Harness
	ProjectHash string
	ProjectName string
	GitRemote   string // git remote URL from host_slugs table (may be empty)
	ProjectPath string // raw session worktree, falling back to the project's canonical cwd
	StartMs     int64
	TurnCount   int
	OutputPath  string // host_slug used to construct filesystem path
}

// PrunePlan is an immutable snapshot of the exact local sessions shown to the
// user before destructive pruning. It never represents Village resources.
type PrunePlan struct {
	sessions []PruneSessionRow
}

// NewPrunePlan copies sessions into an immutable exact prune plan.
func NewPrunePlan(sessions []PruneSessionRow) PrunePlan {
	return PrunePlan{sessions: append([]PruneSessionRow(nil), sessions...)}
}

// Sessions returns a defensive copy of the previewed session rows.
func (p PrunePlan) Sessions() []PruneSessionRow {
	return append([]PruneSessionRow(nil), p.sessions...)
}

// SessionIDs returns the exact identifiers captured by the plan.
func (p PrunePlan) SessionIDs() []SessionID {
	ids := make([]SessionID, len(p.sessions))
	for i, session := range p.sessions {
		ids[i] = session.SessionID
	}
	return ids
}

// IsSelectedBy reports whether this session matches the given selection config
// using the legacy positional matcher. Clone-aware command paths must resolve
// ProjectPath across the complete row cohort and call MatchesCandidate instead.
// A session is selected if its provider appears in the config AND either:
//   - the provider has no project/session restrictions (import all), OR
//   - the session's git remote matches a project's gitRemote, OR
//   - the session's project name matches a project's name, OR
//   - the session ID is explicitly listed.
//
// Branch filtering is not applied here because the DB does not store the branch
// that was active at ingest time. This means --unselected prune is conservative:
// it will NOT prune a session whose project matches but whose branch was excluded.
func (r PruneSessionRow) IsSelectedBy(sel SelectionMatcher) bool {
	return sel.Matches(r.Harness, r.GitRemote, r.ProjectName, r.SessionID)
}

// SelectionMatcher checks whether a session matches the selection config.
// Built once from config.SelectionConfig, used for each DB session row.
type SelectionMatcher struct {
	// harnesses maps harness name → harnessMatcher.
	// Empty map means nothing is selected (mode=selected with no harnesses).
	harnesses map[string]harnessMatcher
}

// DiscoveryIdentityMultiplicity says whether an identity names one candidate
// or more than one candidate in the current discovery set. Its zero value is
// not trusted as unique. Discovery producers must prove uniqueness explicitly
// before remote or name evidence can select a candidate.
type DiscoveryIdentityMultiplicity uint8

const (
	// DiscoveryIdentityUnproven is the fail-closed zero value. The producer did
	// not prove that this remote or name identifies one candidate.
	DiscoveryIdentityUnproven DiscoveryIdentityMultiplicity = iota
	// DiscoveryIdentityUnique means the identity names one candidate.
	DiscoveryIdentityUnique
	// DiscoveryIdentityAmbiguous means the identity names multiple candidates.
	DiscoveryIdentityAmbiguous
)

// DiscoveryCandidate contains the identity evidence for one discovered
// session. ClonePath is physical identity data and is separate from display
// text. An empty ClonePath means that path resolution was unavailable.
type DiscoveryCandidate struct {
	Harness            Harness
	GitRemote          string
	ProjectName        string
	ClonePath          ClonePath
	Branch             string
	SessionID          SessionID
	RemoteMultiplicity DiscoveryIdentityMultiplicity
	NameMultiplicity   DiscoveryIdentityMultiplicity
}

type harnessMatcher struct {
	projects []projectMatcher
	sessions map[string]bool // explicit session IDs
}

type projectMatcher struct {
	gitRemote string
	name      string
	// clonePaths contains resolved absolute physical identities. Empty keeps
	// the legacy remote/name fallback behavior.
	clonePaths map[ClonePath]bool
	// pathBound remains true when clone paths were configured but malformed.
	// Those entries fail closed instead of becoming legacy remote/name rules.
	pathBound bool
	// branches is the set of selected branch names for this project. Empty (nil)
	// means "all branches" under the current selection semantics. It is consulted ONLY by
	// MatchBranch; the project-level Matches deliberately ignores branches so
	// ingest/prune behavior is unchanged.
	branches map[string]bool
	// entry is the entry as the user wrote it, kept unnormalized so a message
	// about this entry names the text they have to find and edit.
	entry SelectionEntry
}

// SelectionEntry names one configured project selection entry by the identity
// the user wrote in the configuration file. It exists so a message about an
// entry can be acted on: the normalized forms the matcher compares against are
// deliberately not what the user reads back in their config.
type SelectionEntry struct {
	GitRemote  string
	Name       string
	ClonePaths []ClonePath
	Branches   []string
}

// String renders the entry the way the configuration file names it.
//
// A clone-path-only entry renders its physical identity when a branch conflict
// must identify the configured rule to the terminal user.
func (e SelectionEntry) String() string {
	identity := ""
	switch {
	case e.GitRemote != "" && e.Name != "":
		identity = fmt.Sprintf("gitRemote %q name %q", e.GitRemote, e.Name)
	case e.Name != "":
		identity = fmt.Sprintf("name %q", e.Name)
	case e.GitRemote == "" && len(e.ClonePaths) > 0:
		identity = fmt.Sprintf("clonePaths %q", e.ClonePaths)
	default:
		identity = fmt.Sprintf("gitRemote %q", e.GitRemote)
	}
	// "all branches" is not decoration: it is the distinction a warning about
	// disagreeing BRANCH rules exists to make legible, and an unrestricted entry
	// is the likeliest permissive side of a real conflict.
	if len(e.Branches) == 0 {
		return identity + " (all branches)"
	}
	return fmt.Sprintf("%s branches %v", identity, e.Branches)
}

// DiscoveryDecision is a discovery match together with the configured entries
// that produced it. A caller that must explain a withheld session reads the
// entries from here instead of re-deriving them from the configuration, so the
// explanation cannot name a different set than the decision used.
type DiscoveryDecision struct {
	Match BranchMatch
	// Admitting and Rejecting are the entries that identify this session and
	// whose branch rules respectively admit and reject it. On
	// BranchMatchWithheldConflict both are non-empty, and together they are
	// exactly the entries that disagree.
	Admitting []SelectionEntry
	Rejecting []SelectionEntry
}

// Conflicting returns the entries that disagree about the session, admitting
// entries first. It is empty for every match other than
// BranchMatchWithheldConflict, because no other match has a disagreement to
// report.
func (d DiscoveryDecision) Conflicting() []SelectionEntry {
	if d.Match != BranchMatchWithheldConflict {
		return nil
	}
	entries := make([]SelectionEntry, 0, len(d.Admitting)+len(d.Rejecting))
	entries = append(entries, d.Admitting...)
	entries = append(entries, d.Rejecting...)
	return entries
}

func (p projectMatcher) matchesClonePath(clonePath ClonePath) bool {
	return clonePath != "" && p.clonePaths[clonePath]
}

func (p projectMatcher) matchesRemote(normalizedRemote string) bool {
	return normalizedRemote != "" && p.gitRemote != "" && p.gitRemote == normalizedRemote
}

func (p projectMatcher) matchesName(normalizedName string) bool {
	return normalizedName != "" && p.name != "" && p.name == normalizedName
}

// NormalizeProjectNameForMatch normalizes a project "name" for
// SELECTION-MATCHING comparison. A configured selection rule's
// `name` is a short, human-typed project name (e.g. "sample-project"),
// but several store-side readers populate their row's "project name" field as
// COALESCE(canonical_cwd, project_hash) — the session's full working-directory
// PATH, since the projects table has no separate short-name column. Comparing
// a short config name against a full path never matches.
//
// If the input looks like a path (contains a path separator), this returns
// its final path segment (the directory's basename) — the natural short name
// a user would type for it — via filepath.Base after cleaning. A value with
// no separator (already a short name, e.g. from discovery-time adapters that
// already populate a basename) passes through unchanged, so this is a safe
// no-op wherever the input was already a real name.
//
// Name alone cannot disambiguate two physical clones with the same basename.
// Candidate producers mark that evidence as ambiguous, and persisted
// ClonePaths provides exact identity after the user selects a clone.
func NormalizeProjectNameForMatch(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if !strings.ContainsAny(name, "/\\") {
		return name
	}
	base := filepath.Base(filepath.Clean(name))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// admitsBranch reports whether this project admits the given branch under MVP
// semantics: empty branch set = all branches; unknown branch ("") = admitted
// (conservative — never drop a candidate for missing branch data); otherwise
// membership in the selected set.
func (p projectMatcher) admitsBranch(gitBranch string) bool {
	if len(p.branches) == 0 {
		return true
	}
	if gitBranch == "" {
		return true
	}
	return p.branches[gitBranch]
}

// harnessSelectionInput is the internal DTO for building a harnessMatcher.
// Mirrors config.SelectionHarnessConfig without importing internal/config.
type harnessSelectionInput struct {
	Projects []projectSelectionInput
	Sessions []string
}

// projectSelectionInput identifies a project by git remote or name for selection matching.
// Mirrors config.ProjectSelection without importing internal/config.
type projectSelectionInput struct {
	GitRemote  string
	Name       string
	ClonePaths []string
	Branches   []string
}

// toBranchSet builds a lookup set from a branch-name slice. Returns nil for
// empty input; a nil set means "all branches" under current MVP semantics.
func toBranchSet(branches []string) map[string]bool {
	if len(branches) == 0 {
		return nil
	}
	s := make(map[string]bool, len(branches))
	for _, b := range branches {
		s[b] = true
	}
	return s
}

func toClonePathSet(paths []string) map[ClonePath]bool {
	if len(paths) == 0 {
		return nil
	}
	set := make(map[ClonePath]bool, len(paths))
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			continue
		}
		set[ClonePath(path)] = true
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// newSelectionMatcher builds a matcher from the harness selection map.
// Used internally and by SelectionMatcherBuilder.
func newSelectionMatcher(harnesses map[string]harnessSelectionInput) SelectionMatcher {
	m := SelectionMatcher{harnesses: make(map[string]harnessMatcher, len(harnesses))}
	for harnessName, input := range harnesses {
		pm := harnessMatcher{
			sessions: make(map[string]bool, len(input.Sessions)),
		}
		for _, sid := range input.Sessions {
			pm.sessions[sid] = true
		}
		for _, proj := range input.Projects {
			clonePaths := toClonePathSet(proj.ClonePaths)
			pm.projects = append(pm.projects, projectMatcher{
				// Normalized ONCE here at construction: the
				// config side's gitRemote/name are compared against
				// per-session values normalized the same way at match time
				// (see matchingProjects), so an SSH-form config
				// remote matches a normalized stored remote, and a config
				// name matches a path-derived stored "name".
				gitRemote:  NormalizeRemoteForMatch(proj.GitRemote),
				name:       NormalizeProjectNameForMatch(proj.Name),
				clonePaths: clonePaths,
				pathBound:  len(proj.ClonePaths) > 0,
				branches:   toBranchSet(proj.Branches),
				entry: SelectionEntry{
					GitRemote:  proj.GitRemote,
					Name:       proj.Name,
					ClonePaths: clonePathSlice(proj.ClonePaths),
					Branches:   append([]string(nil), proj.Branches...),
				},
			})
		}
		m.harnesses[harnessName] = pm
	}
	return m
}

func clonePathSlice(paths []string) []ClonePath {
	if len(paths) == 0 {
		return nil
	}
	result := make([]ClonePath, len(paths))
	for index, path := range paths {
		result[index] = ClonePath(path)
	}
	return result
}

// SelectionMatcherBuilder constructs a SelectionMatcher incrementally.
// Use NewSelectionMatcherBuilder, then call AddHarness / AddProject / AddSession,
// then call Build to obtain the immutable SelectionMatcher.
type SelectionMatcherBuilder struct {
	harnesses map[string]harnessSelectionInput
}

// NewSelectionMatcherBuilder returns a ready-to-use builder.
func NewSelectionMatcherBuilder() *SelectionMatcherBuilder {
	return &SelectionMatcherBuilder{harnesses: make(map[string]harnessSelectionInput)}
}

// AddHarness ensures harness is present in the matcher (no project/session restrictions).
// Calling AddProject or AddSession for a harness implicitly adds it as well.
func (b *SelectionMatcherBuilder) AddHarness(name string) *SelectionMatcherBuilder {
	if _, ok := b.harnesses[name]; !ok {
		b.harnesses[name] = harnessSelectionInput{}
	}
	return b
}

// AddProject adds a project restriction (by git remote and/or name) for the
// given harness. Optional branch names restrict the project to those branches;
// passing no branches means "all branches" (current MVP semantics). Existing
// three-argument callers (e.g. prune's project-level matching) remain valid.
func (b *SelectionMatcherBuilder) AddProject(harness, gitRemote, name string, branches ...string) *SelectionMatcherBuilder {
	return b.AddProjectWithClonePaths(harness, gitRemote, name, nil, branches...)
}

// AddProjectWithClonePaths adds a project restriction with resolved physical
// clone identities. Branches applies to every clone path in the entry.
func (b *SelectionMatcherBuilder) AddProjectWithClonePaths(harness, gitRemote, name string, clonePaths []string, branches ...string) *SelectionMatcherBuilder {
	p := b.harnesses[harness]
	p.Projects = append(p.Projects, projectSelectionInput{
		GitRemote:  gitRemote,
		Name:       name,
		ClonePaths: append([]string(nil), clonePaths...),
		Branches:   append([]string(nil), branches...),
	})
	b.harnesses[harness] = p
	return b
}

// AddSession adds an explicit session ID allowlist entry for the given harness.
func (b *SelectionMatcherBuilder) AddSession(harness, sessionID string) *SelectionMatcherBuilder {
	p := b.harnesses[harness]
	p.Sessions = append(p.Sessions, sessionID)
	b.harnesses[harness] = p
	return b
}

// Build returns the immutable SelectionMatcher.
func (b *SelectionMatcherBuilder) Build() SelectionMatcher {
	return newSelectionMatcher(b.harnesses)
}

// Matches reports whether a session with the given attributes is selected.
func (m SelectionMatcher) Matches(harness Harness, gitRemote, projectName string, sessionID SessionID) bool {
	return m.MatchesCandidate(DiscoveryCandidate{
		Harness:            harness,
		GitRemote:          gitRemote,
		ProjectName:        projectName,
		SessionID:          sessionID,
		RemoteMultiplicity: DiscoveryIdentityUnique,
		NameMultiplicity:   DiscoveryIdentityUnique,
	})
}

// MatchesCandidate is the path-aware project-level counterpart to Matches.
// Branch rules are deliberately ignored, as they were by Matches.
func (m SelectionMatcher) MatchesCandidate(candidate DiscoveryCandidate) bool {
	candidate.Branch = ""
	return m.decideCandidate(candidate, branchPolicy{admitUnknownBranch: true}).Match == BranchMatchYes
}

// DiscoveryNeedsGit reports whether resolving repository attributes can change
// discovery matching for this harness and session. Only project entries consult
// a git remote or branch, so the answer is no when the harness is absent, when
// it carries no project entries at all (unrestricted, or restricted to explicit
// session IDs), or when this session is already on the explicit allowlist.
// Callers that skip resolution must still take the decision from MatchDiscovery;
// this only avoids work that cannot change it.
func (m SelectionMatcher) DiscoveryNeedsGit(harness Harness, sessionID SessionID) bool {
	selected, ok := m.harnesses[harness.String()]
	if !ok || len(selected.projects) == 0 {
		return false
	}
	return !selected.sessions[string(sessionID)]
}

// BranchMatch is the three-valued result of branch-aware selection matching.
type BranchMatch int

const (
	// BranchMatchNo means no project/session rule selects the session.
	BranchMatchNo BranchMatch = iota
	// BranchMatchYes means the session is selected.
	BranchMatchYes
	// BranchMatchWithheldConflict means the session matches two or more projects
	// whose branch rules disagree (some admit, some reject). Under AND-strict it
	// is NOT auto-selected, but the caller should surface it ("withheld") rather
	// than silently drop it. The current behavior withholds and flags conflicts;
	// it does not offer an interactive override.
	BranchMatchWithheldConflict
)

// MatchBranch is the branch-aware counterpart to Matches, consulted ONLY by the
// push path (via PushSessionRow.IsSelectedByBranch). Ingest and prune keep using
// the project-level Matches, so their behavior is unchanged by this method.
//
// A project "admits" a session when its branch set is empty (all branches), the
// session's branch is unknown (""), or the branch is in the set. With multiple
// matching projects the results combine AND-strict: unanimous-admit → Yes;
// unanimous-reject → No; disagreement → WithheldConflict. The explicit
// session-ID allowlist and the no-restriction-harness case select unconditionally.
func (m SelectionMatcher) MatchBranch(harness Harness, gitRemote, projectName, gitBranch string, sessionID SessionID) BranchMatch {
	return m.MatchBranchCandidate(DiscoveryCandidate{
		Harness:            harness,
		GitRemote:          gitRemote,
		ProjectName:        projectName,
		Branch:             gitBranch,
		SessionID:          sessionID,
		RemoteMultiplicity: DiscoveryIdentityUnique,
		NameMultiplicity:   DiscoveryIdentityUnique,
	})
}

// MatchBranchCandidate applies stored-row branch semantics with clone-path
// identity available.
func (m SelectionMatcher) MatchBranchCandidate(candidate DiscoveryCandidate) BranchMatch {
	return m.decideCandidate(candidate, branchPolicy{admitUnknownBranch: true}).Match
}

// MatchDiscovery applies the canonical selection semantics to a newly
// discovered session. Unlike stored rows, a missing branch cannot safely pass
// an explicit branch allowlist. When autoNewBranches is set and a project pins
// an explicit branch list, a resolved branch outside that list is admitted
// anyway — that is the "ingest branches I have not listed yet" setting.
func (m SelectionMatcher) MatchDiscovery(harness Harness, gitRemote, projectName, gitBranch string, sessionID SessionID, autoNewBranches bool) BranchMatch {
	return m.MatchDiscoveryCandidate(DiscoveryCandidate{
		Harness:            harness,
		GitRemote:          gitRemote,
		ProjectName:        projectName,
		Branch:             gitBranch,
		SessionID:          sessionID,
		RemoteMultiplicity: DiscoveryIdentityUnique,
		NameMultiplicity:   DiscoveryIdentityUnique,
	}, autoNewBranches)
}

// MatchDiscoveryCandidate is the authoritative discovery matcher. It uses
// explicit session ID, exact physical clone path, unique remote, then unique
// project name evidence, in that order.
func (m SelectionMatcher) MatchDiscoveryCandidate(candidate DiscoveryCandidate, autoNewBranches bool) BranchMatch {
	return m.MatchDiscoveryCandidateDecision(candidate, autoNewBranches).Match
}

// MatchDiscoveryDecision is MatchDiscovery plus the configured entries behind
// the answer, for callers that must explain a withheld session to the user.
func (m SelectionMatcher) MatchDiscoveryDecision(harness Harness, gitRemote, projectName, gitBranch string, sessionID SessionID, autoNewBranches bool) DiscoveryDecision {
	return m.MatchDiscoveryCandidateDecision(DiscoveryCandidate{
		Harness:            harness,
		GitRemote:          gitRemote,
		ProjectName:        projectName,
		Branch:             gitBranch,
		SessionID:          sessionID,
		RemoteMultiplicity: DiscoveryIdentityUnique,
		NameMultiplicity:   DiscoveryIdentityUnique,
	}, autoNewBranches)
}

// MatchDiscoveryCandidateDecision returns the authoritative candidate result
// together with the configured entries that caused it.
func (m SelectionMatcher) MatchDiscoveryCandidateDecision(candidate DiscoveryCandidate, autoNewBranches bool) DiscoveryDecision {
	return m.decideCandidate(candidate, branchPolicy{autoAdmitNewBranches: autoNewBranches})
}

// branchPolicy names the two axes on which stored-row matching and
// discovery matching differ, so the call sites read as intent rather than as a
// pair of positional booleans with more combinations than legal states.
type branchPolicy struct {
	// admitUnknownBranch admits a session whose branch is unknown ("") against
	// a project that pins an explicit branch list. A stored row may legitimately
	// predate branch capture; a freshly discovered session may not.
	admitUnknownBranch bool
	// autoAdmitNewBranches admits a known branch that is outside a project's
	// explicit branch list.
	autoAdmitNewBranches bool
}

func (m SelectionMatcher) decideCandidate(candidate DiscoveryCandidate, policy branchPolicy) DiscoveryDecision {
	pm, ok := m.harnesses[candidate.Harness.String()]
	if !ok {
		return DiscoveryDecision{Match: BranchMatchNo} // harness not in selection
	}

	// Harness present with no restrictions → select all for this harness.
	if len(pm.projects) == 0 && len(pm.sessions) == 0 {
		return DiscoveryDecision{Match: BranchMatchYes}
	}

	// Explicit session ID allowlist → selected, branch-independent.
	if pm.sessions[string(candidate.SessionID)] {
		return DiscoveryDecision{Match: BranchMatchYes}
	}

	projects := matchingProjects(pm.projects, candidate)
	if len(projects) == 0 {
		return DiscoveryDecision{Match: BranchMatchNo}
	}

	decision := DiscoveryDecision{}
	for _, proj := range projects {
		admitted := proj.admitsBranch(candidate.Branch)
		if len(proj.branches) > 0 && candidate.Branch == "" && !policy.admitUnknownBranch {
			admitted = false
		}
		if len(proj.branches) > 0 && candidate.Branch != "" && policy.autoAdmitNewBranches {
			admitted = true
		}
		if admitted {
			decision.Admitting = append(decision.Admitting, proj.entry)
		} else {
			decision.Rejecting = append(decision.Rejecting, proj.entry)
		}
	}
	switch {
	case len(decision.Admitting) == 0 && len(decision.Rejecting) == 0:
		decision.Match = BranchMatchNo // no entry identifies the session
	case len(decision.Rejecting) == 0:
		decision.Match = BranchMatchYes // all matching projects admit
	case len(decision.Admitting) == 0:
		decision.Match = BranchMatchNo // all matching projects reject
	default:
		decision.Match = BranchMatchWithheldConflict // matching projects disagree
	}
	return decision
}

func matchingProjects(projects []projectMatcher, candidate DiscoveryCandidate) []projectMatcher {
	if candidate.ClonePath != "" {
		exact := make([]projectMatcher, 0, len(projects))
		for _, project := range projects {
			if project.matchesClonePath(candidate.ClonePath) {
				exact = append(exact, project)
			}
		}
		if len(exact) > 0 {
			return exact
		}
	}

	normalizedRemote := NormalizeRemoteForMatch(candidate.GitRemote)
	if normalizedRemote != "" {
		if candidate.RemoteMultiplicity != DiscoveryIdentityUnique {
			return nil
		}
		remoteMatches := make([]projectMatcher, 0, len(projects))
		remoteIsPathBound := false
		for _, project := range projects {
			if !project.matchesRemote(normalizedRemote) {
				continue
			}
			if project.pathBound {
				remoteIsPathBound = true
				continue
			}
			remoteMatches = append(remoteMatches, project)
		}
		if len(remoteMatches) > 0 {
			// Preserve legacy remote-or-name branch conflict semantics for every
			// path-unbound entry. Do not append entries already selected by the
			// same remote.
			if candidate.NameMultiplicity == DiscoveryIdentityUnique {
				normalizedName := NormalizeProjectNameForMatch(candidate.ProjectName)
				for _, project := range projects {
					if !project.pathBound && !project.matchesRemote(normalizedRemote) && project.matchesName(normalizedName) {
						remoteMatches = append(remoteMatches, project)
					}
				}
			}
			return remoteMatches
		}
		if remoteIsPathBound {
			return nil
		}
	}

	if candidate.NameMultiplicity != DiscoveryIdentityUnique {
		return nil
	}
	normalizedName := NormalizeProjectNameForMatch(candidate.ProjectName)
	if normalizedName == "" {
		return nil
	}
	nameMatches := make([]projectMatcher, 0, len(projects))
	for _, project := range projects {
		if !project.pathBound && project.matchesName(normalizedName) {
			nameMatches = append(nameMatches, project)
		}
	}
	return nameMatches
}

// PruneResult holds the outcome of a prune operation.
type PruneResult struct {
	Deleted int
	Errors  []error
}

// PruneStore provides session query and deletion for the prune command.
type PruneStore interface {
	QueryPrunableSessions(ctx context.Context, filter PruneFilter) ([]PruneSessionRow, error)
	PruneSessions(ctx context.Context, sessionIDs []SessionID) (PruneResult, error)
}

// --- IngestLogEntry ---

// IngestLogEntry records one peasant ingest run for audit purposes.
// Written at the end of pipeline.Run(). FinishedAt is nil only if
// the pipeline panicked before reaching the REPORT stage.
// It maps 1:1 to a row in the ingest_log table.
type IngestLogEntry struct {
	ID                int64
	StartedAt         int64  // Unix millis — captured at top of Run()
	FinishedAt        *int64 // Unix millis — nil if pipeline did not complete
	SessionsNew       int
	SessionsUpdated   int
	SessionsUnchanged int
	SessionsError     int
	IndexedCount      int
	ComputedCount     int
	ErrorMessage      *string // non-nil if Run() returned an error
	SourcePath        *string // PipelineConfig.SourcePath
}
