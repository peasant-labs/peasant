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
// Every table is addressed by (session_id, generation_id); none carries a
// foreign key to another in this set, so deletion order does not matter. The
// catalog table is included: its non-active rows are exactly the superseded
// generations whose directories are then removed.
var reclaimTableNames = []string{
	"session_projection_entries",
	"session_projection_content",
	"session_projection_aliases",
	"session_projection_sections",
	"session_context_segments",
	"session_relationship_evidence",
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
	default:
		return nil
	}
}

// ReclaimTableCounts is the per-table row count one reclaim removes. The
// closed set is every generation-scoped table: the five projection tables, the
// context segments, the relationship evidence and the generation catalog.
type ReclaimTableCounts struct {
	ProjectionEntries    int64
	ProjectionContent    int64
	ProjectionAliases    int64
	ProjectionSections   int64
	ContextSegments      int64
	RelationshipEvidence int64
	Generations          int64
}

// Total is the sum of every table count.
func (c ReclaimTableCounts) Total() int64 {
	return c.ProjectionEntries + c.ProjectionContent + c.ProjectionAliases +
		c.ProjectionSections + c.ContextSegments + c.RelationshipEvidence + c.Generations
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

// reclaimCandidate is one session with an active generation. Every such
// session is inspected, so a crash that removed the rows but left an orphan
// generation directory is retried on the next pass even though the session no
// longer has more than one committed generation.
type reclaimCandidate struct {
	sessionID schema.SessionID
	active    string
}

// PlanSupersededGenerationReclaim builds the reclaim forecast. For every
// session with an active generation, every committed generation other than the
// active one is a candidate, and so is any owned generation directory that is
// not active (an orphan left by an interrupted earlier pass). Each candidate's
// per-table row count and on-disk footprint are measured. A session with a
// pending activation intent is reported as skipped and contributes no
// candidates. limit, when positive, bounds how many sessions with work are
// included, so a batched run resumes where the previous one stopped.
//
// The method reads only. It takes no exclusive session lock and changes no row
// and no file, so it runs against a read-only store opened for a dry run.
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
// active generation and the pending intent, and then, in ONE transaction,
// deletes every non-active row across the generation-scoped tables. It then
// releases the lock and removes each superseded generation's directory through
// the ownership-verified cleanup path.
//
// A session with a pending intent is never touched. The active generation is
// never deleted: the row predicate excludes it and the cleanup path refuses
// it. A missing directory is success, so a crash between the row transaction
// and directory removal leaves a readable store and a retryable orphan that
// the next pass removes. limit, when positive, bounds how many sessions with
// work this pass reclaims.
func (s *Store) ReclaimSupersededGenerations(ctx context.Context, limit int) (ReclaimResult, error) {
	var result ReclaimResult
	if err := s.requireGenerationSupport(); err != nil {
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
	footprint          GenerationFootprint
	directoriesRemoved int
	warnings           []error
}

// reclaimOneSession reclaims one candidate session under its exclusive lock.
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
	if active == "" {
		// The active pointer was cleared between enumeration and the lock; the
		// session no longer names a read authority to protect, so leave it.
		return outcome, nil
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
	if len(ids) == 0 {
		return outcome, nil
	}
	rows := sumReclaimRowCounts(rowCounts)

	deleted, err := s.deleteSupersededGenerationRows(ctx, candidate.sessionID, active)
	if err != nil {
		return outcome, err
	}
	_ = deleted // the forecast rowCounts is the reported count; the delete is the action

	// The row transaction has committed. Release the lock before the directory
	// pass so a long cleanup cannot block a reader that shares the lock; the
	// cleanup path re-acquires it and re-verifies ownership.
	release()
	locked = false

	if s.reclaimSeam != nil {
		if err := s.reclaimSeam(reclaimSeamAfterRows); err != nil {
			outcome.rows = rows
			outcome.generations = len(ids)
			return outcome, err
		}
	}

	sort.Strings(ids)
	for _, generationID := range ids {
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
	outcome.rows = rows
	outcome.didWork = rows.Total() > 0 || outcome.directoriesRemoved > 0
	return outcome, nil
}

// deleteSupersededGenerationRows deletes every non-active generation row for
// one session in ONE transaction across the generation-scoped tables. The
// active generation is excluded by the row predicate, so it is never deleted
// and the completeness guard keeps its complete last-good row.
func (s *Store) deleteSupersededGenerationRows(ctx context.Context, sessionID schema.SessionID, active string) (ReclaimTableCounts, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return ReclaimTableCounts{}, fmt.Errorf("store: reclaim superseded generation rows for session %s: take connection: %w; no row was deleted", sessionID, err)
	}
	defer s.pool.Put(conn)
	var counts ReclaimTableCounts
	var txErr error
	end := sqlitex.Transaction(conn)
	for _, table := range reclaimTableNames {
		if txErr != nil {
			break
		}
		txErr = sqlitex.ExecuteTransient(conn, `DELETE FROM `+table+` WHERE session_id = ? AND generation_id != ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), active},
		})
		if txErr == nil {
			if field := reclaimCountField(&counts, table); field != nil {
				*field = int64(conn.Changes())
			}
		}
	}
	end(&txErr)
	if txErr != nil {
		return ReclaimTableCounts{}, fmt.Errorf("store: reclaim superseded generation rows for session %s: %w; the transaction rolled back and every generation row is unchanged", sessionID, txErr)
	}
	return counts, nil
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
// catalog identifiers, sorted.
func (s *Store) reclaimCommittedGenerationIDs(ctx context.Context, sessionID schema.SessionID) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read committed generations for session %s: take connection: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	var ids []string
	err = sqlitex.ExecuteTransient(conn, `SELECT generation_id FROM session_projection_generations WHERE session_id = ? ORDER BY generation_id`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
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

// reclaimCandidateSessions lists every session with an active generation,
// ordered by session identifier so a batched pass is deterministic.
func (s *Store) reclaimCandidateSessions(ctx context.Context) ([]reclaimCandidate, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions with an active generation: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	var candidates []reclaimCandidate
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id, active_generation_id FROM sessions WHERE active_generation_id IS NOT NULL ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			candidates = append(candidates, reclaimCandidate{
				sessionID: schema.SessionID(stmt.ColumnText(0)),
				active:    stmt.ColumnText(1),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list sessions with an active generation: %w", err)
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

// sumReclaimRowCounts sums every generation's per-table count.
func sumReclaimRowCounts(counts map[string]ReclaimTableCounts) ReclaimTableCounts {
	var total ReclaimTableCounts
	for _, entry := range counts {
		total.Add(entry)
	}
	return total
}
