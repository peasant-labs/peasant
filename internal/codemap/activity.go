package codemap

import (
	"path"
	"sort"
	"strings"

	"github.com/peasant-labs/schema"
)

// activityEdgeFloor is the minimum number of distinct shared tasks before a
// co-edit pair becomes an activity edge. The floor of two shared tasks avoids
// one-off connections that would turn the graph into a hairball.
const activityEdgeFloor = 2

// fileStats aggregates the recorded edit signals for one repo-relative file.
type fileStats struct {
	editCount     int    // total edit events (TouchCount input)
	lastEditMs    *int64 // most recent edit timestamp
	errorAdjacent int    // edit events followed by an is_error entry (effort)
	reEdits       int    // re-edit occasions: Σ over sessions of max(0, editsInSession-1)
	reEditFile    bool   // some session edited this file >= 2 times
}

// computeFileStats rolls every task's edit events up to per-file statistics.
//
// Per-file effort signals:
//   - re-edit occasions: within one session, every edit of a file beyond the
//     first counts as a re-edit (the file had to be touched again);
//   - error-adjacent edits: edits followed by an is_error entry within
//     errorAdjacencyWindow entries (the edit is near recorded struggle).
func computeFileStats(tasks []taskData) map[string]*fileStats {
	stats := make(map[string]*fileStats)
	perSession := make(map[string]map[string]int) // sessionID -> file -> edits

	for _, t := range tasks {
		for _, ev := range t.editEvents {
			fs := stats[ev.file]
			if fs == nil {
				fs = &fileStats{}
				stats[ev.file] = fs
			}
			fs.editCount++
			if ev.ms != nil && (fs.lastEditMs == nil || *ev.ms > *fs.lastEditMs) {
				ms := *ev.ms
				fs.lastEditMs = &ms
			}
			if ev.errorAdjacent {
				fs.errorAdjacent++
			}
			if perSession[t.sessionID] == nil {
				perSession[t.sessionID] = make(map[string]int)
			}
			perSession[t.sessionID][ev.file]++
		}
	}

	for _, files := range perSession {
		for file, count := range files {
			if count >= 2 {
				stats[file].reEdits += count - 1
				stats[file].reEditFile = true
			}
		}
	}
	return stats
}

// effortScore is the per-file effort signal rolled up into EffortDensity:
// re-edit occasions plus error-adjacent edits.
func (fs *fileStats) effortScore() int {
	return fs.reEdits + fs.errorAdjacent
}

// computeActivityEdges derives co-edit edges: two package-level nodes edited
// by the same task. Files are aggregated to their containing directory
// (package-level node IDs, matching codegraph's edge grain); root-level
// files have no package node and are skipped. A pair becomes an edge only
// when >= activityEdgeFloor distinct tasks edited both. Output is sorted by
// (From, To).
func computeActivityEdges(tasks []taskData) []schema.ActivityEdge {
	type pair struct{ from, to string }
	counts := make(map[pair]int)

	for _, t := range tasks {
		pkgSet := make(map[string]bool)
		for _, f := range t.editedFiles {
			if d := parentDir(f); d != "" {
				pkgSet[d] = true
			}
		}
		pkgs := make([]string, 0, len(pkgSet))
		for p := range pkgSet {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for i := 0; i < len(pkgs); i++ {
			for j := i + 1; j < len(pkgs); j++ {
				counts[pair{pkgs[i], pkgs[j]}]++
			}
		}
	}

	edges := []schema.ActivityEdge{}
	for p, count := range counts {
		if count >= activityEdgeFloor {
			edges = append(edges, schema.ActivityEdge{From: p.from, To: p.to, TaskCount: count})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return edges
}

// parentDir returns the parent directory of a repo-relative path, or "" for
// top-level entries (mirrors codegraph's path grain).
func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// underNode reports whether file sits at or below the node ID (node itself,
// or any path under node + "/").
func underNode(nodeID, file string) bool {
	return file == nodeID || strings.HasPrefix(file, nodeID+"/")
}

// sortedDistinct sorts a string slice and removes duplicates (input is not
// mutated beyond ordering of the returned copy).
func sortedDistinct(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return dedupeSorted(out)
}
