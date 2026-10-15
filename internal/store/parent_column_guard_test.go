package store

import (
	"bytes"
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/parent_column_sites.yaml
var parentColumnSitesYAML []byte

//go:embed testdata/parent_column_sites.manifest.yaml
var parentColumnSitesManifestYAML []byte

// parentColumnToken is the raw column this guard closes over. sessions.parent_id
// is a nullable FK availability cache; the parent ANSWER is the logical-parent
// rule. Every production declaration that still reads the raw column must be
// classified in testdata/parent_column_sites.yaml with its class and reason, and
// every declaration that answers the parent question must render the rule.
const parentColumnToken = "parent_id"

// parentColumnSite is one classified declaration. The unit is (file, enclosing
// top-level declaration) with the count of token-bearing string literals in it,
// so a new literal inside an already-classified declaration fails the guard
// exactly like a new declaration does.
type parentColumnSite struct {
	Name        string `yaml:"name"`
	File        string `yaml:"file"`
	Declaration string `yaml:"declaration"`
	Literals    int    `yaml:"literals"`
	Class       string `yaml:"class"`
	Reason      string `yaml:"reason"`
}

type parentColumnSitesDocument struct {
	Residual []parentColumnSite `yaml:"residual"`
	Routed   []parentColumnSite `yaml:"routed"`
	Foreign  []parentColumnSite `yaml:"foreign"`
}

type parentColumnSitesManifest struct {
	RequiredResidual []string `yaml:"requiredResidual"`
	RequiredRouted   []string `yaml:"requiredRouted"`
	RequiredForeign  []string `yaml:"requiredForeign"`
}

// The closed residual class set. A class outside it is a fixture error, never a
// silently accepted entry.
var parentColumnResidualClasses = map[string]bool{
	"renderer":          true,
	"availability":      true,
	"integrity-compare": true,
	"migration-oracle":  true,
	"write-guard":       true,
	"writers":           true,
	"ddl":               true,
	// cross-package is the class of a parent answer whose rendering belongs to
	// the cross-package reader slice: it is used only while that slice has not
	// yet routed the declaration, and it disappears from the fixture when those
	// entries move to the routed section. It stays in the closed set so the
	// guard can classify such a site instead of rejecting it.
	"cross-package": true,
}

var parentColumnForeignClasses = map[string]bool{
	"foreign-database": true,
	"non-column":       true,
}

// parsedParentColumnSite is one declaration found by parsing production source.
type parsedParentColumnSite struct {
	File        string
	Declaration string
	Literals    int
}

func (s parsedParentColumnSite) key() string {
	return s.File + "::" + s.Declaration
}

// TestParentColumnSitesAreClassified is the closure of the raw-column reader
// set. It parses every production Go file under internal/ and cmd/, collects
// every string literal that names the raw column, and asserts EXACT membership
// against the classified fixture: a new site fails until it is classified (by
// rendering the rule, or by recording it as a reasoned exception), a new literal
// inside a listed declaration fails on its count, and a removed site fails until
// the fixture and its manifest are updated. Every routed declaration is
// separately asserted to carry no token and to call the renderer, so a reader
// that reverts to a raw spelling reddens both directions.
func TestParentColumnSitesAreClassified(t *testing.T) {
	t.Parallel()
	document, manifest := loadParentColumnSites(t)
	root := parentColumnRepoRoot(t)
	parsed := parseParentColumnSites(t, root)

	declared := make(map[string]parentColumnSite)
	for _, site := range append(append([]parentColumnSite(nil), document.Residual...), document.Foreign...) {
		key := site.File + "::" + site.Declaration
		if _, duplicate := declared[key]; duplicate {
			t.Fatalf("parent column fixture classifies %s twice; each declaration appears once", key)
		}
		declared[key] = site
	}

	seen := make(map[string]bool, len(parsed))
	for _, site := range parsed {
		seen[site.key()] = true
		entry, listed := declared[site.key()]
		if !listed {
			t.Errorf("unclassified raw-column site: %s declares %s in %d literal(s). Classify it: render the parent answer through LogicalParentSQL/parentGenerationScopeSQL, or add it to testdata/parent_column_sites.yaml (and its manifest) with a class and a reason.", site.Declaration, site.File, site.Literals)
			continue
		}
		if entry.Literals != site.Literals {
			t.Errorf("raw-column literal count changed in %s %s: fixture says %d, the parse found %d. A new literal in a classified declaration must be re-classified, and a removed one removed from the fixture.", site.File, site.Declaration, entry.Literals, site.Literals)
		}
	}
	for key, entry := range declared {
		if !seen[key] {
			t.Errorf("classified site no longer carries the raw-column token: %s %s (class %s). If it now renders the rule, move it to the routed section; otherwise remove it from the fixture and its manifest.", entry.File, entry.Declaration, entry.Class)
		}
	}

	assertParentColumnRouted(t, root, document.Routed)
	assertParentColumnNames(t, "residual", manifest.RequiredResidual, document.Residual)
	assertParentColumnNames(t, "routed", manifest.RequiredRouted, document.Routed)
	assertParentColumnNames(t, "foreign", manifest.RequiredForeign, document.Foreign)
}

func loadParentColumnSites(t *testing.T) (parentColumnSitesDocument, parentColumnSitesManifest) {
	t.Helper()
	var document parentColumnSitesDocument
	decoder := yaml.NewDecoder(bytes.NewReader(parentColumnSitesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode parent_column_sites.yaml: %v", err)
	}
	var manifest parentColumnSitesManifest
	decoder = yaml.NewDecoder(bytes.NewReader(parentColumnSitesManifestYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode parent_column_sites manifest: %v", err)
	}
	if len(document.Residual) == 0 || len(document.Routed) == 0 || len(document.Foreign) == 0 {
		t.Fatal("parent column fixture must classify at least one residual, routed and foreign declaration")
	}
	for _, site := range document.Residual {
		if !parentColumnResidualClasses[site.Class] {
			t.Fatalf("parent column fixture entry %q declares the unknown residual class %q", site.Name, site.Class)
		}
		if site.Reason == "" {
			t.Fatalf("parent column fixture entry %q carries no reason; every exception must say why the raw value is required", site.Name)
		}
	}
	for _, site := range document.Foreign {
		if !parentColumnForeignClasses[site.Class] {
			t.Fatalf("parent column fixture entry %q declares the unknown foreign class %q", site.Name, site.Class)
		}
		if site.Reason == "" {
			t.Fatalf("parent column fixture entry %q carries no reason", site.Name)
		}
	}
	return document, manifest
}

// parentColumnRepoRoot resolves the repository root from the test's working
// directory, which is the package directory under `go test`.
func parentColumnRepoRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory to locate the repository root: %v; run the guard from the module checkout", err)
	}
	root := filepath.Dir(filepath.Dir(working))
	for _, directory := range []string{"internal", "cmd"} {
		info, statErr := os.Stat(filepath.Join(root, directory))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("repository root %s does not contain %s/: %v; the guard must run from the module checkout so the production sources can be parsed", root, directory, statErr)
		}
	}
	return root
}

