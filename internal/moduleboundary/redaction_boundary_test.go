package moduleboundary_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	expectedForbiddenImportFixtures = 1
	expectedForbiddenTreeFixtures   = 1
	expectedScannerCallFixtures     = 3
)

//go:embed testdata/redaction.yaml
var redactionFixtureYAML []byte

type boundaryFixture struct {
	Module           moduleFixture        `yaml:"module"`
	ForbiddenImports []string             `yaml:"forbidden_imports"`
	ForbiddenTrees   []string             `yaml:"forbidden_trees"`
	ScannerCalls     []scannerCallFixture `yaml:"scanner_calls"`
}

type moduleFixture struct {
	Path    string `yaml:"path"`
	Version string `yaml:"version"`
}

type scannerCallFixture struct {
	File          string `yaml:"file"`
	Function      string `yaml:"function"`
	ExpectedCalls int    `yaml:"expected_calls"`
}

type goModJSON struct {
	Require []struct {
		Path     string
		Version  string
		Indirect bool
	}
	Replace []struct {
		Old struct {
			Path string
		}
	}
}

func TestRedactionModuleBoundary(t *testing.T) {
	fixture := loadBoundaryFixture(t)
	root := moduleRoot(t)
	assertDirectRequirement(t, root, fixture.Module)
	assertSelectedDependency(t, root, fixture.Module)
	assertForbiddenTreesAbsent(t, root, fixture.ForbiddenTrees)
	assertForbiddenImportsAbsent(t, root, fixture.ForbiddenImports)
	assertScannerCalls(t, root, fixture.ScannerCalls)
}

func loadBoundaryFixture(t *testing.T) boundaryFixture {
	t.Helper()
	fixture, err := decodeBoundaryFixture(redactionFixtureYAML)
	if err != nil {
		t.Fatalf("decode embedded redaction boundary fixture: %v", err)
	}
	if fixture.Module.Path == "" || fixture.Module.Version == "" {
		t.Fatal("redaction boundary fixture must declare a module path and version")
	}
	assertUniqueFixtureRows(t, "forbidden import", fixture.ForbiddenImports, expectedForbiddenImportFixtures)
	assertUniqueFixtureRows(t, "forbidden tree", fixture.ForbiddenTrees, expectedForbiddenTreeFixtures)
	if len(fixture.ScannerCalls) != expectedScannerCallFixtures {
		t.Fatalf("redaction boundary fixture has %d scanner call declarations; want exactly %d", len(fixture.ScannerCalls), expectedScannerCallFixtures)
	}
	seen := make(map[string]struct{}, len(fixture.ScannerCalls))
	for _, call := range fixture.ScannerCalls {
		identity := call.File + "#" + call.Function
		if call.File == "" || call.Function == "" || call.ExpectedCalls < 1 {
			t.Fatalf("redaction boundary fixture has incomplete scanner call declaration %q", identity)
		}
		if _, duplicate := seen[identity]; duplicate {
			t.Fatalf("redaction boundary fixture repeats scanner call identity %q", identity)
		}
		seen[identity] = struct{}{}
	}
	return fixture
}

