package selectionprojection

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

type CohortEvidence struct {
	Harness   ingest.Harness
	Text      string
	CohortKey ingest.RepositoryCohortKey
}
type cohortMultiplicityKey struct {
	harness ingest.Harness
	text    string
}

func CohortMultiplicities(evidence []CohortEvidence) []ingest.DiscoveryIdentityMultiplicity {
	type cohort struct {
		identities map[ingest.RepositoryCohortKey]struct{}
		unresolved bool
	}
	cohorts := make(map[cohortMultiplicityKey]*cohort)
	for _, item := range evidence {
		key := cohortMultiplicityKey{item.Harness, item.Text}
		if key.text == "" {
			continue
		}
		group := cohorts[key]
		if group == nil {
			group = &cohort{identities: make(map[ingest.RepositoryCohortKey]struct{})}
			cohorts[key] = group
		}
		if item.CohortKey == "" {
			group.unresolved = true
		} else {
			group.identities[item.CohortKey] = struct{}{}
		}
	}
	result := make([]ingest.DiscoveryIdentityMultiplicity, len(evidence))
	for i, item := range evidence {
		if item.Text == "" {
			result[i] = ingest.DiscoveryIdentityUnique
			continue
		}
		result[i] = ingest.DiscoveryIdentityAmbiguous
		group := cohorts[cohortMultiplicityKey{item.Harness, item.Text}]
		if group != nil && !group.unresolved && len(group.identities) == 1 {
			result[i] = ingest.DiscoveryIdentityUnique
		}
	}
	return result
}

type LocationLabelEvidence struct {
	PresentationGroup string
	ClonePath         ingest.ClonePath
	CohortKey         ingest.RepositoryCohortKey
}

// DistinctLocationLabels returns one recognizable, display-safe local suffix for
// each proven repository cohort. It deliberately excludes repository remotes,
// Git directories, and the opaque cohort key from rendered output.
func DistinctLocationLabels(evidence []LocationLabelEvidence) []string {
	type groupCohort struct {
		group  string
		cohort ingest.RepositoryCohortKey
	}
	representatives := make(map[groupCohort]ingest.ClonePath)
	for _, item := range evidence {
		key := groupCohort{item.PresentationGroup, item.CohortKey}
		current := representatives[key]
		if item.ClonePath != "" && (current == "" || item.ClonePath.String() < current.String()) {
			representatives[key] = item.ClonePath
		}
	}
	pathsByGroup := make(map[string][]ingest.ClonePath)
	for key, path := range representatives {
		pathsByGroup[key.group] = append(pathsByGroup[key.group], path)
	}
	for group := range pathsByGroup {
		sort.Slice(pathsByGroup[group], func(i, j int) bool { return pathsByGroup[group][i].String() < pathsByGroup[group][j].String() })
	}
	byCohort := make(map[groupCohort]string, len(representatives))
	for key, path := range representatives {
		byCohort[key] = ShortestDistinctCloneSuffix(path, pathsByGroup[key.group])
	}
	labels := make([]string, len(evidence))
	for i, item := range evidence {
		labels[i] = byCohort[groupCohort{item.PresentationGroup, item.CohortKey}]
	}
	return labels
}

// ShortestDistinctCloneSuffix applies the canonical clone/worktree label rule:
// start with two path components and widen only until the suffix is distinct.
func ShortestDistinctCloneSuffix(clonePath ingest.ClonePath, cohort []ingest.ClonePath) string {
	parts := clonePathParts(clonePath)
	if len(parts) == 0 {
		return ""
	}
	width := min(2, len(parts))
	for width < len(parts) && !cloneSuffixUnique(parts, width, clonePath, cohort) {
		width++
	}
	suffix := filepath.Join(parts[len(parts)-width:]...)
	if width == len(parts) && len(parts) > 2 {
		return "…" + string(filepath.Separator) + suffix
	}
	return suffix
}

func clonePathParts(clonePath ingest.ClonePath) []string {
	if clonePath == "" {
		return nil
	}
	clean := filepath.Clean(clonePath.String())
	volume := filepath.VolumeName(clean)
	parts := strings.FieldsFunc(strings.TrimPrefix(clean, volume), func(r rune) bool { return r == '/' || r == '\\' })
	if volume != "" {
		parts = append([]string{volume}, parts...)
	}
	return parts
}

func cloneSuffixUnique(parts []string, width int, own ingest.ClonePath, cohort []ingest.ClonePath) bool {
	suffix := filepath.Join(parts[len(parts)-width:]...)
	for _, candidate := range cohort {
		if candidate == own {
			continue
		}
		candidateParts := clonePathParts(candidate)
		if len(candidateParts) >= width && filepath.Join(candidateParts[len(candidateParts)-width:]...) == suffix {
			return false
		}
	}
	return true
}
