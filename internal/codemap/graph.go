package codemap

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/schema"
)

// Source-file filter for the structure parse: only
// Go and TypeScript sources feed codegraph, plus the resolution manifests
// (go.mod for Go module paths, tsconfig.json for TS path aliases).
const (
	goSourceExt  = ".go"
	tsSourceExt  = ".ts"
	tsxSourceExt = ".tsx"

	goModFile    = "go.mod"
	tsConfigFile = "tsconfig.json"
)

// assembly is the merged Map model for one (project, ref) pair: the parsed
// structure graph plus the recorded-activity overlay.
type assembly struct {
	repoFound bool
	repoPath  string
	ref       string

	graph           *codegraph.Graph // nil when !repoFound
	parsedLanguages []string

	nodes    map[string]*schema.MapNode
	stats    map[string]*fileStats
	cov      coverage
	edges    []schema.MapEdge
	actEdges []schema.ActivityEdge
	viols    []schema.EdgeViolation
}

// assemble builds the merged map for the project at the given commit
// ("" = HEAD). All file content is read at the resolved ref via git show —
// never the worktree — so identical inputs produce identical assemblies.
func (s *Service) assemble(ctx context.Context, pd *projectData, commit string) (*assembly, error) {
	ref := commit
	if ref == "" {
		ref = headRef
	}

	asm := &assembly{
		ref:             ref,
		parsedLanguages: []string{},
		nodes:           make(map[string]*schema.MapNode),
		stats:           computeFileStats(pd.tasks),
	}

	// Repo resolution: canonical_cwd -> gitops. A failing ListFiles means
	// the path is not a git repo (or the ref is unknown) — degrade to the
	// activity-only graph (known hash + no repo => RepoFound
	// false, structure arrays empty, activity data still served).
	var trackedFiles []string
	if pd.cwd != "" {
		repo := s.repoFor(pd.cwd)
		files, err := repo.ListFiles(ctx, ref)
		if err == nil {
			asm.repoFound = true
			asm.repoPath = pd.cwd
			trackedFiles = files

			graph, buildErr := s.buildGraphAt(ctx, repo, pd.cwd, ref, files)
			if buildErr != nil {
				return nil, buildErr
			}
			asm.graph = graph
			asm.parsedLanguages = graph.Languages()
		}
	}

	// 1. Parsed structure nodes.
	if asm.graph != nil {
		for i := range asm.graph.Nodes {
			n := mapNodeFromGraph(&asm.graph.Nodes[i])
			asm.nodes[n.ID] = n
		}
		asm.edges = make([]schema.MapEdge, 0, len(asm.graph.Edges))
		for _, e := range asm.graph.Edges {
			asm.edges = append(asm.edges, schema.MapEdge{From: e.From, To: e.To, Count: e.Count})
		}
		asm.viols = make([]schema.EdgeViolation, 0, len(asm.graph.Violations))
		for _, v := range asm.graph.Violations {
			asm.viols = append(asm.viols, schema.EdgeViolation{
				Kind: schema.EdgeViolationKind(v.Kind),
				From: v.From,
				To:   v.To,
			})
		}
	} else {
		asm.edges = []schema.MapEdge{}
		asm.viols = []schema.EdgeViolation{}
	}

	// 2. Activity-only nodes: files edited in sessions but absent from the
	// parsed graph (unparseable language, or missing at the ref).
	mergeActivityNodes(asm.nodes, asm.stats)

	// 3. Coverage + per-node metric fill.
	asm.cov = computeCoverage(asm.repoFound, trackedFiles, asm.stats)
	fillNodeMetrics(asm.nodes, asm.cov, asm.stats)

	// 4. Activity edges (both endpoints exist by construction: every edited
	// file's package became a node in step 1 or 2).
	asm.actEdges = computeActivityEdges(pd.tasks)

	return asm, nil
}

// buildGraphAt runs codegraph over the source files of one ref, reading
// every file with git show at that ref. Builds are memoized by resolved
// commit SHA (a SHA pins the whole tree, so the memo never goes stale):
// the shared merge-base graph in ReviewChanges is built once instead of
// once per branch. Refs that cannot be resolved to a SHA build uncached.
func (s *Service) buildGraphAt(ctx context.Context, repo gitops.Repository, repoPath, ref string, files []string) (*codegraph.Graph, error) {
	key := s.graphCacheKey(ctx, repo, repoPath, ref)
	if key != "" {
		s.graphMu.Lock()
		cached, ok := s.graphCache[key]
		s.graphMu.Unlock()
		if ok {
			return cached, nil
		}
	}

	filtered := filterSourceFiles(files)

	// Prefetch all source contents in ONE git call (cat-file --batch): the
	// per-file `git show` exec overhead dominates build time on real repos.
	// A failing batch degrades to per-file reads instead of failing the build.
	reader := func(p string) ([]byte, error) {
		return repo.FileAtCommit(ctx, ref, p)
	}
	if prefetched, preErr := repo.FilesAtCommit(ctx, ref, filtered); preErr == nil {
		reader = func(p string) ([]byte, error) {
			if c, ok := prefetched[p]; ok {
				return c, nil
			}
			// Absent from the batch (missing at ref / non-blob): same
			// graceful degradation as a failing git show.
			return nil, fmt.Errorf("codemap: %s not present at %s", p, ref)
		}
	}

	graph, err := s.builder.Build(ctx, filtered, reader)
	if err != nil {
		return nil, err
	}

	if key != "" {
		s.graphMu.Lock()
		if len(s.graphCache) >= maxGraphCacheEntries {
			s.graphCache = make(map[string]*codegraph.Graph)
		}
		s.graphCache[key] = graph
		s.graphMu.Unlock()
	}
	return graph, nil
}

