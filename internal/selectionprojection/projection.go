package selectionprojection

import (
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

type cohortIdentityKind uint8

const (
	cohortIdentityRepositoryCohort cohortIdentityKind = iota
	cohortIdentityPhysicalPath
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
		recordCandidateIdentity(remoteMultiplicity, nameMultiplicity, candidate, candidate.ClonePath, candidate.RepositoryCohortKey)
		for _, descendant := range candidate.Descendants {
			recordCandidateIdentity(
				remoteMultiplicity,
				nameMultiplicity,
				candidate,
				effectiveClonePath(candidate, descendant),
				effectiveRepositoryCohortKey(candidate, descendant),
			)
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
	repositoryCohortKey ingest.RepositoryCohortKey,
) {
	identity, proven := candidateIdentity(candidate, clonePath, repositoryCohortKey)
	recordIdentity(remoteMultiplicity, ingest.NormalizeRemoteForMatch(candidate.GitRemote), identity, proven)
	recordIdentity(nameMultiplicity, ingest.NormalizeProjectNameForMatch(candidate.ProjectName), identity, proven)
}

func candidateIdentity(candidate ProjectCandidate, clonePath ingest.ClonePath, repositoryCohortKey ingest.RepositoryCohortKey) (cohortIdentity, bool) {
	if repositoryCohortKey != "" {
		return cohortIdentity{
			Harness: candidate.Harness,
			Kind:    cohortIdentityRepositoryCohort,
			Value:   repositoryCohortKey.String(),
		}, true
	}
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

func effectiveRepositoryCohortKey(project ProjectCandidate, descendant SessionCandidate) ingest.RepositoryCohortKey {
	if descendant.RepositoryCohortKey != "" {
		return descendant.RepositoryCohortKey
	}
	return project.RepositoryCohortKey
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
	allDescendantsDenied := len(project.Descendants) > 0

	selectedDescendants := 0
	for _, descendant := range project.Descendants {
		if !matcher.ExcludesCandidate(descendant.Direct) {
			allDescendantsDenied = false
		}
		candidateAdmission := descendantAdmission(matcher, descendant)
		if candidateAdmission == ProjectNotEffective {
			continue
		}
		selectedDescendants++
		admission = moreSpecificAdmission(admission, candidateAdmission)
	}
	// A selected project can remain effective when a non-denied available
	// descendant belongs to another clone, but exact denial of every available
	// descendant must not leave a parent-only row or suppress the no-project gate.
	if !allDescendantsDenied && projectRuleAdmission(matcher, project.Project) == ProjectEffectiveWholeProject {
		admission = moreSpecificAdmission(admission, ProjectEffectiveWholeProject)
	}
	return admission, selectedDescendants
}

func descendantAdmission(matcher *ingest.SelectionMatcher, descendant preparedDescendant) ProjectAdmission {
	// Exact denial applies to the original session/path/branch evidence before
	// project admission clears the session ID or a selected parent can admit the
	// child. This keeps viewer projection and the save gate on the matcher's
	// positive-then-deny contract.
	if matcher.ExcludesCandidate(descendant.Direct) {
		return ProjectNotEffective
	}
	projectCandidate := descendant.Direct
	projectCandidate.SessionID = ""
	admission := ProjectNotEffective
	admission = moreSpecificAdmission(admission, projectRuleAdmission(matcher, projectCandidate))
	if explicitSessionSelected(matcher, descendant.Direct) {
		admission = moreSpecificAdmission(admission, ProjectEffectiveExplicitSession)
	}
	if descendant.HasParent && explicitSessionSelected(matcher, descendant.Parent) {
		admission = moreSpecificAdmission(admission, ProjectEffectiveNestedDescendant)
	}
	return admission
}

func explicitSessionSelected(matcher *ingest.SelectionMatcher, candidate ingest.DiscoveryCandidate) bool {
	if candidate.SessionID == "" {
		return false
	}

	// Remove project evidence so the canonical matcher can answer only from the
	// session ID. The empty-session control distinguishes a real explicit entry
	// from an unrestricted harness, which admits both probes.
	candidate.GitRemote = ""
	candidate.ProjectName = ""
	candidate.ClonePath = ""
	candidate.Branch = ""
	candidate.RemoteMultiplicity = ingest.DiscoveryIdentityUnique
	candidate.NameMultiplicity = ingest.DiscoveryIdentityUnique
	if matcher.MatchBranchCandidate(candidate) != ingest.BranchMatchYes {
		return false
	}
	candidate.SessionID = ""
	return matcher.MatchBranchCandidate(candidate) != ingest.BranchMatchYes
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
