package ingest

// LocateOutcome records what LOCATE found for one cohort member. A miss is not
// an outcome: a miss is a ScopeMiss inside SessionScopeError, and no member
// exists for it.
type LocateOutcome uint8

const (
	locateOutcomeUnset LocateOutcome = iota // invalid
	// LocateStoredPath means the session was re-read from the source path the
	// store holds.
	LocateStoredPath
	// LocateDirectLookup means the session was found by name through the
	// harness lookup.
	LocateDirectLookup
	// LocateStoredUnreadable means the session is recorded, but its source
	// cannot be read again. The transcript, its index and its links are not
	// recomputed from a source; the store-side upkeep a full run does for the
	// row still runs, and the run continues.
	LocateStoredUnreadable
)

// IsValid reports whether the outcome is one of the declared values.
func (o LocateOutcome) IsValid() bool {
	switch o {
	case LocateStoredPath, LocateDirectLookup, LocateStoredUnreadable:
		return true
	default:
		return false
	}
}

// located reports whether LOCATE built a source for the member, which is what
// DIFF through INDEX may work on.
func (o LocateOutcome) located() bool {
	return o == LocateStoredPath || o == LocateDirectLookup
}

// UnreadableReason says why a stored session could not be read again. It is set
// only with LocateStoredUnreadable.
type UnreadableReason uint8

const (
	unreadableReasonUnset UnreadableReason = iota // invalid
	// UnreadableSourceGone means the stored path no longer holds this session.
	UnreadableSourceGone
	// UnreadableHarnessDisabled means the harness that recorded the session is
	// not enabled.
	UnreadableHarnessDisabled
	// UnreadableSourceFailed means the source exists but reading it failed (for
	// example an OpenCode database that does not open). Detail holds the first
	// error line. A full run treats this as a local failure and continues.
	UnreadableSourceFailed
)

// IsValid reports whether the reason is one of the declared values.
func (r UnreadableReason) IsValid() bool {
	switch r {
	case UnreadableSourceGone, UnreadableHarnessDisabled, UnreadableSourceFailed:
		return true
	default:
		return false
	}
}

// String returns a short name for the reason, for logs and reports.
func (r UnreadableReason) String() string {
	switch r {
	case UnreadableSourceGone:
		return "source gone"
	case UnreadableHarnessDisabled:
		return "harness disabled"
	case UnreadableSourceFailed:
		return "source failed"
	default:
		return "unknown"
	}
}

// CohortRole says why a member is in the cohort.
type CohortRole uint8

const (
	cohortRoleUnset CohortRole = iota // invalid
	// CohortSelected is a session named by --session.
	CohortSelected
	// CohortChild is a child or subagent of a cohort member, admitted by
	// RelatedInclude.
	CohortChild
	// CohortAncestor is an unstored parent, grandparent, or higher ancestor,
	// admitted by RelatedInclude.
	CohortAncestor
	// CohortContext is read only so a selected session's own fields and links
	// equal a full run's: a stored layout parent, a Claude native-agent owner,
	// and under RelatedExclude the children the build enumerates. A context
	// member is never processed and never part of the upkeep set.
	CohortContext
)

// IsValid reports whether the role is one of the declared values.
func (r CohortRole) IsValid() bool {
	switch r {
	case CohortSelected, CohortChild, CohortAncestor, CohortContext:
		return true
	default:
		return false
	}
}

// LocatedMember is one member LOCATE built.
type LocatedMember struct {
	ID      SessionID
	Harness Harness
	// Source is the discovered source. It is nil exactly when Outcome is
	// LocateStoredUnreadable.
	Source *DiscoveredSession
	// Outcome records how the member was located. A miss has no member.
	Outcome LocateOutcome
	Role    CohortRole
	// Unreadable is set exactly with LocateStoredUnreadable.
	Unreadable UnreadableReason
	// Detail is set only with UnreadableSourceFailed: the first error line.
	Detail string
	// ParentProbe says where one unstored ancestor can be, computed from the
	// child's own source. nil means this harness cannot derive where its
	// parent is.
	ParentProbe *AncestorProbe
}

// ScopeReport records what a scoped run located. A scoped run reports it on
// PipelineResult under a field tagged json:"-", so `harvest --format json`
// keeps exactly today's fields (a scoped run's counts cover only its cohort, so
// the bytes differ); that field lands with the scoped path, and this type is
// the report it carries. The report is not printed: tests read it, and the
// stored-but-unreadable warning is rendered from it.
//
// Members holds at most one member per id. Processed and Upkeep collapse a
// repeated id in case a caller violates that, because DIFF through INDEX must
// never see the same session twice from one report.
type ScopeReport struct {
	// Members holds the selected members first, then children, ancestors, and
	// context members.
	Members []LocatedMember
}

// Processed returns the ids DIFF through INDEX may work on: the members
// LOCATE built a source for. A context member is never processed.
func (r ScopeReport) Processed() []SessionID {
	var ids []SessionID
	seen := make(map[SessionID]struct{}, len(r.Members))
	for _, member := range r.Members {
		if member.Role == CohortContext || !member.Outcome.located() {
			continue
		}
		if _, duplicate := seen[member.ID]; duplicate {
			continue
		}
		seen[member.ID] = struct{}{}
		ids = append(ids, member.ID)
	}
	return ids
}

// Upkeep returns the ids the store-side passes may touch, and it is the run's
// in-memory gate. It is Processed plus every LocateStoredUnreadable member
// whose role is selected or child: a stored-but-unreadable selected session
// still owes the upkeep a full run does for its row, and so do its unreadable
// subagents. An ancestor is unstored by definition and cannot be unreadable, so
// it never joins on that arm. Processed is always a subset. Context members are
// in neither set.
func (r ScopeReport) Upkeep() []SessionID {
	var ids []SessionID
	seen := make(map[SessionID]struct{}, len(r.Members))
	for _, member := range r.Members {
		var included bool
		switch {
		case member.Role == CohortContext:
			included = false
		case member.Outcome.located():
			included = true
		case member.Outcome == LocateStoredUnreadable &&
			(member.Role == CohortSelected || member.Role == CohortChild):
			included = true
		}
		if !included {
			continue
		}
		if _, duplicate := seen[member.ID]; duplicate {
			continue
		}
		seen[member.ID] = struct{}{}
		ids = append(ids, member.ID)
	}
	return ids
}
