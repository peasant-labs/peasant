package kickstart

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// ProjectIdentity is the stable exact-selection identity of one discovered
// harness/worktree pair. Display labels, remotes, and project names are
// deliberately excluded: two harnesses or two physical worktrees remain
// distinct persisted scopes even when one repository row presents them together.
type ProjectIdentity struct {
	Harness   ingest.Harness
	ClonePath ingest.ClonePath
}

// String returns an unambiguous tree key. The harness is length-prefixed so a
// path cannot collide with a different harness/path pair. This value is identity
// data and is never rendered as row text.
func (i ProjectIdentity) String() string {
	if !i.available() {
		return ""
	}
	harness := i.Harness.String()
	return strconv.Itoa(len(harness)) + ":" + harness + i.ClonePath.String()
}

func (i ProjectIdentity) available() bool {
	return i.Harness != "" && i.ClonePath != ""
}

// RepositoryIdentity is the immutable Git topology result used by kickstart.
// Its String method returns only the opaque cohort key; physical Git directory
// diagnostics remain available separately through GitDirectory.
type RepositoryIdentity = ingest.RepositoryIdentity

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
// settings.MetaBranch on a branch node, settings.MetaHarness and exact path
// metadata on each session leaf) are exactly the ones settings.FromTreeNodes
// reads back when deriving the persisted (harness-keyed) SelectionConfig, so a
// cross-harness selection round-trips without a parallel model.
type ScannerTreeSource struct {
	sessions           []ftue.SessionListing
	ingested           map[string]bool
	subagents          SubagentRelation
	resolver           ingest.PathIdentityResolver
	repositoryResolver ingest.RepositoryIdentityResolver
	previewMu          sync.RWMutex
	previewContexts    map[string]ListingPreviewContext
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

// SubagentRelation is the parent-to-subagent relation over the FULL discovered
// set, including the subagent sessions the picker never lists. It exists for a
// single purpose: to resolve a parent row's child COUNT on the production path,
// where the listing cohort holds root sessions only and therefore holds no
// child to count. Nothing named in the relation is rendered, becomes a
// selectable row, or reaches ingest; the cohort alone decides that.
//
// A key is present for EVERY session discovery surfaced, with an empty value
// when that session spawned nothing. Presence is therefore the existence proof
// countSubagents needs: a lookup miss still means "discovery never surfaced
// this session", so a bare identifier can never invent a child.
type SubagentRelation map[string][]string

// WithSubagentRelation supplies the discovered subagent relation used only to
// resolve child counts. Callers that list an already-complete cohort (every
// child present among the listings) can omit it: the cohort's own listings
// remain the fallback index, so the fold behaves identically without it.
func WithSubagentRelation(relation SubagentRelation) ScannerOption {
	return func(s *ScannerTreeSource) {
		if len(relation) == 0 {
			return
		}
		copied := make(SubagentRelation, len(relation))
		for parentID, childIDs := range relation {
			if parentID == "" {
				continue
			}
			copied[parentID] = append([]string(nil), childIDs...)
		}
		s.subagents = copied
	}
}

// WithPathIdentityResolver injects the boundary that turns scanner working
// directories into physical clone identities. Production passes
// ingest.NewPhysicalPathResolver; deterministic fixture tests can inject a
// resolver over their pre-resolved paths.
func WithPathIdentityResolver(resolver ingest.PathIdentityResolver) ScannerOption {
	return func(s *ScannerTreeSource) {
		if resolver != nil {
			s.resolver = resolver
		}
	}
}

// WithRepositoryIdentityResolver injects the Git topology boundary used only
// for scanner grouping, preview diagnostics, and remote/name multiplicity.
// Exact worktree paths still come from WithPathIdentityResolver.
func WithRepositoryIdentityResolver(resolver ingest.RepositoryIdentityResolver) ScannerOption {
	return func(s *ScannerTreeSource) {
		if resolver != nil {
			s.repositoryResolver = resolver
		}
	}
}

// NewScannerTreeSource builds a TreeSource over an already-discovered session
// listing. The listing is copied defensively so a later mutation of the caller's
// slice cannot change what a load returns.
func NewScannerTreeSource(sessions []ftue.SessionListing, opts ...ScannerOption) *ScannerTreeSource {
	cp := append([]ftue.SessionListing(nil), sessions...)
	src := &ScannerTreeSource{
		sessions:           cp,
		resolver:           ingest.NewPhysicalPathResolver(),
		repositoryResolver: ingest.NewGitRepositoryIdentityResolver(),
	}
	for _, opt := range opts {
		opt(src)
	}
	return src
}

var _ kit.TreeSource = (*ScannerTreeSource)(nil)
var _ ListingPreviewContextSource = (*ScannerTreeSource)(nil)

// Load resolves the complete discovery cohort before it builds any tree nodes.
// A listing whose working directory is empty, missing, or otherwise unresolved
// is unavailable for this load and therefore does not become a project row. It
// still participates in ambiguity annotation, so another clone can never use a
// remote or name fallback when physical uniqueness was not proved. Per-listing
// resolution failures keep the existing partial-scan behavior: the available
// forest still loads, and saved unavailable choices remain in the editor's
// unmatched baseline.
func (s *ScannerTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	roots := buildForest(prepareSessionListings(ctx, s.sessions, s.resolver, s.repositoryResolver), s.ingested, s.subagents)
	contexts := scannerPreviewContexts(roots)
	s.previewMu.Lock()
	s.previewContexts = contexts
	s.previewMu.Unlock()
	return roots, nil
}

// ListingPreviewContext returns metadata for a project or branch row from the
// exact forest produced by the latest successful Load. Preview rendering never
// reruns Git or tries to reconstruct repository identity from display labels.
func (s *ScannerTreeSource) ListingPreviewContext(id string) (ListingPreviewContext, bool) {
	if s == nil {
		return ListingPreviewContext{}, false
	}
	s.previewMu.RLock()
	context, ok := s.previewContexts[id]
	s.previewMu.RUnlock()
	if !ok {
		return ListingPreviewContext{}, false
	}
	return cloneListingPreviewContext(context), true
}

// PreparedSessionListing carries one discovered session together with the
// physical project identity and complete-cohort multiplicity evidence used by
// every kickstart matcher call. The scanner tree and post-save ingest callback
// consume this same value so they cannot disagree about clone identity.
type PreparedSessionListing struct {
	Listing            ftue.SessionListing
	ProjectIdentity    ProjectIdentity
	RepositoryIdentity RepositoryIdentity
	Candidate          ingest.DiscoveryCandidate
}

type scannerBranchAgg struct {
	node     *kit.TreeNode
	sessions []PreparedSessionListing
}

type scannerProjectAgg struct {
	identity RepositoryIdentity
	rows     []PreparedSessionListing
	order    []string
	branches map[string]*scannerBranchAgg
}

// PrepareSessionListings resolves every non-empty WorkingDir first, then annotates
// every listing from the complete cohort. Session count never affects
// multiplicity: each normalized remote/name counts distinct RepositoryIdentity
// values only. ProjectIdentity remains the exact worktree carrier on each row.
// A listing with unresolved path evidence remains in the returned cohort so an
// explicit session ID can still match, but it cannot become a scanner project
// row or enable remote/name fallback.
func PrepareSessionListings(sessions []ftue.SessionListing, resolver ingest.PathIdentityResolver) []PreparedSessionListing {
	return prepareSessionListings(context.Background(), sessions, resolver, ingest.NewGitRepositoryIdentityResolver())
}

func prepareSessionListings(
	ctx context.Context,
	sessions []ftue.SessionListing,
	resolver ingest.PathIdentityResolver,
	repositoryResolver ingest.RepositoryIdentityResolver,
) []PreparedSessionListing {
	cohort := make([]PreparedSessionListing, len(sessions))
	remoteEvidence := make([]selectionprojection.CohortEvidence, len(sessions))
	nameEvidence := make([]selectionprojection.CohortEvidence, len(sessions))
	repositoryIdentities := map[ingest.ClonePath]ingest.RepositoryIdentity{}

	for index, listing := range sessions {
		harness := ingest.Harness(listing.Harness)
		row := PreparedSessionListing{
			Listing: listing,
			Candidate: ingest.DiscoveryCandidate{
				Harness:     harness,
				GitRemote:   listing.GitRemote,
				ProjectName: listing.ProjectName,
				Branch:      listing.Branch,
				SessionID:   ingest.SessionID(listing.SessionID),
			},
		}
		if listing.WorkingDir != "" && resolver != nil {
			if clonePath, err := resolver.Resolve(listing.WorkingDir); err == nil && clonePath != "" {
				row.ProjectIdentity = ProjectIdentity{Harness: harness, ClonePath: clonePath}
				row.Candidate.ClonePath = clonePath
				repositoryIdentity, resolved := repositoryIdentities[clonePath]
				if !resolved {
					repositoryIdentity = fallbackRepositoryIdentity(clonePath)
					if repositoryResolver != nil {
						if resolvedIdentity, identityErr := repositoryResolver.ResolveRepositoryIdentity(ctx, clonePath); identityErr == nil && repositoryIdentityAvailable(resolvedIdentity) {
							repositoryIdentity = resolvedIdentity
						} else if resolvedIdentity.GitDirectory != "" {
							repositoryIdentity = fallbackRepositoryIdentityFromGitDirectory(resolvedIdentity.GitDirectory)
						}
					}
					repositoryIdentities[clonePath] = repositoryIdentity
				}
				row.RepositoryIdentity = repositoryIdentity
			}
		}
		cohort[index] = row
		remoteEvidence[index] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeRemoteForMatch(listing.GitRemote), CohortKey: row.RepositoryIdentity.CohortKey}
		nameEvidence[index] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeProjectNameForMatch(listing.ProjectName), CohortKey: row.RepositoryIdentity.CohortKey}
	}

	remoteMultiplicities := selectionprojection.CohortMultiplicities(remoteEvidence)
	nameMultiplicities := selectionprojection.CohortMultiplicities(nameEvidence)
	for index := range cohort {
		row := &cohort[index]
		row.Candidate.RemoteMultiplicity = remoteMultiplicities[index]
		row.Candidate.NameMultiplicity = nameMultiplicities[index]
	}
	return cohort
}

