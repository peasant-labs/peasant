package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The peasant migrate engine (design §7.2): the five-phase in-place
// conversion of file-backed native sessions to the harmonized content
// model. Every phase is resumable because the per-session state in the
// database is the progress record; there is no side file. Stop points are
// every session boundary and every phase boundary: the driver checks the
// context at each one, so an interrupt finishes the current session's
// transaction and exits cleanly with a partial report.
//
// Phase 0  preflight (read-only; --dry-run stops here): counts per phase,
//          pending intents and superseded generations to discard, sampled
//          field/blob mismatches, free disk, and the advisory
//          no-other-writer note. No offline database copy is required.
// Phase 1  drain: discard pending intents and staged directories,
//          superseded generations (through the sweep), and reserved
//          staging directories; clear indexed_input_hash for the affected
//          sessions so the next harvest re-indexes them.
// Phase 2  per file-backed native session (exclusive lock): flag, legacy
//          oracle read, structured row writes, the guarded stats upsert,
//          one catalog transaction with the shadow verify before any
//          delete, old-row deletes, directory removal, sweep, flag clear.
// Phase 3  search consolidation (one transaction; skipped when the old
//          index is gone): retarget triggers, rebuild, drop the old index,
//          clear needs_rebuild.
// Phase 4  cleanup: leftover directories, sweep flagged sessions, the five
//          retirement preconditions, and the final report.
// Phase 5  (documented, offline) optimize + VACUUM: printed, never run.

// migrateDiskFloorBytes is the Phase 0 free-disk floor (design §7.2): the
// ~13 GB peak need (§7.3) plus 10 GB headroom plus the per-session working
// set, rounded to 25 GB.
const migrateDiskFloorBytes = 25 << 30

// migratePreflightSampleSessions bounds the Phase 0 mismatch sample: the
// first that many sessions with conversion work, in session-id order.
// migratePreflightSampleRefs bounds the content records checked per
// sampled session. Both keep the read-only preflight cheap while still
// forecasting rollbacks.
const (
	migratePreflightSampleSessions = 20
	migratePreflightSampleRefs     = 100
)

// migrateDataRollbackMin is the floor of the systematic-cause halt (design
// §7.2 Phase 2d): data rollbacks halt the run when they exceed 1% of
// sessions processed, with a minimum of 20 so early noise never stops a
// run before the rate is meaningful.
const migrateDataRollbackMin = 20

// checkMigrateRollbackHalt enforces the systematic-cause halt in one
// home: data rollbacks halt the run past 1% of sessions processed with
// a minimum of 20, since a systematic cause is likelier than bad data.
// The re-run resumes where the halted pass stopped.
func checkMigrateRollbackHalt(rollbacks, processed int64) error {
	if rollbacks > migrateDataRollbackMin && rollbacks*100 > processed {
		return fmt.Errorf("store: migration halted: %d data rollbacks in %d sessions exceeds 1%% (minimum %d); a systematic cause is likelier than bad data; fix the cause and re-run, which resumes where this pass stopped", rollbacks, processed, migrateDataRollbackMin)
	}
	return nil
}

// MigratePlan is the peasant migrate Phase 0 preflight report (design
// §7.2): the sessions with conversion work plus the drain counts (pending
// intents and superseded generations to discard), the search-consolidation
// need, and the free-disk and no-other-writer advisories. Read-only;
// --dry-run stops here.
type MigratePlan struct {
	// Sessions are the file-backed native sessions with conversion work,
	// in session-id order. A re-run lists only the sessions that still
	// need work, so the plan is the resume record.
	Sessions []schema.SessionID
	// PendingIntents counts the sessions holding a pending generation
	// intent Phase 1 discards.
	PendingIntents int
	// SupersededGenerations counts the non-active file-backed generations
	// Phase 1 discards through the sweep.
	SupersededGenerations int
	// NeedsSearchConsolidation reports whether session_entries_fts still
	// exists, so Phase 3 has work.
	NeedsSearchConsolidation bool
	// MirrorRows estimates the native mirror rows Phase 2 deletes.
	MirrorRows int64
	// SampledSessions counts the work sessions the mismatch sample
	// covered; SampledMismatchSessions counts the sampled sessions with
	// at least one field or blob mismatch, and SampledMismatchedRefs the
	// mismatched refs among them. A sampled mismatch forecasts a Phase
	// 2d data rollback; the conversion still verifies every byte.
	SampledSessions         int
	SampledMismatchSessions int
	SampledMismatchedRefs   int
	// EstimatedBytes estimates the owned-tree bytes Phase 2 frees.
	EstimatedBytes int64
	// DiskFreeBytes is the free space on the database filesystem, or -1
	// when the platform cannot report it.
	DiskFreeBytes int64
	// DiskFreeOK reports whether the free space clears the 25 GB floor
	// (or is unknowable, which warns instead of refusing).
	DiskFreeOK bool
	// Advisory carries the no-other-writer note: the run is recommended
	// with no harvest running.
	Advisory string
}