// graphCacheKey resolves ref to "repoPath@SHA" for the graph memo, or ""
// when the ref cannot be resolved to a commit SHA (symbolic refs in repos
// where the tip lookup fails — those builds are simply not cached).
func (s *Service) graphCacheKey(ctx context.Context, repo gitops.Repository, repoPath, ref string) string {
	sha := ref
	if !isCommitSHA(sha) {
		commits, err := repo.Commits(ctx, ref, 1)
		if err != nil || len(commits) == 0 || !isCommitSHA(commits[0].Hash) {
			return ""
		}
		sha = commits[0].Hash
	}
	return repoPath + "@" + sha
}

// isCommitSHA reports whether s looks like a full commit hash (40 hex chars
// for SHA-1, 64 for SHA-256 repos).
func isCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// filterSourceFiles keeps the parseable sources (.go/.ts/.tsx) and the
// resolution manifests (go.mod, tsconfig.json).
func filterSourceFiles(files []string) []string {
	filtered := make([]string, 0, len(files))
	for _, f := range files {
		switch strings.ToLower(path.Ext(f)) {
		case goSourceExt, tsSourceExt, tsxSourceExt:
			filtered = append(filtered, f)
			continue
		}
		switch path.Base(f) {
		case goModFile, tsConfigFile:
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// mapNodeFromGraph converts a codegraph node to its wire form (the activity
// metrics are filled later by fillNodeMetrics).
func mapNodeFromGraph(n *codegraph.Node) *schema.MapNode {
	return &schema.MapNode{
		ID:              n.ID,
		Parent:          n.Parent,
		Kind:            schema.MapNodeKind(n.Kind),
		Name:            n.Name,
		Language:        n.Language.String(),
		Layer:           n.Layer,
		Order:           n.Order,
		Loc:             n.Loc,
		FileCount:       n.FileCount,
		ReadAttribution: schema.ReadAttributionUnavailable,
		ReadState:       schema.ReadStateGradeNone,
	}
}

// mergeActivityNodes adds nodes for recorded-edit files (and their missing
// ancestor directories) that the parsed graph does not contain.
//
// Layer assignment: parsed nodes keep
// their codegraph layers untouched. An activity-only node inherits the layer
// of its nearest ancestor that exists in the parsed graph (it renders inside
// that region); every node with NO parsed ancestor lands on one shared
// "root shelf" layer directly below the deepest parsed layer
// (maxParsedLayer+1; layer 0 for activity-only graphs where nothing was
// parsed). A single dense shelf keeps unparsed root files (AGENTS.md,
// CLAUDE.md, docs/, llm/) adjacent to the parsed cluster — the previous
// per-top-dir layer increments exiled each group to its own distant row.
// Order: within each layer, activity-only nodes are appended after the
// existing maximum order, in sorted-ID order. The whole assignment is
// deterministic.
func mergeActivityNodes(nodes map[string]*schema.MapNode, stats map[string]*fileStats) {
	// Missing IDs: edited files + their ancestor dirs not present yet.
	missingDirs := make(map[string]bool)
	missingFiles := make(map[string]bool)
	for _, f := range sortedKeys(stats) {
		for d := parentDir(f); d != ""; d = parentDir(d) {
			if _, ok := nodes[d]; !ok {
				missingDirs[d] = true
			}
		}
		if _, ok := nodes[f]; ok {
			continue
		}
		missingFiles[f] = true
	}
	// A path cannot be both a file and a directory; the directory reading wins
	// (mirrors codegraph's buildTree).
	for f := range missingFiles {
		if missingDirs[f] {
			delete(missingFiles, f)
		}
	}
	if len(missingDirs) == 0 && len(missingFiles) == 0 {
		return
	}

	maxParsedLayer := -1
	maxOrderPerLayer := make(map[int]int)
	for _, n := range nodes {
		if n.Layer > maxParsedLayer {
			maxParsedLayer = n.Layer
		}
		if n.Order > maxOrderPerLayer[n.Layer] {
			maxOrderPerLayer[n.Layer] = n.Order
		}
	}

	// Pre-compute layers against the PARSED nodes only, so insertion order
	// cannot influence the result.
	missing := make([]string, 0, len(missingDirs)+len(missingFiles))
	for d := range missingDirs {
		missing = append(missing, d)
	}
	for f := range missingFiles {
		missing = append(missing, f)
	}
	sort.Strings(missing)

	// Root shelf: the single layer for every missing ID with no parsed
	// ancestor (see the doc comment above).
	rootShelfLayer := maxParsedLayer + 1

	layers := make(map[string]int, len(missing))
	for _, id := range missing {
		if layer, ok := nearestExistingLayer(nodes, id); ok {
			layers[id] = layer
		} else {
			layers[id] = rootShelfLayer
		}
	}

	// Insert in sorted-ID order, assigning orders after the existing maxima.
	for _, id := range missing {
		layer := layers[id]
		order := maxOrderPerLayer[layer] + 1
		maxOrderPerLayer[layer] = order

		kind := schema.MapNodeKindFile
		if missingDirs[id] {
			kind = schema.MapNodeKindPackage
			if !strings.Contains(id, "/") {
				kind = schema.MapNodeKindModule
			}
		}
		fileCount := 0
		if kind == schema.MapNodeKindFile {
			fileCount = 1
		}
		nodes[id] = &schema.MapNode{
			ID:              id,
			Parent:          parentDir(id),
			Kind:            kind,
			Name:            path.Base(id),
			Layer:           layer,
			Order:           order,
			FileCount:       fileCount,
			ReadAttribution: schema.ReadAttributionUnavailable,
			ReadState:       schema.ReadStateGradeNone,
		}
	}
}

// nearestExistingLayer walks up the ancestor chain looking for a node that
// already exists (i.e. came from the parsed graph) and returns its layer.
func nearestExistingLayer(nodes map[string]*schema.MapNode, id string) (int, bool) {
	for d := parentDir(id); d != ""; d = parentDir(d) {
		if n, ok := nodes[d]; ok {
			return n.Layer, true
		}
	}
	return 0, false
}

// fillNodeMetrics rolls the coverage universe and per-file activity up the
// node tree:
//
//   - TotalFiles / RecordedFiles count the coverage-universe files at or
//     below each node (the universe includes tracked-but-unparsed files, so
//     TotalFiles can exceed FileCount);
//   - TouchCount sums recorded edit events at or below each node;
//   - EffortDensity is each node's per-file effort score (re-edits +
//     error-adjacent edits) normalized by the maximum node score, 0..1
//     (0 everywhere when no effort was recorded).
func fillNodeMetrics(nodes map[string]*schema.MapNode, cov coverage, stats map[string]*fileStats) {
	bump := func(file string, fn func(n *schema.MapNode)) {
		if n, ok := nodes[file]; ok {
			fn(n)
		}
		for d := parentDir(file); d != ""; d = parentDir(d) {
			if n, ok := nodes[d]; ok {
				fn(n)
			}
		}
	}

	for _, f := range cov.universe {
		recorded := cov.recorded[f]
		bump(f, func(n *schema.MapNode) {
			n.TotalFiles++
			if recorded {
				n.RecordedFiles++
			}
		})
	}

	effort := make(map[string]int, len(nodes))
	for _, f := range sortedKeys(stats) {
		fs := stats[f]
		score := fs.effortScore()
		bump(f, func(n *schema.MapNode) {
			n.TouchCount += fs.editCount
			effort[n.ID] += score
		})
	}

	maxEffort := 0
	for _, score := range effort {
		if score > maxEffort {
			maxEffort = score
		}
	}
	if maxEffort == 0 {
		return
	}
	for id, score := range effort {
		nodes[id].EffortDensity = float64(score) / float64(maxEffort)
	}
}

// toPayload flattens the assembly into the wire payload, nodes sorted by ID.
func (a *assembly) toPayload(projectHash schema.ProjectHash) *schema.MapGraphPayload {
	payload := schema.NewMapGraphPayload(projectHash)
	payload.RepoFound = a.repoFound
	payload.RepoPath = a.repoPath
	payload.ParsedLanguages = append(payload.ParsedLanguages, a.parsedLanguages...)
	for _, id := range sortedKeys(a.nodes) {
		payload.Nodes = append(payload.Nodes, *a.nodes[id])
	}
	payload.StructureEdges = append(payload.StructureEdges, a.edges...)
	payload.ActivityEdges = append(payload.ActivityEdges, a.actEdges...)
	payload.Violations = append(payload.Violations, a.viols...)
	return payload
}

// sortedKeys returns the sorted keys of a map (determinism helper).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
