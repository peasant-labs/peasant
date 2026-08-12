package selectionprojection

import (
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
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
	ProjectName string
	GitRemote   string
	CohortKey   ingest.RepositoryCohortKey
}

func DistinctLocationLabels(evidence []LocationLabelEvidence) []string {
	byName := make(map[string]map[ingest.RepositoryCohortKey]struct{})
	for _, item := range evidence {
		name, ok := projectlabel.FromRemote(item.GitRemote)
		if !ok {
			name = ingest.NormalizeProjectNameForMatch(item.ProjectName)
		}
		if name == "" {
			name = "repository"
		}
		if byName[name] == nil {
			byName[name] = make(map[ingest.RepositoryCohortKey]struct{})
		}
		byName[name][item.CohortKey] = struct{}{}
	}
	ordinals := make(map[string]map[ingest.RepositoryCohortKey]int)
	for name, identities := range byName {
		keys := make([]string, 0, len(identities))
		for key := range identities {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		ordinals[name] = make(map[ingest.RepositoryCohortKey]int)
		for i, key := range keys {
			ordinals[name][ingest.RepositoryCohortKey(key)] = i + 1
		}
	}
	labels := make([]string, len(evidence))
	for i, item := range evidence {
		name, ok := projectlabel.FromRemote(item.GitRemote)
		if !ok {
			name = ingest.NormalizeProjectNameForMatch(item.ProjectName)
		}
		if name == "" {
			name = "repository"
		}
		labels[i] = name
		if len(byName[name]) > 1 {
			labels[i] = fmt.Sprintf("%s (%d)", name, ordinals[name][item.CohortKey])
		}
	}
	return labels
}
