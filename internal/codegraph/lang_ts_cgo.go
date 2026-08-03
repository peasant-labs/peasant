//go:build cgo

// The tree-sitter half of TypeScript import extraction — compiled ONLY in cgo
// builds (the tree-sitter bindings are C). The pure-Go tsconfig/path-alias
// resolution machinery stays in lang_ts.go; the !cgo counterpart of this file
// is lang_ts_nocgo.go (actionable error at construction, same doctrine as
// the public redaction module's maximum_cgo.go / maximum_nocgo.go split).

package codegraph

import (
	"fmt"
	"path"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_ts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// TSAnalysisAvailable reports whether TypeScript import extraction (the
// tree-sitter C bindings) is compiled into this binary. Mirrors
// redact.MaximumAvailable: true only in cgo builds.
const TSAnalysisAvailable = true

// Package-level tree-sitter Language objects: thread-safe and expensive to
// initialize, so created once (same pattern as github.com/peasant-labs/redact).
var (
	tsLanguageTypescript = tree_sitter.NewLanguage(tree_sitter_ts.LanguageTypescript())
	tsLanguageTSX        = tree_sitter.NewLanguage(tree_sitter_ts.LanguageTSX())
)

// tsImportQuery captures the module specifier of static imports and
// re-exports. Dynamic import() and require() calls are deliberately out of
// scope — import extraction only, no semantic analysis.
const tsImportQuery = `(import_statement source: (string (string_fragment) @path))
(export_statement source: (string (string_fragment) @path))`

// tsExtractor owns the per-Build tree-sitter parser and compiled queries.
// Parsers are NOT thread-safe; an extractor is confined to one Build call.
type tsExtractor struct {
	parser   *tree_sitter.Parser
	queryTS  *tree_sitter.Query
	queryTSX *tree_sitter.Query
}

// newTSExtractor compiles the import queries and creates a parser. All
// tree-sitter C objects must be released via close().
func newTSExtractor() (*tsExtractor, error) {
	queryTS, qErr := tree_sitter.NewQuery(tsLanguageTypescript, tsImportQuery)
	if qErr != nil {
		return nil, fmt.Errorf("codegraph: compile typescript import query: %s", qErr.Error())
	}
	queryTSX, qErr := tree_sitter.NewQuery(tsLanguageTSX, tsImportQuery)
	if qErr != nil {
		queryTS.Close()
		return nil, fmt.Errorf("codegraph: compile tsx import query: %s", qErr.Error())
	}
	return &tsExtractor{
		parser:   tree_sitter.NewParser(),
		queryTS:  queryTS,
		queryTSX: queryTSX,
	}, nil
}

// close releases all tree-sitter C objects owned by the extractor.
func (e *tsExtractor) close() {
	e.parser.Close()
	e.queryTS.Close()
	e.queryTSX.Close()
}

// imports returns the raw module specifiers imported by a .ts/.tsx file.
func (e *tsExtractor) imports(filePath string, src []byte) ([]string, error) {
	lang, query := tsLanguageTypescript, e.queryTS
	if strings.ToLower(path.Ext(filePath)) == tsxExt {
		lang, query = tsLanguageTSX, e.queryTSX
	}
	if err := e.parser.SetLanguage(lang); err != nil {
		return nil, fmt.Errorf("codegraph: tree-sitter SetLanguage for %s: %w", filePath, err)
	}
	tree := e.parser.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("codegraph: tree-sitter returned nil tree for %s", filePath)
	}
	defer tree.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	var out []string
	matches := qc.Matches(query, tree.RootNode(), src)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		for _, node := range m.NodesForCaptureIndex(0) {
			if spec := string(node.Utf8Text(src)); spec != "" {
				out = append(out, spec)
			}
		}
	}
	return out, nil
}
