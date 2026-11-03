package store

import "encoding/json"

// SearchFilters narrows transcript search before ranking and pagination. The zero
// value applies no constraints. Search reports entries, including child sessions;
// it does not apply the session list's MainOnly restriction.
type SearchFilters struct {
	ProjectName   *string
	Harness       *string
	StartFromMs   *int64 // inclusive
	StartBeforeMs *int64 // exclusive
	Tag           *string
	SessionIDs    []string // exact set; empty means unconstrained
}

// IsZero reports whether no session constraint is set.
func (f SearchFilters) IsZero() bool {
	return f.ProjectName == nil &&
		f.Harness == nil &&
		f.StartFromMs == nil &&
		f.StartBeforeMs == nil &&
		f.Tag == nil &&
		len(f.SessionIDs) == 0
}

func (f SearchFilters) where() (conditions []string, args []any) {
	conditions, args = buildSessionListFilterWhere(SessionListFilter{
		SessionFilter: SessionFilter{
			ModelHarness: f.Harness,
			StartFrom:    f.StartFromMs,
			StartBefore:  f.StartBeforeMs,
		},
		ProjectName: f.ProjectName,
		Tag:         f.Tag,
	})
	if len(f.SessionIDs) > 0 {
		// A string slice always marshals successfully. One JSON bind avoids the
		// SQLite variable ceiling regardless of the number of selected sessions.
		ids, _ := json.Marshal(f.SessionIDs)
		conditions = append(conditions, "s.session_id IN (SELECT value FROM json_each(?))")
		args = append(args, string(ids))
	}
	return conditions, args
}
