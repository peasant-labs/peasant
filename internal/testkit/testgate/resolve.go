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

// ExecAnchor identifies the exec.Command / exec.CommandContext call that builds
// a registered subprocess child. File is repo-relative; Func names the
// declaration that holds the call; Fragment is a required substring of the
// call. The call is located inside the named function and pinned by the
// fragment, so an edit that only shifts lines above it cannot move the
// reference.
type ExecAnchor struct {
	File     string
	Func     string
	Fragment string
}

// ParseExecAnchor parses an exec_command_site reference of the form
// <file>#<funcName>#<fragment>. The fragment runs to the end of the string, so
// it may itself contain a '#'. The three fields must be non-empty.
func ParseExecAnchor(s string) (ExecAnchor, error) {
	parts := strings.SplitN(s, "#", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ExecAnchor{}, fmt.Errorf("exec_command_site %q is not <file>#<funcName>#<fragment>", s)
	}
	return ExecAnchor{File: parts[0], Func: parts[1], Fragment: parts[2]}, nil
}

// EvidenceAnchor identifies a registry entry's admission evidence by a test
// declared in the entry's package. The file is already implied by the entry's
// package and test, so the anchor does not restate a path that can drift.
type EvidenceAnchor struct {
	Test string
}

// ParseEvidenceAnchor parses an evidence reference of the form #<testName>.
func ParseEvidenceAnchor(s string) (EvidenceAnchor, error) {
	name, ok := strings.CutPrefix(s, "#")
	if !ok || name == "" || strings.Contains(name, "#") {
		return EvidenceAnchor{}, fmt.Errorf("evidence %q is not #<testName>", s)
	}
	return EvidenceAnchor{Test: name}, nil
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

// ResolveExecSite parses anchor.File (relative to root), resolves anchor.Func in
// that file, and resolves the flags of the exec.Command / exec.CommandContext
// call inside that function whose source contains anchor.Fragment.
//
// An identifier argument is resolved through package-level string constants in
// the same directory (including build-tagged files), so a child built with
// nativeCLIRaceFlag resolves to both its race and non-race values. An
// unresolved identifier whose name mentions "race" is treated as unknown, so
// the admission fails closed rather than assuming no detector.
//
// Resolution fails closed: an anchor whose function is absent, whose function
// holds no exec call, whose fragment matches no call, or whose fragment matches
// more than one call is an error, never a first match. A function name declared
// more than once in the file is likewise ambiguous.
func ResolveExecSite(root string, anchor ExecAnchor) (ExecSite, error) {
	abs := filepath.Join(root, anchor.File)
	src, err := os.ReadFile(abs)
	if err != nil {
		return ExecSite{}, fmt.Errorf("read exec site %s: %w", anchor.File, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, abs, src, 0)
	if err != nil {
		return ExecSite{}, fmt.Errorf("parse exec site %s: %w", anchor.File, err)
	}
	decl, err := findFuncDecl(parsed, anchor.Func, anchor.File)
	if err != nil {
		return ExecSite{}, err
	}
	call, err := findExecCall(fset, decl, src, anchor)
	if err != nil {
		return ExecSite{}, err
	}
	consts, err := packageStringConsts(filepath.Dir(abs))
	if err != nil {
		return ExecSite{}, err
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

// findFuncDecl returns the single declaration named name in file. A file that
// declares the name twice (a function and a method, or two methods) is
// ambiguous and is an error.
func findFuncDecl(file *ast.File, name, fileRel string) (*ast.FuncDecl, error) {
	var found []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			found = append(found, fn)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%s declares no function named %s", fileRel, name)
	case 1:
		return found[0], nil
	default:
		return nil, fmt.Errorf("%s declares %d functions named %s; the exec_command_site anchor is ambiguous", fileRel, len(found), name)
	}
}

// findExecCall returns the one exec.Command / exec.CommandContext call inside
// decl whose source contains anchor.Fragment. It fails closed when the function
// holds no exec call, when no call carries the fragment, and when more than one
// does.
func findExecCall(fset *token.FileSet, decl *ast.FuncDecl, src []byte, anchor ExecAnchor) (*ast.CallExpr, error) {
	var all, matched []*ast.CallExpr
	ast.Inspect(decl, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isExecCommand(call.Fun) {
			return true
		}
		all = append(all, call)
		if callContains(fset, call, src, anchor.Fragment) {
			matched = append(matched, call)
		}
		return true
	})

	where := anchor.File + "#" + anchor.Func
	switch {
	case len(all) == 0:
		return nil, fmt.Errorf("%s contains no exec.Command or exec.CommandContext call", where)
	case len(matched) == 0:
		return nil, fmt.Errorf("%s has no exec call containing the required fragment %q", where, anchor.Fragment)
	case len(matched) > 1:
		return nil, fmt.Errorf("%s has %d exec calls containing the required fragment %q; the exec_command_site anchor is ambiguous", where, len(matched), anchor.Fragment)
	}
	return matched[0], nil
}

// callContains reports whether the call's source text contains fragment.
func callContains(fset *token.FileSet, call *ast.CallExpr, src []byte, fragment string) bool {
	start := fset.Position(call.Pos()).Offset
	end := fset.Position(call.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return false
	}
	return strings.Contains(string(src[start:end]), fragment)
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
