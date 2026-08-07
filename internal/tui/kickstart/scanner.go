package kickstart

import (
	"context"
	"sort"
	"strconv"

	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// ScannerTreeSource is the REAL kit.TreeSource the mounted kickstart tree loads
// from: it folds the flat provider discovery listing the legacy wizard already
// produced (internal/tui/ftue discovery, the exact SessionListing rows the old
// ProjectScopePage consumed) into the project-first PROJECT -> BRANCH -> SESSION
// forest the kit tree renders, matching the original FTUE hierarchy (no harness
// grouping axis). It performs no scanning of its own - the live walk stays in the
// existing discovery adapters wired through cmd_kickstart - so this adapter is a
// pure, deterministic re-shaping of already-discovered data. The dev/test loop
// uses scannerfix.FixtureTreeSource instead; both satisfy the same seam.
//
// The node Meta keys it writes (settings.MetaRemote on a project node,
// settings.MetaBranch on a branch node, settings.MetaHarness on each session
// leaf) are exactly the ones settings.FromTreeNodes reads back when deriving the
// persisted (harness-keyed) SelectionConfig, so a selection made over this
// project-first forest round-trips without a parallel model.
type ScannerTreeSource struct {
	sessions []ftue.SessionListing
	ingested map[string]bool
}

// ScannerOption configures a ScannerTreeSource at construction.
type ScannerOption func(*ScannerTreeSource)

// WithIngestedSessionIDs marks the sessions the local store already holds, so
// each matching session node carries the settings.MetaIngested flag. The caller
// reads the ids from the store (Store.AllSessionIDs); an empty set leaves every
// node unmarked.
func WithIngestedSessionIDs(ids []string) ScannerOption {
	return func(s *ScannerTreeSource) {
		if len(ids) == 0 {
			return
		}
		s.ingested = make(map[string]bool, len(ids))
		for _, id := range ids {
			if id != "" {
				s.ingested[id] = true
			}
		}
	}
}

// NewScannerTreeSource builds a TreeSource over an already-discovered session
// listing. The listing is copied defensively so a later mutation of the caller's
// slice cannot change what a load returns.
func NewScannerTreeSource(sessions []ftue.SessionListing, opts ...ScannerOption) *ScannerTreeSource {
	cp := append([]ftue.SessionListing(nil), sessions...)
	src := &ScannerTreeSource{sessions: cp}
	for _, opt := range opts {
		opt(src)
	}
	return src
}

var _ kit.TreeSource = (*ScannerTreeSource)(nil)

// Load returns the forest. It never fails - discovery errors are already folded
// into the empty/partial listing upstream (the legacy wizard shows zero counts
// rather than failing), so a barren listing yields an empty forest, not an error.
func (s *ScannerTreeSource) Load(_ context.Context) ([]*kit.TreeNode, error) {
	return BuildForest(s.sessions, s.ingested), nil
}

// BuildForest folds a flat session listing into the ordered
// PROJECT -> BRANCH -> SESSION forest, matching the original FTUE
// ProjectScopePage hierarchy: project-first, with NO harness grouping axis (the
// harness is a property of an individual session, carried on the node, not a
// top-level bucket). Grouping keys:
//
//   - project node: keyed by git remote URL when known, else by project name, so
//     every session of one project groups together regardless of which harness
//     recorded it; the remote is carried in Meta so the round-trip recovers
//     ProjectSelection.GitRemote vs Name. Its LABEL is the canonical
//     projectlabel.Label form ("github:owner/repo"), never the filesystem path.
//   - branch node: keyed by branch (or "(unknown branch)" when discovery could
//     not resolve one) with the branch carried in Meta.
//   - session node: keyed by the raw session ID, carrying its harness in Meta so
//     settings.FromTreeNodes can rebuild the harness-keyed SelectionConfig.
//
// Only PARENT sessions group into branches: a session whose id appears in
// another session's SubagentIDs is a child (subagent) and is NOT a row of its
// own. A parent session stays a LEAF that carries settings.MetaChildCount, the
// number of subagent sessions discovered transitively beneath it, so its row
// summarises them as a count instead of opening another level of nesting. The
// count is display-only: selecting a parent still selects its children for
// import, which the ingest side expands from the same SubagentIDs. Every
// session node carries its harness in Meta, plus the settings.MetaIngested flag
// when the local store already holds that session.
//
// Within a branch the sessions are GROUPED by import state: the not-yet-
// imported ones first, then the already-imported ones, so a first run reads as
// a list of work to do rather than a list of work already done.
//
// Ordering is deterministic (lexicographic within each level, sessions by
// import state then date then ID) so the rendered tree and any golden capture
// are stable across runs.
func BuildForest(sessions []ftue.SessionListing, ingested map[string]bool) []*kit.TreeNode {
	// Index every session by id, and record which ids are children so a session
	// is added as a top-level node only when it is nobody's subagent.
	byID := make(map[string]ftue.SessionListing, len(sessions))
	childIDs := map[string]bool{}
	for _, sess := range sessions {
		if sess.SessionID == "" {
			continue
		}
		byID[sess.SessionID] = sess
		for _, childID := range sess.SubagentIDs {
			if childID != "" {
				childIDs[childID] = true
			}
		}
	}

	type branchAgg struct {
		node     *kit.TreeNode
		sessions []ftue.SessionListing
	}
	type projectAgg struct {
		node     *kit.TreeNode
		order    []string
		branches map[string]*branchAgg
	}

	projectOrder := []string{}
	projects := map[string]*projectAgg{}

	for _, sess := range sessions {
		if sess.SessionID == "" {
			continue
		}
		// A child (subagent) session is summarised on its parent's row, so it
		// never becomes a row under a branch.
		if childIDs[sess.SessionID] {
			continue
		}
		pKey := sess.GitRemote
		if pKey == "" {
			pKey = "name:" + sess.ProjectName
		}
		p, ok := projects[pKey]
		if !ok {
			meta := map[string]string{}
			if sess.GitRemote != "" {
				meta[settings.MetaRemote] = sess.GitRemote
			}
			label := projectLabel(sess)
			node := &kit.TreeNode{ID: pKey, Label: label}
			if len(meta) > 0 {
				node.Meta = meta
			}
			p = &projectAgg{node: node, branches: map[string]*branchAgg{}}
			projects[pKey] = p
			projectOrder = append(projectOrder, pKey)
		}

		bKey := sess.Branch
		branchLabel := sess.Branch
		if bKey == "" {
			bKey = "(unknown branch)"
			branchLabel = "(unknown branch)"
		}
		b, ok := p.branches[bKey]
		if !ok {
			node := &kit.TreeNode{
				ID:    bKey,
				Label: branchLabel,
				Meta:  map[string]string{settings.MetaBranch: bKey},
			}
			b = &branchAgg{node: node}
			p.branches[bKey] = b
			p.order = append(p.order, bKey)
		}
		b.sessions = append(b.sessions, sess)
	}

	var roots []*kit.TreeNode
	sort.Strings(projectOrder)
	for _, pKey := range projectOrder {
		p := projects[pKey]
		sort.Strings(p.order)
		for _, bKey := range p.order {
			b := p.branches[bKey]
			sortListings(b.sessions)
			for _, sess := range groupByImportState(b.sessions, ingested) {
				b.node.Children = append(b.node.Children, sessionNode(sess, byID, ingested))
			}
			p.node.Children = append(p.node.Children, b.node)
		}
		roots = append(roots, p.node)
	}
	return roots
}

// projectLabel is the project row text: the canonical projectlabel.Label form
// ("github:owner/repo" when a remote is known), falling back to the discovery
// project name and finally an explicit placeholder so a row is never blank. It
// deliberately does NOT surface the filesystem path.
func projectLabel(sess ftue.SessionListing) string {
	fallback := sess.ProjectName
	if fallback == "" {
		fallback = "(unknown project)"
	}
	return projectlabel.Label(sess.GitRemote, fallback)
}

// sortListings orders a worktree's sessions by date (oldest first), then by ID
// for a stable tie-break, so a rebuilt forest is byte-stable across runs.
func sortListings(ss []ftue.SessionListing) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].Date.Equal(ss[j].Date) {
			return ss[i].Date.Before(ss[j].Date)
		}
		return ss[i].SessionID < ss[j].SessionID
	})
}

