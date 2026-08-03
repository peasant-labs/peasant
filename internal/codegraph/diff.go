package codegraph

import "sort"

// GraphDiff describes the structural delta between a base and a head graph.
// Identity is positional, not attribute-based: a node counts as added/removed
// by ID; an edge by (From, To) — a changed Count alone is not a delta; a
// violation by (Kind, From, To). All slices are sorted deterministically and
// never nil.
type GraphDiff struct {
	AddedNodes    []Node
	RemovedNodes  []Node
	AddedEdges    []Edge
	RemovedEdges  []Edge
	NewViolations []Violation
}

// Diff computes the structural delta from base to head. A nil graph is
// treated as empty, so Diff(nil, head) reports everything in head as added.
func Diff(base, head *Graph) GraphDiff {
	if base == nil {
		base = &Graph{}
	}
	if head == nil {
		head = &Graph{}
	}

	d := GraphDiff{
		AddedNodes:    []Node{},
		RemovedNodes:  []Node{},
		AddedEdges:    []Edge{},
		RemovedEdges:  []Edge{},
		NewViolations: []Violation{},
	}

	baseNodes := nodeIDSet(base)
	headNodes := nodeIDSet(head)
	for _, n := range head.Nodes {
		if !baseNodes[n.ID] {
			d.AddedNodes = append(d.AddedNodes, n)
		}
	}
	for _, n := range base.Nodes {
		if !headNodes[n.ID] {
			d.RemovedNodes = append(d.RemovedNodes, n)
		}
	}

	baseEdges := edgeKeySet(base)
	headEdges := edgeKeySet(head)
	for _, e := range head.Edges {
		if !baseEdges[edgeKey{e.From, e.To}] {
			d.AddedEdges = append(d.AddedEdges, e)
		}
	}
	for _, e := range base.Edges {
		if !headEdges[edgeKey{e.From, e.To}] {
			d.RemovedEdges = append(d.RemovedEdges, e)
		}
	}

	baseViol := make(map[Violation]bool, len(base.Violations))
	for _, v := range base.Violations {
		baseViol[v] = true
	}
	for _, v := range head.Violations {
		if !baseViol[v] {
			d.NewViolations = append(d.NewViolations, v)
		}
	}

	sortNodes(d.AddedNodes)
	sortNodes(d.RemovedNodes)
	sortEdges(d.AddedEdges)
	sortEdges(d.RemovedEdges)
	sortViolations(d.NewViolations)
	return d
}

type edgeKey struct{ from, to string }

func nodeIDSet(g *Graph) map[string]bool {
	set := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		set[n.ID] = true
	}
	return set
}

func edgeKeySet(g *Graph) map[edgeKey]bool {
	set := make(map[edgeKey]bool, len(g.Edges))
	for _, e := range g.Edges {
		set[edgeKey{e.From, e.To}] = true
	}
	return set
}

func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
}

func sortViolations(violations []Violation) {
	sort.Slice(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
}
