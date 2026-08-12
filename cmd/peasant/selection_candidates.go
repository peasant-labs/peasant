package main

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// selectionCandidateInput is raw operation-cohort evidence. ProjectPath stays a
// string until prepareSelectionCandidates resolves it through the injected path
// identity boundary.
type selectionCandidateInput struct {
	Harness     ingest.Harness
	GitRemote   string
	ProjectName string
	ProjectHash string
	ProjectPath string
	Branch      string
	SessionID   ingest.SessionID
}

type selectionPhysicalIdentity struct {
	harness ingest.Harness
	kind    selectionPhysicalIdentityKind
	value   string
}

type selectionPhysicalIdentityKind uint8

const (
	selectionIdentityResolvedPath selectionPhysicalIdentityKind = iota
	selectionIdentityParentFallback
	selectionIdentityUnavailable
)

type selectionEvidenceSet struct {
	identities map[selectionPhysicalIdentity]struct{}
	unproven   bool
}

// prepareSelectionCandidates resolves a complete operation cohort before any
// matcher call. Sessions in one physical clone share one identity, so session
// count can never be mistaken for clone count. A path failure stays empty and
// makes populated remote/name evidence ambiguous. ProjectHash groups unresolved
// rows only as parent evidence; it never becomes physical clone identity.
func prepareSelectionCandidates(
	ctx context.Context,
	inputs []selectionCandidateInput,
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
		candidate.ClonePath = resolveSelectionPath(input.ProjectPath, resolver, resolvedByRawPath, failedRawPaths)
		candidates[index] = candidate
	}

	remoteEvidence := make(map[string]*selectionEvidenceSet)
	nameEvidence := make(map[string]*selectionEvidenceSet)
	for index, input := range inputs {
		identity, proven := selectionCandidateIdentity(input, candidates[index])
		if remote := ingest.NormalizeRemoteForMatch(input.GitRemote); remote != "" {
			addSelectionEvidence(remoteEvidence, remote, identity, proven)
		}
		if name := ingest.NormalizeProjectNameForMatch(input.ProjectName); name != "" {
			addSelectionEvidence(nameEvidence, name, identity, proven)
		}
	}

	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates[index].RemoteMultiplicity = selectionMultiplicity(
			ingest.NormalizeRemoteForMatch(input.GitRemote), remoteEvidence,
		)
		candidates[index].NameMultiplicity = selectionMultiplicity(
			ingest.NormalizeProjectNameForMatch(input.ProjectName), nameEvidence,
		)
	}
	return candidates, nil
}

func resolveSelectionPath(
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

func selectionCandidateIdentity(
	input selectionCandidateInput,
	candidate ingest.DiscoveryCandidate,
) (selectionPhysicalIdentity, bool) {
	if candidate.ClonePath != "" {
		return selectionPhysicalIdentity{
			harness: input.Harness,
			kind:    selectionIdentityResolvedPath,
			value:   candidate.ClonePath.String(),
		}, true
	}

	if input.ProjectHash != "" {
		return selectionPhysicalIdentity{
			harness: input.Harness,
			kind:    selectionIdentityParentFallback,
			value:   input.ProjectHash,
		}, false
	}

	return selectionPhysicalIdentity{
		harness: input.Harness,
		kind:    selectionIdentityUnavailable,
		value:   input.SessionID.String(),
	}, false
}

func addSelectionEvidence(
	sets map[string]*selectionEvidenceSet,
	value string,
	identity selectionPhysicalIdentity,
	proven bool,
) {
	set := sets[value]
	if set == nil {
		set = &selectionEvidenceSet{identities: make(map[selectionPhysicalIdentity]struct{})}
		sets[value] = set
	}
	set.identities[identity] = struct{}{}
	if !proven {
		set.unproven = true
	}
}

func selectionMultiplicity(value string, sets map[string]*selectionEvidenceSet) ingest.DiscoveryIdentityMultiplicity {
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