// sessionLabel is the session row text: the session title when discovery
// resolved one, else the raw ID so a row is never blank.
func sessionLabel(sess ftue.SessionListing) string {
	if sess.Title != "" {
		return sess.Title
	}
	return sess.SessionID
}

// groupByImportState returns the sessions with the not-yet-imported ones first
// and the already-imported ones after, preserving the incoming order within
// each group so the result stays deterministic.
func groupByImportState(sessions []ftue.SessionListing, ingested map[string]bool) []ftue.SessionListing {
	out := make([]ftue.SessionListing, 0, len(sessions))
	for _, sess := range sessions {
		if !ingested[sess.SessionID] {
			out = append(out, sess)
		}
	}
	for _, sess := range sessions {
		if ingested[sess.SessionID] {
			out = append(out, sess)
		}
	}
	return out
}

// sessionNode builds one session LEAF. It carries the session's harness in
// Meta, the settings.MetaIngested flag when the store already holds it, and
// settings.MetaChildCount when the session spawned subagents, so the row
// summarises its children as a count rather than nesting another level.
func sessionNode(sess ftue.SessionListing, byID map[string]ftue.SessionListing, ingested map[string]bool) *kit.TreeNode {
	meta := map[string]string{settings.MetaHarness: sess.Harness}
	if ingested[sess.SessionID] {
		meta[settings.MetaIngested] = settings.MetaIngestedValue
	}
	if n := countSubagents(sess, byID, map[string]bool{sess.SessionID: true}); n > 0 {
		meta[settings.MetaChildCount] = strconv.Itoa(n)
	}
	return &kit.TreeNode{
		ID:    sess.SessionID,
		Label: sessionLabel(sess),
		Meta:  meta,
	}
}

// countSubagents counts the subagent sessions discovered transitively beneath
// sess: it walks SubagentIDs through byID, so a subagent that itself spawned
// subagents contributes its whole descendant set. A child id with no matching
// listing is not counted (discovery may not have surfaced it), and the seen set
// - which is never unwound - both terminates a cyclic subagent reference and
// keeps a session reachable by two paths from counting twice.
func countSubagents(sess ftue.SessionListing, byID map[string]ftue.SessionListing, seen map[string]bool) int {
	total := 0
	for _, childID := range sess.SubagentIDs {
		if childID == "" || seen[childID] {
			continue
		}
		child, ok := byID[childID]
		if !ok {
			continue
		}
		seen[childID] = true
		total += 1 + countSubagents(child, byID, seen)
	}
	return total
}
