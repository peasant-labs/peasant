//go:build !cgo

package codegraph

import "errors"

// TSAnalysisAvailable reports whether TypeScript import extraction (the
// tree-sitter C bindings) is compiled into this binary. Mirrors
// redact.MaximumAvailable: false in !cgo builds — Build over a repository
// containing TypeScript sources returns an actionable error.
const TSAnalysisAvailable = false

// tsExtractor's real implementation is tree-sitter-backed and lives in
// lang_ts_cgo.go (cgo builds only). This stub exists so the package compiles
// with CGO_ENABLED=0 — the release binaries are static !cgo builds, and a
// code graph over a Go-only repository must keep working there (the extractor
// is constructed lazily, only when a TypeScript file is encountered).
type tsExtractor struct{}

// newTSExtractor fails actionably in !cgo builds: TypeScript import
// extraction needs the tree-sitter C bindings, which are not linked. Same
// doctrine as the public redaction module's Maximum level (maximum_nocgo.go) — an explicit
// error at construction, never a silent skip that would render a partial
// graph as if it were complete.
func newTSExtractor() (*tsExtractor, error) {
	return nil, errors.New(
		"codegraph: TypeScript analysis is unavailable in this binary: it was built " +
			"without cgo (CGO_ENABLED=0), and TypeScript import extraction requires the " +
			"tree-sitter C bindings. The repository contains TypeScript sources, so the " +
			"code graph cannot be built completely. Use a cgo-enabled peasant build for " +
			"TypeScript code graphs (Go-only repositories are unaffected)")
}

// close and imports are UNREACHABLE in !cgo builds: newTSExtractor always
// errors before an extractor exists. They panic loud (redact idiom) so a
// regression of that guard surfaces immediately instead of silently
// mis-rendering the graph.
func (e *tsExtractor) close() {
	panic("codegraph: BUG — tsExtractor.close() called in a !cgo build; newTSExtractor must error before construction")
}

func (e *tsExtractor) imports(string, []byte) ([]string, error) {
	panic("codegraph: BUG — tsExtractor.imports() called in a !cgo build; newTSExtractor must error before construction")
}
