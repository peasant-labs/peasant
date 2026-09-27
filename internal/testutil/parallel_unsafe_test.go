package testutil_test

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/parallel_unsafe_sites.yaml
var parallelUnsafeSitesYAML []byte

// parallelUnsafeSite is one test that mutates a process-global sink and must
// therefore stay serial.
type parallelUnsafeSite struct {
	Name   string `yaml:"name"`
	File   string `yaml:"file"`
	Line   int    `yaml:"line"`
	Test   string `yaml:"test"`
	Hazard string `yaml:"hazard"`

	WantError string `yaml:"want_error"`
}

type parallelUnsafeFixture struct {
	Version              int                  `yaml:"version"`
	RequiredNames        []string             `yaml:"required_names"`
	RequiredMutationName []string             `yaml:"required_mutation_names"`
	Cases                []parallelUnsafeSite `yaml:"cases"`
	Mutations            []parallelUnsafeSite `yaml:"mutations"`
}

func loadParallelUnsafeFixture(t *testing.T) parallelUnsafeFixture {
	t.Helper()
	var f parallelUnsafeFixture
	if err := testutil.DecodeFixtureYAML(parallelUnsafeSitesYAML, &f); err != nil {
		t.Fatalf("decode testdata/parallel_unsafe_sites.yaml: %v", err)
	}
	if f.Version != 1 {
		t.Fatalf("parallel-unsafe fixture version = %d, want 1", f.Version)
	}
	caseNames := make(map[string]bool, len(f.Cases))
	for _, c := range f.Cases {
		if strings.TrimSpace(c.Name) == "" || caseNames[c.Name] {
			t.Fatalf("parallel-unsafe fixture has an empty or duplicate case name %q", c.Name)
		}
		caseNames[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("testdata/parallel_unsafe_sites.yaml", "site", f.RequiredNames, caseNames); err != nil {
		t.Fatal(err)
	}
	mutationNames := make(map[string]bool, len(f.Mutations))
	for _, m := range f.Mutations {
		if strings.TrimSpace(m.Name) == "" || mutationNames[m.Name] {
			t.Fatalf("parallel-unsafe fixture has an empty or duplicate mutation name %q", m.Name)
		}
		mutationNames[m.Name] = true
	}
	if err := testutil.RequireFixtureNames("testdata/parallel_unsafe_sites.yaml", "mutation", f.RequiredMutationName, mutationNames); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestParallelUnsafeSitesStaySerial asserts every catalogued process-global
// sink site still exists, still lives in the named test, and has NOT gained
// t.Parallel(). The race detector cannot catch this: the writes are to distinct
// package globals serialized by the runtime, so the failure is logical (one
// test's log lines in another's buffer, one test's stdin another's /dev/null).
func TestParallelUnsafeSitesStaySerial(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	fixture := loadParallelUnsafeFixture(t)

	for _, site := range fixture.Cases {
		site := site
		t.Run(site.Name, func(t *testing.T) {
			t.Parallel()
			if err := assertSerialGlobalSinkSite(root, site); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

// TestParallelUnsafeCheckerRejectsMutations proves the checker above is not
// vacuous: each mutation feeds it a real test that violates the rule and must
// be rejected for the named reason.
func TestParallelUnsafeCheckerRejectsMutations(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	fixture := loadParallelUnsafeFixture(t)

	for _, mutation := range fixture.Mutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			t.Parallel()
			err := assertSerialGlobalSinkSite(root, mutation)
			if err == nil {
				t.Fatalf("checker accepted %s/%s; the fixture assertion is vacuous", mutation.File, mutation.Test)
			}
			if !strings.Contains(err.Error(), mutation.WantError) {
				t.Fatalf("checker rejected %s for the wrong reason:\n  got: %v\n want substring: %q", mutation.Name, err, mutation.WantError)
			}
		})
	}
}

// assertSerialGlobalSinkSite checks one catalogued site against the real source
// tree: the test function exists, the recorded line is inside it, the test does
// not call t.Parallel(), and the recorded hazard construct is present.
func assertSerialGlobalSinkSite(root string, site parallelUnsafeSite) error {
	path := filepath.Join(root, filepath.FromSlash(site.File))
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("parse %s: %w", site.File, err)
	}

	decl := findTopLevelTest(parsed, site.Test)
	if decl == nil {
		return fmt.Errorf("%s no longer declares a top-level test named %s; the catalogued process-global sink site is gone", site.File, site.Test)
	}
	start := fset.Position(decl.Pos()).Line
	end := fset.Position(decl.End()).Line
	if site.Line > 0 && (site.Line < start || site.Line > end) {
		return fmt.Errorf("%s:%d is not inside %s (lines %d-%d); the site moved or the fixture line is stale", site.File, site.Line, site.Test, start, end)
	}

	if testCallsParallel(decl) {
		return fmt.Errorf("%s:%d %s calls t.Parallel() but mutates a process-global sink; the failure is logical and invisible to -race, so it must stay serial", site.File, site.Line, site.Test)
	}
	if !hazardPresent(decl, site.Hazard) {
		return fmt.Errorf("%s:%d %s does not contain %s; the catalogued sink mutation is gone", site.File, site.Line, site.Test, site.Hazard)
	}
	return nil
}

func findTopLevelTest(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// testCallsParallel reports whether the function calls <param>.Parallel() on
// its own *testing.T parameter, at any nesting depth (a subtest that calls
// t.Parallel() has the same interleaving hazard for a process-global sink).
func testCallsParallel(fn *ast.FuncDecl) bool {
	param := firstParamName(fn)
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Parallel" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == param {
			found = true
		}
		return true
	})
	return found
}

// hazardPresent reports whether the function body contains the named hazard:
// a `slog.SetDefault(...)` call, or an assignment to `os.Stdin`/`os.Stderr`.
func hazardPresent(fn *ast.FuncDecl, hazard string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && selectorPath(sel) == hazard {
				found = true
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && selectorPath(sel) == hazard {
					found = true
				}
			}
		}
		return true
	})
	return found
}

func selectorPath(sel *ast.SelectorExpr) string {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name + "." + sel.Sel.Name
}

func firstParamName(fn *ast.FuncDecl) string {
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			if len(field.Names) > 0 {
				return field.Names[0].Name
			}
		}
	}
	return "t"
}
