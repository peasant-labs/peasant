package selectionfilter

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// CandidateInput is raw operation-cohort evidence. ProjectPath stays a string
// until PrepareCandidates resolves it through the injected path identity boundary.
type CandidateInput struct {
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	ProjectHash string
	ProjectPath string
	Branch      string
	SessionID   ingest.SessionID
}

type physicalIdentity struct {
	harness ingest.Harness
	kind    physicalIdentityKind
	value   string
}

type physicalIdentityKind uint8

const (
	identityResolvedPath physicalIdentityKind = iota
	identityParentFallback
	identityUnavailable
)

type evidenceSet struct {
	identities map[physicalIdentity]struct{}
	unproven   bool
}

// PrepareCandidates resolves a complete operation cohort before any matcher
// call. Sessions in one physical clone share one identity, so session count
// can never be mistaken for clone count. A path failure stays empty and makes
// populated remote or name evidence ambiguous. ProjectHash groups unresolved
// rows only as parent evidence; it never becomes physical clone identity.
func PrepareCandidates(
	ctx context.Context,
	inputs []CandidateInput,
	resolver ingest.PathIdentityResolver,
) ([]ingest.DiscoveryCandidate, error) {
	candidates := make([]ingest.DiscoveryCandidate, len(inputs))
	resolvedByRawPath := make(map[string]ingest.ClonePath)
	failedRawPaths := make(map[string]struct{})

	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate := ingest.DiscoveryCandidate{
			Harness:     input.Harness,
			GitRemote:   input.GitRemote,
			ProjectName: input.ProjectName,
			Branch:      input.Branch,
			SessionID:   input.SessionID,
		}
		candidate.ClonePath = resolvePath(input.ProjectPath, resolver, resolvedByRawPath, failedRawPaths)
		candidates[index] = candidate
	}

	remoteEvidence := make(map[string]*evidenceSet)
	nameEvidence := make(map[string]*evidenceSet)
	for index, input := range inputs {
		identity, proven := candidateIdentity(input, candidates[index])
		if remote := ingest.NormalizeRemoteForMatch(input.GitRemote); remote != "" {
			addEvidence(remoteEvidence, remote, identity, proven)
		}
		if name := ingest.NormalizeProjectNameForMatch(input.ProjectName); name != "" {
			addEvidence(nameEvidence, name, identity, proven)
		}
	}

	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates[index].RemoteMultiplicity = multiplicity(
			ingest.NormalizeRemoteForMatch(input.GitRemote), remoteEvidence,
		)
		candidates[index].NameMultiplicity = multiplicity(
			ingest.NormalizeProjectNameForMatch(input.ProjectName), nameEvidence,
		)
	}
	return candidates, nil
}

func resolvePath(
	raw string,
	resolver ingest.PathIdentityResolver,
	resolved map[string]ingest.ClonePath,
	failed map[string]struct{},
) ingest.ClonePath {
	if raw == "" || resolver == nil {
		return ""
	}
	if path, ok := resolved[raw]; ok {
		return path
	}
	if _, ok := failed[raw]; ok {
		return ""
	}
	path, err := resolver.Resolve(raw)
	if err != nil || path == "" {
		failed[raw] = struct{}{}
		return ""
	}
	resolved[raw] = path
	return path
}

func candidateIdentity(input CandidateInput, candidate ingest.DiscoveryCandidate) (physicalIdentity, bool) {
	if candidate.ClonePath != "" {
		return physicalIdentity{
			harness: input.Harness,
			kind:    identityResolvedPath,
			value:   candidate.ClonePath.String(),
		}, true
	}
	if input.ProjectHash != "" {
		return physicalIdentity{
			harness: input.Harness,
			kind:    identityParentFallback,
			value:   input.ProjectHash,
		}, false
	}
	return physicalIdentity{
		harness: input.Harness,
		kind:    identityUnavailable,
		value:   input.SessionID.String(),
	}, false
}

func addEvidence(sets map[string]*evidenceSet, value string, identity physicalIdentity, proven bool) {
	set := sets[value]
	if set == nil {
		set = &evidenceSet{identities: make(map[physicalIdentity]struct{})}
		sets[value] = set
	}
	set.identities[identity] = struct{}{}
	if !proven {
		set.unproven = true
	}
}

func multiplicity(value string, sets map[string]*evidenceSet) ingest.DiscoveryIdentityMultiplicity {
	// Empty text cannot match. Mark it explicitly unique as an inert value so a
	// producer never leaves fail-closed multiplicity at its zero value by accident.
	if value == "" {
		return ingest.DiscoveryIdentityUnique
	}
	set := sets[value]
	if set == nil || set.unproven || len(set.identities) != 1 {
		return ingest.DiscoveryIdentityAmbiguous
	}
	return ingest.DiscoveryIdentityUnique
}
