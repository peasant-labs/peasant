package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// reclaimSeamAfterRows is the crash-injection seam an apply pass reports after
// its row transaction has committed and before any generation directory is
// removed. Production never sets the hook; a crash-recovery test sets it to
// prove the row-first ordering leaves a readable store and a retryable orphan
// directory.
const reclaimSeamAfterRows = "after-rows-before-directories"

// reclaimTableNames are the generation-scoped tables one reclaim pass clears.
// Every table is addressed by (session_id, generation_id). The list is the
// single generation-scoped inventory both the reclaim and the representation
// replacement consume: the reclaim deletes every non-active row per table,
// and generationIndexFormat.Delete (index_format_v2.go) deletes every row of
// a replaced session through this same list, so the two hand-maintained
// lists cannot drift apart. The exact membership is pinned by
// TestReclaimTableInventoryMatchesFormatDelete: reclaim never leaves rows
// for a new table.
//
// Children come before the parents their foreign keys cascade from, so the
// per-table counts are exact: an explicit child delete counts its rows
// instead of vanishing into a parent cascade. The catalog tables close the
// list; their cascades find no remaining children.
var reclaimTableNames = []string{
	"session_context_segment_refs",
	"session_section_native_metadata",
	"session_generation_entries",
	"session_generation_content",
	"session_generation_subagents",
	"session_generation_commits",
	"session_generation_associations",
	"session_generation_diagnostics",
	"session_generation_title_refs",
	"session_projection_entries",
	"session_projection_content",
	"session_projection_aliases",
	"session_projection_sections",
	"session_context_segments",
	"session_relationship_evidence",
	"session_generations",
	"session_projection_generations",
}

// reclaimCountField addresses the per-table count inside a ReclaimTableCounts.
// It returns nil for a table outside the closed set, so a caller cannot
// silently accumulate a row count it never reports.
func reclaimCountField(counts *ReclaimTableCounts, table string) *int64 {
	switch table {
	case "session_projection_entries":
		return &counts.ProjectionEntries
	case "session_projection_content":
		return &counts.ProjectionContent
	case "session_projection_aliases":
		return &counts.ProjectionAliases
	case "session_projection_sections":
		return &counts.ProjectionSections
	case "session_context_segments":
		return &counts.ContextSegments
	case "session_relationship_evidence":
		return &counts.RelationshipEvidence
	case "session_projection_generations":
		return &counts.Generations
	case "session_context_segment_refs":
		return &counts.SegmentRefs
	case "session_section_native_metadata":
		return &counts.NativeMetadata
	case "session_generation_entries":
		return &counts.GenerationEntries
	case "session_generation_content":
		return &counts.GenerationContent
	case "session_generation_subagents":
		return &counts.GenerationSubagents
	case "session_generation_commits":
		return &counts.GenerationCommits
	case "session_generation_associations":
		return &counts.GenerationAssociations
	case "session_generation_diagnostics":
		return &counts.GenerationDiagnostics
	case "session_generation_title_refs":
		return &counts.GenerationTitleRefs
	case "session_generations":
		return &counts.SessionGenerations
	default:
		return nil
	}
}

// ReclaimTableCounts is the per-table row count one reclaim removes. The
// closed set is every generation-scoped table: the harmonized catalog and
// its children, the shared generation-keyed tables, and the file-backed
// catalog. A table outside this set has no count field, so a caller cannot
// silently accumulate a row count it never reports.
type ReclaimTableCounts struct {
	ProjectionEntries      int64
	ProjectionContent      int64
	ProjectionAliases      int64
	ProjectionSections     int64
	ContextSegments        int64
	RelationshipEvidence   int64
	Generations            int64
	SegmentRefs            int64
	NativeMetadata         int64
	GenerationEntries      int64
	GenerationContent      int64
	GenerationSubagents    int64
	GenerationCommits      int64
	GenerationAssociations int64
	GenerationDiagnostics  int64
	GenerationTitleRefs    int64
	SessionGenerations     int64
}

