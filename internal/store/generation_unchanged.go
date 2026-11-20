package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ManagedGenerationUnchangedSettleResult reports the
// generation that is the active read authority afterwards: the settled source
// generation.
type ManagedGenerationUnchangedSettleResult struct {
	GenerationID string
}

// ManagedGenerationUnchangedSettleError is the actionable refusal of one unchanged
// settlement. It names what was refused, why, the impact, and the caller's
// next step; nothing changed.
type ManagedGenerationUnchangedSettleError struct {
	SessionID          schema.SessionID
	SourceGenerationID string
	Detail             string
}

func (e *ManagedGenerationUnchangedSettleError) Error() string {
	return fmt.Sprintf(
		"store: refuse the unchanged settlement of session %s generation %s: source-unsupported; where: store.SettleManagedGenerationUnchanged, before any row changed; impact: the active authority is unchanged and the session stays refresh work; fix: %s",
		e.SessionID, e.SourceGenerationID, e.Detail)
}

// ManagedGenerationUnchangedProof describes the actual captured input and SQL
// state to compare. Expected is captured after the metadata pair was installed.
type ManagedGenerationUnchangedProof struct {
	SessionID     ingest.SessionID
	GenerationID  string
	Metadata      ingest.UnifiedMetadata
	InputDigest   string
	IndexerTarget int
	FormatTarget  int
	Expected      *ingest.SessionIndexState
	UpdatedAtMs   int64
	// Validated carries a validator's proven answer for the active
	// generation. When set, the settle replaces the two context digest terms
	// with the candidate comparison and runs it again under the lock.
	Validated *ingest.ValidatedContext
}

// ProveManagedGenerationUnchanged reads the catalog and index in one snapshot.
// It never reads generation mappings, content descriptors, or title references.
func (s *Store) ProveManagedGenerationUnchanged(ctx context.Context, p ManagedGenerationUnchangedProof) (result ingest.UnchangedProof, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: read unchanged proof for %s: %w; restore database access and retry", p.SessionID, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	return evaluateUnchangedTermsOnConn(conn, p, termModeStoredDigests)
}

