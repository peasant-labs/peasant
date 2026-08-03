package codegraph

import (
	goparser "go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
)

// goModFileName is the Go module definition file consulted for intra-repo
// import resolution. Its content arrives through the FileReader.
const goModFileName = "go.mod"

// goModule maps a module path declared by a go.mod file to the repo-relative
// directory that contains it ("" = repository root). Multiple modules are
// supported (e.g. a nested pkg/ module).
type goModule struct {
	dir        string
	modulePath string
}

// collectGoModules finds every go.mod in the file list and parses its module
// path. When the root go.mod is not in the list, a direct read("go.mod") is
// attempted so callers can supply it via the FileReader alone. The result is
// sorted by descending module-path length so the most specific module wins
// during resolution.
func collectGoModules(paths []string, contents map[string][]byte, read FileReader) []goModule {
	var mods []goModule
	rootSeen := false
	for _, p := range paths {
		if path.Base(p) != goModFileName {
			continue
		}
		data, ok := contents[p]
		if !ok {
			continue
		}
		modulePath := parseGoModulePath(data)
		if modulePath == "" {
			continue
		}
		dir := parentDir(p)
		if dir == "" {
			rootSeen = true
		}
		mods = append(mods, goModule{dir: dir, modulePath: modulePath})
	}
	if !rootSeen {
		if data, err := read(goModFileName); err == nil {
			if modulePath := parseGoModulePath(data); modulePath != "" {
				mods = append(mods, goModule{dir: "", modulePath: modulePath})
			}
		}
	}
	sort.Slice(mods, func(i, j int) bool {
		if len(mods[i].modulePath) != len(mods[j].modulePath) {
			return len(mods[i].modulePath) > len(mods[j].modulePath)
		}
		return mods[i].modulePath < mods[j].modulePath
	})
	return mods
}

// parseGoModulePath extracts the module path from go.mod content. Returns ""
// when no module directive is found.
func parseGoModulePath(content []byte) string {
	for line := range strings.Lines(string(content)) {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		rest = strings.Trim(strings.TrimSpace(rest), `"`)
		if rest != "" {
			return rest
		}
	}
	return ""
}

// resolveGoImport maps an import path to a repo-relative package directory
// using the known module paths (most specific first). Returns false for
// external imports.
func resolveGoImport(importPath string, mods []goModule) (string, bool) {
	for _, m := range mods {
		if importPath == m.modulePath {
			return m.dir, true
		}
		if rest, ok := strings.CutPrefix(importPath, m.modulePath+"/"); ok {
			return path.Join(m.dir, rest), true
		}
	}
	return "", false
}

// goFileImports parses a Go file in ImportsOnly mode and returns its import
// paths. Files that fail to parse contribute no imports (graceful skip — the
// file node still exists in the graph).
func goFileImports(filePath string, src []byte) []string {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, filePath, src, goparser.ImportsOnly)
	if err != nil || f == nil {
		return nil
	}
	out := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