// MigrateOptions bounds one peasant migrate pass. Limit caps the sessions
// with conversion work (0 means all); a limited pass is resumed by
// re-running. Progress receives one event per session and phase boundary;
// a nil callback reports nothing.
type MigrateOptions struct {
	Limit    int
	Progress func(MigrateProgress)
}

// MigrateProgress is one resume-boundary event: the phase entered or the
// session finished, with the sessions done and total and the bytes freed
// so far. The command layer derives rate and ETA from the event stream.
type MigrateProgress struct {
	Phase         MigrationPhase
	SessionsDone  int
	SessionsTotal int
	BytesFreed    int64
}

// MigrateRollback records one Phase 2d data rollback: the session, the
// shadow-verify dimension that refused, and what happens next (the input
// proof is cleared, so the next harvest re-indexes it).
type MigrateRollback struct {
	SessionID schema.SessionID
	Dimension string
	Reason    string
}

// RetirementPreconditionStatus is one Release N+1 guard evaluation (design
// §7.1): whether it holds, the blocking row count, and what unblocks it.
type RetirementPreconditionStatus struct {
	Name     RetirementPrecondition
	Passed   bool
	RowCount int64
	Detail   string
}

// MigrateResult is the peasant migrate Phase 4 final report:
// per-session dispositions accumulated across the run, the freed bytes,
// the data rollbacks with their reasons, and the retirement evaluation.
type MigrateResult struct {
	Converted  int64
	RolledBack int64
	Marked     int64
	Skipped    int64
	BytesFreed int64
	// Rollbacks names every data rollback with its dimension, so the
	// report says which sessions need attention and why.
	Rollbacks []MigrateRollback
	// Warnings carries the non-fatal per-session failures Phase 1 and
	// Phase 4 continue past (a warned session keeps its state, so the
	// next pass retries it).
	Warnings []string
	// SearchConsolidated reports whether Phase 3 rebuilt and dropped the
	// old index on this pass.
	SearchConsolidated bool
	// Preconditions evaluates the five Release N+1 guards.
	Preconditions []RetirementPreconditionStatus
	// ReadyForNextRelease reports whether every guard holds.
	ReadyForNextRelease bool
}

// PlanMigration computes the Phase 0 preflight: the sessions with
// conversion work, the drain counts, the search-consolidation need, and
// the free-disk and no-other-writer advisories. It is read-only; a dry
// run stops here and changes nothing.
func (s *Store) PlanMigration(ctx context.Context) (MigratePlan, error) {
	var plan MigratePlan
	if err := s.requireGenerationSupport(); err != nil {
		return plan, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return plan, fmt.Errorf("store: take connection for the migration preflight: %w; nothing was planned", err)
	}
	defer s.pool.Put(conn)
	if err := collectMigrateWorkOnConn(conn, &plan); err != nil {
		return plan, err
	}
	// The drain scope and the intent count ride the same connection the
	// preflight already holds: the pool connection returns before any
	// other lookup, so the preflight never holds two checkouts at once.
	drainSessions, err := migrateDrainSessionsOnConn(conn)
	if err != nil {
		return plan, err
	}
	plan.PendingIntents, err = s.countMigratePendingIntents(ctx, drainSessions)
	if err != nil {
		return plan, err
	}
	plan.SampledSessions, plan.SampledMismatchSessions, plan.SampledMismatchedRefs, err = s.sampleMigrateMismatches(ctx, conn, plan.Sessions)
	if err != nil {
		return plan, err
	}
	plan.EstimatedBytes, err = s.estimateMigrateBytes(ctx, plan.Sessions)
	if err != nil {
		return plan, err
	}
	plan.DiskFreeBytes, plan.DiskFreeOK = checkMigrateDisk(ctx, conn)
	plan.Advisory = "no other writer running is recommended: Phase 2 takes the per-session lock and Phase 3 holds the single SQLite writer for minutes, so a concurrent harvest waits on busy_timeout or retries on its next harvest"
	return plan, nil
}

