// Package codemap is the service layer behind the Map and Review surfaces.
// It composes three lower layers into the github.com/peasant-labs/schema wire
// payloads:
//
//   - internal/store  — recorded activity: sessions, indexed entries,
//     session commits, metrics, annotations;
//   - internal/gitops — repo state: branches, merge state, files at refs;
//   - internal/codegraph — the parsed import graph (structure).
//
// All aggregation is computed live per request; no DB
// migration, no materialization); the only caching is the per-request
// projectData snapshot threaded through the helpers.
package codemap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// Sentinel errors. The API layer maps ErrProjectNotFound to 404; the others
// surface as request errors on the Review endpoints.
var (
	// ErrProjectNotFound means the projectHash is unknown to the store.
	ErrProjectNotFound = errors.New("codemap: project not found")
	// ErrProjectAmbiguous means a legacy display label identifies more than
	// one stored project. Callers must use a canonical hash URL.
	ErrProjectAmbiguous = errors.New("codemap: project identity is ambiguous")
	// ErrProjectIdentityInvalid means the requested identity is neither a
	// valid canonical hash nor a safe legacy display label.
	ErrProjectIdentityInvalid = errors.New("codemap: project identity is malformed")
	// ErrNodeNotFound means the requested node path is in neither the parsed
	// graph nor the recorded-activity universe.
	ErrNodeNotFound = errors.New("codemap: node not found")
	// ErrRepoNotFound means the project's canonical_cwd did not resolve to a
	// git repository (Review detail requires one; Map degrades gracefully).
	ErrRepoNotFound = errors.New("codemap: repository not found")
	// ErrBranchNotFound means the requested branch is neither the default
	// branch nor one of the repo's local branches (Review detail).
	ErrBranchNotFound = errors.New("codemap: branch not found")
)

// headRef is the ref used when no explicit commit is requested. All file
// content is read with git show at the resolved ref — never from the
// worktree — so a Build is deterministic for a given repo state.
const headRef = "HEAD"

// Caps for rail/list payloads.
const (
	maxShapedBy        = 20  // MapNodeDetailPayload.ShapedBy
	maxNodeCommits     = 10  // MapNodeDetailPayload.RecentCommits
	maxNodeConnections = 8   // MapNodeDetailPayload.DependsOn / UsedBy
	maxProjectTasks    = 500 // ProjectTasksPayload.Tasks
	maxReviewCommits   = 200 // ReviewListPayload.RecentCommits
	maxMergedBranch    = 20  // merged branches in ReviewChanges

	// maxStructureDeltaBranches caps how many open branches get the full
	// base-vs-head structure diff in ReviewChanges (two codegraph builds per
	// branch — the dominant cost of the endpoint). The most recently active
	// branches by tip commit time are diffed; the rest keep zero structure
	// columns (NewEdges/RemovedEdges/Violations = 0).
	maxStructureDeltaBranches = 20

	// maxGraphCacheEntries bounds the SHA-keyed graph memo. When full the
	// whole map is dropped (generation cache): hot keys — the shared
	// merge-base graph above all — repopulate on the next build.
	maxGraphCacheEntries = 32

	// Search caps. A query shorter than searchMinQueryLen (after
	// trimming) short-circuits to an empty payload without touching the index.
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	searchMinQueryLen  = 2
)

// Service computes the Map/Review payloads for one peasant store.
type Service struct {
	store   *store.Store
	repoFor func(path string) gitops.Repository
	builder codegraph.Builder

	// nowMs stamps MapGraphPayload.GeneratedAtMs; injectable for tests.
	nowMs func() int64

	// onVisibilityQuery, if set, is called once per projectHasVisibleSession
	// invocation — nil in production. It exists solely so tests can count
	// calls into that seam and prove ResolveProject's not-found and
	// hidden-match legacy-label branches both route through it the same
	// number of times — closing the residual DB-query-count timing
	// side-channel between those two failure paths.
	onVisibilityQuery func()

	// graphCache memoizes codegraph builds keyed by "repoPath@SHA". A commit
	// SHA pins the entire tree, so a graph built at a resolved SHA never goes
	// stale — safe to share across requests. Guarded by graphMu; cached
	// graphs are treated as immutable after Build.
	graphMu    sync.Mutex
	graphCache map[string]*codegraph.Graph
	visibility sessionvisibility.Policy
}

