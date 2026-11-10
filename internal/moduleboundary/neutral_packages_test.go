package moduleboundary_test

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/neutral_packages.yaml
var neutralPackagesYAML []byte

type neutralCase struct {
	Name      string `yaml:"name"`
	Directory string `yaml:"directory"`
	Forbidden string `yaml:"forbidden"`
}
type homonym struct {
	File string `yaml:"file"`
	Name string `yaml:"name"`
}
type neutralFixture struct {
	Cases              []neutralCase `yaml:"cases"`
	NeutralTypes       []string      `yaml:"neutralTypes"`
	GuardedDirectories []string      `yaml:"guardedDirectories"`
	PermittedHomonyms  []homonym     `yaml:"permittedHomonyms"`
}

func TestNeutralPackageBoundaries(t *testing.T) {
	var f neutralFixture
	if err := testutil.DecodeFixtureYAML(neutralPackagesYAML, &f); err != nil {
		t.Fatal(err)
	}
	edges := map[string]neutralCase{
		"onboarding-not-import-internal-tui":    {Directory: "internal/onboarding", Forbidden: "github.com/peasant-labs/peasant/internal/tui"},
		"onboarding-not-import-net-http":        {Directory: "internal/onboarding", Forbidden: "net/http"},
		"selectionfilter-not-import-onboarding": {Directory: "internal/selectionfilter", Forbidden: "github.com/peasant-labs/peasant/internal/onboarding"},
		"api-not-import-internal-tui":           {Directory: "internal/api", Forbidden: "github.com/peasant-labs/peasant/internal/tui"},
		"discovery-not-import-internal-tui":     {Directory: "internal/discovery", Forbidden: "github.com/peasant-labs/peasant/internal/tui"},
		"one-declaration-of-each-neutral-type":  {},
	}
	required := map[string]bool{}
	for name := range edges {
		required[name] = false
	}
	for _, c := range f.Cases {
		if _, ok := required[c.Name]; !ok {
			t.Fatalf("unknown boundary case %q", c.Name)
		}
		if required[c.Name] {
			t.Fatalf("duplicate boundary case %q", c.Name)
		}
		edge := edges[c.Name]
		if c.Directory != edge.Directory || c.Forbidden != edge.Forbidden {
			t.Fatalf("boundary case %q has unexpected edge", c.Name)
		}
		required[c.Name] = true
	}
	for name, found := range required {
		if !found {
			t.Fatalf("missing boundary case %q", name)
		}
	}
	root := moduleRoot(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Name == "one-declaration-of-each-neutral-type" {
				checkNeutralDeclarations(t, root, f)
				return
			}
			if c.Directory == "" || c.Forbidden == "" {
				t.Fatal("boundary case requires directory and forbidden import")
			}
			if c.Name == "selectionfilter-not-import-onboarding" {
				if _, err := os.Stat(filepath.Join(root, c.Directory)); os.IsNotExist(err) {
					t.Skip("selectionfilter is not present on this branch; check its imports after integration")
				} else if err != nil {
					t.Fatalf("inspect selectionfilter directory: %v", err)
				}
			}
			walkGoFiles(t, filepath.Join(root, c.Directory), func(path string, file *ast.File) {
				for _, imp := range file.Imports {
					value, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					if value == c.Forbidden || strings.HasPrefix(value, c.Forbidden+"/") {
						t.Errorf("%s imports forbidden package %s", path, value)
					}
				}
			})
		})
	}
}

func walkGoFiles(t *testing.T, dir string, visit func(string, *ast.File)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		visit(path, file)
		return nil
	})
	if err != nil {
		t.Fatalf("inspect Go sources in %s: %v", dir, err)
	}
}