// collectMigrateWorkOnConn lists the file-backed native sessions with
// conversion work and the Phase 1 drain counts on the caller's
// connection. Settled refusals (a capture failure code this build cannot
// lift) are never selected: conversion does not touch their capture rows
// or their failure code, and the migration never clears their input
// proof. Non-native sessions (no generation-backed rows at all) are not
// listed either.
func collectMigrateWorkOnConn(conn *sqlite.Conn, plan *MigratePlan) error {
	if err := sqlitex.ExecuteTransient(conn, `SELECT s.session_id FROM sessions s
WHERE s.active_generation_id IS NOT NULL
AND EXISTS (SELECT 1 FROM session_projection_generations g
  WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id)
AND NOT EXISTS (SELECT 1 FROM session_content_captures c
  WHERE c.session_id = s.session_id AND c.failure_code IS NOT NULL)
ORDER BY s.session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			plan.Sessions = append(plan.Sessions, schema.SessionID(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: list file-backed sessions for the migration preflight: %w; nothing was planned", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_projection_generations g
JOIN sessions s ON s.session_id = g.session_id
WHERE g.generation_id IS NOT s.active_generation_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			plan.SupersededGenerations = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: count superseded generations for the migration preflight: %w; nothing was planned", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries WHERE session_id IN (
SELECT s.session_id FROM sessions s
WHERE s.active_generation_id IS NOT NULL
AND EXISTS (SELECT 1 FROM session_projection_generations g
  WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id))`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			plan.MirrorRows = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: count mirror rows for the migration preflight: %w; nothing was planned", err)
	}
	plan.NeedsSearchConsolidation = tableExistsOnConn(conn, "session_entries_fts")
	return nil
}

// tableExistsOnConn reports whether a table or virtual table names an
// existing relation.
func tableExistsOnConn(conn *sqlite.Conn, name string) bool {
	exists := false
	_ = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sqlite_master WHERE name = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{name},
		ResultFunc: func(*sqlite.Stmt) error {
			exists = true
			return nil
		},
	})
	return exists
}

// estimateMigrateBytes sums the owned generation footprints of the work
// sessions: the file bytes Phase 2 frees. A missing directory is the zero
// footprint, never an error.
func (s *Store) estimateMigrateBytes(ctx context.Context, sessions []schema.SessionID) (int64, error) {
	var total int64
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		footprint, err := s.sessionGenerationsFootprint(ctx, sessionID)
		if err != nil {
			return total, err
		}
		total += footprint.Bytes
	}
	return total, nil
}

// sessionGenerationsFootprint sums the on-disk size of one session's owned
// generation directories. Without artifact support there is no owned tree,
// so the estimate is zero.
func (s *Store) sessionGenerationsFootprint(ctx context.Context, sessionID schema.SessionID) (GenerationFootprint, error) {
	var footprint GenerationFootprint
	if s.generationArtifacts == nil {
		return footprint, nil
	}
	dirs, err := s.generationArtifacts.ListGenerationDirectories(ctx, sessionID)
	if err != nil {
		return footprint, fmt.Errorf("store: list generation directories while estimating session %s: %w", sessionID, err)
	}
	for _, generationID := range dirs {
		sized, err := s.generationArtifacts.GenerationSize(ctx, sessionID, generationID)
		if err != nil {
			return footprint, fmt.Errorf("store: size generation %s of session %s while estimating: %w", generationID, sessionID, err)
		}
		footprint.Add(sized)
	}
	return footprint, nil
}

// checkMigrateDisk reports the free space on the database filesystem
// against the 25 GB floor. An unknowable filesystem warns instead of
// refusing: correctness never depends on the guard.
func checkMigrateDisk(ctx context.Context, conn *sqlite.Conn) (int64, bool) {
	if err := ctx.Err(); err != nil {
		return -1, false
	}
	free, ok := migrateFreeBytes(dbMainFileOnConn(conn))
	if !ok {
		return -1, true
	}
	return int64(free), free >= migrateDiskFloorBytes
}

// dbMainFileOnConn reads the main database file from the connection's
// own database list, so the disk probe needs no stored path.
func dbMainFileOnConn(conn *sqlite.Conn) string {
	file := ""
	_ = sqlitex.ExecuteTransient(conn, `PRAGMA database_list`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnText(1) == "main" {
				file = stmt.ColumnText(2)
			}
			return nil
		},
	})
	return file
}

