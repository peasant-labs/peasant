package kickstart

import (
	"errors"
	"sort"
	"strconv"
	"strings"

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

// ConvertLegacySelected replaces selected-mode project rules that do not carry
// physical clone paths with exact choices derived from the complete stored
// session cohort. Scanner rows are deliberately not an input: a newly discovered
// sibling must stay clear until the user selects it.
//
// The transform is in-memory only. The caller installs the result in a Draft,
// and Draft.Commit remains the only persistence boundary. A selected config that
// has no pathless project rules is returned as a defensive field-equivalent copy
// without consulting the resolver.
func ConvertLegacySelected(
	current config.SelectionConfig,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) (config.SelectionConfig, error) {
	converted := cloneLegacySelectionConfig(current)
	pathlessByHarness := legacyPathlessProjects(current)
	if current.Mode != config.SelectionModeSelected || len(pathlessByHarness) == 0 {
		return converted, nil
	}
	if resolver == nil {
		return config.SelectionConfig{}, errors.New(
			"convert legacy selected project selection: " +
				"what: Peasant cannot replace pathless project rules with exact local clone paths; " +
				"why: no path identity resolver was provided; " +
				"where: kickstart.ConvertLegacySelected; " +
				"when: before resolving the complete stored-session cohort; " +
				"meaning: the selected project policy cannot be narrowed without risking a sibling-clone match; " +
				"fix: provide ingest.NewPhysicalPathResolver and retry kickstart")
	}

	evidence, _ := prepareLegacyStoredEvidence(stored, resolver)
	cohorts := buildLegacySelectedCohorts(current, pathlessByHarness, evidence)
	canonical, cohortPaths := buildLegacySelectedCanonicalProjects(cohorts)
	newSessions := buildLegacySelectedUnresolvedSessions(current, pathlessByHarness, evidence)

	for harness := range pathlessByHarness {
		configured, present := current.Harnesses[harness]
		if !present {
			continue
		}
		rebuilt := rebuildLegacySelectedHarness(
			configured,
			canonical[harness],
			cohortPaths[harness],
			newSessions[harness],
		)
		// Once its pathless positives are consumed, an otherwise empty harness
		// must disappear. Keeping an exclusions-only harness would mean "select
		// every session, then deny a few" under the canonical matcher and would
		// widen the legacy rule.
		if len(rebuilt.Projects) == 0 && len(rebuilt.Sessions) == 0 {
			delete(converted.Harnesses, harness)
			continue
		}
		converted.Harnesses[harness] = rebuilt
	}
	if len(converted.Harnesses) == 0 {
		converted.Harnesses = nil
	}
	return converted, nil
}

func legacyPathlessProjects(selection config.SelectionConfig) map[string][]config.ProjectSelection {
	pathless := map[string][]config.ProjectSelection{}
	for harness, configured := range selection.Harnesses {
		for _, project := range configured.Projects {
			if len(project.ClonePaths) == 0 {
				pathless[harness] = append(pathless[harness], cloneLegacyProjectSelection(project))
			}
		}
	}
	if len(pathless) == 0 {
		return nil
	}
	return pathless
}

type legacySelectedCohort struct {
	identity ProjectIdentity
	rows     []store.IngestedSessionRow
	rules    []config.ProjectSelection
	branches []string
	admitted bool
}

func buildLegacySelectedCohorts(
	current config.SelectionConfig,
	pathless map[string][]config.ProjectSelection,
	evidence []legacyStoredEvidence,
) map[string]*legacySelectedCohort {
	cohorts := map[string]*legacySelectedCohort{}
	for _, storedEvidence := range evidence {
		if !storedEvidence.identity.available() || len(pathless[storedEvidence.row.Harness]) == 0 {
			continue
		}
		key := storedEvidence.identity.String()
		cohort := cohorts[key]
		if cohort == nil {
			cohort = &legacySelectedCohort{identity: storedEvidence.identity}
			cohorts[key] = cohort
		}
		cohort.rows = append(cohort.rows, storedEvidence.row)
	}

	for key, cohort := range cohorts {
		configured := current.Harnesses[cohort.identity.Harness.String()]
		for _, project := range pathless[cohort.identity.Harness.String()] {
			if legacyPathlessProjectMatchesAnyStoredRow(project, cohort.rows) {
				cohort.rules = append(cohort.rules, cloneLegacyProjectSelection(project))
			}
		}
		if len(cohort.rules) == 0 {
			delete(cohorts, key)
			continue
		}
		for _, project := range configured.Projects {
			if len(project.ClonePaths) > 0 && legacyProjectContainsPath(project, cohort.identity.ClonePath.String()) {
				cohort.rules = append(cohort.rules, cloneLegacyProjectSelection(project))
			}
		}
		cohort.branches, cohort.admitted = intersectLegacySelectedBranches(cohort.rules)
	}
	return cohorts
}

func legacyPathlessProjectMatchesAnyStoredRow(project config.ProjectSelection, rows []store.IngestedSessionRow) bool {
	for _, row := range rows {
		if legacyPathlessProjectMatchesStoredRow(project, row) {
			return true
		}
	}
	return false
}

func legacyPathlessProjectMatchesStoredRow(project config.ProjectSelection, row store.IngestedSessionRow) bool {
	configuredRemote := ingest.NormalizeRemoteForMatch(project.GitRemote)
	storedRemote := ingest.NormalizeRemoteForMatch(row.GitRemote)
	if configuredRemote != "" && storedRemote != "" {
		return configuredRemote == storedRemote
	}
	configuredName := ingest.NormalizeProjectNameForMatch(project.Name)
	storedName := ingest.NormalizeProjectNameForMatch(row.CanonicalCwd)
	return configuredName != "" && storedName != "" && configuredName == storedName
}

func legacyProjectContainsPath(project config.ProjectSelection, clonePath string) bool {
	for _, configuredPath := range project.ClonePaths {
		if configuredPath == clonePath {
			return true
		}
	}
	return false
}

func intersectLegacySelectedBranches(rules []config.ProjectSelection) ([]string, bool) {
	var intersection map[string]struct{}
	restricted := false
	for _, rule := range rules {
		if len(rule.Branches) == 0 {
			continue
		}
		branches := make(map[string]struct{}, len(rule.Branches))
		for _, branch := range rule.Branches {
			branches[branch] = struct{}{}
		}
		if !restricted {
			intersection = branches
			restricted = true
			continue
		}
		for branch := range intersection {
			if _, present := branches[branch]; !present {
				delete(intersection, branch)
			}
		}
	}
	if !restricted {
		return nil, true
	}
	if len(intersection) == 0 {
		return nil, false
	}
	branches := make([]string, 0, len(intersection))
	for branch := range intersection {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	return branches, true
}

type legacySelectedCanonicalKey struct {
	kind   legacySelectionGroupKind
	text   string
	policy string
}

type legacySelectedCanonicalGroup struct {
	selection config.ProjectSelection
	paths     map[string]struct{}
}

func buildLegacySelectedCanonicalProjects(
	cohorts map[string]*legacySelectedCohort,
) (map[string][]config.ProjectSelection, map[string]map[string]struct{}) {
	grouped := map[string]map[legacySelectedCanonicalKey]*legacySelectedCanonicalGroup{}
	cohortPaths := map[string]map[string]struct{}{}
	for _, cohort := range cohorts {
		harness := cohort.identity.Harness.String()
		if cohortPaths[harness] == nil {
			cohortPaths[harness] = map[string]struct{}{}
		}
		cohortPaths[harness][cohort.identity.ClonePath.String()] = struct{}{}
		if !cohort.admitted {
			continue
		}

		key, selection := legacySelectedCanonicalIdentity(cohort)
		key.policy = legacySelectedPolicyKey(cohort.branches)
		if grouped[harness] == nil {
			grouped[harness] = map[legacySelectedCanonicalKey]*legacySelectedCanonicalGroup{}
		}
		group := grouped[harness][key]
		if group == nil {
			selection.Branches = cloneLegacyStrings(cohort.branches)
			group = &legacySelectedCanonicalGroup{selection: selection, paths: map[string]struct{}{}}
			grouped[harness][key] = group
		}
		group.paths[cohort.identity.ClonePath.String()] = struct{}{}
	}

	canonical := make(map[string][]config.ProjectSelection, len(grouped))
	for harness, groups := range grouped {
		keys := make([]legacySelectedCanonicalKey, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].kind != keys[j].kind {
				return keys[i].kind < keys[j].kind
			}
			if keys[i].text != keys[j].text {
				return keys[i].text < keys[j].text
			}
			return keys[i].policy < keys[j].policy
		})
		for _, key := range keys {
			group := groups[key]
			paths := make([]string, 0, len(group.paths))
			for path := range group.paths {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			selection := group.selection
			selection.ClonePaths = paths
			canonical[harness] = append(canonical[harness], selection)
		}
	}
	return canonical, cohortPaths
}

func legacySelectedCanonicalIdentity(cohort *legacySelectedCohort) (legacySelectedCanonicalKey, config.ProjectSelection) {
	remotes := map[string]struct{}{}
	names := map[string]struct{}{}
	for _, row := range cohort.rows {
		if remote := ingest.NormalizeRemoteForMatch(row.GitRemote); remote != "" {
			remotes[remote] = struct{}{}
		}
		if name := ingest.NormalizeProjectNameForMatch(row.CanonicalCwd); name != "" {
			names[name] = struct{}{}
		}
	}
	if remote := firstLegacySortedValue(remotes); remote != "" {
		return legacySelectedCanonicalKey{kind: legacySelectionByRemote, text: remote},
			config.ProjectSelection{GitRemote: remote}
	}
	name := firstLegacySortedValue(names)
	return legacySelectedCanonicalKey{
		kind: legacySelectionByPath,
		text: cohort.identity.ClonePath.String(),
	}, config.ProjectSelection{Name: name}
}

func firstLegacySortedValue(values map[string]struct{}) string {
	if len(values) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}
	sort.Strings(sorted)
	return sorted[0]
}

