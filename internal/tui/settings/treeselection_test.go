package settings

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/treeselection.yaml
var treeSelectionData []byte

type tsFixtureNode struct {
	ID       string            `yaml:"id"`
	Label    string            `yaml:"label"`
	State    string            `yaml:"state"`
	Meta     map[string]string `yaml:"meta"`
	Children []tsFixtureNode   `yaml:"children"`
}

type tsExpectProject struct {
	GitRemote string   `yaml:"gitRemote"`
	Name      string   `yaml:"name"`
	Branches  []string `yaml:"branches"`
}

type tsExpectHarness struct {
	Projects []tsExpectProject `yaml:"projects"`
	Sessions []string          `yaml:"sessions"`
}

type tsCase struct {
	Name            string                     `yaml:"name"`
	ExpectMode      string                     `yaml:"expectMode"`
	ExpectConflict  bool                       `yaml:"expectConflict"`
	ExpectHarnesses map[string]tsExpectHarness `yaml:"expectHarnesses"`
	Roots           []tsFixtureNode            `yaml:"roots"`
}

type tsDocument struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	Cases             []tsCase `yaml:"cases"`
}

func parseTriState(t *testing.T, s string) kit.TriState {
	t.Helper()
	switch s {
	case "", "unchecked":
		return kit.Unchecked
	case "partial":
		return kit.Partial
	case "checked":
		return kit.Checked
	case "conflict":
		return kit.Conflict
	default:
		t.Fatalf("unknown tri-state %q", s)
		return kit.Unchecked
	}
}

func toNode(t *testing.T, fn tsFixtureNode) *kit.TreeNode {
	t.Helper()
	n := &kit.TreeNode{ID: fn.ID, Label: fn.Label, State: parseTriState(t, fn.State)}
	if len(fn.Meta) > 0 {
		n.Meta = map[string]string{}
		for k, v := range fn.Meta {
			n.Meta[k] = v
		}
	}
	for _, c := range fn.Children {
		n.Children = append(n.Children, toNode(t, c))
	}
	return n
}

func loadTreeSelectionDoc(t *testing.T) tsDocument {
	t.Helper()
	var doc tsDocument
	dec := yaml.NewDecoder(bytes.NewReader(treeSelectionData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/treeselection.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("treeselection.yaml must hold exactly one document")
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

func expectedSelection(c tsCase) TreeSelection {
	ts := TreeSelection{Mode: config.SelectionMode(c.ExpectMode)}
	if len(c.ExpectHarnesses) == 0 {
		return ts
	}
	ts.Harnesses = map[string]config.SelectionHarnessConfig{}
	for h, eh := range c.ExpectHarnesses {
		hc := config.SelectionHarnessConfig{Sessions: eh.Sessions}
		for _, p := range eh.Projects {
			hc.Projects = append(hc.Projects, config.ProjectSelection{
				GitRemote: p.GitRemote, Name: p.Name, Branches: p.Branches,
			})
		}
		ts.Harnesses[h] = hc
	}
	return ts
}

func TestTreeSelection_RoundTrip(t *testing.T) {
	doc := loadTreeSelectionDoc(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			var roots []*kit.TreeNode
			for _, r := range c.Roots {
				n := toNode(t, r)
				rollup(n)
				roots = append(roots, n)
			}

			if got := HasConflict(roots); got != c.ExpectConflict {
				t.Fatalf("HasConflict = %v, want %v", got, c.ExpectConflict)
			}

			got := FromTreeNodes(roots)
			want := expectedSelection(c)
			if got.Mode != want.Mode {
				t.Fatalf("Mode = %q, want %q", got.Mode, want.Mode)
			}
			if !selectionsEqual(got, want) {
				t.Fatalf("selection mismatch\n got: %#v\nwant: %#v", got, want)
			}

			// A conflict forest must never persist the conflict node, and it
			// must fail validation at the tree-field boundary. The derived
			// selection itself stays internally consistent.
			if err := got.Validate(); err != nil {
				t.Fatalf("derived selection failed Validate: %v", err)
			}
		})
	}
}

// TestTreeSelection_AllRoundTripsToConfig proves the derived selection converts
// straight into the real config.SelectionConfig the ingest pipeline consumes,
// preserving an existing auto-ingest flag.
func TestTreeSelection_ToSelectionConfig(t *testing.T) {
	harness := defaults.HarnessClaudeCode.String()
	ts := TreeSelection{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness: {Sessions: []string{"sess-p1"}},
		},
	}
	sc := ts.ToSelectionConfig(true)
	if sc.Mode != config.SelectionModeSelected || !sc.AutoIngestNewBranches {
		t.Fatalf("unexpected config: %#v", sc)
	}
	if len(sc.Harnesses[harness].Sessions) != 1 {
		t.Fatalf("harness sessions not preserved: %#v", sc.Harnesses)
	}
}
