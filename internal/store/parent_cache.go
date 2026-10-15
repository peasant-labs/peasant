package store

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
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
//
// THE COMPLETE SET OF STATEMENTS THAT SET sessions.parent_id IS EXACTLY FOUR:
//
//  1. sqlInsertSession (writer.go) — the metadata upsert. Unchanged in every
//     input, guard and branch: its value is the session's logical ParentUUID or
//     the resolved scheduling edge, it refuses in-batch cycle members, it skips
//     a non-independent-harness orphan whose parent is absent, and its conflict
//     branch keys on parentAuthoritative. It is NOT routed through
//     startedByTarget/DesiredParentCacheID: V1 lanes carry only ParentUUID and
//     no relationships, so that predicate answers nil for them.
//  2. sqlInsertSessionBeforeSourceFingerprint (writer.go) — the same upsert
//     selected when the row has no source_fingerprint column. Also unchanged.
//  3. the generation pointing UPDATE in pointSessionAtGenerationOnConn
//     (index_format_v2.go). Its value computation re-expresses through
//     startedByTarget + DesiredParentCacheID, with the FK-existence check as
//     the availability input.
//  4. writeParentCacheOnConn (this file) — the one standalone NULL-only writer,
//     used by the migration conversion and the reverse reconciliation heal. It
//     moves the heal and the origin requeue in ONE statement: the same UPDATE
//     that sets the cache resets the row's origin watermark, so a repaired link
//     is always re-judged by the next origin resolve pass.
//
// No fifth statement computes the column. Statements 1 and 2 are listed so the
// set is exact; they are deliberately not re-expressed.

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

// writeParentCacheOnConn applies the one standalone cache write for one child:
// it resolves the FK-safe value with DesiredParentCacheID (the parent-existence
// check is the availability input) and applies it NULL-ONLY, so a populated
// cache is never overwritten from this path.
//
// The heal and the origin requeue move in ONE statement: the same UPDATE resets
// origin_version to 0, so no child is ever observable with a repaired parent
// link still carrying the origin verdict judged before the link existed, and
// the next resolve pass re-judges it against its current parent.
//
// The NULL-only predicate is an ADDITIONAL invariant, not a replacement for a
// caller's own guards: the reverse reconciliation decides NULL-only in Go and
// keeps its parent-existence and cycle refusals, and the migration conversion
// heals only a linkage the row never captured.
func writeParentCacheOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, logicalParent *ingest.SessionID) error {
	if logicalParent == nil {
		return nil
	}
	parentAvailable := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(*logicalParent)},
		ResultFunc: func(*sqlite.Stmt) error { parentAvailable = true; return nil },
	}); err != nil {
		return fmt.Errorf("store: check whether the logical parent %s of session %s is stored before writing the parent cache: %w; the availability cache was left unchanged; restore database access and retry", *logicalParent, sessionID, err)
	}
	cache := DesiredParentCacheID(sessionID, logicalParent, parentAvailable)
	if cache == nil {
		return nil
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET parent_id = ?, origin_version = 0 WHERE session_id = ? AND parent_id IS NULL`, &sqlitex.ExecOptions{
		Args: []any{string(*cache), string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: write the parent cache of session %s from its logical parent %s: %w; the availability cache was left unchanged; restore database access and retry", sessionID, *logicalParent, err)
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
