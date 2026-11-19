package gitconfig

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema/testcase"
	"github.com/peasant-labs/schema/testcase/assert"
)

// The guard fails when a test package imports os/exec in a _test.go file but
// has no TestMain calling RunWithIsolatedGitConfig. TESTING.md ("Tests never
// read your git configuration") lists what it cannot see.

//go:embed testdata/git_isolation_scanner.yaml
var gitIsolationScannerCorpus []byte

//go:embed testdata/git_isolation_scanner_manifest.yaml
var gitIsolationScannerManifest []byte

const (
	isolationCall = "RunWithIsolatedGitConfig"
	// The embed directives above must name the same files.
	gitIsolationScannerPath         = "testdata/git_isolation_scanner.yaml"
	gitIsolationScannerManifestPath = "testdata/git_isolation_scanner_manifest.yaml"
	// testdataDirName is the directory the go tool ignores; nothing in it is a package.
	testdataDirName = "testdata"
)

type gitIsolationInput struct {
	Files map[string]string `yaml:"files"`
}

type gitIsolationExpected struct {
	Needs      []string `yaml:"needs"`
	Violations []string `yaml:"violations"`
}

type gitIsolationReport struct {
	Needs      []string
	Violations []string
}

// scanGitIsolation walks the module rooted at root, skipping testdata, hidden
// directories, node_modules and nested modules. It reports, by slash-separated
// directory relative to root, the test packages that import os/exec and those
// of them with no TestMain calling the helper. A TestMain in a file with a
// build constraint does not count: it may not be compiled on every platform.
func scanGitIsolation(root string) (gitIsolationReport, error) {
	needs := map[string]bool{}
	isolated := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if name == testdataDirName || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(p, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("scan git isolation: parse %s: %w", p, err)
		}
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == "os/exec" {
				needs[pkg] = true
			}
		}
		if !hasBuildConstraint(file) && callsIsolationFromTestMain(file) {
			isolated[pkg] = true
		}
		return nil
	})
	if err != nil {
		return gitIsolationReport{}, err
	}
	var report gitIsolationReport
	for pkg := range needs {
		report.Needs = append(report.Needs, pkg)
		if !isolated[pkg] {
			report.Violations = append(report.Violations, pkg)
		}
	}
	sort.Strings(report.Needs)
	sort.Strings(report.Violations)
	return report, nil
}

func hasBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text) {
				return true
			}
		}
	}
	return false
}

// callsIsolationFromTestMain accepts one unconditional final exit of the
// isolation runner, and no competing test runner or exit anywhere in TestMain.
func callsIsolationFromTestMain(file *ast.File) bool {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil || len(fn.Body.List) == 0 || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			continue
		}
		runner := fn.Type.Params.List[0].Names[0].Name
		last, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ExprStmt)
		if !ok {
			continue
		}
		exit, ok := last.X.(*ast.CallExpr)
		if !ok || !selectorIs(exit.Fun, "os", "Exit") || len(exit.Args) != 1 {
			continue
		}
		isolation, ok := exit.Args[0].(*ast.CallExpr)
		if !ok || !callNamed(isolation.Fun, isolationCall) {
			continue
		}
		exits, isolations, competing := 0, 0, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selectorIs(call.Fun, "os", "Exit") {
				exits++
			}
			if callNamed(call.Fun, isolationCall) {
				isolations++
			}
			if selectorIs(call.Fun, runner, "Run") || selectorIs(call.Fun, "goleak", "VerifyTestMain") {
				competing = true
			}
			return true
		})
		if exits == 1 && isolations == 1 && !competing {
			return true
		}
	}
	return false
}

func selectorIs(expr ast.Expr, receiver, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == receiver
}

func callNamed(expr ast.Expr, name string) bool {
	switch fun := expr.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == name
	case *ast.Ident:
		return fun.Name == name
	}
	return false
}

func moduleRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// TestEveryPackageThatStartsProcessesIsolatesGitConfig is the guard for this
// module.
func TestEveryPackageThatStartsProcessesIsolatesGitConfig(t *testing.T) {
	report, err := scanGitIsolation(moduleRootDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Needs) == 0 {
		t.Fatal("the scanner found no test package that imports os/exec; this module has several, so the scanner is broken")
	}
	for _, pkg := range report.Violations {
		t.Errorf("package %s imports os/exec in its tests but lacks a safe isolating TestMain; "+
			"end TestMain with `os.Exit(gitconfig.%s(m))` and remove other m.Run, goleak.VerifyTestMain, and os.Exit calls "+
			"in a _test.go file in %s with no build constraint", pkg, isolationCall, pkg)
	}
}

// TestGitIsolationScannerCorpus proves the scanner detects each way the
// isolation can be missing and accepts each way it can be present.
func TestGitIsolationScannerCorpus(t *testing.T) {
	corpus, err := testcase.LoadCorpus[gitIsolationInput, gitIsolationExpected](gitIsolationScannerCorpus)
	if err != nil {
		t.Fatalf("load %s: %v", gitIsolationScannerPath, err)
	}
	assert.RequireValid(t, corpus)
	names := make([]string, 0, len(corpus.Cases))
	for _, c := range corpus.Cases {
		names = append(names, c.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(gitIsolationScannerManifest, gitIsolationScannerManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, gitIsolationScannerManifestPath); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range c.Input.Files {
				p := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			report, err := scanGitIsolation(root)
			if err != nil {
				t.Fatal(err)
			}
			if c.Classification == testcase.MustFail && len(report.Violations) == 0 {
				t.Fatalf("must-fail case produced no violation (needs %v)", report.Needs)
			}
			if c.Classification == testcase.MustPass && len(report.Violations) != 0 {
				t.Fatalf("must-pass case produced violations %v", report.Violations)
			}
			if !slices.Equal(report.Needs, c.Expected.Needs) {
				t.Errorf("needs = %v, want %v", report.Needs, c.Expected.Needs)
			}
			if !slices.Equal(report.Violations, c.Expected.Violations) {
				t.Errorf("violations = %v, want %v", report.Violations, c.Expected.Violations)
			}
		})
	}
}