// Total is the sum of every table count.
func (c ReclaimTableCounts) Total() int64 {
	return c.ProjectionEntries + c.ProjectionContent + c.ProjectionAliases +
		c.ProjectionSections + c.ContextSegments + c.RelationshipEvidence + c.Generations +
		c.SegmentRefs + c.NativeMetadata + c.GenerationEntries + c.GenerationContent +
		c.GenerationSubagents + c.GenerationCommits + c.GenerationAssociations +
		c.GenerationDiagnostics + c.GenerationTitleRefs + c.SessionGenerations
}

// Add accumulates another count into this one.
func (c *ReclaimTableCounts) Add(other ReclaimTableCounts) {
	c.ProjectionEntries += other.ProjectionEntries
	c.ProjectionContent += other.ProjectionContent
	c.ProjectionAliases += other.ProjectionAliases
	c.ProjectionSections += other.ProjectionSections
	c.ContextSegments += other.ContextSegments
	c.RelationshipEvidence += other.RelationshipEvidence
	c.Generations += other.Generations
	c.SegmentRefs += other.SegmentRefs
	c.NativeMetadata += other.NativeMetadata
	c.GenerationEntries += other.GenerationEntries
	c.GenerationContent += other.GenerationContent
	c.GenerationSubagents += other.GenerationSubagents
	c.GenerationCommits += other.GenerationCommits
	c.GenerationAssociations += other.GenerationAssociations
	c.GenerationDiagnostics += other.GenerationDiagnostics
	c.GenerationTitleRefs += other.GenerationTitleRefs
	c.SessionGenerations += other.SessionGenerations
}

// ReclaimGeneration is one superseded generation selected for reclaim: its
// per-table row count and its on-disk footprint. Committed reports whether the
// generation still carries a catalog row; an orphan directory left by a crash
// between row deletion and directory removal has Committed false.
type ReclaimGeneration struct {
	GenerationID string
	Committed    bool
	Rows         ReclaimTableCounts
	Footprint    GenerationFootprint
}

// ReclaimSession is one session's superseded generations.
type ReclaimSession struct {
	SessionID          schema.SessionID
	ActiveGenerationID string
	Generations        []ReclaimGeneration
}

// Rows returns the session's summed per-table counts.
func (s ReclaimSession) Rows() ReclaimTableCounts {
	var counts ReclaimTableCounts
	for _, generation := range s.Generations {
		counts.Add(generation.Rows)
	}
	return counts
}

// Footprint returns the session's summed on-disk footprint.
func (s ReclaimSession) Footprint() GenerationFootprint {
	var footprint GenerationFootprint
	for _, generation := range s.Generations {
		footprint.Add(generation.Footprint)
	}
	return footprint
}

// ReclaimPlan is the frozen forecast a dry run previews. It is read-only: the
// plan names the sessions, generations, rows and bytes a subsequent apply pass
// reclaims, but changes no row and no file.
type ReclaimPlan struct {
	Sessions []ReclaimSession
	// PendingIntentSessions are sessions skipped because a durable activation
	// intent is pending. Their superseded generations are never touched: the
	// intent may still commit or replay the generation it names.
	PendingIntentSessions []schema.SessionID
}

// SessionCount is the number of sessions with at least one superseded
// generation to reclaim.
func (p ReclaimPlan) SessionCount() int { return len(p.Sessions) }

// GenerationCount is the number of superseded generations to reclaim.
func (p ReclaimPlan) GenerationCount() int {
	total := 0
	for _, session := range p.Sessions {
		total += len(session.Generations)
	}
	return total
}

// Totals sums the plan's per-table row counts and on-disk footprint.
func (p ReclaimPlan) Totals() (ReclaimTableCounts, GenerationFootprint) {
	var rows ReclaimTableCounts
	var footprint GenerationFootprint
	for _, session := range p.Sessions {
		rows.Add(session.Rows())
		footprint.Add(session.Footprint())
	}
	return rows, footprint
}

