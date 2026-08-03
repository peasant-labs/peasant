package codemap

import "github.com/peasant-labs/schema"

// SelectionState reports whether the persisted kickstart selection is
// narrowing what ProjectSummaries returns, and by how much, WITHOUT
// revealing which specific projects or sessions it hides — selection
// scoping is a discovery/list boundary, not an access-control one (see the
// polyrepo AGENTS.md "Kickstart selection" invariant). A sparse or
// single-row picker must be explainable instead of looking broken
// after project-selection usability testing.
//
// Counts come from the exact same per-project visibility pass
// ProjectSummaries already performs to build Projects, so they cannot drift
// from what the list itself shows.
type SelectionState struct {
	// Active is true when the persisted selection mode is "selected"
	// (narrowing discovery), false under mode "all".
	Active bool `json:"active"`
	// HiddenProjects counts projects the store knows about that contributed
	// zero visible sessions, so they produced no picker row at all.
	HiddenProjects int `json:"hiddenProjects"`
	// HiddenSessions counts sessions across every project (whether or not
	// the project itself produced a visible row) that the selection hides.
	HiddenSessions int `json:"hiddenSessions"`
}

// ProjectSummariesResult is ProjectSummaries' return value: the home-picker
// rows plus selection-state metadata. Selection is a peasant-local
// same-process UX affordance, not part of the schema module's cross-backend
// wire contract, so it is not a schema.* type.
type ProjectSummariesResult struct {
	Projects  []schema.ProjectSummary `json:"projects"`
	Selection SelectionState          `json:"selection"`
}
