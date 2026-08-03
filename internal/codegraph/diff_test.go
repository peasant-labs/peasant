//go:build cgo

// cgo-only: the shared fixture tree (fixtures_test.go) contains TypeScript
// sources, whose import extraction needs the tree-sitter C bindings. The
// build-mode availability gate that runs in BOTH legs is availability_test.go.

package codegraph_test

import (
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
)

// TestDiff_AddRemove builds head as a mutation of the base fixture:
//   - docs/guide.md removed (drops the docs module too),
//   - internal/metrics added (new package + file + edge to util),
//   - internal/util now imports internal/svc, closing a svc -> store -> util
//     -> svc cycle (new edge + new cycle violations).
func TestDiff_AddRemove(t *testing.T) {
	baseFiles := fixtureFiles()
	base := mustBuild(t, baseFiles)

	headFiles := fixtureFiles()
	delete(headFiles, "docs/guide.md")
	headFiles["internal/metrics/metrics.go"] = `package metrics

import "example.com/proj/internal/util"

const M = util.V
`
	headFiles["internal/util/util.go"] = `package util

import "example.com/proj/internal/svc"

const V = 1

var _ = svc.Run
`
	head := mustBuild(t, headFiles)

	d := codegraph.Diff(base, head)

	wantAddedIDs := []string{"internal/metrics", "internal/metrics/metrics.go"}
	if got := nodeIDs(d.AddedNodes); !reflect.DeepEqual(got, wantAddedIDs) {
		t.Errorf("AddedNodes = %v, want %v", got, wantAddedIDs)
	}
	wantRemovedIDs := []string{"docs", "docs/guide.md"}
	if got := nodeIDs(d.RemovedNodes); !reflect.DeepEqual(got, wantRemovedIDs) {
		t.Errorf("RemovedNodes = %v, want %v", got, wantRemovedIDs)
	}

	wantAddedEdges := []codegraph.Edge{
		{From: "internal/metrics", To: "internal/util", Count: 1},
		{From: "internal/util", To: "internal/svc", Count: 1},
	}
	if !reflect.DeepEqual(d.AddedEdges, wantAddedEdges) {
		t.Errorf("AddedEdges = %#v, want %#v", d.AddedEdges, wantAddedEdges)
	}
	if len(d.RemovedEdges) != 0 {
		t.Errorf("RemovedEdges = %#v, want empty", d.RemovedEdges)
	}

	wantViolations := []codegraph.Violation{
		{Kind: codegraph.ViolationCycle, From: "internal/store", To: "internal/util"},
		{Kind: codegraph.ViolationCycle, From: "internal/svc", To: "internal/store"},
		{Kind: codegraph.ViolationCycle, From: "internal/svc", To: "internal/util"},
		{Kind: codegraph.ViolationCycle, From: "internal/util", To: "internal/svc"},
	}
	if !reflect.DeepEqual(d.NewViolations, wantViolations) {
		t.Errorf("NewViolations = %#v, want %#v", d.NewViolations, wantViolations)
	}
}

func TestDiff_Identity(t *testing.T) {
	g := mustBuild(t, fixtureFiles())
	d := codegraph.Diff(g, g)
	if len(d.AddedNodes)+len(d.RemovedNodes)+len(d.AddedEdges)+len(d.RemovedEdges)+len(d.NewViolations) != 0 {
		t.Errorf("Diff(g, g) not empty: %+v", d)
	}
	if d.AddedNodes == nil || d.RemovedNodes == nil || d.AddedEdges == nil ||
		d.RemovedEdges == nil || d.NewViolations == nil {
		t.Errorf("diff slices must be non-nil: %+v", d)
	}
}

func TestDiff_NilGraphsTreatedAsEmpty(t *testing.T) {
	g := mustBuild(t, fixtureFiles())

	d := codegraph.Diff(nil, g)
	if len(d.AddedNodes) != len(g.Nodes) {
		t.Errorf("Diff(nil, g).AddedNodes = %d nodes, want %d", len(d.AddedNodes), len(g.Nodes))
	}
	if len(d.RemovedNodes) != 0 {
		t.Errorf("Diff(nil, g).RemovedNodes = %d nodes, want 0", len(d.RemovedNodes))
	}

	d = codegraph.Diff(g, nil)
	if len(d.RemovedNodes) != len(g.Nodes) {
		t.Errorf("Diff(g, nil).RemovedNodes = %d nodes, want %d", len(d.RemovedNodes), len(g.Nodes))
	}
	if len(d.AddedEdges) != 0 {
		t.Errorf("Diff(g, nil).AddedEdges = %d, want 0", len(d.AddedEdges))
	}
}

// TestDiff_EdgeCountChangeIsNotADelta: edge identity is (From, To) — a Count
// change alone must not appear as added or removed.
func TestDiff_EdgeCountChangeIsNotADelta(t *testing.T) {
	baseFiles := map[string]string{
		"go.mod":          "module example.com/m\n",
		"internal/a/a.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
		"internal/b/b.go": "package b\n\nconst B = 1\n",
	}
	headFiles := map[string]string{
		"go.mod":          "module example.com/m\n",
		"internal/a/a.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
		"internal/a/c.go": "package a\n\nimport \"example.com/m/internal/b\"\n",
		"internal/b/b.go": "package b\n\nconst B = 1\n",
	}
	d := codegraph.Diff(mustBuild(t, baseFiles), mustBuild(t, headFiles))
	if len(d.AddedEdges) != 0 || len(d.RemovedEdges) != 0 {
		t.Errorf("count-only change produced edge deltas: added %#v removed %#v",
			d.AddedEdges, d.RemovedEdges)
	}
	if got := nodeIDs(d.AddedNodes); !reflect.DeepEqual(got, []string{"internal/a/c.go"}) {
		t.Errorf("AddedNodes = %v, want [internal/a/c.go]", got)
	}
}

func nodeIDs(nodes []codegraph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}
