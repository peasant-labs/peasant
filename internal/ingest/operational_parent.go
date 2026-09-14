package ingest

import (
	"context"
	"errors"
	"io/fs"
)

// Independent child admission and operational parent scheduling.
//
// A child session receives its own selection decision. No parent is rescued
// into the admitted cohort because a child exists, and no sibling or parent is
// widened into visibility because a child was selected. SchedulingParentID
// carries the local operational edge used by FILTER topology, root dispatch,
// staging, and the output path. ParentUUID keeps the logical evidence and is
// never used as the scheduling gate for Codex and OpenCode sessions.

// IndependentAdmissionHarness reports whether a harness uses independent child
// admission. Codex and OpenCode children are admitted on their own selection
// evidence; every other harness keeps its existing parent-inheritance path.
func IndependentAdmissionHarness(h Harness) bool {
	return h == HarnessCodex || h == HarnessOpenCode
}

// OperationalParentID returns the scheduling edge for one admitted entry.
// It is nil for operational roots. Callers must not substitute ParentUUID.
func OperationalParentID(session DiscoveredSession) *SessionID {
	return session.SchedulingParentID
}

// storeLookupReasonCode classifies a stored scheduling-parent lookup failure
// into a bounded, safe reason code.
//
// The raw dependency error can embed a private filesystem path or a stored
// value, so it is never logged, wrapped into the message, or rendered into a
// diagnostic. Only this closed-set code names why the lookup failed; the
// caller keeps the operation, effect and recovery wording.
func storeLookupReasonCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	default:
		return "unavailable"
	}
}

// BuildSchedulingParents derives the operational edge for every admitted entry.
//
// The tentative edge is the logical ParentUUID when the parent is an available
// processing target:
//
//   - the parent is in the admitted cohort, or
//   - the parent is an independently admitted harness whose target already
//     committed to the store, so the child keeps its stable nested location
//     and FK cache instead of being re-homed as an orphan.
//
// A missing, unselected, or unavailable parent yields no edge. Self-parent
// edges yield no edge. Internal cycle edges are removed while the logical
// evidence is retained, so every member of a cycle becomes an independent
// dispatch root and an admitted child always drains.
//
// Other harnesses keep their existing scheduling shape: their edge mirrors the
// logical parent only when it is in the admitted cohort because their
// admission path still inherits the parent decision and the staging gate falls
// back to the logical evidence for an already-stored parent.
func BuildSchedulingParents(entries []DiffEntry, parentStored map[SessionID]bool) map[SessionID]*SessionID {
	byID := make(map[SessionID]int, len(entries))
	for i := range entries {
		byID[entries[i].Session.SessionID] = i
	}
	edges := make(map[SessionID]*SessionID, len(entries))
	for i := range entries {
		session := entries[i].Session
		if session.ParentUUID == nil {
			edges[session.SessionID] = nil
			continue
		}
		parent := *session.ParentUUID
		if parent == session.SessionID {
			edges[session.SessionID] = nil
			continue
		}
		if _, inCohort := byID[parent]; !inCohort {
			// A parent outside the admitted cohort is still an available
			// acyclic target for an independently admitted child when its row
			// already exists in the committed store. Every other case has no
			// operational edge; the logical evidence stays on the entry.
			if !IndependentAdmissionHarness(session.Harness) || !parentStored[parent] {
				edges[session.SessionID] = nil
				continue
			}
		}
		cp := parent
		edges[session.SessionID] = &cp
	}
	breakSchedulingCycles(entries, edges)
	return edges
}

// breakSchedulingCycles removes every edge that participates in an operational
// cycle. Members of a cycle keep their logical ParentUUID but lose only the
// scheduling edge, so each becomes a dispatch root and no admitted child waits
// on a parent that waits on it.
func breakSchedulingCycles(entries []DiffEntry, edges map[SessionID]*SessionID) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[SessionID]int, len(entries))
	var stack []SessionID
	var visit func(id SessionID)
	visit = func(id SessionID) {
		color[id] = gray
		stack = append(stack, id)
		parent := edges[id]
		if parent != nil {
			switch color[*parent] {
			case gray:
				for i := len(stack) - 1; i >= 0; i-- {
					edges[stack[i]] = nil
					if stack[i] == *parent {
						break
					}
				}
			case white:
				visit(*parent)
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	for i := range entries {
		id := entries[i].Session.SessionID
		if color[id] == white {
			visit(id)
		}
	}
}

// ApplySchedulingParents writes the derived operational edges back onto the
// admitted entries. It returns the same slice for chaining. parentStored names
// the out-of-cohort logical parents whose rows already exist in the store.
func ApplySchedulingParents(entries []DiffEntry, parentStored map[SessionID]bool) []DiffEntry {
	edges := BuildSchedulingParents(entries, parentStored)
	for i := range entries {
		entries[i].Session.SchedulingParentID = edges[entries[i].Session.SessionID]
	}
	return entries
}
