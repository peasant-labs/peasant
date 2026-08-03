//go:build cgo

// cgo-only: the shared fixture tree (fixtures_test.go) contains TypeScript
// sources, whose import extraction needs the tree-sitter C bindings. The
// build-mode availability gate that runs in BOTH legs is availability_test.go.

package codegraph_test

import (
	"context"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
)

func TestBuild_Fixture_Layers(t *testing.T) {
	g := mustBuild(t, fixtureFiles())

	cases := []struct {
		id    string
		layer int
	}{
		// Go chain: cmd/app -> svc -> store -> util (longest path wins).
		{"cmd/app", 0},
		{"internal/svc", 1},
		{"internal/store", 2},
		{"internal/util", 3},
		// TS chain: app -> components/lib -> lib/util.
		{"web/src/app", 0},
		{"web/src/components", 1},
		{"web/src/lib", 1},
		{"web/src/lib/util", 2},
		// Directories outside the digraph inherit min descendant layer.
		{"cmd", 0},
		{"internal", 1},
		{"web", 0},
		{"web/src", 0},
		{"docs", 0},
		// Files inherit their directory's layer.
		{"cmd/app/main.go", 0},
		{"internal/util/util.go", 3},
		{"web/src/lib/util/format.ts", 2},
		{"README.md", 0},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if n := nodeByID(t, g, tc.id); n.Layer != tc.layer {
				t.Errorf("Layer = %d, want %d", n.Layer, tc.layer)
			}
		})
	}
}

// TestBuild_Fixture_OrderWithinLayers asserts the contract property directly:
// within each layer, Order is the index of a stable sort by (directory,
// name), covering 0..n-1 without gaps.
func TestBuild_Fixture_OrderWithinLayers(t *testing.T) {
	g := mustBuild(t, fixtureFiles())

	byLayer := make(map[int][]codegraph.Node)
	for _, n := range g.Nodes {
		byLayer[n.Layer] = append(byLayer[n.Layer], n)
	}
	for layer, nodes := range byLayer {
		sorted := append([]codegraph.Node(nil), nodes...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Parent != sorted[j].Parent {
				return sorted[i].Parent < sorted[j].Parent
			}
			return sorted[i].Name < sorted[j].Name
		})
		for i, n := range sorted {
			if n.Order != i {
				t.Errorf("layer %d: node %q Order = %d, want %d", layer, n.ID, n.Order, i)
			}
		}
	}
}

// TestBuild_Determinism builds the same fixture from shuffled and reversed
// input orders: every permutation must produce a byte-identical graph,
// including Layer and Order assignments.
func TestBuild_Determinism(t *testing.T) {
	files := fixtureFiles()
	reader := mapReader(files)
	sorted := pathsOf(files)

	baseline, err := codegraph.NewGraphBuilder().Build(context.Background(), sorted, reader)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	variants := map[string][]string{
		"second build same order": append([]string(nil), sorted...),
	}
	reversed := append([]string(nil), sorted...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	variants["reversed"] = reversed
	for _, seed := range []int64{1, 2, 3} {
		shuffled := append([]string(nil), sorted...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		variants["shuffled"+string(rune('a'+seed))] = shuffled
	}

	for name, order := range variants {
		t.Run(name, func(t *testing.T) {
			g, err := codegraph.NewGraphBuilder().Build(context.Background(), order, reader)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !reflect.DeepEqual(g, baseline) {
				t.Errorf("graph differs from baseline for input order %v", order)
			}
		})
	}
}

func TestBuild_CycleDetection(t *testing.T) {
	g := mustBuild(t, cycleFixture())

	want := []codegraph.Violation{
		{Kind: codegraph.ViolationCycle, From: "internal/a", To: "internal/b"},
		{Kind: codegraph.ViolationCycle, From: "internal/b", To: "internal/a"},
	}
	if !reflect.DeepEqual(g.Violations, want) {
		t.Errorf("violations mismatch:\n got: %#v\nwant: %#v", g.Violations, want)
	}

	// Cycle members collapse to a single layer below their importer.
	if n := nodeByID(t, g, "cmd/tool"); n.Layer != 0 {
		t.Errorf("cmd/tool Layer = %d, want 0", n.Layer)
	}
	a := nodeByID(t, g, "internal/a")
	b := nodeByID(t, g, "internal/b")
	if a.Layer != 1 || b.Layer != 1 {
		t.Errorf("cycle members layers = %d, %d, want 1, 1", a.Layer, b.Layer)
	}
}

func TestBuild_WrongWayDetection(t *testing.T) {
	g := mustBuild(t, wrongWayFixture())

	want := []codegraph.Violation{
		{Kind: codegraph.ViolationWrongWay, From: "internal/a", To: "cmd/app"},
	}
	if !reflect.DeepEqual(g.Violations, want) {
		t.Errorf("violations mismatch:\n got: %#v\nwant: %#v", g.Violations, want)
	}

	// The cmd/ pin holds cmd/app at layer 0 even though internal/a imports it.
	if n := nodeByID(t, g, "cmd/app"); n.Layer != 0 {
		t.Errorf("cmd/app Layer = %d, want 0 (pinned)", n.Layer)
	}
	if n := nodeByID(t, g, "internal/a"); n.Layer != 0 {
		t.Errorf("internal/a Layer = %d, want 0", n.Layer)
	}
}
