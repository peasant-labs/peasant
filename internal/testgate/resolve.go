package testgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// testDeclFile returns the absolute path of the test file in pkgAbs that
// declares a top-level function named test.
func testDeclFile(pkgAbs, test string) (string, error) {
	entries, err := os.ReadDir(pkgAbs)
	if err != nil {
		return "", fmt.Errorf("read package directory %s: %w", pkgAbs, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pkgAbs, entry.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == test {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("package %s does not declare a top-level test named %s", filepath.ToSlash(pkgAbs), test)
}

// ExecSite is the resolved build posture of a subprocess entry's exec.Command
// call: the flags that reach the child build, whether any enables the race
// detector, and the name of any argument that could not be resolved.
type ExecSite struct {
	Flags       []string
	RaceEnabled bool
	FlagsKnown  bool
	Unresolved  string
}

// ResolveExecSite parses file (relative to root) and resolves the flags of the
// exec.Command / exec.CommandContext call covering line.
//
// An identifier argument is resolved through package-level string constants in
// the same directory (including build-tagged files), so a child built with
// nativeCLIRaceFlag resolves to both its race and non-race values. An
// unresolved identifier whose name mentions "race" is treated as unknown, so
// the admission fails closed rather than assuming no detector.
func ResolveExecSite(root, file string, line int) (ExecSite, error) {
	abs := filepath.Join(root, file)
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		return ExecSite{}, fmt.Errorf("parse exec site %s: %w", file, err)
	}
	consts, err := packageStringConsts(filepath.Dir(abs))
	if err != nil {
		return ExecSite{}, err
	}

	var call *ast.CallExpr
	for _, decl := range parsed.Decls {
		ast.Inspect(decl, func(node ast.Node) bool {
			c, ok := node.(*ast.CallExpr)
			if !ok || !isExecCommand(c.Fun) {
				return true
			}
			if fset.Position(c.Pos()).Line <= line && line <= fset.Position(c.End()).Line {
				call = c
				return false
			}
			return true
		})
		if call != nil {
			break
		}
	}
	if call == nil {
		return ExecSite{}, fmt.Errorf("no exec.Command call covers %s:%d", file, line)
	}

	// For exec.CommandContext the first argument is the context; the program
	// name follows. For exec.Command it is first. Either way only flag-shaped
	// arguments matter, so scan every argument.
	site := ExecSite{FlagsKnown: true}
	seen := map[string]bool{}
	for _, arg := range call.Args {
		values, ok := resolveStringExpr(arg, consts)
		if !ok {
			name := exprName(arg)
			if strings.Contains(strings.ToLower(name), "race") {
				site.FlagsKnown = false
				site.Unresolved = name
			}
			continue
		}
		for _, v := range values {
			if !strings.HasPrefix(v, "-") || seen[v] {
				continue
			}
			seen[v] = true
			site.Flags = append(site.Flags, v)
			if enablesRace(v) {
				site.RaceEnabled = true
			}
		}
	}
	return site, nil
}

func isExecCommand(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "exec" {
		return false
	}
	return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
}

func exprName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.CallExpr:
		return exprName(e.Fun)
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return "?"
	}
}

// resolveStringExpr resolves an expression to the string values it can take: a
// string literal is itself; an identifier is every package-level string
// constant with that name. The bool reports whether the expression resolved.
func resolveStringExpr(expr ast.Expr, consts map[string][]string) ([]string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil, false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil, false
		}
		return []string{v}, true
	case *ast.Ident:
		if values, ok := consts[e.Name]; ok {
			return values, true
		}
		return nil, false
	default:
		return nil, false
	}
}

// packageStringConsts collects every package-level string constant declared in
// the directory's .go files, keyed by name. Build-tagged files all contribute,
// so an identifier with one value under `race` and another without yields both.
func packageStringConsts(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read exec site package %s: %w", dir, err)
	}
	out := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out[name.Name] = append(out[name.Name], v)
				}
			}
		}
	}
	return out, nil
}