// NewService wires the service. repoFor turns a project's canonical_cwd into
// a gitops.Repository (production: gitops.NewExecGitRepository; tests:
// testutil.StubGitRepository).
func NewService(s *store.Store, repoFor func(path string) gitops.Repository, builder codegraph.Builder, visibility sessionvisibility.Policy) *Service {
	return &Service{
		store:      s,
		repoFor:    repoFor,
		builder:    builder,
		nowMs:      func() int64 { return time.Now().UnixMilli() },
		graphCache: make(map[string]*codegraph.Graph),
		visibility: visibility,
	}
}

// MapGraph builds the full map graph for a project. commit is the optional
// ?commit= ref ("" = HEAD). Unknown projectHash returns ErrProjectNotFound;
// a known project whose canonical_cwd is not a git repo degrades to an
// activity-only graph with RepoFound=false.
func (s *Service) MapGraph(ctx context.Context, projectHash schema.ProjectHash, commit string) (*schema.MapGraphPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}

	asm, err := s.assemble(ctx, pd, commit)
	if err != nil {
		return nil, err
	}

	payload := asm.toPayload(projectHash)
	payload.GeneratedAtMs = s.nowMs()
	payload.AtCommit = commit
	return payload, nil
}

// MapNodeDetail builds the rail panel for one node (identified by its
// repo-relative path). Returns ErrNodeNotFound when the path is in neither
// the parsed graph nor the activity universe.
func (s *Service) MapNodeDetail(ctx context.Context, projectHash schema.ProjectHash, path string) (*schema.MapNodeDetailPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}

	asm, err := s.assemble(ctx, pd, "")
	if err != nil {
		return nil, err
	}

	node, ok := asm.nodes[path]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNodeNotFound, path)
	}

	return s.nodeDetail(pd, asm, node)
}