func legacySelectedPolicyKey(branches []string) string {
	if branches == nil {
		return "all"
	}
	var key strings.Builder
	key.WriteString("finite:")
	for _, branch := range branches {
		key.WriteString(strconv.Itoa(len(branch)))
		key.WriteByte(':')
		key.WriteString(branch)
	}
	return key.String()
}

type legacySelectedUnresolved struct {
	harness       string
	sessionID     ingest.SessionID
	row           store.IngestedSessionRow
	rules         []config.ProjectSelection
	contradictory bool
}

func buildLegacySelectedUnresolvedSessions(
	current config.SelectionConfig,
	pathless map[string][]config.ProjectSelection,
	evidence []legacyStoredEvidence,
) map[string][]string {
	unresolved := map[string]*legacySelectedUnresolved{}
	for _, storedEvidence := range evidence {
		if storedEvidence.identity.available() {
			continue
		}
		rules := pathless[storedEvidence.row.Harness]
		if len(rules) == 0 {
			continue
		}
		sessionID, err := ingest.NewSessionID(storedEvidence.row.SessionID)
		if err != nil {
			continue
		}
		key := storedEvidence.row.Harness + "\x00" + sessionID.String()
		aggregate := unresolved[key]
		if aggregate == nil {
			aggregate = &legacySelectedUnresolved{
				harness:   storedEvidence.row.Harness,
				sessionID: sessionID,
				row:       storedEvidence.row,
			}
			unresolved[key] = aggregate
		} else if !sameLegacySelectedUnresolvedCandidate(aggregate.row, storedEvidence.row) {
			aggregate.contradictory = true
		}
		for _, rule := range rules {
			if legacyPathlessProjectMatchesStoredRow(rule, storedEvidence.row) {
				aggregate.rules = appendLegacySelectedSemanticRule(aggregate.rules, rule)
			}
		}
	}

	selected := map[string][]string{}
	for _, aggregate := range unresolved {
		if aggregate.contradictory || len(aggregate.rules) == 0 || !legacySelectedUnresolvedAdmitted(current, aggregate) {
			continue
		}
		selected[aggregate.harness] = append(selected[aggregate.harness], aggregate.sessionID.String())
	}
	for harness := range selected {
		sort.Strings(selected[harness])
	}
	return selected
}

