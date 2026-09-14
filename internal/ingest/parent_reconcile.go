package ingest

import (
	"context"
	"log/slog"
	"sort"
)

// Reverse logical-target cache reconciliation for independently admitted
// children.
//
// An independently admitted child admitted while its logical parent was
// missing, unselected or otherwise unavailable is stored with a nil FK
// availability cache while its managed metadata keeps the logical ParentUUID.
// When a later harvest makes that logical parent available, this pass heals the
// cache. It reconciles only children whose PERSISTED logical-parent evidence
// names a parent this harvest discovered, and the store reads that evidence
// from the database: no managed metadata file of an unrelated stored root is
// opened, and the pass never re-extracts, relocates, re-indexes or re-selects a
// child.
//
// The target set is the independently admitted session ids this harvest
// discovered, whether they were just stored or were already stored. Naming an
// already-stored parent is deliberate: a reconciliation that failed
// transiently on the harvest that first made the parent available is retried on
// the next harvest that discovers the same parent, without waiting for another
// new session. The store refuses any update that would close a parent-cache
// cycle. A store without the reconciliation capability keeps the cache
// unchanged.
func (p *Pipeline) reconcileOrphanParentCaches(ctx context.Context, discovered, written []DiffEntry) {
	if p.store == nil || p.config.DryRun {
		return
	}
	reconciler, ok := p.store.(OrphanParentReconciler)
	if !ok {
		return
	}
	targets := make([]SessionID, 0, len(discovered))
	seen := make(map[SessionID]struct{}, len(discovered))
	for i := range discovered {
		if !IndependentAdmissionHarness(discovered[i].Session.Harness) {
			continue
		}
		id := discovered[i].Session.SessionID
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return
	}
	// A child written this harvest already had its FK cache decided by the
	// write path, including the in-batch cycle and unavailable-parent cases.
	// Only children that were not rewritten are healed from the reverse pass.
	rewritten := make(map[SessionID]struct{}, len(written))
	for i := range written {
		rewritten[written[i].Session.SessionID] = struct{}{}
	}

	candidates, err := reconciler.ListUncachedChildrenOfParents(ctx, targets, []Harness{HarnessCodex, HarnessOpenCode})
	if err != nil {
		p.warnParentReconcile("list uncached independent children", storeLookupReasonCode(err))
		return
	}
	updates := make([]ParentCacheReconcile, 0, len(candidates))
	for _, update := range candidates {
		if _, justWritten := rewritten[update.Child]; justWritten {
			continue
		}
		updates = append(updates, update)
	}
	if len(updates) == 0 {
		return
	}
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Child != updates[j].Child {
			return updates[i].Child < updates[j].Child
		}
		return updates[i].Parent < updates[j].Parent
	})
	if err := reconciler.ReconcileParentCache(ctx, updates); err != nil {
		p.warnParentReconcile("reconcile stored child parent cache", storeLookupReasonCode(err))
	}
}

// warnParentReconcile reports a cache-reconciliation failure with a bounded
// reason code. The raw dependency error is never logged: it can carry a private
// path or stored value.
func (p *Pipeline) warnParentReconcile(step, reason string) {
	slog.Warn("pipeline: reconcile independent child parent cache",
		"step", step,
		"reason", reason,
		"what", "could not reconcile the FK availability cache of a stored independently admitted child",
		"why", "the stored cache reconciliation step failed",
		"user_impact", "the child keeps a nil FK cache while its managed metadata keeps the logical parent; it is retried on the next harvest that discovers the same parent",
		"how_to_fix", "restore database access and rerun harvest")
}