// parseParentColumnSites walks production Go files under internal/ and cmd/ and
// returns one entry per declaration that carries the token in a string literal.
// Test files and testdata are outside the closure, and so are directories the Go
// tool itself ignores: a name beginning with "." or "_", and testdata. The walk
// also skips a directory named node_modules, which is not part of the package
// tree either. The Go-ignored rule matters because other tests materialize
// scratch trees beside the sources they exercise (for example the OpenCode
// build-topology cases), and a scratch copy of a scanned file must never be
// classified as a production site.
func parseParentColumnSites(t *testing.T, root string) []parsedParentColumnSite {
	t.Helper()
	var sites []parsedParentColumnSite
	for _, directory := range []string{"internal", "cmd"} {
		base := filepath.Join(root, directory)
		err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				name := info.Name()
				if path != base && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			relative = filepath.ToSlash(relative)
			sites = append(sites, parseParentColumnFile(t, relative, source)...)
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s for raw-column literals: %v", base, err)
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].key() < sites[j].key() })
	return sites
}

// parseParentColumnFile parses one file and counts the token-bearing literals in
// each of its declarations. A grouped const/var block contributes one entry per
// named value, and a method is named by its receiver type.
func parseParentColumnFile(t *testing.T, relative string, source []byte) []parsedParentColumnSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relative, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v; the guard cannot classify a file it cannot read", relative, err)
	}
	var sites []parsedParentColumnSite
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if count := countParentColumnLiterals(typed); count > 0 {
				sites = append(sites, parsedParentColumnSite{File: relative, Declaration: parentColumnFuncName(typed), Literals: count})
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				count := countParentColumnLiterals(values)
				if count == 0 {
					continue
				}
				for _, name := range values.Names {
					sites = append(sites, parsedParentColumnSite{File: relative, Declaration: name.Name, Literals: count})
				}
			}
		}
	}
	return sites
}

