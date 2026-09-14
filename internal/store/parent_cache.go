package store

import (
	"github.com/peasant-labs/peasant/internal/ingest"
)

// Parent-cache resolution for independently admitted children.
//
// sessions.parent_id is a nullable FK availability cache, not the durable
// logical evidence. The logical parent is the durable started_by relationship
// where present, otherwise the legacy ParentUUID carried in the managed
// metadata. A nil cache never proves a root session or an operator session,
// and it never hides a known relationship: readers consult the logical
// evidence, not the accidental nil.
//
// The activation transaction owns the atomic cache write. These helpers compute
// the desired cache value so the pipeline, the writer, and later backfill agree
// without a competing transaction.

// DesiredParentCacheID resolves the FK-safe parent cache for one child.
//
// It returns the logical parent when the target is available and acyclic, and
// nil otherwise (missing, unselected, unavailable, self-parent, or cycle).
// Callers pass parentInBatch or parentStored for availability; both mean the
// target row will exist when the child row commits.
func DesiredParentCacheID(child ingest.SessionID, logicalParent *ingest.SessionID, parentAvailable bool) *ingest.SessionID {
	if logicalParent == nil {
		return nil
	}
	if *logicalParent == child {
		return nil
	}
	if !parentAvailable {
		return nil
	}
	cp := *logicalParent
	return &cp
}

// BatchParentCycles returns the session IDs whose logical parent edge
// participates in an in-batch cycle. Members of a cycle keep their logical
// evidence but receive a nil scheduling and writer cache so every admitted
// child drains and stores.
func BatchParentCycles(ids []ingest.SessionID, parentOf map[ingest.SessionID]*ingest.SessionID) map[ingest.SessionID]bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[ingest.SessionID]int, len(ids))
	cyclic := make(map[ingest.SessionID]bool)
	var stack []ingest.SessionID
	var visit func(id ingest.SessionID)
	visit = func(id ingest.SessionID) {
		color[id] = gray
		stack = append(stack, id)
		if parent, ok := parentOf[id]; ok && parent != nil {
			switch color[*parent] {
			case gray:
				for i := len(stack) - 1; i >= 0; i-- {
					cyclic[stack[i]] = true
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
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return cyclic
}