// SettleManagedGenerationUnchanged repeats the proof under the session lock
// and an immediate transaction, so changed authority cannot be settled. A
// request that carries a validated answer settles only the generation that
// answer names, and only while it is still active: the non-context terms run
// again and the candidate's context is compared with the stored context rows
// again, under the lock. The candidate itself is never recomputed.
func (s *Store) SettleManagedGenerationUnchanged(ctx context.Context, p ManagedGenerationUnchangedProof) (result ManagedGenerationUnchangedSettleResult, err error) {
	if s.sessionLocker == nil || p.UpdatedAtMs <= 0 {
		return result, fmt.Errorf("store: settle unchanged generation for %s: session locking or capture time is missing; nothing was written; configure generation support and supply the capture time", p.SessionID)
	}
	release, err := s.sessionLocker.LockExclusive(ctx, p.SessionID)
	if err != nil {
		return result, fmt.Errorf("store: lock session %s for unchanged settlement: %w; nothing was written; retry", p.SessionID, err)
	}
	defer release()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: take connection for unchanged settlement of %s: %w; nothing was written; retry", p.SessionID, err)
	}
	defer s.pool.Put(conn)
	end, err := beginWrite(conn, "settle the unchanged generation of session "+string(p.SessionID))
	if err != nil {
		return result, err
	}
	defer end(&err)
	mode := termModeStoredDigests
	if p.Validated != nil {
		if p.Validated.GenerationID != p.GenerationID {
			return result, &ingest.StaleIndexWorkError{SessionID: p.SessionID}
		}
		mode = termModeSettleValidated
	}
	proof, err := evaluateUnchangedTermsOnConn(conn, p, mode)
	if err != nil {
		return result, err
	}
	if !proof.Proven {
		return result, &ingest.StaleIndexWorkError{SessionID: p.SessionID}
	}
	stored, readErr := readCapturedStatsOnConn(conn, p.SessionID)
	if readErr != nil && !errors.Is(readErr, ErrNoCapturedStats) {
		return result, fmt.Errorf("store: read parse counts for unchanged settlement of %s: %w; nothing was written; restore database access and retry", p.SessionID, readErr)
	}
	if readErr != nil || stored.TurnCount == nil || stored.ConversationTurnCount == nil || stored.InputSubmissionCount == nil {
		return result, &ManagedGenerationUnchangedSettleError{
			SessionID: p.SessionID, SourceGenerationID: p.GenerationID,
			Detail: "the stored parse counts are incomplete; re-parse the source to restore authoritative counts",
		}
	}
	// A parse writes the builder's stats: its metadata half's non-strict
	// stats with the projection's strict counts. A validated settle carries
	// that half, so it writes the half's stats; the stored strict counts are
	// the projection's for these bytes, as on a proven settle.
	if p.Validated != nil {
		p.Metadata.Stats = p.Validated.Candidate.Metadata.Stats
	}
	p.Metadata.Stats.TurnCount = *stored.TurnCount
	p.Metadata.Stats.InputSubmissionCount = stored.InputSubmissionCount
	conversation := int64(*stored.ConversationTurnCount)
	p.Metadata.Stats.ConversationTurnCount = &conversation
	stats := capturedStatsForHarnessWrite(p.SessionID, p.Metadata.Stats, seedJSONForStats(p.Metadata.Stats), p.UpdatedAtMs)
	var warnings []schema.DiagnosticEntry
	stats.Overflow, warnings, err = retainPriorStatsOverflowOnConn(conn, stats)
	if err != nil {
		return result, err
	}
	if len(warnings) != 0 {
		return result, fmt.Errorf("store: settle unchanged generation for %s: stats overflow needs diagnostic bookkeeping outside this small transaction; nothing was written; use the ordinary parse path", p.SessionID)
	}
	skip := unchangedSettleMutation("")
	if p.Validated != nil && settleValidatedMutationForTest != nil {
		skip = settleValidatedMutationForTest()
	}
	if skip != settleSkipsStatsWrite {
		err = sqlitex.Execute(conn, sqlUpsertCapturedStats, &sqlitex.ExecOptions{Args: []any{string(stats.SessionID), nullableArg(stats.TurnCount), nullableArg64(stats.InputSubmissionCount), nullableArg(stats.ToolCallCount), nullableArg(stats.SubagentCount), nullableArg64(stats.DurationMs), nullableArg(stats.TokensIn), nullableArg(stats.TokensOut), nullableArg(stats.ThoughtTokens), nullableArg(stats.CachedReadTokens), nullableArg(stats.CachedWriteTokens), nullableStr(stats.SeedJSON), string(stats.Source), stats.UpdatedAtMs, nullableStr(stats.Overflow), nullableArg(stats.ConversationTurnCount), nullableArg(stats.ConversationTurnCount)}})
		if err != nil {
			return result, fmt.Errorf("store: merge unchanged stats for %s: %w; transaction rolled back; retry", p.SessionID, err)
		}
	}
	if skip != settleSkipsMirrorWrite {
		if err = syncInputSubmissionMirrorOnConn(conn, p.SessionID); err != nil {
			return result, err
		}
	}
	revision := p.Expected.PublicationCaptureRevision
	if revision > 0 {
		if err = checkPublicationIndexRevision(conn, p.SessionID, revision); err != nil {
			return result, err
		}
		if err = sqlitex.Execute(conn, `UPDATE session_content_captures SET publication_capture_revision=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{revision, string(p.SessionID)}}); err != nil {
			return result, err
		}
		if err = stampPublicationIndex(conn, p.SessionID, revision); err != nil {
			return result, err
		}
	}
	return ManagedGenerationUnchangedSettleResult{GenerationID: p.GenerationID}, nil
}

// unchangedSettleMutation names one write a test removes from the validated
// settle, to show the whole-store parity test notices it.
type unchangedSettleMutation string

const (
	settleSkipsStatsWrite  unchangedSettleMutation = "skipped-stats-write"
	settleSkipsMirrorWrite unchangedSettleMutation = "skipped-mirror-write"
)

// settleValidatedMutationForTest is nil in production. A test sets it to
// remove one write from the validated settle; only a settle that carries a
// validated answer reads it, and no production file assigns it (a source
// guard enforces that).
var settleValidatedMutationForTest func() unchangedSettleMutation
