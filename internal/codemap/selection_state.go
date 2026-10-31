package codemap

import "github.com/peasant-labs/schema"

// SelectionState is a retained compatibility carrier for the home-picker
// payload. Browsing applies NO saved-selection filter: the selection only
// controls what Peasant ingests next, so no stored project or session is hidden
// from this list by it. Active is therefore always false and both hidden counts
// are always zero.
//
// The counts describe browsing only. Ingest and push metadata elsewhere may
// still report that a saved selection excludes a session; that is a statement
// about future publication scope, never a reason to omit an already-ingested
// row from the viewer.
type SelectionState struct {
	// Active is always false: no browsing surface applies the saved selection.
	Active bool `json:"active"`
	// HiddenProjects is always zero: no stored project is hidden by selection.
	HiddenProjects int `json:"hiddenProjects"`
	// HiddenSessions is always zero: no stored session is hidden by selection.
	HiddenSessions int `json:"hiddenSessions"`
}

// ProjectSummariesResult is ProjectSummaries' return value: the home-picker
// rows plus retained selection-state metadata. Selection is a peasant-local
// same-process UX shape, not part of the schema module's cross-backend wire
// contract, so it is not a schema.* type.
type ProjectSummariesResult struct {
	Projects  []schema.ProjectSummary `json:"projects"`
	Selection SelectionState          `json:"selection"`
}