// ReclaimResult reports one applied pass.
type ReclaimResult struct {
	// Sessions is the number of sessions with work reclaimed by this pass.
	Sessions int
	// Generations is the number of superseded generations reclaimed.
	Generations int
	// Rows is the per-table row count removed by this pass.
	Rows ReclaimTableCounts
	// BodiesDeleted counts the orphan entry bodies the pass swept: entry
	// rows no generation references after the superseded rows went away.
	BodiesDeleted int64
	// BlobsDeleted counts the orphan content blobs the pass swept, whole
	// objects whose chunks cascade.
	BlobsDeleted int64
	// Footprint is the on-disk size of the directories removed by this pass.
	Footprint GenerationFootprint
	// DirectoriesRemoved counts the generation directories successfully
	// removed. A missing directory is success, so this includes an already
	// absent directory.
	DirectoriesRemoved int
	// PendingIntentSessions are sessions skipped because a durable activation
	// intent was pending at reclaim time.
	PendingIntentSessions []schema.SessionID
	// Warnings are non-fatal directory-removal failures. The row transaction
	// for the session already committed, so the next pass retries the removal.
	Warnings []error
}

// reclaimCandidate is one session the sweep flag marks. Every flagged
// session is inspected, so a crash that removed the rows but left an orphan
// generation directory is retried on the next pass even though the session no
// longer has more than one committed generation.
type reclaimCandidate struct {
	sessionID schema.SessionID
	active    string
}

// PlanSupersededGenerationReclaim builds the reclaim forecast. For every
// flagged session, every committed generation other than the active one is
// a candidate, and so is any owned generation directory that is not active
// (an orphan left by an interrupted earlier pass). Each candidate's
// per-table row count and on-disk footprint are measured. A session with a
// pending activation intent is reported as skipped and contributes no
// candidates. limit, when positive, bounds how many sessions with work are
// included, so a batched run resumes where the previous one stopped.
//
// The method reads only. It takes no exclusive session lock and changes no row
// and no file, so it runs against a read-only store opened for a dry run. A
// store opened without the owned-artifact store still forecasts the row counts
// and the committed catalog generations; it reports a zero footprint and no
// owned directories, so a dry run over a missing artifact root stays useful.
func (s *Store) PlanSupersededGenerationReclaim(ctx context.Context, limit int) (ReclaimPlan, error) {
	var plan ReclaimPlan
	candidates, err := s.reclaimCandidateSessions(ctx)
	if err != nil {
		return plan, err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		if limit > 0 && plan.SessionCount() >= limit {
			break
		}
		session, pendingIntent, err := s.planSessionReclaim(ctx, candidate)
		if err != nil {
			return plan, err
		}
		if pendingIntent {
			plan.PendingIntentSessions = append(plan.PendingIntentSessions, candidate.sessionID)
			continue
		}
		if len(session.Generations) == 0 {
			continue
		}
		plan.Sessions = append(plan.Sessions, session)
	}
	return plan, nil
}