// ProjectTasks lists the project's tasks, reverse-chronological, capped at
// 500. fileFilter ("" = all) restricts to tasks that edited the given file
// or any file under the given directory path.
func (s *Service) ProjectTasks(ctx context.Context, projectHash schema.ProjectHash, fileFilter string) (*schema.ProjectTasksPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}

	payload := schema.NewProjectTasksPayload(projectHash)
	payload.FileFilter = fileFilter

	tasks := pd.tasks
	if fileFilter != "" {
		filtered := make([]taskData, 0, len(tasks))
		for _, t := range tasks {
			if t.touchesPath(fileFilter) {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	sorted := sortTasksReverseChron(tasks)
	if len(sorted) > maxProjectTasks {
		sorted = sorted[:maxProjectTasks]
	}
	for _, t := range sorted {
		payload.Tasks = append(payload.Tasks, pd.taskSummary(t))
	}
	return payload, nil
}

// ChangeDiff builds the rendered unified diff of one changed file of a branch
// (lazy per-file companion to ChangeDetail). Same error contract as
// ChangeDetail (ErrProjectNotFound / ErrRepoNotFound / ErrBranchNotFound).
func (s *Service) ChangeDiff(ctx context.Context, projectHash schema.ProjectHash, branch, file string) (*schema.ChangeDiffPayload, error) {
	pd, err := s.loadProjectData(ctx, projectHash)
	if err != nil {
		return nil, err
	}
	return s.changeDiff(ctx, pd, branch, file)
}

// Search runs a global full-text search over recorded (redacted) message
// entries. query is raw user input; it is sanitized into an
// FTS5 MATCH string (whitespace tokens, each quoted, implicit AND) so arbitrary
// input — FTS5 operators, colons, unbalanced quotes — never errors. A query
// shorter than searchMinQueryLen (after trimming) returns an empty payload
// without touching the index. limit defaults to searchDefaultLimit and is
// clamped to searchMaxLimit. Search is global (no projectHash), so it has no
// ErrProjectNotFound path.
func (s *Service) Search(ctx context.Context, query string, limit int) (*schema.SearchPayload, error) {
	payload := schema.NewSearchPayload(query)

	if len(strings.TrimSpace(query)) < searchMinQueryLen {
		return payload, nil
	}
	match := sanitizeFTSQuery(query)
	if match == "" {
		return payload, nil
	}

	switch {
	case limit <= 0:
		limit = searchDefaultLimit
	case limit > searchMaxLimit:
		limit = searchMaxLimit
	}

	// The caller's limit is semantic: it caps visible results, not raw FTS
	// rows. Page by the same bounded size, advance strictly by raw rows, and
	// keep scanning hidden prefixes until the visible limit is filled or the
	// ranked source is exhausted.
	pageSize := limit
	rawOffset := 0
	for len(payload.Results) < limit {
		rows, err := s.querySearch(ctx, match, pageSize, rawOffset)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}

		for _, r := range rows {
			visible, visibilityErr := s.visibility.Visible(sessionvisibility.Candidate{
				SessionID:   ingest.SessionID(r.sessionID),
				Harness:     defaults.Harness(r.harness),
				GitRemote:   r.gitRemote,
				ProjectName: r.projectName,
				GitBranch:   r.gitBranch,
			})
			if visibilityErr != nil {
				return nil, fmt.Errorf(
					"codemap: search visibility failed for session %q at raw offset %d while filling the visible result limit; no partial search payload was returned because the persisted kickstart selection could not be evaluated safely; repair the selection with `peasant kickstart` and retry: %w",
					r.sessionID, rawOffset, visibilityErr,
				)
			}
			if !visible {
				continue
			}
			payload.Results = append(payload.Results, schema.SearchResult{
				SessionID:   r.sessionID,
				Project:     r.project,
				ProjectHash: r.hash,
				EntryIndex:  r.entryIndex,
				Role:        r.role,
				Snippet:     r.snippet,
				// bm25 is negative (more negative = better); negate so higher =
				// more relevant. Result order is authoritative regardless.
				Score: -r.bm25,
			})
			if len(payload.Results) == limit {
				break
			}
		}

		nextOffset := rawOffset + len(rows)
		if nextOffset <= rawOffset {
			return nil, fmt.Errorf(
				"codemap: search paging did not advance beyond raw offset %d after receiving %d rows while filling the visible result limit; no partial payload was returned because continuing could loop forever; update peasant and retry",
				rawOffset, len(rows),
			)
		}
		rawOffset = nextOffset
		if len(rows) < pageSize {
			break
		}
	}
	return payload, nil
}

// sanitizeFTSQuery turns raw user input into a safe FTS5 MATCH string: each
// whitespace-separated token is wrapped in double quotes (embedded quotes
// doubled per FTS5 string-literal rules), and the quoted tokens are joined by
// spaces — implicit AND. Quoting every token means FTS5 operators (AND/OR/NOT,
// NEAR, column filters, prefixes) and stray punctuation are matched literally
// rather than parsed, so no user input can raise an FTS5 syntax error.
//
// Tokens with no letter or digit (pure punctuation like "()" or "--") are
// dropped: the unicode61 tokenizer would reduce them to a zero-token phrase,
// and a MATCH of only empty phrases is itself an error. Returns "" when nothing
// usable remains (the caller then short-circuits to an empty result set).
func sanitizeFTSQuery(query string) string {
	var quoted []string
	for _, f := range strings.Fields(query) {
		if !strings.ContainsFunc(f, func(r rune) bool {
			return unicode.IsLetter(r) || unicode.IsNumber(r)
		}) {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

// sortTasksReverseChron orders tasks newest-first by StartMs (tasks without
// a timestamp sort last), tie-broken by (sessionID, entryIndex) for
// determinism. Returns a new slice; the input is not mutated.
func sortTasksReverseChron(tasks []taskData) []taskData {
	sorted := make([]taskData, len(tasks))
	copy(sorted, tasks)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		switch {
		case a.startMs != nil && b.startMs == nil:
			return true
		case a.startMs == nil && b.startMs != nil:
			return false
		case a.startMs != nil && b.startMs != nil && *a.startMs != *b.startMs:
			return *a.startMs > *b.startMs
		}
		if a.sessionID != b.sessionID {
			return a.sessionID < b.sessionID
		}
		return a.entryIndex > b.entryIndex
	})
	return sorted
}