func sameLegacySelectedUnresolvedCandidate(left, right store.IngestedSessionRow) bool {
	return left.Branch == right.Branch &&
		ingest.NormalizeRemoteForMatch(left.GitRemote) == ingest.NormalizeRemoteForMatch(right.GitRemote) &&
		ingest.NormalizeProjectNameForMatch(left.CanonicalCwd) == ingest.NormalizeProjectNameForMatch(right.CanonicalCwd)
}

func appendLegacySelectedSemanticRule(
	rules []config.ProjectSelection,
	incoming config.ProjectSelection,
) []config.ProjectSelection {
	key := legacySelectedPolicyKey(canonicalLegacySelectedBranches(incoming.Branches))
	for _, rule := range rules {
		if legacySelectedPolicyKey(canonicalLegacySelectedBranches(rule.Branches)) == key {
			return rules
		}
	}
	return append(rules, cloneLegacyProjectSelection(incoming))
}

func canonicalLegacySelectedBranches(branches []string) []string {
	if len(branches) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		set[branch] = struct{}{}
	}
	canonical := make([]string, 0, len(set))
	for branch := range set {
		canonical = append(canonical, branch)
	}
	sort.Strings(canonical)
	return canonical
}

func legacySelectedUnresolvedAdmitted(
	current config.SelectionConfig,
	aggregate *legacySelectedUnresolved,
) bool {
	const syntheticProjectName = "stored-legacy-migration-candidate"
	projects := make([]config.ProjectSelection, 0, len(aggregate.rules))
	for _, rule := range aggregate.rules {
		projects = append(projects, config.ProjectSelection{
			Name:     syntheticProjectName,
			Branches: canonicalLegacySelectedBranches(rule.Branches),
		})
	}
	configured := current.Harnesses[aggregate.harness]
	temporary := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			aggregate.harness: {
				Projects:   projects,
				Exclusions: cloneLegacyExclusions(configured.Exclusions),
			},
		},
	}
	matcher := config.CompileSelectionMatcher(temporary)
	return matcher.MatchBranchCandidate(ingest.DiscoveryCandidate{
		Harness:            ingest.Harness(aggregate.harness),
		ProjectName:        syntheticProjectName,
		Branch:             aggregate.row.Branch,
		SessionID:          aggregate.sessionID,
		RemoteMultiplicity: ingest.DiscoveryIdentityUnique,
		NameMultiplicity:   ingest.DiscoveryIdentityUnique,
	}) == ingest.BranchMatchYes
}