func checkNeutralDeclarations(t *testing.T, root string, f neutralFixture) {
	t.Helper()
	// These lists are the protected contract, not a count that can silently shrink.
	types := map[string]bool{"ProjectIdentity": false, "PreparedSessionListing": false, "SessionListing": false, "SessionSource": false, "SessionSourceOrigin": false, "Forest": false, "Node": false, "Stop": false, "Field": false, "Draft": false, "Residual": false, "StopKind": false, "FieldKind": false, "ModelScope": false, "FlowKey": false}
	dirs := map[string]bool{"internal/tui/settings": false, "internal/tui/kickstart": false, "internal/tui/ftue": false, "internal/discovery": false}
	for _, name := range f.NeutralTypes {
		if seen, ok := types[name]; !ok || seen {
			t.Fatalf("unknown or duplicate neutral type %q", name)
		}
		types[name] = true
	}
	for name, seen := range types {
		if !seen {
			t.Fatalf("missing neutral type %q", name)
		}
	}
	for _, dir := range f.GuardedDirectories {
		if seen, ok := dirs[dir]; !ok || seen {
			t.Fatalf("unknown or duplicate guarded directory %q", dir)
		}
		dirs[dir] = true
	}
	for dir, seen := range dirs {
		if !seen {
			t.Fatalf("missing guarded directory %q", dir)
		}
	}
	allowed := map[homonym]bool{{"internal/codegraph/graph.go", "Node"}: false, {"internal/tui/settings/field.go", "Field"}: false, {"internal/tui/settings/field.go", "FieldKind"}: false, {"internal/tui/settings/draft.go", "Draft"}: false}
	for _, h := range f.PermittedHomonyms {
		if seen, ok := allowed[h]; !ok || seen {
			t.Fatalf("unknown or duplicate permitted homonym %+v", h)
		}
		allowed[h] = true
	}
	for h, seen := range allowed {
		if !seen {
			t.Fatalf("missing permitted homonym %+v", h)
		}
		allowed[h] = false
	}
	ownerCounts := map[string]int{}
	inspect := func(owner bool) func(string, *ast.File) {
		return func(path string, file *ast.File) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatal(err)
			}
			rel = filepath.ToSlash(rel)
			imports := map[string]string{}
			for _, imp := range file.Imports {
				value, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				name := filepath.Base(value)
				if imp.Name != nil {
					name = imp.Name.Name
				}
				imports[name] = value
			}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					typ := spec.(*ast.TypeSpec)
					name := typ.Name.Name
					if !types[name] {
						continue
					}
					h := homonym{rel, name}
					if _, ok := allowed[h]; ok {
						if typ.Assign.IsValid() {
							t.Errorf("permitted homonym %s %s is unexpectedly an alias", rel, name)
						}
						allowed[h] = true
						continue
					}
					if owner {
						if typ.Assign.IsValid() {
							t.Errorf("owner %s %s is an alias", rel, name)
						}
						ownerCounts[name]++
						continue
					}
					selector, ok := typ.Type.(*ast.SelectorExpr)
					if !typ.Assign.IsValid() || !ok {
						t.Errorf("%s redeclares neutral type %s", rel, name)
						continue
					}
					ident, ok := selector.X.(*ast.Ident)
					if !ok || selector.Sel.Name != name {
						t.Errorf("%s aliases %s to a different type", rel, name)
						continue
					}
					pkg := imports[ident.Name]
					if pkg != "github.com/peasant-labs/peasant/internal/onboarding" && !(strings.HasPrefix(rel, "internal/tui/ftue/") && pkg == "github.com/peasant-labs/peasant/internal/discovery" && (name == "SessionListing" || name == "SessionSource" || name == "SessionSourceOrigin")) {
						t.Errorf("%s aliases %s from unexpected owner %s", rel, name, pkg)
					}
				}
			}
		}
	}
	walkGoFiles(t, filepath.Join(root, "internal/onboarding"), inspect(true))
	for dir := range dirs {
		walkGoFiles(t, filepath.Join(root, dir), inspect(false))
	}
	// Positive control: the unrelated graph node is real and deliberately outside the scope.
	walkGoFiles(t, filepath.Join(root, "internal/codegraph"), func(path string, file *ast.File) {
		if filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "internal/codegraph/graph.go")) {
			inspect(false)(path, file)
		}
	})
	for name := range types {
		if ownerCounts[name] != 1 {
			t.Errorf("onboarding owns %d declarations of %s, want one", ownerCounts[name], name)
		}
	}
	for h, seen := range allowed {
		if !seen {
			t.Errorf("permitted homonym %s not found", fmt.Sprint(h))
		}
	}
}