// buildForest folds a fully resolved and annotated scanner cohort into the ordered
// PROJECT -> BRANCH -> SESSION forest, matching the original FTUE
// ProjectScopePage hierarchy: project-first, with NO harness grouping axis (the
// harness is a property of an individual session, carried on the node, not a
// top-level bucket). Grouping keys:
//
//   - project node: keyed only by RepositoryIdentity (transient Git
//     common-directory path, with exact ClonePath as the fail-safe non-Git
//     fallback). Sessions from every harness therefore share one repository
//     root. Remote/name/multiplicity metadata is carried separately for the
//     canonical matcher and config round-trip. A remote label never becomes an
//     identity key.
//   - branch node: keyed by branch (or "(unknown branch)" when discovery could
//     not resolve one) with the branch carried in Meta.
//   - session node: keyed by the raw session ID, carrying its harness in Meta so
//     settings.FromTreeNodes can rebuild the harness-keyed SelectionConfig.
//
// Only PARENT sessions group into branches: a session whose id appears in
// another session's SubagentIDs is a child (subagent) and is NOT a row of its
// own. That exclusion is the CHILD-IDENTIFIER mechanism and fires regardless
// of origin: a child with a User or Unknown origin is still hidden here,
// because it is nobody's own top-level row to select. A parent session stays
// a LEAF that carries settings.MetaChildCount, the number of subagent
// sessions DISCOVERED transitively beneath it (never the metrics Task-tool
// heuristic count, which disagrees with discovery today and is out of scope
// for this fold), so its row summarises them as a count instead of opening
// another level of nesting. The count resolves against the discovered subagent
// relation when the caller supplies one, because the production cohort holds
// root sessions ONLY and therefore holds no child to count; the cohort's own
// listings are the fallback index. The count is display-only: selecting a
// parent still selects its children for import, which the ingest side expands
// from the same SubagentIDs. Every session node carries its harness in Meta, plus
// the settings.MetaIngested flag when the local store already holds that
// session.
//
// Separately, a top-level row whose OWN origin is Agent is dropped by a
// second, independent mechanism: positive evidence that a program, not a
// person, drove it (internal/sessionorigin). User and Unknown origins are
// always visible; Unknown is the fail-safe and behaves exactly like User.
// This is a DISCOVERY-scope filter over what the picker lists, never an
// access-control gate — a session hidden here remains reachable by any path
// that does not go through this listing.
//
// Within a branch the sessions are GROUPED by import state: the not-yet-
// imported ones first, then the already-imported ones, so a first run reads as
// a list of work to do rather than a list of work already done.
//
// Ordering is deterministic (lexicographic within each level, sessions by
// import state then date then ID) so the rendered tree and any golden capture
// are stable across runs.
func buildForest(cohort []PreparedSessionListing, ingested map[string]bool, relation SubagentRelation) []*kit.TreeNode {
	// Record which ids are children so a session is added as a top-level node
	// only when it is nobody's subagent. This reads the COHORT alone: what the
	// picker lists is decided by what the picker was given, never by the
	// count-only relation.
	childIDs := map[string]bool{}
	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		for _, childID := range sess.SubagentIDs {
			if childID != "" {
				childIDs[childID] = true
			}
		}
	}
	counts := subagentCountIndex(cohort, relation)

	projectOrder := []string{}
	projects := map[string]*scannerProjectAgg{}

	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		// A project row cannot exist without a resolved physical identity. The
		// saved counterpart remains in UnmatchedBaseline and can return when the
		// directory becomes available again.
		if !row.ProjectIdentity.available() || !repositoryIdentityAvailable(row.RepositoryIdentity) {
			continue
		}
		// A child (subagent) session is summarised on its parent's row, so it
		// never becomes a row under a branch. This fires regardless of origin.
		if childIDs[sess.SessionID] {
			continue
		}
		// An agent-driven top-level session is hidden by positive evidence,
		// independently of the child-identifier check above. User and Unknown
		// stay visible; Unknown is the fail-safe and behaves like User.
		if sess.Origin == sessionorigin.Agent {
			continue
		}
		pKey := row.RepositoryIdentity.CohortKey.String()
		p, ok := projects[pKey]
		if !ok {
			p = &scannerProjectAgg{identity: row.RepositoryIdentity, branches: map[string]*scannerBranchAgg{}}
			projects[pKey] = p
			projectOrder = append(projectOrder, pKey)
		}
		p.rows = append(p.rows, row)

		bKey := sess.Branch
		branchLabel := sess.Branch
		if bKey == "" {
			bKey = "(unknown branch)"
			branchLabel = "(unknown branch)"
		}
		b, ok := p.branches[bKey]
		if !ok {
			node := &kit.TreeNode{
				ID:    scannerBranchID(pKey, bKey),
				Label: branchLabel,
				Meta:  map[string]string{settings.MetaBranch: bKey},
			}
			b = &scannerBranchAgg{node: node}
			p.branches[bKey] = b
			p.order = append(p.order, bKey)
		}
		b.sessions = append(b.sessions, row)
	}

	var roots []*kit.TreeNode
	sort.Strings(projectOrder)
	projectLabels := scannerProjectLabels(projectOrder, projects)
	for _, pKey := range projectOrder {
		p := projects[pKey]
		representative := projectRepresentative(p.rows)
		pNode := &kit.TreeNode{
			ID:    p.identity.CohortKey.String(),
			Label: projectLabels[pKey],
			Meta:  scannerProjectMeta(representative, p.identity, p.rows),
		}
		sort.Strings(p.order)
		for _, bKey := range p.order {
			b := p.branches[bKey]
			sortListings(b.sessions)
			annotateScannerScopeMeta(b.node, p.identity, b.sessions)
			for _, row := range groupByImportState(b.sessions, ingested) {
				b.node.Children = append(b.node.Children, sessionNode(row, counts, ingested))
			}
			pNode.Children = append(pNode.Children, b.node)
		}
		roots = append(roots, pNode)
	}
	return roots
}