// ReclaimSupersededGenerations reclaims every superseded generation. For each
// candidate session it takes the exclusive per-session lock, re-reads the
// active generation and the pending intent, and deletes every non-active row
// across the generation-scoped tables in bounded batches. It then releases
// the lock and removes each superseded generation's directory through the
// ownership-verified cleanup path, and finally sweeps the orphan objects
// the row deletes uncovered and clears the sweep flag.
//
// A session with a pending intent is never touched. The active generation is
// never deleted: the row predicate excludes it and the cleanup path refuses
// it. A missing directory is success, so a crash between the row deletes and
// directory removal leaves a readable store and a retryable orphan that the
// next pass removes. limit, when positive, bounds how many sessions with
// work this pass reclaims.
func (s *Store) ReclaimSupersededGenerations(ctx context.Context, limit int) (ReclaimResult, error) {
	var result ReclaimResult
	if err := s.requireGenerationSupport(); err != nil {
		return result, err
	}
	if _, err := s.EnsureSearchIndexHealthy(ctx); err != nil {
		return result, err
	}
	candidates, err := s.reclaimCandidateSessions(ctx)
	if err != nil {
		return result, err
	}
	processed := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if limit > 0 && processed >= limit {
			break
		}
		outcome, err := s.reclaimOneSession(ctx, candidate)
		if err != nil {
			return result, err
		}
		if outcome.pendingIntent {
			result.PendingIntentSessions = append(result.PendingIntentSessions, candidate.sessionID)
			continue
		}
		if !outcome.didWork {
			continue
		}
		processed++
		result.Sessions++
		result.Generations += outcome.generations
		result.Rows.Add(outcome.rows)
		result.BodiesDeleted += outcome.bodiesDeleted
		result.BlobsDeleted += outcome.blobsDeleted
		result.Footprint.Add(outcome.footprint)
		result.DirectoriesRemoved += outcome.directoriesRemoved
		result.Warnings = append(result.Warnings, outcome.warnings...)
	}
	return result, nil
}

// planSessionReclaim measures one session's reclaimable generations. A pending
// intent reports pendingIntent true and no candidates.
func (s *Store) planSessionReclaim(ctx context.Context, candidate reclaimCandidate) (ReclaimSession, bool, error) {
	if s.generationArtifacts != nil {
		intent, err := s.generationArtifacts.ReadIntent(ctx, candidate.sessionID)
		if err != nil {
			return ReclaimSession{}, false, fmt.Errorf("store: plan the superseded-generation reclaim for session %s: %w; no generation was inspected", candidate.sessionID, err)
		}
		if intent != nil {
			return ReclaimSession{}, true, nil
		}
	}
	rowCounts, err := s.reclaimRowCounts(ctx, candidate.sessionID, candidate.active)
	if err != nil {
		return ReclaimSession{}, false, err
	}
	committed, err := s.reclaimCommittedGenerationIDs(ctx, candidate.sessionID)
	if err != nil {
		return ReclaimSession{}, false, err
	}
	var dirs []string
	if s.generationArtifacts != nil {
		dirs, err = s.generationArtifacts.ListGenerationDirectories(ctx, candidate.sessionID)
		if err != nil {
			return ReclaimSession{}, false, fmt.Errorf("store: plan the superseded-generation reclaim for session %s: %w; no generation was inspected", candidate.sessionID, err)
		}
	}
	ids := reclaimGenerationUnion(rowCounts, committed, dirs, candidate.active)
	if len(ids) == 0 {
		return ReclaimSession{}, false, nil
	}
	session := ReclaimSession{SessionID: candidate.sessionID, ActiveGenerationID: candidate.active}
	committedSet := make(map[string]struct{}, len(committed))
	for _, id := range committed {
		committedSet[id] = struct{}{}
	}
	for _, generationID := range ids {
		var footprint GenerationFootprint
		if s.generationArtifacts != nil {
			footprint, err = s.generationArtifacts.GenerationSize(ctx, candidate.sessionID, generationID)
			if err != nil {
				return ReclaimSession{}, false, fmt.Errorf("store: plan the superseded-generation reclaim for session %s generation %s: %w; no generation was inspected", candidate.sessionID, generationID, err)
			}
		}
		_, isCommitted := committedSet[generationID]
		session.Generations = append(session.Generations, ReclaimGeneration{
			GenerationID: generationID,
			Committed:    isCommitted,
			Rows:         rowCounts[generationID],
			Footprint:    footprint,
		})
	}
	return session, false, nil
}

// reclaimSessionOutcome is one session's applied result.
type reclaimSessionOutcome struct {
	pendingIntent      bool
	didWork            bool
	generations        int
	rows               ReclaimTableCounts
	bodiesDeleted      int64
	blobsDeleted       int64
	footprint          GenerationFootprint
	directoriesRemoved int
	warnings           []error
}

