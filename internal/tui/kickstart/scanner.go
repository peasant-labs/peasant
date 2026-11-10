package kickstart

import (
	"context"
	"sync"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// ProjectIdentity is the stable exact-selection identity of one discovered
// harness/worktree pair. Display labels are not part of the identity.
type ProjectIdentity = discovery.ProjectIdentity

// RepositoryIdentity is the immutable Git topology result used by kickstart.
type RepositoryIdentity = ingest.RepositoryIdentity

// PreparedSessionListing carries one discovered session together with the
// physical project identity and complete-cohort multiplicity evidence.
type PreparedSessionListing = discovery.PreparedSessionListing

// SubagentRelation is the parent-to-subagent relation over the full discovered
// set. It resolves child counts and never widens the picker listing.
type SubagentRelation = discovery.SubagentRelation

// ScannerTreeSource is the kit.TreeSource the mounted kickstart tree loads.
// Grouping lives in discovery.BuildForest. This adapter only projects that
// forest into the tree the terminal renders.
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

// WithIngestedSessionIDs marks the sessions the local store already holds.
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

// WithSubagentRelation supplies the discovered subagent relation used only to
// resolve child counts.
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

// WithPathIdentityResolver injects the boundary that turns working directories
// into physical clone identities.
func WithPathIdentityResolver(resolver ingest.PathIdentityResolver) ScannerOption {
	return func(s *ScannerTreeSource) {
		if resolver != nil {
			s.resolver = resolver
		}
	}
}

// WithRepositoryIdentityResolver injects the Git topology boundary used for
// grouping, preview diagnostics, and remote/name multiplicity.
func WithRepositoryIdentityResolver(resolver ingest.RepositoryIdentityResolver) ScannerOption {
	return func(s *ScannerTreeSource) {
		if resolver != nil {
			s.repositoryResolver = resolver
		}
	}
}

// NewScannerTreeSource builds a TreeSource over an already-discovered session
// listing. The listing is copied so a later mutation of the caller's slice
// cannot change what a load returns.
func NewScannerTreeSource(sessions []ftue.SessionListing, opts ...ScannerOption) *ScannerTreeSource {
	src := &ScannerTreeSource{
		sessions:           append([]ftue.SessionListing(nil), sessions...),
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

// Load projects the discovery forest into the terminal tree. A listing whose
// working directory cannot be resolved does not become a project row. It still
// participates in ambiguity annotation.
func (s *ScannerTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	prepared := prepareSessionListings(ctx, s.sessions, s.resolver, s.repositoryResolver)
	forest := discovery.BuildForest(prepared, s.ingested, s.subagents)
	contexts := discovery.PreviewContexts(forest)
	s.previewMu.Lock()
	s.previewContexts = contexts
	s.previewMu.Unlock()
	return treeFromForest(forest, s.ingested), nil
}

// ListingPreviewContext returns metadata for a project or branch row from the
// exact forest produced by the latest successful Load.
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

// PrepareSessionListings resolves every non-empty working directory, then
// annotates every listing from the complete cohort.
func PrepareSessionListings(sessions []ftue.SessionListing, resolver ingest.PathIdentityResolver) []PreparedSessionListing {
	return discovery.PrepareSessionListings(sessions, resolver)
}

func prepareSessionListings(
	ctx context.Context,
	sessions []ftue.SessionListing,
	resolver ingest.PathIdentityResolver,
	repositoryResolver ingest.RepositoryIdentityResolver,
) []PreparedSessionListing {
	return discovery.PrepareSessionListingsWith(ctx, sessions, resolver, repositoryResolver)
}

func projectRepresentative(rows []PreparedSessionListing) PreparedSessionListing {
	return discovery.ProjectRepresentative(rows)
}

func uniqueScannerClonePath(rows []PreparedSessionListing) (ingest.ClonePath, bool) {
	return discovery.UniqueClonePath(rows)
}

func fallbackRepositoryIdentity(clonePath ingest.ClonePath) ingest.RepositoryIdentity {
	return discovery.FallbackRepositoryIdentity(clonePath)
}

func fallbackRepositoryIdentityFromGitDirectory(gitDirectory ingest.RepositoryPath) ingest.RepositoryIdentity {
	return discovery.FallbackRepositoryIdentityFromGitDirectory(gitDirectory)
}

func repositoryIdentityAvailable(identity ingest.RepositoryIdentity) bool {
	return discovery.RepositoryIdentityAvailable(identity)
}

func treeFromForest(forest onboarding.Forest, ingested map[string]bool) []*kit.TreeNode {
	children := map[string][]onboarding.Node{}
	var roots []onboarding.Node
	for _, node := range forest.Nodes {
		if node.Kind == onboarding.NodeKindProject {
			roots = append(roots, node)
			continue
		}
		children[node.ParentID] = append(children[node.ParentID], node)
	}
	out := make([]*kit.TreeNode, 0, len(roots))
	for _, root := range roots {
		out = append(out, kitNode(root, children, ingested, root.ID))
	}
	return out
}

func kitNode(node onboarding.Node, children map[string][]onboarding.Node, ingested map[string]bool, projectID string) *kit.TreeNode {
	tree := &kit.TreeNode{
		ID:    node.ID,
		Label: node.Label,
		Meta:  nodeMeta(node, ingested, projectID),
	}
	for _, child := range children[node.ID] {
		tree.Children = append(tree.Children, kitNode(child, children, ingested, projectID))
	}
	return tree
}

func nodeMeta(node onboarding.Node, ingested map[string]bool, projectID string) map[string]string {
	switch node.Kind {
	case onboarding.NodeKindProject:
		meta := map[string]string{
			settings.MetaProjectIdentity:    node.ID,
			settings.MetaRemoteMultiplicity: multiplicityText(node.Candidate.RemoteMultiplicity),
			settings.MetaNameMultiplicity:   multiplicityText(node.Candidate.NameMultiplicity),
		}
		if clonePath := node.Candidate.ClonePath.String(); clonePath != "" {
			meta[settings.MetaClonePath] = clonePath
		}
		if node.Candidate.GitRemote != "" {
			meta[settings.MetaRemote] = node.Candidate.GitRemote
		}
		if node.Candidate.ProjectName != "" {
			meta[settings.MetaProjectName] = node.Candidate.ProjectName
		}
		return meta
	case onboarding.NodeKindBranch:
		meta := map[string]string{
			settings.MetaBranch:          node.Label,
			settings.MetaProjectIdentity: projectID,
		}
		if clonePath := node.Candidate.ClonePath.String(); clonePath != "" {
			meta[settings.MetaClonePath] = clonePath
		}
		return meta
	default:
		meta := map[string]string{
			settings.MetaHarness:            string(node.Harness),
			settings.MetaProjectIdentity:    projectID,
			settings.MetaClonePath:          node.Candidate.ClonePath.String(),
			settings.MetaGitDirectory:       node.Labels.Location,
			settings.MetaRemoteMultiplicity: multiplicityText(node.Candidate.RemoteMultiplicity),
			settings.MetaNameMultiplicity:   multiplicityText(node.Candidate.NameMultiplicity),
		}
		if node.Candidate.GitRemote != "" {
			meta[settings.MetaRemote] = node.Candidate.GitRemote
		}
		if node.Candidate.ProjectName != "" {
			meta[settings.MetaProjectName] = node.Candidate.ProjectName
		}
		if ingested[node.SessionID] {
			meta[settings.MetaIngested] = settings.MetaIngestedValue
		}
		if node.ChildCount > 0 {
			meta[settings.MetaChildCount] = itoa(node.ChildCount)
		}
		return meta
	}
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
