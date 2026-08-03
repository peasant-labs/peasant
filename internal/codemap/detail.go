package codemap

import (
	"sort"

	"github.com/peasant-labs/schema"
)

// nodeDetail builds the rail panel payload for one assembled node.
func (s *Service) nodeDetail(pd *projectData, asm *assembly, node *schema.MapNode) (*schema.MapNodeDetailPayload, error) {
	payload := schema.NewMapNodeDetailPayload(node.ID)
	payload.Kind = node.Kind
	payload.Language = node.Language
	payload.Loc = node.Loc
	payload.RecordedFiles = node.RecordedFiles
	payload.TotalFiles = node.TotalFiles

	// Structural role from the parsed import
	// graph, deterministic input only. DependsOn = node IDs this node imports;
	// UsedBy = node IDs that import it. Most-connected first, capped.
	payload.DependsOn, payload.UsedBy = nodeConnections(asm, node.ID)

	// Tasks (and their sessions) that edited files under this node.
	var touching []taskData
	sessionSet := make(map[string]bool)
	for _, t := range pd.tasks {
		if t.touchesPath(node.ID) {
			touching = append(touching, t)
			sessionSet[t.sessionID] = true
		}
	}
	payload.TaskCount = len(touching)
	payload.SessionCount = len(sessionSet)

	// Last recorded touch under the node.
	var lastTouch *int64
	for _, f := range sortedKeys(asm.stats) {
		if !underNode(node.ID, f) {
			continue
		}
		if ms := asm.stats[f].lastEditMs; ms != nil && (lastTouch == nil || *ms > *lastTouch) {
			v := *ms
			lastTouch = &v
		}
	}
	payload.LastTouchMs = lastTouch

	// Shaped by: most recent touching tasks first, cap 20.
	for _, t := range sortTasksReverseChron(touching) {
		if len(payload.ShapedBy) >= maxShapedBy {
			break
		}
		payload.ShapedBy = append(payload.ShapedBy, pd.taskSummary(t))
	}

	// Recent commits "touching this node": the gitops interface has no
	// per-file commit log, so these are the commits linked to the sessions
	// that touched the node — recorded commits by construction (HasSession
	// always true). Unrecorded commits touching the node need a per-file
	// gitops addition; accepted degraded mode for this build.
	recentCommits, err := commitsForSessions(pd, sessionSet, maxNodeCommits)
	if err != nil {
		return nil, err
	}
	payload.RecentCommits = recentCommits

	// Footnote metrics over the touching sessions.
	costSum := 0.0
	costKnown := false
	for _, id := range sortedKeys(sessionSet) {
		m, ok := pd.metrics[id]
		if !ok {
			continue
		}
		if m.retryLoops != nil {
			payload.RetryLoops += *m.retryLoops
		}
		if m.costTotalUSD != nil {
			costSum += *m.costTotalUSD
			costKnown = true
		}
	}
	if costKnown {
		payload.CostUsd = &costSum
	}

	// Re-edited files within the node (a session edited the file >= 2 times).
	for _, f := range sortedKeys(asm.stats) {
		if underNode(node.ID, f) && asm.stats[f].reEditFile {
			payload.ReEdits++
		}
	}

	return payload, nil
}

// nodeConnections returns the node's structural role from the parsed import
// graph: the node IDs it depends on (outgoing edges) and the IDs
// that use it (incoming edges), each most-connected first (edge count desc,
// then ID asc) and capped at maxNodeConnections. Both results are non-nil.
//
// codegraph aggregates file-level imports to PACKAGE (directory) grain, so
// connections resolve for package nodes — the natural "area" grain. Module and
// file nodes (whose IDs never match a package-grain edge endpoint) get empty
// lists; the rail simply omits the block. Self-edges are ignored.
func nodeConnections(asm *assembly, id string) (dependsOn, usedBy []string) {
	deps := map[string]int{}
	uses := map[string]int{}
	for _, e := range asm.edges {
		switch {
		case e.From == id && e.To != id:
			deps[e.To] += e.Count
		case e.To == id && e.From != id:
			uses[e.From] += e.Count
		}
	}
	top := func(m map[string]int) []string {
		ids := make([]string, 0, len(m))
		for k := range m {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool {
			if m[ids[i]] != m[ids[j]] {
				return m[ids[i]] > m[ids[j]]
			}
			return ids[i] < ids[j]
		})
		if len(ids) > maxNodeConnections {
			ids = ids[:maxNodeConnections]
		}
		out := []string{}
		return append(out, ids...)
	}
	return top(deps), top(uses)
}

// commitsForSessions collects the distinct commits linked to the given
// sessions, newest first (commits without a time sort last), capped.
func commitsForSessions(pd *projectData, sessionSet map[string]bool, limit int) ([]schema.CommitRef, error) {
	byHash := make(map[string]commitRow)
	sessionsByHash := make(map[string][]commitRow)
	for _, id := range sortedKeys(sessionSet) {
		for _, c := range pd.commitsByID[id] {
			if _, ok := byHash[c.hash]; !ok {
				byHash[c.hash] = c
			}
			sessionsByHash[c.hash] = append(sessionsByHash[c.hash], c)
		}
	}

	rows := make([]commitRow, 0, len(byHash))
	for _, h := range sortedKeys(byHash) {
		rows = append(rows, byHash[h])
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.timeMs != nil && b.timeMs == nil:
			return true
		case a.timeMs == nil && b.timeMs != nil:
			return false
		case a.timeMs != nil && b.timeMs != nil && *a.timeMs != *b.timeMs:
			return *a.timeMs > *b.timeMs
		}
		return a.hash < b.hash
	})

	refs := []schema.CommitRef{}
	for _, r := range rows {
		if len(refs) >= limit {
			break
		}
		ref := schema.NewCommitRef(r.hash, r.subject)
		ref.TimeMs = r.timeMs
		for _, binding := range sessionsByHash[r.hash] {
			ref.SessionIDs = append(ref.SessionIDs, schema.SessionID(binding.sessionID))
		}
		ref.HasSession = len(ref.SessionIDs) > 0
		ref.Associations = recordedCommitAssociations(sessionsByHash[r.hash])
		refs = append(refs, ref)
	}
	return refs, nil
}
