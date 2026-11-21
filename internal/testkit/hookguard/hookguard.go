// Package hookguard checks the production files of one package for the test
// hooks it declares: nil-default package variables or struct fields that only
// tests set. A production file must never assign a hook, and only the named
// functions may read it, so a hook cannot change production behaviour and a
// mutation applied through it reaches only the code it is meant to reach. It
// imports only the standard library, so white-box package tests can use it.
package hookguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Hook names one test hook and the functions allowed to read it. A function
// is written as Name for a plain function or Recv.Name for a method.
type Hook struct {
	Name    string   `yaml:"name"`
	Readers []string `yaml:"readers"`
}

// Check parses every non-test Go file in dir and returns one finding per
// violation: a hook assigned (or incremented, or address-taken) in a
// production file, a hook referenced outside its readers, a hook never
// declared, or a reader that never references its hook. An empty result means
// the package keeps every hook test-only.
func Check(dir string, hooks []Hook) ([]string, error) {
	if len(hooks) == 0 {
		return nil, fmt.Errorf("hookguard: no hooks were named for %s; a guard with nothing to check proves nothing; name the package's hooks", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("hookguard: read %s: %w", dir, err)
	}
	byName := make(map[string]Hook, len(hooks))
	for _, hook := range hooks {
		byName[hook.Name] = hook
	}
	declared := map[string]bool{}
	readBy := map[string]map[string]bool{}
	var findings []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("hookguard: parse %s: %w", name, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				recordDeclarations(d, byName, declared)
				ast.Inspect(d, func(n ast.Node) bool {
					if field, ok := n.(*ast.Field); ok {
						for _, ident := range field.Names {
							if _, ok := byName[ident.Name]; ok {
								declared[ident.Name] = true
							}
						}
					}
					return true
				})
			case *ast.FuncDecl:
				function := funcName(d)
				ast.Inspect(d, func(n ast.Node) bool {
					for _, hook := range assignedHooks(n, byName) {
						findings = append(findings, fmt.Sprintf("%s: %s assigns the test hook %s; only tests may set it", fset.Position(n.Pos()), function, hook))
					}
					if hook, ok := hookReference(n, byName); ok {
						if !slices.Contains(byName[hook].Readers, function) && !readBy[hook][function] {
							findings = append(findings, fmt.Sprintf("%s: %s reads the test hook %s; only %v may", fset.Position(n.Pos()), function, hook, byName[hook].Readers))
						}
						if readBy[hook] == nil {
							readBy[hook] = map[string]bool{}
						}
						readBy[hook][function] = true
					}
					return true
				})
			}
		}
	}
	for _, hook := range hooks {
		if declared[hook.Name+initializedSuffix] {
			findings = append(findings, fmt.Sprintf("%s: the test hook %s has a package-level initializer; a hook stays nil in production", dir, hook.Name))
		}
		if !declared[hook.Name] {
			findings = append(findings, fmt.Sprintf("%s: the test hook %s is not declared in a production file", dir, hook.Name))
		}
		for _, reader := range hook.Readers {
			if !readBy[hook.Name][reader] {
				findings = append(findings, fmt.Sprintf("%s: %s is listed as a reader of %s but never references it", dir, reader, hook.Name))
			}
		}
	}
	sort.Strings(findings)
	return findings, nil
}

// initializedSuffix marks, in the declared set, a hook whose declaration
// carries an initializer.
const initializedSuffix = "#initialized"

func recordDeclarations(d *ast.GenDecl, hooks map[string]Hook, declared map[string]bool) {
	if d.Tok != token.VAR {
		return
	}
	for _, spec := range d.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, ident := range value.Names {
			if _, ok := hooks[ident.Name]; !ok {
				continue
			}
			declared[ident.Name] = true
			if i < len(value.Values) {
				// A package-level initializer is a production assignment.
				declared[ident.Name+initializedSuffix] = true
			}
		}
	}
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	recv := d.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

// hookName returns the hook an expression names: a bare identifier or the
// selected field of a selector.
func hookName(expr ast.Expr, hooks map[string]Hook) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		_, ok := hooks[e.Name]
		return e.Name, ok
	case *ast.SelectorExpr:
		_, ok := hooks[e.Sel.Name]
		return e.Sel.Name, ok
	case *ast.ParenExpr:
		return hookName(e.X, hooks)
	}
	return "", false
}

func assignedHooks(n ast.Node, hooks map[string]Hook) []string {
	var out []string
	switch s := n.(type) {
	case *ast.AssignStmt:
		for _, lhs := range s.Lhs {
			if name, ok := hookName(lhs, hooks); ok {
				out = append(out, name)
			}
		}
	case *ast.IncDecStmt:
		if name, ok := hookName(s.X, hooks); ok {
			out = append(out, name)
		}
	case *ast.UnaryExpr:
		if s.Op == token.AND {
			if name, ok := hookName(s.X, hooks); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// hookReference reports a read of a hook: an identifier, or a selector whose
// field is the hook. Selectors are reported once, at the selector.
func hookReference(n ast.Node, hooks map[string]Hook) (string, bool) {
	switch e := n.(type) {
	case *ast.SelectorExpr:
		if _, ok := hooks[e.Sel.Name]; ok {
			return e.Sel.Name, true
		}
	case *ast.Ident:
		if _, ok := hooks[e.Name]; ok {
			return e.Name, true
		}
	}
	return "", false
}