// Migrate runs the Phase 1–4 conversion pass: drain, per-session
// conversion, search consolidation, and cleanup. Phase 0 stays a separate
// read-only call (PlanMigration), so --dry-run never opens a writer. A
// data mismatch rolls its session back, clears the input proof, records
// the rollback, and continues; a defect mismatch halts the run naming the
// session and the dimension; data rollbacks past the systematic-cause
// threshold halt the run. The context is honored at every session and
// phase boundary: an interrupt finishes the current session's transaction
// and returns the partial report.
func (s *Store) Migrate(ctx context.Context, opts MigrateOptions) (MigrateResult, error) {
	var result MigrateResult
	if err := s.requireGenerationSupport(); err != nil {
		return result, err
	}
	plan, err := s.PlanMigration(ctx)
	if err != nil {
		return result, err
	}
	total := len(plan.Sessions)
	if opts.Limit > 0 && total > opts.Limit {
		total = opts.Limit
		plan.Sessions = plan.Sessions[:opts.Limit]
	}
	progress := opts.Progress
	report := func(phase MigrationPhase, done int, bytes int64) {
		if progress == nil {
			return
		}
		progress(MigrateProgress{Phase: phase, SessionsDone: done, SessionsTotal: total, BytesFreed: bytes})
	}
	report(MigrationPhasePreflight, 0, 0)

	drained, drainBytes, err := s.migrateDrain(ctx, &result)
	if err != nil {
		return result, err
	}
	result.BytesFreed += drainBytes
	result.Marked += drained
	report(MigrationPhaseDrain, 0, result.BytesFreed)

	var processed, rollbacks int64
	for i, sessionID := range plan.Sessions {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("store: migration interrupted before session %s: %w; %d of %d sessions converted and the rest keep their state for the next pass", sessionID, err, i, total)
		}
		before, _ := s.sessionGenerationsFootprint(ctx, sessionID)
		outcome, sessionErr := s.MigrateSession(ctx, sessionID)
		if sessionErr != nil {
			var dataRollback *MigrateDataRollbackError
			if errors.As(sessionErr, &dataRollback) && outcome == MigrateOutcomeRolledBack {
				processed++
				result.RolledBack++
				rollbacks++
				result.Rollbacks = append(result.Rollbacks, MigrateRollback{
					SessionID: dataRollback.SessionID,
					Dimension: dataRollback.Dimension,
					Reason:    dataRollback.Reason,
				})
				report(MigrationPhaseConvertSessions, i+1, result.BytesFreed)
				if err := checkMigrateRollbackHalt(rollbacks, processed); err != nil {
					return result, err
				}
				continue
			}
			return result, sessionErr
		}
		processed++
		switch outcome {
		case MigrateOutcomeConverted:
			result.Converted++
			result.BytesFreed += before.Bytes
		case MigrateOutcomeRolledBack:
			result.RolledBack++
			rollbacks++
		case MigrateOutcomeMarked:
			result.Marked++
		default:
			result.Skipped++
		}
		report(MigrationPhaseConvertSessions, i+1, result.BytesFreed)
		if err := checkMigrateRollbackHalt(rollbacks, processed); err != nil {
			return result, err
		}
	}

	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("store: migration interrupted before search consolidation: %w; converted sessions keep their state and the next pass resumes at Phase 3", err)
	}
	consolidated, err := s.migrateConsolidateSearch(ctx)
	if err != nil {
		return result, err
	}
	result.SearchConsolidated = consolidated
	report(MigrationPhaseConsolidateSearch, total, result.BytesFreed)

	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("store: migration interrupted before cleanup: %w; converted sessions keep their state and the next pass resumes at Phase 4", err)
	}
	if err := s.migrateCleanup(ctx, &result); err != nil {
		return result, err
	}
	report(MigrationPhaseCleanup, total, result.BytesFreed)
	return result, nil
}

// countMigratePendingIntents counts the sessions holding a pending
// generation intent Phase 1 discards, over the drain scope the caller
// lists on its own connection. Reading intents touches only the owned
// files, never the pool, so the caller's connection stays the only one
// checked out.
func (s *Store) countMigratePendingIntents(ctx context.Context, sessions []schema.SessionID) (int, error) {
	if s.generationArtifacts == nil {
		return 0, nil
	}
	count := 0
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
		if err != nil {
			return count, fmt.Errorf("store: read the pending intent for session %s: %w", sessionID, err)
		}
		if intent != nil {
			count++
		}
	}
	return count, nil
}

