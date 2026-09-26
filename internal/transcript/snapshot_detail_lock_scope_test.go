package transcript

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

// jsonMarshalSelectorName returns the local selector name for the
// encoding/json import as it appears in this file (normally "json", or the
// import alias when one is used). Resolving it from the parsed imports keeps
// the standard-library package name out of this test as a bare literal.
func jsonMarshalSelectorName(file *ast.File) string {
	const importPath = "encoding/json"
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// The shared durable payload boundary releases its snapshot lock when
// BuildSnapshotDetailBytes returns, so the safety contract is a SOURCE fact:
// hydration, folding, validation AND the final json.Marshal must all run inside
// the reader.WithSessionSnapshot callback. A runtime test can prove hydration
// lives inside the callback (the resolver observes it), but it cannot isolate
// the serialization step: moving only json.Marshal after the callback still
// leaves the lock held through every content read a runtime probe can see.
//
// This guard is the deterministic oracle for that remaining step. It reads the
// production source and fails if the final json.Marshal, the owned-bytes
// assignment, or the payload assignment leave the callback. It does not mock
// serialization and it does not re-run it: it asserts where the production
// serialization lives.
func TestBuildSnapshotDetailBytesSerializesInsideSnapshotCallback(t *testing.T) {
	path := filepath.Join(testutil.ModuleRoot(t), "internal", "transcript", "snapshot_detail.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	decl := findFuncDecl(parsed, "BuildSnapshotDetailBytes")
	if decl == nil {
		t.Fatalf("%s no longer declares BuildSnapshotDetailBytes; the shared durable payload boundary the runtime tests exercise is gone", path)
	}
	callback := snapshotCallbackLiteral(decl)
	if callback == nil {
		t.Fatal("BuildSnapshotDetailBytes no longer calls reader.WithSessionSnapshot with a callback literal; the boundary no longer shows that hydration and serialization run inside the snapshot lock")
	}

	marshalPackage := jsonMarshalSelectorName(parsed)
	if marshalPackage == "" {
		t.Fatalf("%s no longer imports encoding/json; the guard cannot locate the production serialization call", path)
	}
	marshalResult := assignTargetForSelectorCall(callback.Body, marshalPackage, "Marshal")
	if marshalResult == "" {
		t.Fatal("the final json.Marshal is not inside the WithSessionSnapshot callback; serialization would run after the shared lock is released, where a concurrent activation can remove the captured generation mid-encode")
	}
	if !assignsIdentFrom(callback.Body, "owned", marshalResult) {
		t.Fatalf("the callback no longer assigns the owned bytes from the marshal result %q; the bytes handed to callers must be materialized under the shared lock", marshalResult)
	}
	if !assignsAny(callback.Body, "payload") {
		t.Fatal("the callback no longer assigns the validated payload; the boundary no longer returns the payload it serialized under the shared lock")
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// snapshotCallbackLiteral returns the function literal passed to the
// WithSessionSnapshot call inside decl.
func snapshotCallbackLiteral(decl *ast.FuncDecl) *ast.FuncLit {
	var found *ast.FuncLit
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WithSessionSnapshot" || len(call.Args) == 0 {
			return true
		}
		literal, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
		if !ok {
			return true
		}
		found = literal
		return false
	})
	return found
}

// assignTargetForSelectorCall returns the left-hand variable assigned from a
// call of pkg.name(...), skipping the conventional error and blank targets.
func assignTargetForSelectorCall(body *ast.BlockStmt, pkg, name string) string {
	target := ""
	ast.Inspect(body, func(node ast.Node) bool {
		if target != "" {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return true
		}
		pkgIdent, ok := selector.X.(*ast.Ident)
		if !ok || pkgIdent.Name != pkg {
			return true
		}
		for _, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || ident.Name == "err" || ident.Name == "_" {
				continue
			}
			target = ident.Name
			return false
		}
		return true
	})
	return target
}

// assignsIdentFrom reports whether body assigns the identifier lhs from the
// identifier rhs.
func assignsIdentFrom(body *ast.BlockStmt, lhs, rhs string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if found {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		value, ok := assign.Rhs[0].(*ast.Ident)
		if !ok || value.Name != rhs {
			return true
		}
		for _, target := range assign.Lhs {
			if ident, ok := target.(*ast.Ident); ok && ident.Name == lhs {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// assignsAny reports whether body assigns the identifier lhs from anything.
func assignsAny(body *ast.BlockStmt, lhs string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if found {
			return false
		}
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, target := range assign.Lhs {
			if ident, ok := target.(*ast.Ident); ok && ident.Name == lhs {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
