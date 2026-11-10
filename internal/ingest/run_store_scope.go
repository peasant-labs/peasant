package ingest

import (
	"context"
	"fmt"
	"log/slog"
)

// runStoreScope returns the store scope every store-wide pass reads. A full run
// keeps the all-store value, so its SQL result is unchanged. A scoped run
// derives the scope from the same allowlist the in-memory gate uses, so a pass
// and a per-row filter cannot disagree about what this run may touch.
//
// The allowlist is the run's upkeep set: every located member a full run would
// maintain a stored row for, including a stored member whose source could not
// be read again. LOCATE sets it together with the run's scope, before any pass
// runs, so a scoped pass never starts from an unset allowlist.
func (p *Pipeline) runStoreScope() StoreScope {
	if p.config.Scope == nil {
		return AllStoredSessions()
	}
	ids := make([]SessionID, 0, len(p.config.AllowedSessionIDs))
	for id := range p.config.AllowedSessionIDs {
		ids = append(ids, id)
	}
	return StoredSessions(ids)
}

// reportStaleIndexOutsideSelection reports the stored index maintenance the run
// left because it fell outside the run's session selection. That count is a
// full-run report: a scoped run's inventory is already its cohort, so every row
// it lists is this run's upkeep and "outside this run's selection" names nothing
// the user could act on from the scoped command.
func (p *Pipeline) reportStaleIndexOutsideSelection(selectionSkipped int) {
	if selectionSkipped <= 0 || p.config.Scope != nil {
		return
	}
	p.reportDiagnostic(DiagnosticEntry{
		ErrorType:   "stale_index_outside_selection",
		Location:    "stored index maintenance",
		Message:     fmt.Sprintf("%d stored session(s) outside this run's session selection still need index maintenance (an older indexer revision or an unfinished repair) and were not processed by this run", selectionSkipped),
		Remediation: "Run peasant harvest index to process every stored session that needs index maintenance, or include these sessions' projects in the kickstart selection and run again.",
	})
}

// projectScopeForCleanup returns the project identities the orphan-project
// cleanup may consider. A full run keeps the all-project value, so its SQL
// result is unchanged.
//
// A scoped run names the projects its cohort occupied BEFORE and AFTER this
// run, because a cohort session whose project identity changed leaves its old
// project row with no remaining session: the pre-write location snapshot holds
// the hash the row carried before the run rewrote it, and one primary-key read
// at cleanup time holds the hash it carries now. Those are the only two values
// the cleanup may act on, so it never scans the session population and never
// considers a project outside the cohort. A member whose source could not be
// read is not in the processed set, but its row is unchanged by this run, so the
// after read names its project too.
//
// A failed after read is not fatal: the cleanup is best effort, and the before
// set alone can only under-delete, never touch a project the cohort never held.
func (p *Pipeline) projectScopeForCleanup(ctx context.Context) ProjectScope {
	ids, narrowed := p.runStoreScope().Narrowed()
	if !narrowed {
		return AllProjects()
	}
	hashes := make(map[ProjectHash]struct{}, len(ids))
	for _, id := range ids {
		if location, ok := p.locationCache[id]; ok && location.ProjectHash != "" {
			hashes[ProjectHash(location.ProjectHash)] = struct{}{}
		}
	}
	if p.store != nil && len(ids) > 0 {
		locations, err := p.store.BulkLookupSessionLocations(ctx, ids)
		if err != nil {
			slog.Warn("pipeline: read cohort projects for orphan cleanup",
				"error", err,
				"what", "the project identities the cohort holds after this run were not read",
				"why", "the bulk session-location read failed",
				"user_impact", "a project row this run emptied may survive until the next harvest; no other project is touched",
				"how_to_fix", "restore database access and re-run peasant harvest")
		}
		for _, location := range locations {
			if location.ProjectHash != "" {
				hashes[ProjectHash(location.ProjectHash)] = struct{}{}
			}
		}
	}
	narrowedHashes := make([]ProjectHash, 0, len(hashes))
	for hash := range hashes {
		narrowedHashes = append(narrowedHashes, hash)
	}
	return Projects(narrowedHashes)
}

// cleanupOrphanProjects removes project rows and their daily summaries that no
// stored session names any more. Best effort and non-fatal, as it has always
// been: the scope bounds what it may delete, and a failure leaves the rows for
// the next run.
func (p *Pipeline) cleanupOrphanProjects(ctx context.Context, logPrefix string) {
	if p.store == nil {
		return
	}
	if err := p.store.CleanupOrphanProjects(ctx, p.projectScopeForCleanup(ctx)); err != nil {
		slog.Warn(logPrefix+": cleanup orphan projects",
			"error", err,
			"what", "failed to remove project rows with zero sessions",
			"why", "DB error during DELETE from projects or daily_summary_by_project",
			"user_impact", "stale project names may appear in the session viewer filter list",
			"how_to_fix", "re-run peasant ingest; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest")
	}
}
