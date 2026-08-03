package codegraph

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"path"
	"slices"
	"strings"
)

// GraphBuilder is the production Builder. It parses Go imports with the
// stdlib go/parser (ImportsOnly) and TypeScript/TSX imports with tree-sitter,
// resolves them repo-relatively, and produces a deterministic Graph.
//
// A GraphBuilder is stateless; the zero value is ready to use, and a single
// instance may be shared across goroutines (each Build call creates its own
// parsers).
type GraphBuilder struct{}

// Compile-time guard: GraphBuilder must implement Builder.
var _ Builder = (*GraphBuilder)(nil)

// NewGraphBuilder returns a new GraphBuilder.
func NewGraphBuilder() *GraphBuilder { return &GraphBuilder{} }

// File extensions with import extraction support.
const (
	goExt  = ".go"
	tsExt  = ".ts"
	tsxExt = ".tsx"
)

// Build constructs the import graph for the given repo-relative file paths.
//
// Graceful degradation rules (all deterministic):
//   - unreadable files produce nodes with Loc 0 and no edges;
//   - files that fail to parse contribute no edges;
//   - external or unresolvable imports are skipped silently;
//   - files at the repository root cannot anchor package-level edges (there
//     is no root node) and contribute no edges.
func (b *GraphBuilder) Build(ctx context.Context, files []string, read FileReader) (*Graph, error) {
	if read == nil {
		return nil, errors.New("codegraph: Build requires a non-nil FileReader")
	}

	paths := normalizeFiles(files)

	contents := make(map[string][]byte, len(paths))
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data, err := read(p); err == nil {
			contents[p] = data
		}
	}

	tree := buildTree(paths, contents)
	mods := collectGoModules(paths, contents, read)
	cfgs := collectTSConfigs(paths, contents, read)

	edges, err := collectEdges(ctx, tree, contents, mods, cfgs)
	if err != nil {
		return nil, err
	}

	violations := applyLayout(tree, edges)

	nodes := make([]Node, 0, len(tree.nodes))
	for _, id := range slices.Sorted(maps.Keys(tree.nodes)) {
		nodes = append(nodes, *tree.nodes[id])
	}

	return &Graph{Nodes: nodes, Edges: edges, Violations: violations}, nil
}

// fileTree is the intermediate path-derived node tree built before layering.
type fileTree struct {
	nodes        map[string]*Node
	fileSet      map[string]bool // normalized file paths (excluding paths that are also dirs)
	dirSet       map[string]bool // every proper ancestor directory of every file
	dirHasSource map[string]bool // dir directly contains at least one Go/TS source file
}

// sortedFiles returns the file paths in deterministic (sorted) order.
func (t *fileTree) sortedFiles() []string {
	return slices.Sorted(maps.Keys(t.fileSet))
}

// normalizeFiles cleans, validates, dedupes, and sorts the input paths.
// Absolute paths and paths escaping the repo root are dropped.
func normalizeFiles(files []string) []string {
	set := make(map[string]bool, len(files))
	for _, f := range files {
		p := path.Clean(f)
		if p == "" || p == "." || p == ".." ||
			path.IsAbs(p) || strings.HasPrefix(p, "../") {
			continue
		}
		set[p] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// buildTree derives module/package/file nodes from the file paths and rolls
// Loc, FileCount, and uniform Language up the directory chain.
func buildTree(paths []string, contents map[string][]byte) *fileTree {
	t := &fileTree{
		nodes:        make(map[string]*Node),
		fileSet:      make(map[string]bool),
		dirSet:       make(map[string]bool),
		dirHasSource: make(map[string]bool),
	}

	for _, p := range paths {
		for d := parentDir(p); d != ""; d = parentDir(d) {
			t.dirSet[d] = true
		}
	}
	for _, p := range paths {
		// A path that is also an ancestor directory of another file cannot be
		// a file; treat it as a directory only.
		if !t.dirSet[p] {
			t.fileSet[p] = true
		}
	}

	for _, d := range slices.Sorted(maps.Keys(t.dirSet)) {
		kind := NodeKindPackage
		if !strings.Contains(d, "/") {
			kind = NodeKindModule
		}
		t.nodes[d] = &Node{ID: d, Parent: parentDir(d), Kind: kind, Name: path.Base(d)}
	}

	dirLangs := make(map[string]map[Language]bool)
	for _, p := range t.sortedFiles() {
		lang := languageForFile(p)
		loc := countLines(contents[p])
		dir := parentDir(p)
		t.nodes[p] = &Node{
			ID:        p,
			Parent:    dir,
			Kind:      NodeKindFile,
			Name:      path.Base(p),
			Language:  lang,
			Loc:       loc,
			FileCount: 1,
		}
		if lang != "" && dir != "" {
			t.dirHasSource[dir] = true
		}
		for d := dir; d != ""; d = parentDir(d) {
			n := t.nodes[d]
			n.Loc += loc
			n.FileCount++
			if lang != "" {
				if dirLangs[d] == nil {
					dirLangs[d] = make(map[Language]bool)
				}
				dirLangs[d][lang] = true
			}
		}
	}

	// A directory carries a language only when its source files are uniform.
	for d, set := range dirLangs {
		if len(set) == 1 {
			for lang := range set {
				t.nodes[d].Language = lang
			}
		}
	}

	return t
}

// collectEdges extracts file-level imports and aggregates them into
// package-level edges. Self-edges (imports within the same directory) and
// edges anchored at the repository root are dropped.
func collectEdges(
	ctx context.Context,
	tree *fileTree,
	contents map[string][]byte,
	mods []goModule,
	cfgs map[string]*tsConfig,
) ([]Edge, error) {
	counts := make(map[edgeKey]int)

	var ts *tsExtractor
	defer func() {
		if ts != nil {
			ts.close()
		}
	}()

	for _, p := range tree.sortedFiles() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		src, ok := contents[p]
		if !ok {
			continue
		}
		fromDir := parentDir(p)
		if fromDir == "" {
			continue // root-level files have no package node to anchor edges
		}

		switch languageForFile(p) {
		case LanguageGo:
			for _, imp := range goFileImports(p, src) {
				target, ok := resolveGoImport(imp, mods)
				if !ok || target == "" || target == fromDir || !tree.dirHasSource[target] {
					continue
				}
				counts[edgeKey{fromDir, target}]++
			}
		case LanguageTypeScript:
			if ts == nil {
				var err error
				if ts, err = newTSExtractor(); err != nil {
					return nil, err
				}
			}
			specs, err := ts.imports(p, src)
			if err != nil {
				continue // unparsable file: contributes no edges
			}
			for _, spec := range specs {
				resolved, ok := resolveTSImport(spec, fromDir, cfgs, tree.fileSet)
				if !ok {
					continue
				}
				toDir := parentDir(resolved)
				if toDir == "" || toDir == fromDir {
					continue
				}
				counts[edgeKey{fromDir, toDir}]++
			}
		}
	}

	edges := make([]Edge, 0, len(counts))
	for key, count := range counts {
		edges = append(edges, Edge{From: key.from, To: key.to, Count: count})
	}
	sortEdges(edges)
	return edges, nil
}

// parentDir returns the parent directory of a repo-relative path, or "" for
// top-level entries.
func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// languageForFile maps a file extension to a supported Language, or "" when
// the file has no import extraction support.
func languageForFile(p string) Language {
	switch strings.ToLower(path.Ext(p)) {
	case goExt:
		return LanguageGo
	case tsExt, tsxExt:
		return LanguageTypeScript
	}
	return ""
}

// countLines counts the lines in content: newline-terminated lines plus a
// trailing partial line.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}