// sampleMigrateMismatches scans the first sample of work sessions for
// the mismatches Phase 2d would roll back on: an emitted ref whose
// entry field hashes differently than its integrity digest, and a
// content record whose blob file is missing. Blob bytes are never read
// here (conversion verifies them); a damaged-but-present file is found
// at conversion time, not in the forecast. It reports the sampled
// session count, the sessions carrying at least one mismatch, and the
// mismatched refs among them.
func (s *Store) sampleMigrateMismatches(ctx context.Context, conn *sqlite.Conn, sessions []schema.SessionID) (sampled, mismatchSessions, mismatchedRefs int, err error) {
	if len(sessions) == 0 {
		return 0, 0, 0, nil
	}
	if len(sessions) > migratePreflightSampleSessions {
		sessions = sessions[:migratePreflightSampleSessions]
	}
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			return sampled, mismatchSessions, mismatchedRefs, err
		}
		mismatches, err := s.sampleMigrateSessionMismatches(ctx, conn, sessionID)
		if err != nil {
			return sampled, mismatchSessions, mismatchedRefs, err
		}
		sampled++
		if mismatches > 0 {
			mismatchSessions++
			mismatchedRefs += mismatches
		}
	}
	return sampled, mismatchSessions, mismatchedRefs, nil
}