// scannerBranchID keeps identical branch names under independent repositories
// distinct for preview loading while MetaBranch remains the persisted branch
// identity.
func scannerBranchID(repositoryID, branch string) string {
	return "branch:" + strconv.Itoa(len(repositoryID)) + ":" + repositoryID + branch
}

func projectRepresentative(rows []PreparedSessionListing) PreparedSessionListing {
	if len(rows) == 0 {
		return PreparedSessionListing{}
	}
	ordered := append([]PreparedSessionListing(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i].Listing, ordered[j].Listing
		if (left.GitRemote == "") != (right.GitRemote == "") {
			return left.GitRemote != ""
		}
		if left.GitRemote != right.GitRemote {
			return left.GitRemote < right.GitRemote
		}
		if (left.ProjectName == "") != (right.ProjectName == "") {
			return left.ProjectName != ""
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.SessionID < right.SessionID
	})
	return ordered[0]
}

func scannerProjectMeta(row PreparedSessionListing, identity ingest.RepositoryIdentity, rows []PreparedSessionListing) map[string]string {
	meta := map[string]string{
		settings.MetaProjectIdentity:    identity.CohortKey.String(),
		settings.MetaRemoteMultiplicity: multiplicityText(row.Candidate.RemoteMultiplicity),
		settings.MetaNameMultiplicity:   multiplicityText(row.Candidate.NameMultiplicity),
	}
	if clonePath, unique := uniqueScannerClonePath(rows); unique {
		meta[settings.MetaClonePath] = clonePath.String()
	}
	if row.Listing.GitRemote != "" {
		meta[settings.MetaRemote] = row.Listing.GitRemote
	}
	if projectName := ingest.NormalizeProjectNameForMatch(row.Listing.ProjectName); projectName != "" {
		meta[settings.MetaProjectName] = projectName
	}
	return meta
}

