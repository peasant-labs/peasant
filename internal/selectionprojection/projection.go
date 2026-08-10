package selectionprojection

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

type cohortIdentityKind uint8

const (
	cohortIdentityPhysicalPath cohortIdentityKind = iota
	cohortIdentityParentProject
)

type cohortIdentity struct {
	Harness ingest.Harness
	Kind    cohortIdentityKind
	Value   string
}

type identityMultiplicity struct {
	Identities map[cohortIdentity]struct{}
	Unproven   bool
}

type preparedProject struct {
	Candidate   ProjectCandidate
	Project     ingest.DiscoveryCandidate
	Descendants []preparedDescendant
}

type preparedDescendant struct {
	Direct    ingest.DiscoveryCandidate
	Parent    ingest.DiscoveryCandidate
	HasParent bool
}

func effectiveProjects(
	matcher *ingest.SelectionMatcher,
	selection config.SelectionConfig,
	candidates []ProjectCandidate,
) []EffectiveProject {
	if len(candidates) == 0 {
		return nil
	}
	if selection.Mode == config.SelectionModeAll {
		result := make([]EffectiveProject, len(candidates))
		for index, candidate := range candidates {
			result[index] = EffectiveProject{
				Candidate:               candidate,
				Admission:               ProjectEffectiveWholeProject,
				SelectedDescendantCount: len(candidate.Descendants),
			}
		}
		return result
	}
	if selection.Mode != config.SelectionModeSelected || matcher == nil {
		return nil
	}

	// Preparing the entire cohort before the first matcher call is required:
	// remote and name evidence is safe only when its multiplicity is known over
	// every available physical identity in this operation.
	prepared := prepareCohort(candidates)
	result := make([]EffectiveProject, 0, len(prepared))
	for _, candidate := range prepared {
		admission, selectedDescendants := projectAdmission(matcher, candidate)
		if admission == ProjectNotEffective {
			continue
		}
		result = append(result, EffectiveProject{
			Candidate:               candidate.Candidate,
			Admission:               admission,
			SelectedDescendantCount: selectedDescendants,
		})
	}
	return result
}

func prepareCohort(candidates []ProjectCandidate) []preparedProject {
	remoteMultiplicity := make(map[string]*identityMultiplicity)
	nameMultiplicity := make(map[string]*identityMultiplicity)
	for _, candidate := range candidates {
		recordCandidateIdentity(remoteMultiplicity, nameMultiplicity, candidate, candidate.ClonePath)
		for _, descendant := range candidate.Descendants {
			recordCandidateIdentity(remoteMultiplicity, nameMultiplicity, candidate, effectiveClonePath(candidate, descendant))
		}
	}

	prepared := make([]preparedProject, len(candidates))
	for projectIndex, candidate := range candidates {
		remote := explicitMultiplicity(candidate.GitRemote, ingest.NormalizeRemoteForMatch(candidate.GitRemote), remoteMultiplicity)
		name := explicitMultiplicity(candidate.ProjectName, ingest.NormalizeProjectNameForMatch(candidate.ProjectName), nameMultiplicity)
		project := preparedProject{
			Candidate: candidate,
			Project: discoveryCandidate(
				candidate,
				SessionCandidate{},
				candidate.ClonePath,
				remote,
				name,
			),
			Descendants: make([]preparedDescendant, len(candidate.Descendants)),
		}
		for sessionIndex, descendant := range candidate.Descendants {
			direct := discoveryCandidate(
				candidate,
				descendant,
				effectiveClonePath(candidate, descendant),
				remote,
				name,
			)
			preparedDescendant := preparedDescendant{Direct: direct}
			if descendant.ParentSessionID != "" {
				preparedDescendant.Parent = direct
				preparedDescendant.Parent.SessionID = descendant.ParentSessionID
				preparedDescendant.HasParent = true
			}
			project.Descendants[sessionIndex] = preparedDescendant
		}
		prepared[projectIndex] = project
	}
	return prepared
}

func recordCandidateIdentity(
	remoteMultiplicity map[string]*identityMultiplicity,
	nameMultiplicity map[string]*identityMultiplicity,
	candidate ProjectCandidate,
	clonePath ingest.ClonePath,
) {
	identity, proven := candidateIdentity(candidate, clonePath)
	recordIdentity(remoteMultiplicity, ingest.NormalizeRemoteForMatch(candidate.GitRemote), identity, proven)
	recordIdentity(nameMultiplicity, ingest.NormalizeProjectNameForMatch(candidate.ProjectName), identity, proven)
}

func candidateIdentity(candidate ProjectCandidate, clonePath ingest.ClonePath) (cohortIdentity, bool) {
	if clonePath != "" {
		return cohortIdentity{
			Harness: candidate.Harness,
			Kind:    cohortIdentityPhysicalPath,
			Value:   clonePath.String(),
		}, true
	}
	return cohortIdentity{
		Harness: candidate.Harness,
		Kind:    cohortIdentityParentProject,
		Value:   string(candidate.ParentProjectID),
	}, false
}