// sampleMigrateSessionMismatches counts one session's sampled mismatches:
// emitted field-against-digest disagreements plus missing blob files,
// over at most migratePreflightSampleRefs content records.
func (s *Store) sampleMigrateSessionMismatches(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID) (int, error) {
	var active string
	if err := sqlitex.ExecuteTransient(conn, `SELECT active_generation_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			active = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read the active generation while sampling session %s: %w", sessionID, err)
	}
	type record struct {
		ref        schema.SourceEntryRef
		digest     string
		relative   string
		byteLength int64
	}
	var records []record
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref, integrity_digest, relative_blob, byte_length FROM session_projection_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref LIMIT ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), active, migratePreflightSampleRefs},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ref, err := schema.NewSourceEntryRef(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			records = append(records, record{ref: ref, digest: stmt.ColumnText(1), relative: stmt.ColumnText(2), byteLength: stmt.ColumnInt64(2)})
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read content records while sampling session %s: %w", sessionID, err)
	}
	mismatches := 0
	for _, content := range records {
		if err := ctx.Err(); err != nil {
			return mismatches, err
		}
		entry, found, err := readMigrateSampleEntry(conn, sessionID, active, content.ref)
		if err != nil {
			return mismatches, err
		}
		if found {
			sum := sha256.Sum256([]byte(harmonizedContentField(entry)))
			if !equalDigests(hex.EncodeToString(sum[:]), content.digest) {
				mismatches++
				continue
			}
		}
		if s.generationArtifacts == nil {
			continue
		}
		present, err := s.generationArtifacts.BlobExists(ctx, sessionID, active, indexformat.ContentRecord{
			Ref:          content.ref,
			RelativeBlob: content.relative,
			ByteLength:   content.byteLength,
			Digest:       content.digest,
		})
		if err != nil {
			return mismatches, fmt.Errorf("store: check blob presence while sampling session %s: %w", sessionID, err)
		}
		if !present {
			mismatches++
		}
	}
	return mismatches, nil
}

// readMigrateSampleEntry reads one legacy entry by source ref, preferring
// the main partition. It reports whether any entry emits the ref.
func readMigrateSampleEntry(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, ref schema.SourceEntryRef) (schema.SessionEntry, bool, error) {
	var entry schema.SessionEntry
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT entry_json FROM session_projection_entries WHERE session_id = ? AND generation_id = ? AND source_entry_ref = ? ORDER BY partition_id LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID, string(ref)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &entry); err != nil {
				return fmt.Errorf("decode sampled entry: %w", err)
			}
			return nil
		},
	}); err != nil {
		return entry, false, fmt.Errorf("store: read the sampled entry for ref %q: %w", ref, err)
	}
	return entry, found, nil
}

// migrateDrain discards the Phase 1 sets (design §7.2): every pending
// generation intent with its staged directory, every superseded
// generation through the sweep, and every reserved .tmp-gen-* directory,
// without replay. It clears indexed_input_hash for every session it
// touched, so the next harvest re-indexes them, and reports the marked
// session count with the bytes freed. A per-session failure is recorded
// as a warning and the pass continues; the session keeps its state, so
// the next pass retries it.
func (s *Store) migrateDrain(ctx context.Context, result *MigrateResult) (int64, int64, error) {
	sessions, err := s.migrateDrainSessions(ctx)
	if err != nil {
		return 0, 0, err
	}
	var marked, bytes int64
	for _, sessionID := range sessions {
		if err := ctx.Err(); err != nil {
			return marked, bytes, fmt.Errorf("store: migration drain interrupted before session %s: %w; drained sessions keep their state and the next pass resumes the drain", sessionID, err)
		}
		// The freed bytes are the owned generations footprint delta
		// around the drain: reserved staging names fall outside the
		// generation listing, so their bytes stay uncounted as
		// immaterial next to the discarded generations.
		before, err := s.sessionGenerationsFootprint(ctx, sessionID)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("drain session %s: %v", sessionID, err))
			continue
		}
		touched, err := s.migrateDrainSession(ctx, sessionID)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("drain session %s: %v", sessionID, err))
			continue
		}
		if touched {
			marked++
			after, err := s.sessionGenerationsFootprint(ctx, sessionID)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("drain session %s: %v", sessionID, err))
				continue
			}
			if after.Bytes < before.Bytes {
				bytes += before.Bytes - after.Bytes
			}
		}
	}
	return marked, bytes, nil
}

// migrateDrainSessions lists every session owning file-backed generation
// rows: the drain scope. Converted sessions own none, so they are never
// touched.
func (s *Store) migrateDrainSessions(ctx context.Context) ([]schema.SessionID, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list drain sessions: %w; nothing was drained", err)
	}
	defer s.pool.Put(conn)
	return migrateDrainSessionsOnConn(conn)
}

// migrateDrainSessionsOnConn lists the drain scope on the caller's
// connection, so a caller that already holds one never checks out a
// second.
func migrateDrainSessionsOnConn(conn *sqlite.Conn) ([]schema.SessionID, error) {
	var sessions []schema.SessionID
	if err := sqlitex.ExecuteTransient(conn, `SELECT DISTINCT session_id FROM session_projection_generations ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = append(sessions, schema.SessionID(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list drain sessions: %w; nothing was drained", err)
	}
	return sessions, nil
}

// migrateDrainSession discards one session's Phase 1 sets and reports
// whether anything changed. The intent goes first (its staged directory
// is then just another non-active directory), then the superseded rows,
// then the non-active directories, then the reserved staging names. The
// active file-backed generation is never touched.
func (s *Store) migrateDrainSession(ctx context.Context, sessionID schema.SessionID) (bool, error) {
	if err := s.requireGenerationSupport(); err != nil {
		return false, err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("store: lock session %s for the migration drain: %w; nothing was drained", sessionID, err)
	}
	defer func() { _ = release() }()
	touched := false
	if s.generationArtifacts != nil {
		intent, err := s.generationArtifacts.ReadIntent(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("store: read the pending intent for session %s: %w; nothing was drained", sessionID, err)
		}
		if intent != nil {
			if err := s.generationArtifacts.ClearIntent(ctx, sessionID); err != nil {
				return false, fmt.Errorf("store: discard the pending intent for session %s: %w; nothing was drained", sessionID, err)
			}
			touched = true
		}
	}
	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return false, err
	}
	deleted, err := s.deleteSupersededGenerationRows(ctx, sessionID, active)
	if err != nil {
		return false, err
	}
	if deleted.Total() > 0 {
		touched = true
	}
	if s.generationArtifacts != nil {
		dirs, err := s.removeLeftoverGenerationDirs(ctx, sessionID, active)
		if err != nil {
			return false, err
		}
		if dirs > 0 {
			touched = true
		}
		reserved, err := s.generationArtifacts.RemoveReservedStagingDirs(ctx, sessionID)
		if err != nil {
			return false, fmt.Errorf("store: remove reserved staging directories for session %s: %w; drained rows stay deleted and the next pass retries the directories", sessionID, err)
		}
		if reserved > 0 {
			touched = true
		}
	}
	if touched {
		if err := s.clearIndexedInputHash(ctx, sessionID); err != nil {
			return false, err
		}
	}
	return touched, nil
}

// clearIndexedInputHash clears the consumed-input proof for one session,
// so the repair predicate re-selects it and the next harvest re-indexes
// it. The drain, the data rollbacks, and corruption repair share this
// marking; settled refusals never pass through it.
func (s *Store) clearIndexedInputHash(ctx context.Context, sessionID schema.SessionID) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to mark session %s for re-index: %w; the input proof is unchanged", sessionID, err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET indexed_input_hash = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: mark session %s for re-index: %w; the input proof is unchanged", sessionID, err)
	}
	return nil
}

