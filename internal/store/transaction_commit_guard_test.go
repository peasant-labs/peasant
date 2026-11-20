package store

import (
	"embed"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This guard checks deferred single-address finalizers, including those wrapped
// in a deferred closure. Their error must be a named result of the function
// whose return triggers the defer. It conservatively flags unresolved names.
// It is a syntax/identity guard, not a proof of transaction behavior; arbitrary
// helper calls and aliases remain review decisions. V23 is guard-only because
// the historical schema needed for a realistic migration test is absent.
//
//go:embed testdata/transaction_commit_guard.yaml
var commitGuardYAML []byte

//go:embed testdata/transaction_commit_guard_probes.yaml
var commitGuardProbesYAML []byte

//go:embed testdata/transaction_commit_guard_probes
var commitGuardSources embed.FS

const commitGuardProbesDir = "testdata/transaction_commit_guard_probes"

type commitGuardEntry struct {
	File     string `yaml:"file"`
	Function string `yaml:"function"`
	Reason   string `yaml:"reason"`
}

type commitGuardFinding struct {
	scope    guardScope
	position token.Position
}

func scanCommitGuard(names []string, read func(string) ([]byte, error)) ([]commitGuardFinding, error) {
	scan, err := scanGuardSources(names, read)
	if err != nil {
		return nil, err
	}
	var findings []commitGuardFinding
	for i, file := range scan.files {
		name, fset := names[i], scan.fset
		info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object)}
		// Files are intentionally checked independently: the store includes platform
		// siblings and probes can be incomplete. Errors are expected; local bindings
		// remain useful and an unresolved identifier is always rejected.
		conf := types.Config{Error: func(error) {}, FakeImportC: true}
		_, _ = conf.Check("guard", fset, []*ast.File{file}, info)
		var check func(guardScope, *ast.FuncType, *ast.BlockStmt)
		check = func(scope guardScope, typ *ast.FuncType, body *ast.BlockStmt) {
			if body == nil {
				return
			}
			results := make(map[types.Object]bool)
			if typ.Results != nil {
				for _, field := range typ.Results.List {
					for _, id := range field.Names {
						if obj := info.Defs[id]; obj != nil && types.Identical(obj.Type(), types.Universe.Lookup("error").Type()) {
							results[obj] = true
						}
					}
				}
			}
			inspectCall := func(call *ast.CallExpr, pos token.Pos) {
				if len(call.Args) != 1 {
					return
				}
				unary, ok := call.Args[0].(*ast.UnaryExpr)
				if !ok || unary.Op != token.AND {
					return
				}
				id, ok := unary.X.(*ast.Ident)
				if !ok {
					return
				}
				obj := info.Uses[id]
				if obj == nil || !results[obj] {
					findings = append(findings, commitGuardFinding{scope, fset.Position(pos)})
				}
			}
			ast.Inspect(body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.DeferStmt:
					if lit, ok := n.Call.Fun.(*ast.FuncLit); ok {
						// Calls directly inside the wrapper run as part of this defer. Nested
						// closures are not assumed to execute and are checked independently.
						ast.Inspect(lit.Body, func(inner ast.Node) bool {
							if nested, ok := inner.(*ast.FuncLit); ok {
								check(guardScope{scope.File, scope.Function + "<closure>"}, nested.Type, nested.Body)
								return false
							}
							if call, ok := inner.(*ast.CallExpr); ok {
								inspectCall(call, call.Pos())
							}
							return true
						})
						return false
					}
					inspectCall(n.Call, n.Pos())
					return true
				case *ast.FuncLit:
					check(guardScope{scope.File, scope.Function + "<closure>"}, n.Type, n.Body)
					return false
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				check(guardScope{name, storeFuncName(fn)}, fn.Type, fn.Body)
			}
		}
	}
	return findings, nil
}

func commitGuardExceptions(entries []commitGuardEntry) (map[guardScope]bool, error) {
	result := make(map[guardScope]bool)
	for _, entry := range entries {
		key := guardScope{entry.File, entry.Function}
		if entry.File == "" || entry.Function == "" || strings.TrimSpace(entry.Reason) == "" {
			return nil, fmt.Errorf("exception needs file, function and reason: %+v", entry)
		}
		if result[key] {
			return nil, fmt.Errorf("duplicate exception: %+v", key)
		}
		result[key] = true
	}
	return result, nil
}

func decodeCommitGuard(t *testing.T, data []byte, out any) {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredTransactionErrorReturned(t *testing.T) {
	var fixture struct {
		Exceptions []commitGuardEntry `yaml:"exceptions"`
	}
	decodeCommitGuard(t, commitGuardYAML, &fixture)
	allowed, err := commitGuardExceptions(fixture.Exceptions)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := scanCommitGuard(storeProductionFiles(t), os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[guardScope]int)
	for _, finding := range findings {
		exception := finding.scope
		exception.Function = strings.TrimPrefix(exception.Function, "Store.")
		if allowed[exception] {
			seen[exception]++
			continue
		}
		t.Errorf("%s: %s: deferred finalizer error must be a named result", finding.position, finding.scope.Function)
	}
	for scope := range allowed {
		if seen[scope] != 1 {
			t.Errorf("reviewed exception %v occurs %d times, want exactly one; remove stale or review additional sites", scope, seen[scope])
		}
	}
}

func TestDeferredTransactionErrorGuardProbe(t *testing.T) {
	var fixture struct {
		Probes []struct {
			Name   string `yaml:"name"`
			Source string `yaml:"source"`
		} `yaml:"probes"`
	}
	decodeCommitGuard(t, commitGuardProbesYAML, &fixture)
	if len(fixture.Probes) == 0 {
		t.Fatal("no commit guard probes")
	}
	seen := make(map[string]bool)
	for _, probe := range fixture.Probes {
		if probe.Name == "" || probe.Source == "" || seen[probe.Name] {
			t.Fatalf("invalid or duplicate probe: %+v", probe)
		}
		seen[probe.Name] = true
		t.Run(probe.Name, func(t *testing.T) {
			read := func(name string) ([]byte, error) {
				return commitGuardSources.ReadFile(commitGuardProbesDir + "/" + name)
			}
			src, err := read(probe.Source)
			if err != nil {
				t.Fatal(err)
			}
			var want []int
			for i, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, "// want") {
					want = append(want, i+1)
				}
			}
			findings, err := scanCommitGuard([]string{probe.Source}, read)
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, finding := range findings {
				got = append(got, finding.position.Line)
			}
			sort.Ints(got)
			sort.Ints(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("finding lines = %v, // want lines = %v", got, want)
			}
		})
	}
}
