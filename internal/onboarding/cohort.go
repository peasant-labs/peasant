package onboarding

import (
	"context"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
)

// ProjectIdentity is the stable exact-selection identity of one discovered
// harness/worktree pair. Display labels, remotes, and project names are
// excluded: two harnesses or two physical worktrees remain distinct persisted
// scopes even when one repository row presents them together.
type ProjectIdentity struct {
	Harness   ingest.Harness
	ClonePath ingest.ClonePath
}

// String returns an unambiguous tree key. The harness is length-prefixed so a
// path cannot collide with a different harness/path pair. This value is identity
// data and is never rendered as row text.
func (i ProjectIdentity) String() string {
	if !i.Available() {
		return ""
	}
	harness := i.Harness.String()
	return fmt.Sprintf("%d:%s%s", len(harness), harness, i.ClonePath.String())
}

// Available reports that both the harness and the physical clone path are set.
func (i ProjectIdentity) Available() bool {
	return i.Harness != "" && i.ClonePath != ""
}

// PreparedSessionListing carries one discovered session together with the
// physical project identity and complete-cohort multiplicity evidence. The
// forest and the post-save ingest callback consume this same value so they
// cannot disagree about clone identity.
type PreparedSessionListing struct {
	Listing            SessionListing
	ProjectIdentity    ProjectIdentity
	RepositoryIdentity ingest.RepositoryIdentity
	Candidate          ingest.DiscoveryCandidate
}

// PrepareSessionListings resolves every non-empty working directory, then
// annotates every listing from the complete cohort. A listing with unresolved
// path evidence remains in the cohort so an explicit session ID can still
// match, but it cannot become a project row.
func PrepareSessionListings(sessions []SessionListing, resolver ingest.PathIdentityResolver) []PreparedSessionListing {
	return PrepareSessionListingsWith(context.Background(), sessions, resolver, ingest.NewGitRepositoryIdentityResolver())
}

// PrepareSessionListingsWith is PrepareSessionListings with the repository
// identity resolver injected. Grouping and preview diagnostics use that
// resolver. Exact worktree paths still come from resolver.
func PrepareSessionListingsWith(
	ctx context.Context,
	sessions []SessionListing,
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
					repositoryIdentity = FallbackRepositoryIdentity(clonePath)
					if repositoryResolver != nil {
						if resolvedIdentity, identityErr := repositoryResolver.ResolveRepositoryIdentity(ctx, clonePath); identityErr == nil && RepositoryIdentityAvailable(resolvedIdentity) {
							repositoryIdentity = resolvedIdentity
						} else if resolvedIdentity.GitDirectory != "" {
							repositoryIdentity = FallbackRepositoryIdentityFromGitDirectory(resolvedIdentity.GitDirectory)
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

// ProjectRepresentative chooses the listing that names a project row. A remote
// wins over a missing remote, then a name, then the session ID.
func ProjectRepresentative(rows []PreparedSessionListing) PreparedSessionListing {
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

// FallbackRepositoryIdentity is the non-Git identity used when repository
// topology cannot be resolved. The clone path is both the cohort key and the
// diagnostic directory.
func FallbackRepositoryIdentity(clonePath ingest.ClonePath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(clonePath.String()),
		GitDirectory: ingest.RepositoryPath(clonePath.String()),
	}
}

// FallbackRepositoryIdentityFromGitDirectory keeps a resolved physical Git
// directory as the cohort when the rest of the topology lookup failed.
func FallbackRepositoryIdentityFromGitDirectory(gitDirectory ingest.RepositoryPath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(gitDirectory.String()),
		GitDirectory: gitDirectory,
	}
}

// RepositoryIdentityAvailable reports that both the cohort key and the Git
// directory were resolved.
func RepositoryIdentityAvailable(identity ingest.RepositoryIdentity) bool {
	return identity.CohortKey != "" && identity.GitDirectory != ""
}
