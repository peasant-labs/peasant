package ingest

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Source verification reads actual production syntax. It deliberately contains
// no harness kind lists. Tests may replace source bytes to prove the same gate
// rejects a newly added handler rather than comparing two copied vocabularies.
type recordKindSourceTree struct {
	files  map[string]*ast.File
	values map[string]ast.Expr
}

func readRecordKindSourceTree(overrides map[string][]byte) (*recordKindSourceTree, error) {
	tree := &recordKindSourceTree{files: map[string]*ast.File{}, values: map[string]ast.Expr{}}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, ok := overrides[path]
		if !ok {
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, err
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
		if err != nil {
			return nil, err
		}
		tree.files[path] = file
		for _, decl := range file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range group.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i < len(value.Values) {
						tree.values[name.Name] = value.Values[i]
					}
				}
			}
		}
	}
	return tree, nil
}

func sourceExpression(expr ast.Expr) string {
	var out bytes.Buffer
	_ = format.Node(&out, token.NewFileSet(), expr)
	return out.String()
}

func (tree *recordKindSourceTree) stringValue(expr ast.Expr, depth int) (string, bool) {
	if depth > 12 {
		return "", false
	}
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			text, err := strconv.Unquote(value.Value)
			return text, err == nil
		}
	case *ast.Ident:
		if target := tree.values[value.Name]; target != nil {
			return tree.stringValue(target, depth+1)
		}
	case *ast.CallExpr:
		if len(value.Args) == 1 {
			return tree.stringValue(value.Args[0], depth+1)
		}
	}
	return "", false
}

func sourceSymbol(fn *ast.FuncDecl) string {
	if fn.Recv == nil {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	return sourceExpression(receiver) + "." + fn.Name.Name
}

func (tree *recordKindSourceTree) inventory(source RecordKindSource) (map[string]RecordKindMatch, error) {
	file := tree.files[source.File]
	if file == nil {
		return nil, fmt.Errorf("missing production file %s", source.File)
	}
	var root ast.Node
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && sourceSymbol(fn) == source.Symbol {
			root = fn.Body
		}
		if group, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range group.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok {
					for i, name := range value.Names {
						if name.Name == source.Symbol && i < len(value.Values) {
							root = value.Values[i]
						}
					}
				}
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("missing production symbol %s in %s", source.Symbol, source.File)
	}
	result := map[string]RecordKindMatch{}
	add := func(expr ast.Expr, match RecordKindMatch) {
		if value, ok := tree.stringValue(expr, 0); ok {
			result[value] = match
		}
	}
	ast.Inspect(root, func(node ast.Node) bool {
		if source.Switch != "" {
			if stmt, ok := node.(*ast.SwitchStmt); ok && sourceExpression(stmt.Tag) == source.Switch {
				for _, raw := range stmt.Body.List {
					clause := raw.(*ast.CaseClause)
					for _, expr := range clause.List {
						add(expr, RecordKindLiteral)
					}
				}
			}
			return true
		}
		if source.PrefixArgument != "" {
			if call, ok := node.(*ast.CallExpr); ok && sourceExpression(call.Fun) == "strings.HasPrefix" && len(call.Args) == 2 && sourceExpression(call.Args[0]) == source.PrefixArgument {
				add(call.Args[1], RecordKindPrefix)
			}
			return true
		}
		switch value := node.(type) {
		case *ast.CompositeLit:
			switch value.Type.(type) {
			case *ast.MapType:
				for _, element := range value.Elts {
					if kv, ok := element.(*ast.KeyValueExpr); ok {
						add(kv.Key, RecordKindLiteral)
					}
				}
			case *ast.ArrayType:
				for _, element := range value.Elts {
					add(element, RecordKindLiteral)
				}
			}
		case *ast.CallExpr:
			if !source.ListsOnly && sourceExpression(value.Fun) == "append" {
				for _, argument := range value.Args[1:] {
					add(argument, RecordKindLiteral)
				}
			}
		}
		return true
	})
	if len(result) == 0 {
		return nil, fmt.Errorf("empty production inventory %s %s; repair its selector", source.File, source.Symbol)
	}
	return result, nil
}

func verifyRecordKindSources(registry RecordKindRegistry, tree *recordKindSourceTree) error {
	for harness, section := range registry.Harnesses {
		for _, inventory := range section.Inventories {
			found := map[string]RecordKindMatch{}
			for _, source := range inventory.Sources {
				kinds, err := tree.inventory(source)
				if err != nil {
					return err
				}
				for kind, match := range kinds {
					found[kind] = match
				}
			}
			for _, row := range inventory.Kinds {
				match := row.Match
				if match == "" {
					match = RecordKindLiteral
				}
				if actual, exists := found[row.Kind]; !exists || actual != match {
					return fmt.Errorf("%s/%s/%s/%s is not matched by production syntax", harness, inventory.Context, inventory.Namespace, row.Kind)
				}
				delete(found, row.Kind)
			}
			for kind := range found {
				return fmt.Errorf("%s/%s/%s: production kind %q has no registry row", harness, inventory.Context, inventory.Namespace, kind)
			}
		}
	}
	return nil
}

func TestRecordKindsProductionSources(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := readRecordKindSourceTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordKindSources(registry, tree); err != nil {
		t.Fatal(err)
	}
}
