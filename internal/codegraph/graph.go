// Package codegraph builds a deterministic, language-aware import graph for a
// repository snapshot.
//
// The package is pure: it never touches git, the store, or the local
// filesystem directly. All file content arrives through an injected
// FileReader, which abstracts "worktree read" vs "file at commit" for the
// caller (internal/codemap).
//
// The model is a three-level node tree derived from file paths:
//
//   - module:  top-level directories ("cmd", "internal", "web")
//   - package: nested directories ("internal/ingest", "web/src/lib")
//   - file:    leaves ("internal/ingest/pipeline.go")
//
// Import edges are extracted per file (Go via the stdlib go/parser in
// ImportsOnly mode; TypeScript/TSX via tree-sitter) and aggregated up to
// package (directory) level. Layer/Order assignment is deterministic: the
// same input always yields the same layout — see layering.go for the
// algorithm.
package codegraph

import (
	"context"
	"maps"
	"slices"
)

// FileReader abstracts worktree vs at-commit reads. Implementations return
// the content of the repo-relative path, or an error when the file cannot
// be read. Unreadable files still produce graph nodes (with zero Loc) but
// contribute no import edges.
type FileReader func(path string) ([]byte, error)

// Builder constructs a Graph from a list of repo-relative file paths.
type Builder interface {
	Build(ctx context.Context, files []string, read FileReader) (*Graph, error)
}

// NodeKind classifies a graph node within the path-derived tree.
type NodeKind string

// NodeKind values. Top-level directories are modules, nested directories are
// packages, and files are leaves.
const (
	NodeKindModule  NodeKind = "module"
	NodeKindPackage NodeKind = "package"
	NodeKindFile    NodeKind = "file"
)

// String returns the wire representation of the node kind.
func (k NodeKind) String() string { return string(k) }

// IsValid reports whether k is one of the defined node kinds.
func (k NodeKind) IsValid() bool {
	switch k {
	case NodeKindModule, NodeKindPackage, NodeKindFile:
		return true
	}
	return false
}

// Language identifies the source language of a file node. Directory nodes
// carry a language only when every source file beneath them shares one.
type Language string

// Languages with import extraction support.
const (
	LanguageGo         Language = "go"
	LanguageTypeScript Language = "typescript"
)

// String returns the wire representation of the language.
func (l Language) String() string { return string(l) }

// Node is one element of the path-derived module/package/file tree.
type Node struct {
	ID        string   // repo-relative path ("internal/ingest", "web/src/lib/api.ts")
	Parent    string   // ID of the parent node; "" for top-level entries
	Kind      NodeKind // module | package | file
	Name      string   // display leaf ("ingest", "api.ts")
	Language  Language // file language, or the uniform descendant language for dirs
	Layer     int      // 0 = top row; deterministic (see layering.go)
	Order     int      // stable sort position within the layer
	Loc       int      // line count (rolled up for directories)
	FileCount int      // 1 for files; descendant file count for directories
}

// Edge is a package-level import dependency: From imports To. Count is the
// number of underlying file-level import statements aggregated into the edge.
type Edge struct {
	From  string
	To    string
	Count int
}

// ViolationKind classifies a structural violation detected during layering.
type ViolationKind string

// ViolationKind values. Cycle violations are edges that participate in an
// import cycle (a strongly connected component with more than one member).
// Wrong-way violations are edges that point upward (or sideways) after
// layering — only possible via the cmd/ layer-0 pin, since longest-path
// layering otherwise guarantees every DAG edge points downward.
const (
	ViolationCycle    ViolationKind = "cycle"
	ViolationWrongWay ViolationKind = "wrong_way"
)

// String returns the wire representation of the violation kind.
func (k ViolationKind) String() string { return string(k) }

// IsValid reports whether k is one of the defined violation kinds.
func (k ViolationKind) IsValid() bool {
	switch k {
	case ViolationCycle, ViolationWrongWay:
		return true
	}
	return false
}

// Violation flags a package-level edge that breaks the layering discipline.
type Violation struct {
	Kind ViolationKind
	From string
	To   string
}

// Graph is the full import graph for one repository snapshot. All slices are
// sorted deterministically: Nodes by ID, Edges by (From, To), Violations by
// (Kind, From, To). Slices are never nil.
type Graph struct {
	Nodes      []Node
	Edges      []Edge
	Violations []Violation
}

// Languages returns the sorted, distinct set of non-empty file-node languages
// present in the graph (e.g. ["go", "typescript"]).
func (g *Graph) Languages() []string {
	set := make(map[string]bool)
	for _, n := range g.Nodes {
		if n.Kind == NodeKindFile && n.Language != "" {
			set[n.Language.String()] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}