func recordIdentity(
	multiplicities map[string]*identityMultiplicity,
	normalized string,
	identity cohortIdentity,
	proven bool,
) {
	if normalized == "" {
		return
	}
	multiplicity := multiplicities[normalized]
	if multiplicity == nil {
		multiplicity = &identityMultiplicity{Identities: make(map[cohortIdentity]struct{})}
		multiplicities[normalized] = multiplicity
	}
	multiplicity.Identities[identity] = struct{}{}
	if !proven {
		multiplicity.Unproven = true
	}
}

func explicitMultiplicity(
	raw string,
	normalized string,
	multiplicities map[string]*identityMultiplicity,
) ingest.DiscoveryIdentityMultiplicity {
	if strings.TrimSpace(raw) == "" {
		// Empty identity text cannot match. Unique is an explicit inert value,
		// not an unproven zero left for a matcher to interpret.
		return ingest.DiscoveryIdentityUnique
	}
	multiplicity := multiplicities[normalized]
	if normalized == "" || multiplicity == nil || multiplicity.Unproven || len(multiplicity.Identities) != 1 {
		return ingest.DiscoveryIdentityAmbiguous
	}
	return ingest.DiscoveryIdentityUnique
}

func effectiveClonePath(project ProjectCandidate, descendant SessionCandidate) ingest.ClonePath {
	if descendant.ClonePath != "" {
		return descendant.ClonePath
	}
	return project.ClonePath
}

func discoveryCandidate(
	project ProjectCandidate,
	descendant SessionCandidate,
	clonePath ingest.ClonePath,
	remoteMultiplicity ingest.DiscoveryIdentityMultiplicity,
	nameMultiplicity ingest.DiscoveryIdentityMultiplicity,
) ingest.DiscoveryCandidate {
	return ingest.DiscoveryCandidate{
		Harness:            project.Harness,
		GitRemote:          project.GitRemote,
		ProjectName:        project.ProjectName,
		ClonePath:          clonePath,
		Branch:             descendant.Branch,
		SessionID:          descendant.SessionID,
		RemoteMultiplicity: remoteMultiplicity,
		NameMultiplicity:   nameMultiplicity,
	}
}

func projectAdmission(matcher *ingest.SelectionMatcher, project preparedProject) (ProjectAdmission, int) {
	admission := ProjectNotEffective
	if projectRuleAdmission(matcher, project.Project) == ProjectEffectiveWholeProject {
		admission = ProjectEffectiveWholeProject
	}

	selectedDescendants := 0
	for _, descendant := range project.Descendants {
		candidateAdmission := descendantAdmission(matcher, descendant)
		if candidateAdmission == ProjectNotEffective {
			continue
		}
		selectedDescendants++
		admission = moreSpecificAdmission(admission, candidateAdmission)
	}
	return admission, selectedDescendants
}

func descendantAdmission(matcher *ingest.SelectionMatcher, descendant preparedDescendant) ProjectAdmission {
	projectCandidate := descendant.Direct
	projectCandidate.SessionID = ""
	if admission := projectRuleAdmission(matcher, projectCandidate); admission != ProjectNotEffective {
		return admission
	}
	if matcher.MatchBranchCandidate(descendant.Direct) == ingest.BranchMatchYes {
		return ProjectEffectiveExplicitSession
	}
	if descendant.HasParent && matcher.MatchBranchCandidate(descendant.Parent) == ingest.BranchMatchYes {
		return ProjectEffectiveNestedDescendant
	}
	return ProjectNotEffective
}

func projectRuleAdmission(matcher *ingest.SelectionMatcher, candidate ingest.DiscoveryCandidate) ProjectAdmission {
	if matcher.MatchBranchCandidate(candidate) != ingest.BranchMatchYes {
		return ProjectNotEffective
	}

	// The candidate decision exposes the exact matching selection entries. Its
	// entry metadata distinguishes an unrestricted project from a branch rule;
	// MatchBranchCandidate above remains the admission decision for stored rows.
	decision := matcher.MatchDiscoveryCandidateDecision(candidate, false)
	if len(decision.Admitting) == 0 && len(decision.Rejecting) == 0 {
		return ProjectEffectiveWholeProject
	}
	for _, entry := range decision.Admitting {
		if len(entry.Branches) == 0 {
			return ProjectEffectiveWholeProject
		}
	}
	for _, entry := range decision.Rejecting {
		if len(entry.Branches) == 0 {
			return ProjectEffectiveWholeProject
		}
	}
	return ProjectEffectiveBranch
}

func moreSpecificAdmission(current, candidate ProjectAdmission) ProjectAdmission {
	if candidate > current {
		return candidate
	}
	return current
}