func annotateScannerScopeMeta(node *kit.TreeNode, identity ingest.RepositoryIdentity, rows []PreparedSessionListing) {
	if node.Meta == nil {
		node.Meta = map[string]string{}
	}
	node.Meta[settings.MetaProjectIdentity] = identity.CohortKey.String()
	if clonePath, unique := uniqueScannerClonePath(rows); unique {
		node.Meta[settings.MetaClonePath] = clonePath.String()
	} else {
		delete(node.Meta, settings.MetaClonePath)
	}
}

func scannerPreviewContexts(roots []*kit.TreeNode) map[string]ListingPreviewContext {
	contexts := make(map[string]ListingPreviewContext)
	for _, root := range roots {
		contexts[root.ID] = scannerPreviewContext(root, nil)
		for _, branch := range root.Children {
			contexts[branch.ID] = scannerPreviewContext(root, branch)
		}
	}
	return contexts
}

func scannerPreviewContext(root, selectedBranch *kit.TreeNode) ListingPreviewContext {
	kind := ListingPreviewProject
	branches := root.Children
	branchName := ""
	if selectedBranch != nil {
		kind = ListingPreviewBranch
		branches = []*kit.TreeNode{selectedBranch}
		branchName = selectedBranch.Meta[settings.MetaBranch]
	}

	harnessSet := map[string]struct{}{}
	remoteSet := map[string]struct{}{}
	clonePathSet := map[string]struct{}{}
	branchSet := map[string]struct{}{}
	sessionCount := 0
	for _, branch := range branches {
		if name := branch.Meta[settings.MetaBranch]; name != "" {
			branchSet[name] = struct{}{}
		}
		for _, session := range branch.Children {
			sessionCount++
			if harness := session.Meta[settings.MetaHarness]; harness != "" {
				harnessSet[harness] = struct{}{}
			}
			if remote := scannerPreviewRemote(session.Meta[settings.MetaRemote]); remote != "" {
				remoteSet[remote] = struct{}{}
			}
			if clonePath := session.Meta[settings.MetaClonePath]; clonePath != "" {
				clonePathSet[clonePath] = struct{}{}
			}
		}
	}
	if len(remoteSet) == 0 {
		if remote := scannerPreviewRemote(root.Meta[settings.MetaRemote]); remote != "" {
			remoteSet[remote] = struct{}{}
		}
	}

	return ListingPreviewContext{
		Kind:           kind,
		Project:        root.Label,
		Harnesses:      sortedScannerPreviewValues(harnessSet),
		Remotes:        sortedScannerPreviewValues(remoteSet),
		GitDirectories: scannerPreviewGitDirectories(branches),
		ClonePaths:     sortedScannerPreviewValues(clonePathSet),
		Branches:       sortedScannerPreviewValues(branchSet),
		Branch:         branchName,
		SessionCount:   sessionCount,
	}
}