// reclaimOneSession reclaims one candidate session. It takes the exclusive
// per-session lock, re-reads the active generation and the pending intent,
// and deletes every non-active row across the generation-scoped tables. It
// then releases the lock and removes each superseded generation's directory
// through the ownership-verified cleanup path. Finally it re-acquires the
// lock, sweeps the orphan objects the row deletes uncovered, and clears the
// sweep flag when the session is fully clean.
//
// A session with a pending intent is never touched. A session with no
// active generation keeps nothing: every catalog row counts as superseded.
// The active generation is never deleted: the row predicate excludes it and
// the cleanup path refuses it. A missing directory is success, so a crash
// between the row transaction and directory removal leaves a readable store
// and a retryable orphan that the next pass removes. limit, when positive,
// bounds how many sessions with work this pass reclaims.
func (s *Store) reclaimOneSession(ctx context.Context, candidate reclaimCandidate) (reclaimSessionOutcome, error) {
	var outcome reclaimSessionOutcome
	release, err := s.sessionLocker.LockExclusive(ctx, candidate.sessionID)
	if err != nil {
		return outcome, err
	}
	locked := true
	defer func() {
		if locked {
			_ = release()
		}
	}()

	active, err := s.activeGenerationID(ctx, candidate.sessionID)
	if err != nil {
		return outcome, err
	}
	intent, err := s.generationArtifacts.ReadIntent(ctx, candidate.sessionID)
	if err != nil {
		return outcome, fmt.Errorf("store: reclaim superseded generations for session %s: %w; no row was deleted", candidate.sessionID, err)
	}
	if intent != nil {
		outcome.pendingIntent = true
		return outcome, nil
	}

	rowCounts, err := s.reclaimRowCounts(ctx, candidate.sessionID, active)
	if err != nil {
		return outcome, err
	}
	committed, err := s.reclaimCommittedGenerationIDs(ctx, candidate.sessionID)
	if err != nil {
		return outcome, err
	}
	dirs, err := s.generationArtifacts.ListGenerationDirectories(ctx, candidate.sessionID)
	if err != nil {
		return outcome, fmt.Errorf("store: reclaim superseded generations for session %s: %w; no row was deleted", candidate.sessionID, err)
	}
	ids := reclaimGenerationUnion(rowCounts, committed, dirs, active)

	deleted, err := s.deleteSupersededGenerationRows(ctx, candidate.sessionID, active)
	if err != nil {
		return outcome, err
	}

	// The row deletes have committed. Release the lock before the directory
	// pass so a long cleanup cannot block a reader that shares the lock; the
	// cleanup path re-acquires it and re-verifies ownership.
	release()
	locked = false

	if len(ids) > 0 && s.reclaimSeam != nil {
		if err := s.reclaimSeam(reclaimSeamAfterRows); err != nil {
			outcome.rows = deleted
			outcome.generations = len(ids)
			return outcome, err
		}
	}

	// Only generations with an owned directory attempt removal: a
	// harmonized generation without one has nothing to remove, and
	// attempting it would refuse ownership no manifest can prove.
	present := make(map[string]struct{}, len(dirs))
	for _, generationID := range dirs {
		present[generationID] = struct{}{}
	}
	sort.Strings(ids)
	for _, generationID := range ids {
		if _, ok := present[generationID]; !ok {
			continue
		}
		footprint, sizeErr := s.generationArtifacts.GenerationSize(ctx, candidate.sessionID, generationID)
		if sizeErr != nil {
			outcome.warnings = append(outcome.warnings, fmt.Errorf("store: measure superseded generation %s for session %s: %w; the directory was left in place", generationID, candidate.sessionID, sizeErr))
			continue
		}
		if err := s.CleanupInactiveGeneration(ctx, candidate.sessionID, generationID); err != nil {
			outcome.warnings = append(outcome.warnings, fmt.Errorf("store: remove superseded generation %s for session %s: %w; the next pass retries the removal", generationID, candidate.sessionID, err))
			continue
		}
		outcome.directoriesRemoved++
		outcome.footprint.Add(footprint)
	}
	outcome.generations = len(ids)
	outcome.rows = deleted

	// The row deletes uncovered orphan objects: bodies and blobs no live
	// generation references. Sweep them under a fresh lock span, then clear
	// the sweep flag when the session is fully clean. A directory warning
	// keeps the flag set, so the next pass retries the refused removal.
	orphanRelease, err := s.sessionLocker.LockExclusive(ctx, candidate.sessionID)
	if err != nil {
		return outcome, err
	}
	limit := s.writeConfigForLane().SweepRows
	if limit < 1 {
		limit = 1
	}
	bodies, blobs, _, orphanErr := s.sweepUnreferencedObjects(ctx, candidate.sessionID, limit)
	if orphanErr != nil {
		_ = orphanRelease()
		return outcome, orphanErr
	}
	outcome.bodiesDeleted = bodies
	outcome.blobsDeleted = blobs
	if len(outcome.warnings) == 0 {
		if flagErr := s.clearSweepFlag(ctx, candidate.sessionID); flagErr != nil {
			_ = orphanRelease()
			return outcome, flagErr
		}
	}
	_ = orphanRelease()

	outcome.didWork = deleted.Total() > 0 || outcome.directoriesRemoved > 0 || bodies > 0 || blobs > 0
	return outcome, nil
}