// migrateConsolidateSearch runs Phase 3 in one transaction (design §7.2):
// drop the three session_entries triggers, re-create them against
// session_search_fts, rebuild over the union view, drop the old index,
// and clear needs_rebuild (this rebuild supersedes any pending one). It
// reports whether it consolidated; a missing old index skips the phase.
// A crash rolls the one transaction back and the old index stays intact,
// so the re-run repeats Phase 3.
func (s *Store) migrateConsolidateSearch(ctx context.Context) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection for search consolidation: %w; the index is unchanged", err)
	}
	defer s.pool.Put(conn)
	if !tableExistsOnConn(conn, "session_entries_fts") {
		return false, nil
	}
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	for _, trigger := range []string{"session_entries_ai", "session_entries_ad", "session_entries_au"} {
		if err := sqlitex.ExecuteTransient(conn, `DROP TRIGGER IF EXISTS `+trigger, nil); err != nil {
			txnErr = fmt.Errorf("store: drop trigger %s for search consolidation: %w; the index is unchanged", trigger, err)
			return false, txnErr
		}
	}
	for _, trigger := range []string{migrateSearchTriggerAI, migrateSearchTriggerAD, migrateSearchTriggerAU} {
		if err := sqlitex.ExecuteTransient(conn, trigger, nil); err != nil {
			txnErr = fmt.Errorf("store: retarget a session_entries trigger at session_search_fts for search consolidation: %w; the index is unchanged", err)
			return false, txnErr
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, nil); err != nil {
		txnErr = fmt.Errorf("store: rebuild the consolidated search index: %w; the old index is intact and the re-run repeats Phase 3", err)
		return false, txnErr
	}
	if err := sqlitex.ExecuteTransient(conn, `DROP TABLE session_entries_fts`, nil); err != nil {
		txnErr = fmt.Errorf("store: drop the retired search index: %w; the rebuilt index stands and the re-run finishes the drop", err)
		return false, txnErr
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1`, nil); err != nil {
		txnErr = fmt.Errorf("store: clear the search rebuild flag after consolidation: %w; the index is rebuilt but search still refuses until the flag clears", err)
		return false, txnErr
	}
	if err := ctx.Err(); err != nil {
		txnErr = fmt.Errorf("store: search consolidation interrupted: %w; the transaction rolled back and the old index is intact", err)
		return false, txnErr
	}
	return true, nil
}

// The three retargeted session_entries triggers (design §7.2 Phase 3):
// after consolidation the mirror keeps serving non-native sessions, so
// its writes index into session_search_fts. The rowid space stays split
// by BodyRowIDBase: mirror rowids below it, body ids at or above it.
const (
	migrateSearchTriggerAI = `CREATE TRIGGER session_entries_ai AFTER INSERT ON session_entries BEGIN
    INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END`
	migrateSearchTriggerAD = `CREATE TRIGGER session_entries_ad AFTER DELETE ON session_entries BEGIN
    INSERT INTO session_search_fts(session_search_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
END`
	migrateSearchTriggerAU = `CREATE TRIGGER session_entries_au AFTER UPDATE ON session_entries BEGIN
    INSERT INTO session_search_fts(session_search_fts, rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES ('delete', old.rowid, old.content_preview, old.tool_input, old.tool_output, old.session_id, old.entry_index);
    INSERT INTO session_search_fts(rowid, content_preview, tool_input, tool_output, session_id, entry_index)
    VALUES (new.rowid, new.content_preview, new.tool_input, new.tool_output, new.session_id, new.entry_index);
END`
)

// migrateCleanup runs Phase 4 (design §7.2): remove leftover generation
// directories of converted sessions, delete leftover mirror rows of
// harmonized sessions, sweep every flagged session, and evaluate the five
// Release N+1 preconditions.
func (s *Store) migrateCleanup(ctx context.Context, result *MigrateResult) error {
	harmonized, err := s.migrateHarmonizedSessions(ctx)
	if err != nil {
		return err
	}
	for _, sessionID := range harmonized {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("store: migration cleanup interrupted before session %s: %w; converted sessions keep their state and the next pass resumes cleanup", sessionID, err)
		}
		if err := s.deleteConvertedMirrorRows(ctx, sessionID); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("cleanup mirror rows of session %s: %v", sessionID, err))
			continue
		}
	}
	flagged, err := s.migrateFlaggedSessions(ctx)
	if err != nil {
		return err
	}
	converted := make(map[schema.SessionID]bool, len(harmonized))
	for _, sessionID := range harmonized {
		converted[sessionID] = true
	}
	for _, sessionID := range flagged {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("store: migration cleanup interrupted before flagged session %s: %w; the flag stays set and the next pass retries it", sessionID, err)
		}
		// Owned files go only for converted sessions: a flagged
		// file-backed session still reads its generation directory,
		// so the sweep below clears its orphans without touching a
		// live file.
		if s.generationArtifacts != nil && converted[sessionID] {
			footprint, err := s.generationArtifacts.RemoveConvertedSessionFiles(ctx, sessionID)
			if err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("cleanup files of session %s: %v", sessionID, err))
				continue
			}
			result.BytesFreed += footprint.Bytes
		}
	}
	swept, err := s.SweepFlaggedSessions(ctx)
	if err != nil {
		return fmt.Errorf("store: sweep flagged sessions in migration cleanup: %w", err)
	}
	for _, warning := range swept.Warnings {
		result.Warnings = append(result.Warnings, warning.Error())
	}
	preconditions, err := s.EvaluateRetirementPreconditions(ctx)
	if err != nil {
		return err
	}
	result.Preconditions = preconditions
	result.ReadyForNextRelease = true
	for _, precondition := range preconditions {
		if !precondition.Passed {
			result.ReadyForNextRelease = false
			break
		}
	}
	return nil
}

// migrateHarmonizedSessions lists the sessions whose active generation is
// harmonized, in session-id order.
func (s *Store) migrateHarmonizedSessions(ctx context.Context) ([]schema.SessionID, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list harmonized sessions: %w", err)
	}
	defer s.pool.Put(conn)
	var sessions []schema.SessionID
	if err := sqlitex.ExecuteTransient(conn, `SELECT s.session_id FROM sessions s
WHERE s.active_generation_id IS NOT NULL
AND EXISTS (SELECT 1 FROM session_generations g
  WHERE g.session_id = s.session_id AND g.generation_id = s.active_generation_id)
ORDER BY s.session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = append(sessions, schema.SessionID(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list harmonized sessions: %w", err)
	}
	return sessions, nil
}

// migrateFlaggedSessions lists the sessions carrying the sweep flag, in
// session-id order.
func (s *Store) migrateFlaggedSessions(ctx context.Context) ([]schema.SessionID, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list flagged sessions: %w", err)
	}
	defer s.pool.Put(conn)
	var sessions []schema.SessionID
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id FROM sessions WHERE content_sweep_pending = 1 ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = append(sessions, schema.SessionID(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list flagged sessions: %w", err)
	}
	return sessions, nil
}

// EvaluateRetirementPreconditions evaluates the five Release N+1 guards
// (design §7.1). A failing guard names its row count, why it blocks the
// upgrade, and the fix: run peasant migrate with Release N, then upgrade.
func (s *Store) EvaluateRetirementPreconditions(ctx context.Context) ([]RetirementPreconditionStatus, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to evaluate the retirement preconditions: %w", err)
	}
	defer s.pool.Put(conn)
	counts := []struct {
		name  RetirementPrecondition
		query string
		block string
	}{
		{RetirementPreconditionProjectionGenerationsEmpty, `SELECT COUNT(*) FROM session_projection_generations`, "unconverted file-backed generations remain"},
		{RetirementPreconditionProjectionEntriesEmpty, `SELECT COUNT(*) FROM session_projection_entries`, "unconverted file-backed entries remain"},
		{RetirementPreconditionProjectionContentEmpty, `SELECT COUNT(*) FROM session_projection_content`, "unconverted file-backed content records remain"},
		{RetirementPreconditionNoNativeFullContentRows, `SELECT COUNT(*) FROM session_entry_full_content f JOIN sessions s USING(session_id) WHERE s.active_generation_id IS NOT NULL`, "native full-content rows remain beside their harmonized bodies"},
	}
	statuses := make([]RetirementPreconditionStatus, 0, len(AllRetirementPreconditions))
	for _, count := range counts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rows int64
		if err := sqlitex.ExecuteTransient(conn, count.query, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("store: evaluate %s: %w", count.name, err)
		}
		status := RetirementPreconditionStatus{Name: count.name, RowCount: rows, Passed: rows == 0}
		if !status.Passed {
			status.Detail = fmt.Sprintf("%s (%d rows); run `peasant migrate` with Release N, then upgrade", count.block, rows)
		}
		statuses = append(statuses, status)
	}
	ftsPresent := tableExistsOnConn(conn, "session_entries_fts")
	ftsStatus := RetirementPreconditionStatus{Name: RetirementPreconditionSessionEntriesFTSAbsent, Passed: !ftsPresent}
	if ftsPresent {
		ftsStatus.RowCount = 1
		ftsStatus.Detail = "the retired search index is still present, which means search consolidation never ran (on a fresh install Phase 3 is the only phase with work); run `peasant migrate` with Release N, then upgrade"
	}
	statuses = append(statuses, ftsStatus)
	ordered := make([]RetirementPreconditionStatus, 0, len(statuses))
	for _, name := range AllRetirementPreconditions {
		for _, status := range statuses {
			if status.Name == name {
				ordered = append(ordered, status)
			}
		}
	}
	return ordered, nil
}