func scannerPreviewGitDirectories(branches []*kit.TreeNode) []string {
	set := make(map[string]struct{})
	for _, branch := range branches {
		for _, session := range branch.Children {
			if gitDirectory := session.Meta[settings.MetaGitDirectory]; gitDirectory != "" {
				set[gitDirectory] = struct{}{}
			}
		}
	}
	return sortedScannerPreviewValues(set)
}

func scannerPreviewRemote(remote string) string {
	if normalized := ingest.NormalizeRemoteForMatch(remote); normalized != "" {
		return normalized
	}
	return remote
}

func sortedScannerPreviewValues(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func uniqueScannerClonePath(rows []PreparedSessionListing) (ingest.ClonePath, bool) {
	var clonePath ingest.ClonePath
	for _, row := range rows {
		candidate := row.Candidate.ClonePath
		if candidate == "" {
			return "", false
		}
		if clonePath == "" {
			clonePath = candidate
			continue
		}
		if candidate != clonePath {
			return "", false
		}
	}
	return clonePath, clonePath != ""
}

func multiplicityText(value ingest.DiscoveryIdentityMultiplicity) string {
	switch value {
	case ingest.DiscoveryIdentityUnique:
		return settings.MetaMultiplicityUnique
	case ingest.DiscoveryIdentityAmbiguous:
		return settings.MetaMultiplicityAmbiguous
	default:
		return settings.MetaMultiplicityUnproven
	}
}

// scannerProjectLabels keeps identity and row text separate. Git projects use
// their canonical remote label. Non-Git projects use the shortest path suffix
// that distinguishes equal names in this load, so common duplicate names remain
// clear without rendering an absolute physical path by default.
func scannerProjectLabels(order []string, projects map[string]*scannerProjectAgg) map[string]string {
	labels := make(map[string]string, len(order))
	nonGitByName := map[string][]string{}
	for _, key := range order {
		representative := projectRepresentative(projects[key].rows)
		if representative.Listing.GitRemote != "" {
			labels[key] = projectlabel.Label(representative.Listing.GitRemote, projectFallbackName(representative.Listing))
			continue
		}
		name := projectFallbackName(representative.Listing)
		nonGitByName[name] = append(nonGitByName[name], key)
	}
	for name, keys := range nonGitByName {
		paths := make([]ingest.ClonePath, len(keys))
		for index, key := range keys {
			paths[index] = representativeClonePath(projects[key].rows, projects[key].identity.GitDirectory)
		}
		for index, key := range keys {
			shortPath := selectionprojection.ShortestDistinctCloneSuffix(paths[index], paths)
			if shortPath == "" {
				labels[key] = name
				continue
			}
			labels[key] = fmt.Sprintf("%s (%s)", name, shortPath)
		}
	}
	return labels
}

func representativeClonePath(rows []PreparedSessionListing, fallback ingest.RepositoryPath) ingest.ClonePath {
	for _, row := range rows {
		if row.Candidate.ClonePath != "" {
			return row.Candidate.ClonePath
		}
	}
	return ingest.ClonePath(fallback.String())
}

func projectFallbackName(sess ftue.SessionListing) string {
	fallback := ingest.NormalizeProjectNameForMatch(sess.ProjectName)
	if fallback == "" {
		fallback = "(unknown project)"
	}
	return fallback
}

// sortListings orders a branch's sessions by date (oldest first), then by ID
// for a stable tie-break, so a rebuilt forest is byte-stable across runs. One
// branch can contain exact session paths from several linked worktrees.
func sortListings(ss []PreparedSessionListing) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].Listing.Date.Equal(ss[j].Listing.Date) {
			return ss[i].Listing.Date.Before(ss[j].Listing.Date)
		}
		return ss[i].Listing.SessionID < ss[j].Listing.SessionID
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
func groupByImportState(sessions []PreparedSessionListing, ingested map[string]bool) []PreparedSessionListing {
	out := make([]PreparedSessionListing, 0, len(sessions))
	for _, row := range sessions {
		if !ingested[row.Listing.SessionID] {
			out = append(out, row)
		}
	}
	for _, row := range sessions {
		if ingested[row.Listing.SessionID] {
			out = append(out, row)
		}
	}
	return out
}