func decodeBoundaryFixture(data []byte) (boundaryFixture, error) {
	var fixture boundaryFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return boundaryFixture{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return boundaryFixture{}, fmt.Errorf("redaction boundary fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

func TestRedactionBoundaryFixtureStrictDecoding(t *testing.T) {
	unknownField := append([]byte("unexpected_fixture_field: true\n"), redactionFixtureYAML...)
	if _, err := decodeBoundaryFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}

	trailingDocument := append(append([]byte{}, redactionFixtureYAML...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeBoundaryFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

func assertUniqueFixtureRows(t *testing.T, kind string, rows []string, expected int) {
	t.Helper()
	if len(rows) != expected {
		t.Fatalf("redaction boundary fixture has %d %s declarations; want exactly %d", len(rows), kind, expected)
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row == "" {
			t.Fatalf("redaction boundary fixture has an empty %s declaration", kind)
		}
		if _, duplicate := seen[row]; duplicate {
			t.Fatalf("redaction boundary fixture repeats %s identity %q", kind, row)
		}
		seen[row] = struct{}{}
	}
}

func assertDirectRequirement(t *testing.T, root string, want moduleFixture) {
	t.Helper()
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("inspect root go.mod with go mod edit: %v", err)
	}
	var mod goModJSON
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("decode go mod edit output: %v", err)
	}
	found := false
	for _, requirement := range mod.Require {
		if requirement.Path == want.Path {
			found = true
			if requirement.Indirect || requirement.Version != want.Version {
				t.Fatalf("root go.mod must directly require %s %s; got version %q, indirect=%v", want.Path, want.Version, requirement.Version, requirement.Indirect)
			}
		}
	}
	if !found {
		t.Fatalf("root go.mod does not directly require %s %s", want.Path, want.Version)
	}
	for _, replacement := range mod.Replace {
		if replacement.Old.Path == want.Path {
			t.Fatalf("root go.mod replaces %s; consume the published %s directly", want.Path, want.Version)
		}
	}
}

func assertSelectedDependency(t *testing.T, root string, want moduleFixture) {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-json", want.Path)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("inspect selected build dependency %s: %v", want.Path, err)
	}
	var selected struct {
		Path    string
		Version string
		Replace *struct {
			Path    string
			Version string
		}
	}
	if err := json.Unmarshal(out, &selected); err != nil {
		t.Fatalf("decode selected build dependency %s: %v", want.Path, err)
	}
	if selected.Path != want.Path || selected.Version != want.Version || selected.Replace != nil {
		t.Fatalf("selected build dependency for %s must be unreplaced %s; got path %q, version %q, replacement=%v", want.Path, want.Version, selected.Path, selected.Version, selected.Replace)
	}
}

func assertForbiddenTreesAbsent(t *testing.T, root string, trees []string) {
	t.Helper()
	for _, tree := range trees {
		path := filepath.Join(root, filepath.FromSlash(tree))
		err := filepath.WalkDir(path, func(found string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				return fmt.Errorf("retired package tree %s contains %s", tree, found)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func assertForbiddenImportsAbsent(t *testing.T, root string, forbidden []string) {
	t.Helper()
	forbiddenSet := make(map[string]struct{}, len(forbidden))
	for _, importPath := range forbidden {
		forbiddenSet[importPath] = struct{}{}
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse Go imports in %s: %w", path, err)
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return fmt.Errorf("decode Go import in %s: %w", path, err)
			}
			if _, stale := forbiddenSet[value]; stale {
				return fmt.Errorf("non-historical Go source %s imports retired package %s", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertScannerCalls(t *testing.T, root string, calls []scannerCallFixture) {
	t.Helper()
	for _, want := range calls {
		path := filepath.Join(root, filepath.FromSlash(want.File))
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse mounted redaction source %s: %v", want.File, err)
		}
		matchedFunction := false
		matchedCalls := 0
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != want.Function {
				continue
			}
			matchedFunction = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isSelectorCall(call.Fun, "redact", "RedactJSONLBytes") {
					return true
				}
				matchedCalls++
				if !hasScannerOption(call.Args) {
					t.Errorf("%s function %s calls redact.RedactJSONLBytes without redact.WithRedactScannerBufSize(defaults.ScannerInitBuf, defaults.ScannerMaxLine)", want.File, want.Function)
				}
				return true
			})
		}
		if !matchedFunction {
			t.Errorf("%s does not contain mounted function %s", want.File, want.Function)
		} else if matchedCalls != want.ExpectedCalls {
			t.Errorf("%s function %s has %d redact.RedactJSONLBytes calls; want exactly %d", want.File, want.Function, matchedCalls, want.ExpectedCalls)
		}
	}
}

func hasScannerOption(arguments []ast.Expr) bool {
	for _, argument := range arguments {
		call, ok := argument.(*ast.CallExpr)
		if !ok || !isSelectorCall(call.Fun, "redact", "WithRedactScannerBufSize") || len(call.Args) != 2 {
			continue
		}
		if isSelectorCall(call.Args[0], "defaults", "ScannerInitBuf") && isSelectorCall(call.Args[1], "defaults", "ScannerMaxLine") {
			return true
		}
	}
	return false
}

func isSelectorCall(expression ast.Expr, packageName, member string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != member {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == packageName
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get boundary test working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("locate Peasant module root from %s: no go.mod found", strings.TrimSpace(dir))
		}
		dir = parent
	}
}
