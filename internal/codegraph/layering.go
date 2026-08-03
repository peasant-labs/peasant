package codegraph

// Deterministic layering:
//
//  1. Build the package-level import digraph (vertices = directories that
//     directly contain Go/TS source files; edges = aggregated imports).
//  2. Condense it with Tarjan's SCC algorithm. Every edge inside a
//     multi-member SCC is flagged as a cycle violation.
//  3. Longest-path layering on the condensation DAG: an SCC nothing imports
//     sits at layer 0; every edge u→v forces layer(v) ≥ layer(u)+1, so
//     dependencies point downward (deeper deps = higher layer number).
//  4. Any SCC containing a cmd/ package is pinned to layer 0.
//  5. After pinning, every inter-SCC edge whose target layer is not strictly
//     below its source layer is flagged as a wrong-way violation. (Without
//     the pin, longest-path layering guarantees downward edges, so wrong-way
//     violations arise only around cmd/.)
//  6. Directories outside the digraph inherit the minimum layer of their
//     descendant packages (or 0); files inherit their directory's layer.
//  7. Order = index within the layer after a stable sort by (directory,
//     name).
//
// Every step iterates in sorted order, so identical input always yields
// identical Layer/Order assignments and violation lists.

import (
	"maps"
	"slices"
	"sort"
	"strings"
)

// cmdDirName is the conventional Go binaries directory pinned to layer 0.
const cmdDirName = "cmd"

// applyLayout assigns Layer and Order to every node in the tree and returns
// the sorted violation list.
func applyLayout(t *fileTree, edges []Edge) []Violation {
	pkgs := slices.Sorted(maps.Keys(t.dirHasSource))
	layerOf, violations := layerPackages(pkgs, edges)

	// Directories outside the digraph inherit the minimum layer among their
	// descendant packages.
	minAncestor := make(map[string]int)
	for _, pkg := range pkgs {
		layer := layerOf[pkg]
		for a := parentDir(pkg); a != ""; a = parentDir(a) {
			if cur, ok := minAncestor[a]; !ok || layer < cur {
				minAncestor[a] = layer
			}
		}
	}

	for _, d := range slices.Sorted(maps.Keys(t.dirSet)) {
		n := t.nodes[d]
		if layer, ok := layerOf[d]; ok {
			n.Layer = layer
		} else if layer, ok := minAncestor[d]; ok {
			n.Layer = layer
		} else {
			n.Layer = 0
		}
	}
	for _, f := range t.sortedFiles() {
		n := t.nodes[f]
		if n.Parent != "" {
			n.Layer = t.nodes[n.Parent].Layer
		} else {
			n.Layer = 0
		}
	}

	assignOrder(t)
	return violations
}

// layerPackages runs SCC condensation + longest-path layering over the
// package digraph and returns the layer of every package plus the cycle and
// wrong-way violations.
func layerPackages(pkgs []string, edges []Edge) (map[string]int, []Violation) {
	adj := make(map[string][]string, len(pkgs))
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To) // edges pre-sorted => adjacency sorted
	}

	sccs := tarjanSCC(pkgs, adj)

	sccOf := make(map[string]int, len(pkgs))
	for i, members := range sccs {
		for _, m := range members {
			sccOf[m] = i
		}
	}

	violations := []Violation{}

	// Cycle violations: every edge inside a multi-member SCC.
	for _, e := range edges {
		if sccOf[e.From] == sccOf[e.To] && len(sccs[sccOf[e.From]]) > 1 {
			violations = append(violations, Violation{Kind: ViolationCycle, From: e.From, To: e.To})
		}
	}

	// Condensation DAG (deduplicated, sorted adjacency).
	condensed := make(map[int]map[int]bool)
	for _, e := range edges {
		from, to := sccOf[e.From], sccOf[e.To]
		if from == to {
			continue
		}
		if condensed[from] == nil {
			condensed[from] = make(map[int]bool)
		}
		condensed[from][to] = true
	}

	// Tarjan emits SCCs in reverse topological order (an SCC is emitted only
	// after every SCC it points to), so descending emission index is
	// topological order. Relax forward for longest-path layering.
	layerSCC := make([]int, len(sccs))
	for i := len(sccs) - 1; i >= 0; i-- {
		for _, succ := range slices.Sorted(maps.Keys(condensed[i])) {
			if layerSCC[i]+1 > layerSCC[succ] {
				layerSCC[succ] = layerSCC[i] + 1
			}
		}
	}

	// Pin cmd/ to layer 0.
	for i, members := range sccs {
		for _, m := range members {
			if topSegment(m) == cmdDirName {
				layerSCC[i] = 0
				break
			}
		}
	}

	// Wrong-way violations: inter-SCC edges not pointing strictly downward.
	for _, e := range edges {
		from, to := sccOf[e.From], sccOf[e.To]
		if from != to && layerSCC[to] <= layerSCC[from] {
			violations = append(violations, Violation{Kind: ViolationWrongWay, From: e.From, To: e.To})
		}
	}

	layerOf := make(map[string]int, len(pkgs))
	for i, members := range sccs {
		for _, m := range members {
			layerOf[m] = layerSCC[i]
		}
	}

	sortViolations(violations)
	return layerOf, violations
}

// tarjanSCC computes strongly connected components. Vertices are visited and
// neighbors expanded in sorted order, so the emission order — and therefore
// every downstream layer assignment — is deterministic.
func tarjanSCC(vertices []string, adj map[string][]string) [][]string {
	index := make(map[string]int, len(vertices))
	low := make(map[string]int, len(vertices))
	onStack := make(map[string]bool, len(vertices))
	var stack []string
	counter := 0
	var sccs [][]string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = counter
		low[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && index[w] < low[v] {
				low[v] = index[w]
			}
		}

		if low[v] == index[v] {
			var members []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				members = append(members, w)
				if w == v {
					break
				}
			}
			sort.Strings(members)
			sccs = append(sccs, members)
		}
	}

	for _, v := range vertices {
		if _, seen := index[v]; !seen {
			strongconnect(v)
		}
	}
	return sccs
}

// assignOrder gives every node a stable position within its layer, sorted by
// (parent directory, name).
func assignOrder(t *fileTree) {
	byLayer := make(map[int][]*Node)
	for _, n := range t.nodes {
		byLayer[n.Layer] = append(byLayer[n.Layer], n)
	}
	for _, layer := range slices.Sorted(maps.Keys(byLayer)) {
		nodes := byLayer[layer]
		sort.Slice(nodes, func(i, j int) bool {
			if nodes[i].Parent != nodes[j].Parent {
				return nodes[i].Parent < nodes[j].Parent
			}
			return nodes[i].Name < nodes[j].Name
		})
		for i, n := range nodes {
			n.Order = i
		}
	}
}

// topSegment returns the first path segment of a repo-relative path.
func topSegment(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}