// sessionNode builds one session LEAF. It carries the session's harness in
// Meta, the settings.MetaIngested flag when the store already holds it, and
// settings.MetaChildCount when the session spawned subagents, so the row
// summarises its children as a count rather than nesting another level.
func sessionNode(row PreparedSessionListing, counts SubagentRelation, ingested map[string]bool) *kit.TreeNode {
	sess := row.Listing
	meta := map[string]string{
		settings.MetaHarness:            sess.Harness,
		settings.MetaProjectIdentity:    row.RepositoryIdentity.CohortKey.String(),
		settings.MetaClonePath:          row.Candidate.ClonePath.String(),
		settings.MetaGitDirectory:       row.RepositoryIdentity.GitDirectory.String(),
		settings.MetaRemoteMultiplicity: multiplicityText(row.Candidate.RemoteMultiplicity),
		settings.MetaNameMultiplicity:   multiplicityText(row.Candidate.NameMultiplicity),
	}
	if sess.GitRemote != "" {
		meta[settings.MetaRemote] = sess.GitRemote
	}
	if sess.ProjectName != "" {
		meta[settings.MetaProjectName] = sess.ProjectName
	}
	if ingested[sess.SessionID] {
		meta[settings.MetaIngested] = settings.MetaIngestedValue
	}
	if n := countSubagents(sess.SubagentIDs, counts, map[string]bool{sess.SessionID: true}); n > 0 {
		meta[settings.MetaChildCount] = strconv.Itoa(n)
	}
	return &kit.TreeNode{
		ID:    sess.SessionID,
		Label: sessionLabel(sess),
		Meta:  meta,
	}
}

