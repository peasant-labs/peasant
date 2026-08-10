package kickstart

import (
	"errors"
	"sort"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

// LegacyAllConversion is the in-memory replacement for a legacy mode-all
// policy. Initial contains only exact available scanner projects. Unmatched
// carries stored sessions whose physical project is not available, so the
// commit path can preserve those IDs instead of deleting evidence it could not
// present for editing.
type LegacyAllConversion struct {
	Initial   config.SelectionConfig
	Unmatched settings.UnmatchedBaseline
}

// ConvertLegacyAll builds the first exact selected-mode policy from complete
// stored and scanner cohorts. The caller detects mode all before invoking this
// function; selected-mode matching never performs the mode transition.
//
// Each stored row contributes physical project evidence from GitWorktree when
// populated, otherwise CanonicalCwd. Only that chosen value is resolved. A
// missing or unresolvable value never falls back to the second path, a remote,
// a name, a branch, a title, or a date. Its SessionID remains in Unmatched under
// the stored Harness. Scanner candidates are prepared as one complete cohort,
// with explicit unique or ambiguous remote/name multiplicities, before the
// canonical candidate matcher is consulted.
//
// This function performs no file write. A kickstart caller applies the returned
// values to its draft and persists them only through the existing user-confirmed
// atomic commit. AutoIngestNewBranches is copied to Initial unchanged.
func ConvertLegacyAll(
	scan []ftue.SessionListing,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
	autoIngestNewBranches bool,
) (LegacyAllConversion, error) {
	if resolver == nil {
		return LegacyAllConversion{}, errors.New(
			"convert legacy project selection: " +
				"what: Peasant cannot build exact local project identities; " +
				"why: no path identity resolver was provided; " +
				"where: kickstart.ConvertLegacyAll; " +
				"when: before preparing the stored and scanner cohorts; " +
				"meaning: the legacy all-projects policy cannot be narrowed safely; " +
				"fix: provide ingest.NewPhysicalPathResolver and retry kickstart")
	}

	storedEvidence, exactStoredSelection := prepareLegacyStoredEvidence(stored, resolver)
	matcher := config.CompileSelectionMatcher(exactStoredSelection)
	scannerProjects := prepareLegacyScannerProjects(scan, resolver)

	selectedIdentities := make(map[string]struct{}, len(scannerProjects))
	selectedGroups := map[string]map[legacySelectionGroupKey]*legacySelectionGroup{}
	for _, project := range scannerProjects {
		representative := projectRepresentative(project.rows)
		candidate := representative.Candidate
		// Conversion is a project-level decision. Clear descendant-only fields so
		// a future explicit-session rule cannot silently change this boundary.
		candidate.Branch = ""
		candidate.SessionID = ""
		if !matcher.MatchesCandidate(candidate) {
			continue
		}

		selectedIdentities[project.identity.String()] = struct{}{}
		addLegacySelectedProject(selectedGroups, project.identity, representative)
	}

	return LegacyAllConversion{
		Initial: config.SelectionConfig{
			Mode:                  config.SelectionModeSelected,
			AutoIngestNewBranches: autoIngestNewBranches,
			Harnesses:             buildLegacySelectedHarnesses(selectedGroups),
		},
		Unmatched: buildLegacyUnmatchedBaseline(storedEvidence, selectedIdentities),
	}, nil
}

type legacyStoredEvidence struct {
	row      store.IngestedSessionRow
	identity ProjectIdentity
}

// prepareLegacyStoredEvidence resolves every stored row before any scanner
// matching starts. The matcher input contains path-only rules, so remote and name
// text from the store can never identify a project during conversion.
func prepareLegacyStoredEvidence(
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) ([]legacyStoredEvidence, config.SelectionConfig) {
	evidence := make([]legacyStoredEvidence, len(stored))
	harnesses := map[string]config.SelectionHarnessConfig{}
	for index, row := range stored {
		resolved := legacyStoredEvidence{row: row}
		rawPath := row.GitWorktree
		if rawPath == "" {
			rawPath = row.CanonicalCwd
		}
		if rawPath != "" {
			clonePath, err := resolver.Resolve(rawPath)
			if err == nil && clonePath != "" {
				identity := ProjectIdentity{Harness: ingest.Harness(row.Harness), ClonePath: clonePath}
				if row.SessionID != "" && identity.available() {
					resolved.identity = identity
					harness := harnesses[row.Harness]
					harness.Projects = appendLegacyPathRule(harness.Projects, clonePath)
					harnesses[row.Harness] = harness
				}
			}
		}
		evidence[index] = resolved
	}
	for harness, selected := range harnesses {
		sort.Slice(selected.Projects, func(i, j int) bool {
			return selected.Projects[i].ClonePaths[0] < selected.Projects[j].ClonePaths[0]
		})
		harnesses[harness] = selected
	}
	if len(harnesses) == 0 {
		harnesses = nil
	}
	return evidence, config.SelectionConfig{
		Mode:      config.SelectionModeSelected,
		Harnesses: harnesses,
	}
}

func appendLegacyPathRule(projects []config.ProjectSelection, clonePath ingest.ClonePath) []config.ProjectSelection {
	for _, project := range projects {
		if len(project.ClonePaths) == 1 && project.ClonePaths[0] == clonePath.String() {
			return projects
		}
	}
	return append(projects, config.ProjectSelection{ClonePaths: []string{clonePath.String()}})
}

type legacyScannerProject struct {
	identity ProjectIdentity
	rows     []PreparedSessionListing
}

// prepareLegacyScannerProjects resolves and annotates the complete scanner
// cohort through PrepareSessionListings, then groups sessions by the only
// project identity conversion trusts: stored harness plus physical clone path.
func prepareLegacyScannerProjects(
	scan []ftue.SessionListing,
	resolver ingest.PathIdentityResolver,
) []legacyScannerProject {
	byIdentity := map[string]*legacyScannerProject{}
	for _, row := range PrepareSessionListings(scan, resolver) {
		if !row.ProjectIdentity.available() {
			continue
		}
		key := row.ProjectIdentity.String()
		project := byIdentity[key]
		if project == nil {
			project = &legacyScannerProject{identity: row.ProjectIdentity}
			byIdentity[key] = project
		}
		project.rows = append(project.rows, row)
	}

	keys := make([]string, 0, len(byIdentity))
	for key := range byIdentity {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	projects := make([]legacyScannerProject, 0, len(keys))
	for _, key := range keys {
		projects = append(projects, *byIdentity[key])
	}
	return projects
}

type legacySelectionGroupKind uint8

const (
	legacySelectionByRemote legacySelectionGroupKind = iota + 1
	legacySelectionByName
	legacySelectionByPath
)

type legacySelectionGroupKey struct {
	kind legacySelectionGroupKind
	text string
}

type legacySelectionGroup struct {
	selection  config.ProjectSelection
	clonePaths map[string]struct{}
}

func addLegacySelectedProject(
	groups map[string]map[legacySelectionGroupKey]*legacySelectionGroup,
	identity ProjectIdentity,
	representative PreparedSessionListing,
) {
	harness := identity.Harness.String()
	if groups[harness] == nil {
		groups[harness] = map[legacySelectionGroupKey]*legacySelectionGroup{}
	}

	key, selection := legacySelectionIdentity(representative, identity.ClonePath)
	group := groups[harness][key]
	if group == nil {
		group = &legacySelectionGroup{selection: selection, clonePaths: map[string]struct{}{}}
		groups[harness][key] = group
	} else {
		// Equivalent remote spellings can normalize to one entry. Preserve a
		// deterministic scanner spelling without allowing it to decide identity.
		if selection.GitRemote != "" && (group.selection.GitRemote == "" || selection.GitRemote < group.selection.GitRemote) {
			group.selection.GitRemote = selection.GitRemote
		}
	}
	group.clonePaths[identity.ClonePath.String()] = struct{}{}
}

func legacySelectionIdentity(
	representative PreparedSessionListing,
	clonePath ingest.ClonePath,
) (legacySelectionGroupKey, config.ProjectSelection) {
	if remote := ingest.NormalizeRemoteForMatch(representative.Listing.GitRemote); remote != "" {
		return legacySelectionGroupKey{kind: legacySelectionByRemote, text: remote},
			config.ProjectSelection{GitRemote: representative.Listing.GitRemote}
	}
	if name := ingest.NormalizeProjectNameForMatch(representative.Listing.ProjectName); name != "" {
		return legacySelectionGroupKey{kind: legacySelectionByName, text: name},
			config.ProjectSelection{Name: name}
	}
	return legacySelectionGroupKey{kind: legacySelectionByPath, text: clonePath.String()}, config.ProjectSelection{}
}

func buildLegacySelectedHarnesses(
	groups map[string]map[legacySelectionGroupKey]*legacySelectionGroup,
) map[string]config.SelectionHarnessConfig {
	if len(groups) == 0 {
		return nil
	}
	harnesses := make(map[string]config.SelectionHarnessConfig, len(groups))
	for harness, grouped := range groups {
		keys := make([]legacySelectionGroupKey, 0, len(grouped))
		for key := range grouped {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].kind != keys[j].kind {
				return keys[i].kind < keys[j].kind
			}
			return keys[i].text < keys[j].text
		})

		selected := config.SelectionHarnessConfig{}
		for _, key := range keys {
			group := grouped[key]
			paths := make([]string, 0, len(group.clonePaths))
			for clonePath := range group.clonePaths {
				paths = append(paths, clonePath)
			}
			sort.Strings(paths)
			selection := group.selection
			selection.ClonePaths = paths
			selected.Projects = append(selected.Projects, selection)
		}
		harnesses[harness] = selected
	}
	return harnesses
}

func buildLegacyUnmatchedBaseline(
	stored []legacyStoredEvidence,
	selectedIdentities map[string]struct{},
) settings.UnmatchedBaseline {
	harnesses := map[string]config.SelectionHarnessConfig{}
	for _, evidence := range stored {
		if evidence.row.SessionID == "" {
			continue
		}
		if evidence.identity.available() {
			if _, selected := selectedIdentities[evidence.identity.String()]; selected {
				continue
			}
		}
		harness := harnesses[evidence.row.Harness]
		harness.Sessions = appendLegacyUniqueString(harness.Sessions, evidence.row.SessionID)
		harnesses[evidence.row.Harness] = harness
	}
	for harness, unmatched := range harnesses {
		sort.Strings(unmatched.Sessions)
		harnesses[harness] = unmatched
	}
	if len(harnesses) == 0 {
		harnesses = nil
	}
	return settings.UnmatchedBaseline{Harnesses: harnesses}
}

func appendLegacyUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