func countParentColumnLiterals(node ast.Node) int {
	count := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		literal, ok := candidate.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		if strings.Contains(literal.Value, parentColumnToken) {
			count++
		}
		return true
	})
	return count
}

func parentColumnFuncName(declaration *ast.FuncDecl) string {
	if declaration.Recv != nil && len(declaration.Recv.List) > 0 {
		return parentColumnReceiverName(declaration.Recv.List[0].Type) + "." + declaration.Name.Name
	}
	return declaration.Name.Name
}

func parentColumnReceiverName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return parentColumnReceiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return parentColumnReceiverName(typed.X)
	case *ast.IndexListExpr:
		return parentColumnReceiverName(typed.X)
	}
	return "?"
}

// assertParentColumnRouted proves each routed declaration carries no
// token-bearing literal and calls the renderer, so a reader that reverts to a
// raw spelling reddens here as well as on the residual set.
func assertParentColumnRouted(t *testing.T, root string, routed []parentColumnSite) {
	t.Helper()
	for _, site := range routed {
		path := filepath.Join(root, filepath.FromSlash(site.File))
		source, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("routed declaration %q names %s, which cannot be read: %v", site.Name, site.File, err)
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, site.File, source, 0)
		if err != nil {
			t.Errorf("parse routed declaration file %s: %v", site.File, err)
			continue
		}
		declaration := findParentColumnDeclaration(file, site.Declaration)
		if declaration == nil {
			t.Errorf("routed declaration %s %s no longer exists; a routed reader must stay in place and keep rendering the rule", site.File, site.Declaration)
			continue
		}
		if count := countParentColumnLiterals(declaration); count != 0 {
			t.Errorf("routed declaration %s %s carries %d raw-column literal(s); render the parent answer through LogicalParentSQL/parentGenerationScopeSQL instead", site.File, site.Declaration, count)
		}
		text := parentColumnDeclarationText(t, source, fset, declaration)
		if !strings.Contains(text, "LogicalParentSQL(") && !strings.Contains(text, "parentGenerationScopeSQL(") {
			t.Errorf("routed declaration %s %s does not call LogicalParentSQL( or parentGenerationScopeSQL(; the parent answer must render the one rule", site.File, site.Declaration)
		}
	}
}

func findParentColumnDeclaration(file *ast.File, name string) ast.Node {
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if parentColumnFuncName(typed) == name {
				return typed
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, candidate := range values.Names {
					if candidate.Name == name {
						return values
					}
				}
			}
		}
	}
	return nil
}

func parentColumnDeclarationText(t *testing.T, source []byte, fset *token.FileSet, declaration ast.Node) string {
	t.Helper()
	start := fset.Position(declaration.Pos()).Offset
	end := fset.Position(declaration.End()).Offset
	if start < 0 || end > len(source) || start >= end {
		t.Fatalf("declaration offsets %d..%d are outside the %d-byte source", start, end, len(source))
	}
	return string(source[start:end])
}

// assertParentColumnNames enforces exact membership between one fixture section
// and its manifest list: a listed name that is absent fails, and a section entry
// with no listed name fails. Removing a classified site without updating the
// manifest is refused.
func assertParentColumnNames(t *testing.T, section string, required []string, entries []parentColumnSite) {
	t.Helper()
	if len(required) == 0 {
		t.Fatalf("the parent column manifest declares no required %s names; list every %s entry", section, section)
	}
	listed := make(map[string]bool, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("the parent column manifest has a blank required %s name", section)
		}
		if listed[name] {
			t.Fatalf("the parent column manifest repeats the required %s name %q", section, name)
		}
		listed[name] = true
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.Name == "" {
			t.Fatalf("a parent column %s entry carries no name", section)
		}
		if present[entry.Name] {
			t.Fatalf("the parent column %s section repeats the name %q", section, entry.Name)
		}
		present[entry.Name] = true
		if !listed[entry.Name] {
			t.Errorf("parent column %s entry %q has no manifest name; add it to testdata/parent_column_sites.manifest.yaml", section, entry.Name)
		}
	}
	for _, name := range required {
		if !present[name] {
			t.Errorf("parent column %s is missing the required entry %q; restore the entry or remove it from the manifest", section, name)
		}
	}
}