func fallbackRepositoryIdentity(clonePath ingest.ClonePath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(clonePath.String()),
		GitDirectory: ingest.RepositoryPath(clonePath.String()),
	}
}

func fallbackRepositoryIdentityFromGitDirectory(gitDirectory ingest.RepositoryPath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(gitDirectory.String()),
		GitDirectory: gitDirectory,
	}
}

func repositoryIdentityAvailable(identity ingest.RepositoryIdentity) bool {
	return identity.CohortKey != "" && identity.GitDirectory != ""
}

// subagentCountIndex is the index countSubagents walks: session id to the
// subagent ids that session spawned, over every session DISCOVERY surfaced.
// The discovered relation comes first because it is the fuller truth - on the
// production path the cohort is roots-only and holds no child at all - and the
// cohort's own listings then fill in any id the relation does not carry, which
// keeps a caller that supplies no relation behaving exactly as before.
func subagentCountIndex(cohort []PreparedSessionListing, relation SubagentRelation) SubagentRelation {
	counts := make(SubagentRelation, len(relation)+len(cohort))
	for parentID, childIDs := range relation {
		if parentID != "" {
			counts[parentID] = childIDs
		}
	}
	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		if _, indexed := counts[sess.SessionID]; !indexed {
			counts[sess.SessionID] = sess.SubagentIDs
		}
	}
	return counts
}

// countSubagents counts the subagent sessions discovered transitively beneath a
// session: it walks childIDs through counts, so a subagent that itself spawned
// subagents contributes its whole descendant set. A child id absent from counts
// is not counted - discovery never surfaced that session, and counting a bare
// identifier would invent a child - and the seen set, which is never unwound,
// both terminates a cyclic subagent reference and keeps a session reachable by
// two paths from counting twice.
func countSubagents(childIDs []string, counts SubagentRelation, seen map[string]bool) int {
	total := 0
	for _, childID := range childIDs {
		if childID == "" || seen[childID] {
			continue
		}
		grandchildren, discovered := counts[childID]
		if !discovered {
			continue
		}
		seen[childID] = true
		total += 1 + countSubagents(grandchildren, counts, seen)
	}
	return total
}