// deleteSupersededGenerationRows deletes every non-active generation row for
// one session across the generation-scoped tables, in bounded batches (the
// configured write.sweep_rows). The active generation is excluded by a
// NULL-safe row predicate, so it is never deleted; a session with no active
// generation keeps nothing. Children go before the parents their foreign
// keys cascade from, so the per-table counts are exact. Each batch commits
// on its own, so a crash between batches leaves a partial, safe deletion
// the next pass completes; the seam fires between batches. The completeness
// guard keeps the active generation's complete last-good row.
func (s *Store) deleteSupersededGenerationRows(ctx context.Context, sessionID schema.SessionID, active string) (ReclaimTableCounts, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return ReclaimTableCounts{}, fmt.Errorf("store: reclaim superseded generation rows for session %s: take connection: %w; no row was deleted", sessionID, err)
	}
	defer s.pool.Put(conn)
	limit := s.writeConfigForLane().SweepRows
	if limit < 1 {
		limit = 1
	}
	var counts ReclaimTableCounts
	for _, table := range reclaimTableNames {
		for {
			if err := ctx.Err(); err != nil {
				return ReclaimTableCounts{}, err
			}
			changed, err := deleteSupersededBatch(conn, table, sessionID, active, limit)
			if err != nil {
				return ReclaimTableCounts{}, err
			}
			if field := reclaimCountField(&counts, table); field != nil {
				*field += int64(changed)
			}
			if changed == 0 {
				break
			}
			if err := s.reportSweepBatch(ctx, sessionID); err != nil {
				return ReclaimTableCounts{}, err
			}
		}
	}
	return counts, nil
}

// deleteSupersededBatch deletes one bounded batch of non-active generation
// rows from one table and reports how many rows went away. The engine has no
// UPDATE/DELETE LIMIT, so the batch addresses its rows by key: the rowid for
// rowid tables, the full primary key for the WITHOUT ROWID descriptor
// table. A batch that deletes nothing ends its table's loop.
func deleteSupersededBatch(conn *sqlite.Conn, table string, sessionID schema.SessionID, active string, limit int) (int64, error) {
	statement := `DELETE FROM ` + table + ` WHERE rowid IN (SELECT rowid FROM ` + table + ` WHERE session_id = ? AND generation_id IS NOT ? LIMIT ?)`
	if table == "session_generation_content" {
		statement = `DELETE FROM session_generation_content WHERE (session_id, generation_id, source_entry_ref) IN (SELECT session_id, generation_id, source_entry_ref FROM session_generation_content WHERE session_id = ? AND generation_id IS NOT ? LIMIT ?)`
	}
	if err := sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), active, int64(limit)},
	}); err != nil {
		return 0, fmt.Errorf("store: reclaim superseded generation rows for session %s in %s: %w; the committed batches stay deleted and the next pass completes the sweep", sessionID, table, err)
	}
	return int64(conn.Changes()), nil
}

