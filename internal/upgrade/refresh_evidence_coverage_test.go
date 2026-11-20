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
// Behavioural fixtures exercise decisions, but cannot expose a newly added
// evidence field whose zero value happens not to affect those decisions. This
// inventory guards that boundary; it scans only planRefresh, runs without
// processes or shared state, and can go when the evidence type makes omitted
// fields impossible to represent. The fixture and the planner's reported
// unread kinds provide positive and negative controls.
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
	typ := reflect.TypeOf(ingest.RefreshEvidence{})
	var constructor *ast.FuncDecl
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "planRefresh" {
			constructor = fn
			break
		}
	}
	if constructor == nil {
		t.Fatal("planRefresh constructor site not found")
	}
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		selector, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RefreshEvidence" {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != filepath.Base(typ.PkgPath()) {
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
