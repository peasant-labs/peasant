package ingest

import (
	"fmt"
	"slices"
)

// RelatedPolicy says whether a scoped run follows a selected session's
// related sessions. Its zero value is invalid, so an unset policy is refused
// at the boundary instead of quietly behaving like one of the two real
// choices.
type RelatedPolicy uint8

const (
	relatedPolicyUnset RelatedPolicy = iota // invalid
	// RelatedInclude is the default: selected sessions plus their children
	// and any unstored ancestors.
	RelatedInclude
	// RelatedExclude is --no-related: selected sessions only.
	RelatedExclude
)

// IsValid reports whether the policy is one of the declared choices.
func (p RelatedPolicy) IsValid() bool {
	switch p {
	case RelatedInclude, RelatedExclude:
		return true
	default:
		return false
	}
}

// SessionScope narrows a run to named sessions. A nil *SessionScope in
// PipelineConfig is the full run.
//
// Build a scope only with NewSessionScope. Pipeline.Run refuses a scope with
// no ids or an unset policy, because a caller can still write the zero value
// (&SessionScope{}) or forget the policy, and either one would otherwise
// select work the user never asked for.
type SessionScope struct {
	ids     []SessionID // sorted, deduplicated, never empty
	related RelatedPolicy
}

// NewSessionScope validates the boundary input and returns a scope whose ids
// are sorted and deduplicated. The scope owns its own copy of ids, so a later
// change to the caller's slice cannot change what the scope names.
func NewSessionScope(ids []SessionID, related RelatedPolicy) (SessionScope, error) {
	if !related.IsValid() {
		return SessionScope{}, fmt.Errorf(
			"ingest: NewSessionScope was given an unset related policy; a scope must say whether a selected session's children and unstored ancestors join the cohort, and the zero value is invalid so that a forgotten argument cannot silently pick one; where: ingest.NewSessionScope in internal/ingest/session_scope.go; when: building the scope before a run; impact: no scope is returned, so the caller must fix the policy rather than run with a guess; fix: pass RelatedInclude (the default) or RelatedExclude (--no-related)")
	}
	if len(ids) == 0 {
		return SessionScope{}, fmt.Errorf(
			"ingest: NewSessionScope was given no session ids; a scope with no ids would record nothing, and the empty scope must not exist because a nil scope already means the full run; where: ingest.NewSessionScope in internal/ingest/session_scope.go; when: building the scope before a run; impact: no scope is returned and no session is selected; fix: pass at least one id from --session, or leave the scope nil for a full run")
	}

	unique := make([]SessionID, 0, len(ids))
	seen := make(map[SessionID]struct{}, len(ids))
	for position, id := range ids {
		if id == "" {
			return SessionScope{}, fmt.Errorf(
				"ingest: NewSessionScope was given an empty session id at position %d of %d; an empty id names no session, so it can never be located and would fail the run after selection; where: ingest.NewSessionScope in internal/ingest/session_scope.go; when: reading the --session ids; impact: no scope is returned; fix: drop the empty id and pass only the ids the user named", position, len(ids))
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	slices.Sort(unique)
	return SessionScope{ids: unique, related: related}, nil
}

// IDs returns the selected session ids in sorted order. The result is a copy:
// mutating it never changes the scope.
func (s SessionScope) IDs() []SessionID {
	return slices.Clone(s.ids)
}

// Related returns the scope's related-session policy.
func (s SessionScope) Related() RelatedPolicy { return s.related }

// valid reports whether the scope can narrow a run. It is the defensive check
// Pipeline.Run applies to a scope a caller built without NewSessionScope.
func (s SessionScope) valid() bool {
	return len(s.ids) > 0 && s.related.IsValid()
}

// validateScope refuses a pipeline configuration whose scope cannot narrow a
// run or whose two narrowing mechanisms disagree. It runs before the first
// stage, so a refused run touches neither the store nor a source.
func (c *PipelineConfig) validateScope() error {
	if c.Scope == nil {
		return nil
	}
	if len(c.Scope.ids) == 0 {
		return fmt.Errorf(
			"ingest: Pipeline.Run was given a session scope that names no sessions; the run would record nothing, and a scope must be built by NewSessionScope from the ids the user passed; where: ingest.PipelineConfig.Scope, checked at the start of Pipeline.Run in internal/ingest/pipeline.go; when: before the run lock and before any stage; impact: no source was read and no session or store row was touched; fix: pass at least one --session id, or set the scope to nil for a full run")
	}
	if !c.Scope.related.IsValid() {
		return fmt.Errorf(
			"ingest: Pipeline.Run was given a session scope with no related policy; a scope must say whether children and unstored ancestors join the cohort, and the zero value is invalid so that a forgotten argument cannot silently pick one; where: ingest.PipelineConfig.Scope, checked at the start of Pipeline.Run in internal/ingest/pipeline.go; when: before the run lock and before any stage; impact: no source was read and no session or store row was touched; fix: build the scope with NewSessionScope and pass RelatedInclude (the default) or RelatedExclude (--no-related)")
	}
	if c.AllowedSessionIDs != nil {
		return fmt.Errorf(
			"ingest: PipelineConfig sets both Scope and AllowedSessionIDs; the scope already selects this run's cohort, and a second allowlist would filter the located members by an unrelated set, so the two narrowings can disagree; where: ingest.PipelineConfig.Scope and AllowedSessionIDs, checked at the start of Pipeline.Run in internal/ingest/pipeline.go; when: before the run lock and before any stage; impact: no source was read and no session or store row was touched; fix: pass --session (Scope) or an allowlist, never both")
	}
	return nil
}
