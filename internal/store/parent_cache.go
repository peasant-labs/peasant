package store

import (
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
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

// startedByTarget is the ONE in-memory spelling of the parent-answer rule: the
// first started_by relationship whose target state is known or
// target_known_retained and whose target local identifier is non-nil and
// non-empty. Its only SQL mirror is LogicalParentSQL (parent_reader.go); the
// validator layer keeps its own pinned copy in
// indexformat.durableStartedByTarget because indexformat sits below store and
// cannot import it.
//
// It is used only where the relationships are already in memory: the activation
// value computation, the standalone cache writer, and the served-generation
// document derivation. It never reads the availability cache.
func startedByTarget(relationships []schema.SessionRelationship) *schema.SessionID {
	for i := range relationships {
		relationship := relationships[i]
		if relationship.Kind != schema.SessionRelationshipStartedBy {
			continue
		}
		if relationship.TargetState == schema.RelationshipTargetKnown || relationship.TargetState == schema.RelationshipTargetKnownRetained {
			if relationship.TargetLocalID != nil && *relationship.TargetLocalID != "" {
				target := *relationship.TargetLocalID
				return &target
			}
		}
	}
	return nil
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
