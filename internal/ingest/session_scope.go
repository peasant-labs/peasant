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
		return SessionScope{}, scopeUnsetPolicyError(scopeConstructorRefusal)
	}
	if len(ids) == 0 {
		return SessionScope{}, scopeNoIDsError(scopeConstructorRefusal)
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

// The two unusable-scope rules are stated once, here, and used by both
// boundaries that can meet them: the constructor, and the run's refusal of a
// scope a caller built by hand. Only the clauses a boundary can state
// truthfully differ: where it was met, when, and what it prevented.
type scopeRefusal struct {
	where  string
	when   string
	impact string
}

var (
	// scopeConstructorRefusal is NewSessionScope: no pipeline runs, so what the
	// caller loses is the scope itself.
	scopeConstructorRefusal = scopeRefusal{
		where:  "ingest.NewSessionScope in internal/ingest/session_scope.go",
		when:   "while building the scope, before any run exists",
		impact: "no scope is returned, so nothing can be read until the input is fixed",
	}
	// scopeRunRefusal is Pipeline.Run's check of a scope a caller built by hand.
	scopeRunRefusal = scopeRefusal{
		where:  "ingest.PipelineConfig.Scope, checked at the start of Pipeline.Run in internal/ingest/pipeline.go",
		when:   "before the run lock and before any stage",
		impact: "no source was read and no session or store row was touched",
	}
)

// scopeNoIDsError is the one statement of "a scope must name a session".
func scopeNoIDsError(refusal scopeRefusal) error {
	return fmt.Errorf("ingest: the session scope names no sessions; a scope with no ids would select nothing and must not exist, because a nil scope already means the full run; where: %s; when: %s; impact: %s; fix: pass at least one --session id, or leave the scope nil for a full run", refusal.where, refusal.when, refusal.impact)
}

// scopeUnsetPolicyError is the one statement of "a scope must carry a policy".
func scopeUnsetPolicyError(refusal scopeRefusal) error {
	return fmt.Errorf("ingest: the session scope carries no related policy; a scope must say whether a selected session's children and unstored ancestors join the cohort, and the zero value is invalid so a forgotten argument cannot silently pick one; where: %s; when: %s; impact: %s; fix: pass RelatedInclude (the default) or RelatedExclude (--no-related)", refusal.where, refusal.when, refusal.impact)
}

// validateScope refuses a pipeline configuration whose scope cannot narrow a
// run or whose two narrowing mechanisms disagree. It runs before the first
// stage, so a refused run touches neither the store nor a source.
func (c *PipelineConfig) validateScope() error {
	if c.Scope == nil {
		return nil
	}
	if len(c.Scope.ids) == 0 {
		return scopeNoIDsError(scopeRunRefusal)
	}
	if !c.Scope.related.IsValid() {
		return scopeUnsetPolicyError(scopeRunRefusal)
	}
	if c.AllowedSessionIDs != nil {
		return fmt.Errorf(
			"ingest: PipelineConfig sets both Scope and AllowedSessionIDs; the scope already selects this run's cohort, and a second allowlist would filter the located members by an unrelated set, so the two narrowings can disagree; where: %s; when: %s; impact: %s; fix: pass --session (Scope) or an allowlist, never both",
			scopeRunRefusal.where, scopeRunRefusal.when, scopeRunRefusal.impact)
	}
	if c.Reindex && c.Scope != nil {
		// The reindex path rebuilds from saved pairs and never locates a
		// cohort, so a scope there would be silently ignored: the user would
		// get a full reindex while believing it was narrowed. Refuse before the
		// run lock; the scoped reindex is not implemented yet.
		return fmt.Errorf(
			"ingest: PipelineConfig sets both Reindex and Scope; the reindex path scans the saved output tree and never locates the scope's cohort, so the scope would be silently ignored and the whole output tree rebuilt; where: ingest.PipelineConfig.Reindex with .Scope, checked at the start of Pipeline.Run in internal/ingest/pipeline.go; when: before the run lock and before any stage; impact: no source was read, no output pair was rebuilt, and no store row was touched; fix: drop Reindex to harvest the named sessions, or drop the scope to rebuild the whole output tree")
	}
	return nil
}