// reclaimRowCounts returns the per-generation, per-table row counts for one
// session's non-active generations. It reads every generation-scoped table
// with one grouped query each, so the totals match the delete predicate
// `generation_id != active` exactly.
func (s *Store) reclaimRowCounts(ctx context.Context, sessionID schema.SessionID, active string) (map[string]ReclaimTableCounts, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: count superseded generation rows for session %s: take connection: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	counts := make(map[string]ReclaimTableCounts)
	for _, table := range reclaimTableNames {
		err := sqlitex.ExecuteTransient(conn, `SELECT generation_id, COUNT(*) FROM `+table+` WHERE session_id = ? GROUP BY generation_id`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				generationID := stmt.ColumnText(0)
				if generationID == active {
					return nil
				}
				entry := counts[generationID]
				if field := reclaimCountField(&entry, table); field != nil {
					*field = stmt.ColumnInt64(1)
				}
				counts[generationID] = entry
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("store: count superseded generation rows for session %s in %s: %w", sessionID, table, err)
		}
	}
	return counts, nil
}

// reclaimCommittedGenerationIDs returns the session's committed generation
// catalog identifiers from both catalog tables, sorted: the file-backed
// catalog Release N still reads and the harmonized catalog the new writers
// install. A generation with a catalog row in either table is committed,
// even when its mapping rows are already gone.
func (s *Store) reclaimCommittedGenerationIDs(ctx context.Context, sessionID schema.SessionID) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read committed generations for session %s: take connection: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	var ids []string
	err = sqlitex.ExecuteTransient(conn, `SELECT generation_id FROM session_projection_generations WHERE session_id = ? UNION SELECT generation_id FROM session_generations WHERE session_id = ? ORDER BY generation_id`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ids = append(ids, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read committed generations for session %s: %w", sessionID, err)
	}
	return ids, nil
}

// reclaimCandidateSessions lists every session the sweep flag marks,
// ordered by session identifier so a batched pass is deterministic. The
// flag is complete for catalog rows and objects: a session holds superseded
// rows or orphans only while its flag is set (the v62 backfill flags every
// session with a non-active projection row; staging, migration, and
// representation replacement set it before any object write or row removal;
// the sweep clears it after). Selecting through the partial index removes
// the scan of every session.
func (s *Store) reclaimCandidateSessions(ctx context.Context) ([]reclaimCandidate, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions with a set sweep flag: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	var candidates []reclaimCandidate
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id, active_generation_id FROM sessions WHERE content_sweep_pending = 1 ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			active := ""
			if stmt.ColumnType(1) != sqlite.TypeNull {
				active = stmt.ColumnText(1)
			}
			candidates = append(candidates, reclaimCandidate{
				sessionID: schema.SessionID(stmt.ColumnText(0)),
				active:    active,
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list sessions with a set sweep flag: %w", err)
	}
	return candidates, nil
}

// reclaimGenerationUnion merges the row-bearing generation identifiers, the
// committed catalog identifiers and the owned directory names into one sorted
// set that excludes the active generation.
func reclaimGenerationUnion(rowCounts map[string]ReclaimTableCounts, committed, dirs []string, active string) []string {
	seen := make(map[string]struct{}, len(rowCounts)+len(committed)+len(dirs))
	for generationID := range rowCounts {
		if generationID != active {
			seen[generationID] = struct{}{}
		}
	}
	for _, generationID := range committed {
		if generationID != active {
			seen[generationID] = struct{}{}
		}
	}
	for _, generationID := range dirs {
		if generationID != active {
			seen[generationID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for generationID := range seen {
		ids = append(ids, generationID)
	}
	sort.Strings(ids)
	return ids
}