func rebuildLegacySelectedHarness(
	configured config.SelectionHarnessConfig,
	canonical []config.ProjectSelection,
	cohortPaths map[string]struct{},
	newSessions []string,
) config.SelectionHarnessConfig {
	rebuilt := config.SelectionHarnessConfig{
		Sessions:   cloneLegacyStrings(configured.Sessions),
		Exclusions: cloneLegacyExclusions(configured.Exclusions),
	}
	inserted := false
	insertCanonical := func() {
		if inserted {
			return
		}
		for _, project := range canonical {
			rebuilt.Projects = append(rebuilt.Projects, cloneLegacyProjectSelection(project))
		}
		inserted = true
	}

	for _, project := range configured.Projects {
		if len(project.ClonePaths) == 0 {
			insertCanonical()
			continue
		}
		residual := make([]string, 0, len(project.ClonePaths))
		touched := false
		for _, clonePath := range project.ClonePaths {
			if _, migrated := cohortPaths[clonePath]; migrated {
				touched = true
				continue
			}
			residual = append(residual, clonePath)
		}
		if !touched {
			rebuilt.Projects = append(rebuilt.Projects, cloneLegacyProjectSelection(project))
			continue
		}
		if len(residual) > 0 {
			copy := cloneLegacyProjectSelection(project)
			copy.ClonePaths = residual
			rebuilt.Projects = append(rebuilt.Projects, copy)
		}
		insertCanonical()
	}
	insertCanonical()
	if len(rebuilt.Projects) == 0 {
		rebuilt.Projects = nil
	}
	for _, sessionID := range newSessions {
		rebuilt.Sessions = appendLegacyUniqueString(rebuilt.Sessions, sessionID)
	}
	return rebuilt
}

func cloneLegacySelectionConfig(source config.SelectionConfig) config.SelectionConfig {
	clone := source
	clone.Harnesses = cloneLegacyHarnesses(source.Harnesses)
	clone.DeprecatedProviders = cloneLegacyHarnesses(source.DeprecatedProviders)
	return clone
}

func cloneLegacyHarnesses(source map[string]config.SelectionHarnessConfig) map[string]config.SelectionHarnessConfig {
	if source == nil {
		return nil
	}
	clone := make(map[string]config.SelectionHarnessConfig, len(source))
	for harness, configured := range source {
		copy := config.SelectionHarnessConfig{
			Projects:   cloneLegacyProjects(configured.Projects),
			Sessions:   cloneLegacyStrings(configured.Sessions),
			Exclusions: cloneLegacyExclusions(configured.Exclusions),
		}
		clone[harness] = copy
	}
	return clone
}

func cloneLegacyProjects(source []config.ProjectSelection) []config.ProjectSelection {
	if source == nil {
		return nil
	}
	clone := make([]config.ProjectSelection, len(source))
	for index, project := range source {
		clone[index] = cloneLegacyProjectSelection(project)
	}
	return clone
}

func cloneLegacyProjectSelection(project config.ProjectSelection) config.ProjectSelection {
	project.ClonePaths = cloneLegacyStrings(project.ClonePaths)
	project.Branches = cloneLegacyStrings(project.Branches)
	return project
}

func cloneLegacyExclusions(source config.SelectionExclusions) config.SelectionExclusions {
	clone := config.SelectionExclusions{Sessions: cloneLegacyStrings(source.Sessions)}
	if source.Branches != nil {
		clone.Branches = make([]config.BranchExclusion, len(source.Branches))
		for index, exclusion := range source.Branches {
			clone.Branches[index] = exclusion
			clone.Branches[index].Branches = cloneLegacyStrings(exclusion.Branches)
		}
	}
	return clone
}

func cloneLegacyStrings(source []string) []string {
	if source == nil {
		return nil
	}
	return append([]string{}, source...)
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
		if len(project.ClonePaths) == 1 && ingest.ClonePath(project.ClonePaths[0]) == clonePath {
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
