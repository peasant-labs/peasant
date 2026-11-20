package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

type commitGuardFinding struct {
	position token.Position
	function string
}

// commitGuardFindings checks each function separately, including closures.
// A single address argument to a deferred call is treated conservatively as
// a transaction finalizer, including renamed and directly invoked finalizers.
// Object identity also catches a local err shadowing the named result.
func commitGuardFindings(fset *token.FileSet, file *ast.File) []commitGuardFinding {
	var findings []commitGuardFinding
	var check func(string, *ast.FuncType, *ast.BlockStmt)
	check = func(function string, typ *ast.FuncType, body *ast.BlockStmt) {
		if body == nil {
			return
		}
		results := make(map[*ast.Object]bool)
		if typ.Results != nil {
			for _, field := range typ.Results.List {
				for _, name := range field.Names {
					results[name.Obj] = true
				}
			}
		}
		ast.Inspect(body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				check(function+"<closure>", lit.Type, lit.Body)
				return false
			}
			d, ok := n.(*ast.DeferStmt)
			if !ok {
				return true
			}
			if len(d.Call.Args) != 1 {
				return true
			}
			arg, ok := d.Call.Args[0].(*ast.UnaryExpr)
			if !ok || arg.Op != token.AND {
				return true
			}
			id, ok := arg.X.(*ast.Ident)
			if ok && !results[id.Obj] {
				findings = append(findings, commitGuardFinding{fset.Position(d.Pos()), function})
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			check(fn.Name.Name, fn.Type, fn.Body)
		}
	}
	return findings
}

func TestDeferredTransactionErrorReturned(t *testing.T) {
	// This batch returns per-item results; its earlier defer reads txnErr
	// after transaction finalization and marks every affected result failed.
	const exception = "entries_writer.go:IndexSessionEntryBatch"
	exceptionSeen := false
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range commitGuardFindings(fset, file) {
			key := name + ":" + finding.function
			if key == exception && !exceptionSeen {
				exceptionSeen = true
				continue
			}
			t.Errorf("%s: deferred transaction err must be the named err result", finding.position)
		}
	}
	if !exceptionSeen {
		t.Errorf("reviewed exception %s is missing", exception)
	}
}

const commitGuardProbePath = "testdata/commit_guard_probe.gosrc"

func TestDeferredTransactionErrorGuardProbe(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, commitGuardProbePath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	findings := commitGuardFindings(fset, file)
	got := make(map[int]bool)
	for _, finding := range findings {
		got[finding.position.Line] = true
	}
	if !got[4] || !got[10] || !got[20] || !got[25] || len(got) != 4 {
		t.Fatalf("guard findings = %v, want unsafe defers on lines 4, 10, 20 and 25", findings)
	}
}
