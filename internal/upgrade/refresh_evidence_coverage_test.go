package upgrade

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_evidence_fields.yaml
var refreshEvidenceFields []byte

// TestRefreshEvidenceCoverage requires every decision field to be populated by
// the planner or assigned to an honestly reported unread evidence kind.
func TestRefreshEvidenceCoverage(t *testing.T) {
	var fixture struct {
		Fields map[string]string `yaml:"fields"`
	}
	if err := yaml.Unmarshal(refreshEvidenceFields, &fixture); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "absent.db")
	plan, err := PlanRefresh(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "refresh_plan.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		selector, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RefreshEvidence" {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "ingest" {
			return true
		}
		for _, element := range lit.Elts {
			key, ok := element.(*ast.KeyValueExpr)
			if !ok {
				t.Fatal("refresh evidence has an unkeyed field")
			}
			name, ok := key.Key.(*ast.Ident)
			if !ok {
				t.Fatal("refresh evidence has a non-identifier key")
			}
			read[name.Name] = true
		}
		return true
	})
	typ := reflect.TypeOf(ingest.RefreshEvidence{})
	seen := map[string]bool{}
	kinds := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		kind, omitted := fixture.Fields[name]
		if read[name] == omitted {
			t.Errorf("%s must be either populated or listed as unread, not both or neither", name)
		}
		if omitted {
			kinds[kind] = true
			if !slices.Contains(plan.EvidenceNotRead, kind) {
				t.Errorf("%s is unread but %q is not reported", name, kind)
			}
		}
	}
	for name := range fixture.Fields {
		if !seen[name] {
			t.Errorf("unread evidence fixture names nonexistent field %s", name)
		}
	}
	for _, kind := range plan.EvidenceNotRead {
		if !kinds[kind] {
			t.Errorf("reported unread kind %q has no field", kind)
		}
	}
}
