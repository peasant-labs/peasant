package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/tree_annotations.yaml
var treeAnnotationData []byte

// treeAnnotationCase is one rendered session row: the Meta a source attached,
// the width it renders at, and what its row must and must not say.
type treeAnnotationCase struct {
	Name         string   `yaml:"name"`
	Label        string   `yaml:"label"`
	ChildCount   string   `yaml:"childCount"`
	Ingested     bool     `yaml:"ingested"`
	Tracked      bool     `yaml:"tracked"`
	State        string   `yaml:"state"`
	Width        int      `yaml:"width"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

// treeAnnotationDocument is the whole fixture plus its row-count guard.
type treeAnnotationDocument struct {
	ExpectedCaseCount int                  `yaml:"expectedCaseCount"`
	Cases             []treeAnnotationCase `yaml:"cases"`
}

func loadTreeAnnotations(t *testing.T) treeAnnotationDocument {
	t.Helper()
	var doc treeAnnotationDocument
	dec := yaml.NewDecoder(bytes.NewReader(treeAnnotationData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/tree_annotations.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("tree_annotations.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	for _, c := range doc.Cases {
		requireCaseAssertions(t, "tree annotation", c.Name, len(c.WantContains)+len(c.WantMissing))
		if c.Width <= 0 {
			t.Fatalf("tree annotation fixture case %q declares width %d; a non-positive width renders nothing to assert on", c.Name, c.Width)
		}
		if c.State != "" && c.State != "unchecked" && c.State != "checked" {
			t.Fatalf("tree annotation fixture case %q declares unknown current state %q", c.Name, c.State)
		}
	}
	return doc
}

// requireCaseAssertions fails when a fixture case declares no expectations at
// all. An empty want list turns its case into a guaranteed pass, so an edit that
// empties one must fail loudly rather than quietly stop testing anything.
func requireCaseAssertions(t *testing.T, corpus, caseName string, total int) {
	t.Helper()
	if total == 0 {
		t.Fatalf("%s fixture case %q declares no expected values; an empty want list turns the case into a guaranteed pass - state what the run must observe",
			corpus, caseName)
	}
}

// annotatedForest builds the one-project/one-session forest a case renders.
func annotatedForest(c treeAnnotationCase) []*kit.TreeNode {
	meta := map[string]string{}
	if c.ChildCount != "" {
		meta[kit.MetaChildCount] = c.ChildCount
	}
	if c.Ingested {
		meta[kit.MetaIngested] = kit.MetaIngestedValue
	}
	if c.Tracked {
		meta[kit.MetaTracked] = kit.MetaTrackedValue
	}
	state := kit.Unchecked
	if c.State == "checked" {
		state = kit.Checked
	}
	return []*kit.TreeNode{{
		ID:       "project",
		Label:    "acme/tool",
		Children: []*kit.TreeNode{{ID: "sess", Label: c.Label, Meta: meta, State: state}},
	}}
}

// TestTree_RowAnnotations proves each row says exactly what its Meta earns it:
// a parent session summarises its subagents as a correctly-pluralised count, an
// already-stored session says so, an absent/zero/unreadable count annotates
// nothing, and a row too narrow to carry the count keeps its title instead.
func TestTree_RowAnnotations(t *testing.T) {
	t.Parallel()
	doc := loadTreeAnnotations(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			view := loadStaticTree(t, annotatedForest(c), c.Width, 4).View()
			for _, want := range c.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("row must contain %q; view:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(view, missing) {
					t.Errorf("row must not contain %q; view:\n%s", missing, view)
				}
			}
		})
	}
}

// TestTree_RowAnnotationsGolden captures each annotated row as a rendered
// screen, so the muted child-session count, the already-imported marker, and
// what a row looks like when it is too narrow to carry either are all visible in
// the test artifact rather than only asserted as substrings.
func TestTree_RowAnnotationsGolden(t *testing.T) {
	doc := loadTreeAnnotations(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := loadStaticTree(t, annotatedForest(c), c.Width, 4).View()
			golden.RequireEqual(t, []byte(view))
		})
	}
}

// TestTree_AnnotatedRowFitsItsWidth proves the goldens cannot bake in an
// overflow: an annotation is fitted into the row's budget, never appended past
// it, so every captured line is exactly the width the tree was sized to.
func TestTree_AnnotatedRowFitsItsWidth(t *testing.T) {
	doc := loadTreeAnnotations(t)
	for _, c := range doc.Cases {
		for i, line := range strings.Split(loadStaticTree(t, annotatedForest(c), c.Width, 4).View(), "\n") {
			if got := lipgloss.Width(line); got != c.Width {
				t.Errorf("case %q line %d is %d cells, want exactly %d", c.Name, i, got, c.Width)
			}
		}
	}
}

// TestTree_AnnotatedRowStaysOneLine proves an annotated row obeys the
// one-row-per-node invariant: the tree still renders exactly its height in
// lines, so the annotation never wraps a session onto a second row and never
// breaks the cursor/scroll math that counts one row per line.
func TestTree_AnnotatedRowStaysOneLine(t *testing.T) {
	t.Parallel()
	doc := loadTreeAnnotations(t)
	const height = 4
	for _, c := range doc.Cases {
		view := loadStaticTree(t, annotatedForest(c), c.Width, height).View()
		if got := strings.Count(view, "\n") + 1; got != height {
			t.Errorf("case %q rendered %d lines at height %d; an annotation split a row", c.Name, got, height)
		}
	}
}
