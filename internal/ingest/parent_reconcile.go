package ingest

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// Reverse logical-target cache reconciliation for independently admitted
// children.
//
// An independently admitted child admitted while its logical parent was
// missing, unselected or otherwise unavailable is stored with a nil FK
// availability cache while its managed metadata keeps the logical ParentUUID.
// When a later harvest stores that logical parent, this pass heals the cache.
// It reads the child's own managed evidence for the logical target and writes
// only the cache: the child is never re-extracted, relocated, re-indexed or
// re-selected, and its content and managed metadata stay byte-identical.
//
// The pass runs only when this harvest newly stores an independently admitted
// session, and the store refuses any update that would close a parent-cache
// cycle. A store without the reconciliation capability keeps the cache
// unchanged.
func (p *Pipeline) reconcileOrphanParentCaches(ctx context.Context, admitted []DiffEntry) {
	if p.store == nil || p.config.DryRun {
		return
	}
	reconciler, ok := p.store.(OrphanParentReconciler)
	if !ok {
		return
	}
	admittedIDs := make(map[SessionID]bool, len(admitted))
	newlyAvailable := false
	for i := range admitted {
		id := admitted[i].Session.SessionID
		admittedIDs[id] = true
		if !IndependentAdmissionHarness(admitted[i].Session.Harness) {
			continue
		}
		if _, stored := p.locationCache[id]; !stored {
			newlyAvailable = true
		}
	}
	if !newlyAvailable {
		return
	}

	candidates, err := reconciler.ListUncachedIndependentChildren(ctx, []Harness{HarnessCodex, HarnessOpenCode})
	if err != nil {
		p.warnParentReconcile("list uncached independent children", storeLookupReasonCode(err))
		return
	}
	locations, err := p.store.BulkLookupSessionLocations(ctx, candidates)
	if err != nil {
		p.warnParentReconcile("resolve stored child locations", storeLookupReasonCode(err))
		return
	}

	output := string(p.config.OutputDir)
	logical := make(map[SessionID]SessionID)
	parentNeeded := make(map[SessionID]bool)
	for _, child := range candidates {
		if admittedIDs[child] {
			continue
		}
		location, found := locations[child]
		if !found || location.HostSlug == "" {
			continue
		}
		metaPath := filepath.Join(output, location.HostSlug, string(child), string(child)+defaults.MetadataSuffix)
		data, readErr := p.fs.ReadFile(metaPath)
		if readErr != nil {
			if !errors.Is(readErr, fs.ErrNotExist) {
				p.warnParentReconcile("read stored child metadata", storeLookupReasonCode(readErr))
			}
			continue
		}
		meta, decodeErr := decodeManagedMetadata(data, p.managedRelativePath(metaPath))
		if decodeErr != nil {
			continue
		}
		if meta.ParentUUID == nil || *meta.ParentUUID == child {
			continue
		}
		logical[child] = *meta.ParentUUID
		parentNeeded[*meta.ParentUUID] = true
	}
	if len(logical) == 0 {
		return
	}

	needed := make([]SessionID, 0, len(parentNeeded))
	for id := range parentNeeded {
		needed = append(needed, id)
	}
	stored := map[SessionID]SessionLocation{}
	if len(needed) > 0 {
		stored, err = p.store.BulkLookupSessionLocations(ctx, needed)
		if err != nil {
			p.warnParentReconcile("resolve stored parent locations", storeLookupReasonCode(err))
			return
		}
	}

	updates := make([]ParentCacheReconcile, 0, len(logical))
	for child, parent := range logical {
		if !admittedIDs[parent] {
			if _, found := stored[parent]; !found {
				continue // the logical parent is still not available
			}
		}
		updates = append(updates, ParentCacheReconcile{Child: child, Parent: parent})
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
		"user_impact", "the child keeps a nil FK cache while its managed metadata keeps the logical parent; it is retried on the next harvest",
		"how_to_fix", "restore database access and rerun harvest")
}
