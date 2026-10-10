package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

type generationIndexFormat struct{}

var _ IndexFormat = generationIndexFormat{}

// V2IndexFormat returns the concrete managed-generation index-format handler. A
// store that installs or reads managed generations must register it with
// WithIndexFormats; without it, activation and generation snapshots fail closed
// instead of pretending an unknown representation is an empty transcript.
func V2IndexFormat() IndexFormat { return generationIndexFormat{} }

// Version reports the concrete managed-generation format version.
func (generationIndexFormat) Version() int { return 2 }

// Validate refuses any result that is not a fully validated indexformat.V2, so a
// half-built generation can never reach the database.
func (generationIndexFormat) Validate(result indexformat.Result) error {
	value, ok := result.(indexformat.V2)
	if !ok {
		return fmt.Errorf("index format 2 requires an indexformat.V2 result, got %T; no generation was activated; use the matching concrete indexer result", result)
	}
	if err := value.Validate(); err != nil {
		return fmt.Errorf("index format 2 rejected the managed generation before activation: %w", err)
	}
	return nil
}

// Write installs one harmonized generation in the caller's transaction
// (design §4.1 C1–C6, minus the lock, the batching, and the C4 stamps the
// common writer owns). The generation's objects are already staged — the
// foreign keys prove it, so an unstaged candidate refuses here instead of
// writing partial rows. It enforces the completeness transition
// transactionally over both catalog tables: an incomplete_new candidate is
// refused when a complete generation already exists for the session, so a
// last-good complete generation is never replaced by an incomplete capture.
// Then it inserts the catalog row with its flattened metadata, the
// stats-excluded capture anchor, the activation binding, and the prior
// evidence, plus every child, mapping, descriptor, alias, section, segment,
// and evidence row; points the session at the new generation; merges the
// captured stats with the mirror in the same transaction; and remaps the
// carried annotations against the new main entries. A generation identifier
// that already names a harmonized row commits only when the stored binding
// matches (AlreadyCommitted); any other reuse refuses, as does an
// identifier present in the file-backed table, which only the migration
// moves.
func (generationIndexFormat) Write(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID, result indexformat.Result) (entries []schema.SessionEntry, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, ok := asV2Value(result)
	if !ok {
		return nil, generationIndexFormat{}.Validate(result)
	}
	generation := value.Generation
	if generation.Metadata.SessionID != sessionID {
		return nil, fmt.Errorf("store: managed generation for session %s names session %s; no generation was activated; build the generation for its own captured metadata", sessionID, generation.Metadata.SessionID)
	}
	if generation.Completeness == indexformat.GenerationCompletenessIncompleteNew {
		if err := refuseIncompleteWhenCompleteExists(conn, sessionID, generation.ID); err != nil {
			return nil, err
		}
	}
	prepared, err := prepareHarmonizedCandidate(sessionID, generation, nil)
	if err != nil {
		return nil, err
	}
	if err := refuseReusedGenerationIdentity(conn, sessionID, generation.ID, prepared.binding); err != nil {
		return nil, err
	}
	previous, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if err := insertHarmonizedGenerationOnConn(conn, prepared, value.PriorEvidence, now); err != nil {
		return nil, err
	}
	var previousHarmonized *string
	if previous != nil {
		if generationIsHarmonized(conn, sessionID, *previous) {
			previousHarmonized = previous
		}
	}
	if err := pointSessionAtGenerationOnConn(conn, sessionID, generation); err != nil {
		return nil, err
	}
	if err := remapHarmonizedAnnotations(conn, sessionID, generation.Main.Entries, previousHarmonized, nil); err != nil {
		return nil, err
	}
	return generation.Main.Entries, nil
}

// Delete removes every generation-scoped row for a session when the
// session's representation is replaced by a different format. It sets the
// sweep flag before removing anything, so a crash leaves the leftovers
// flagged for the per-session sweep; it removes no objects itself — the
// unreferenced entry rows and blobs go through the verified sweep (§4.5).
// The table list is the shared reclaim inventory (reclaimTableNames): the
// same closed set the reclaim and the sweep delete per generation, so the
// two hand-maintained lists stay in step by construction.
func (generationIndexFormat) Delete(ctx context.Context, conn *sqlite.Conn, sessionID schema.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
		return fmt.Errorf("store: flag session %s for sweep before representation replacement: %w; the prior representation is preserved", sessionID, err)
	}
	for _, table := range reclaimTableNames {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM `+table+` WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
			return fmt.Errorf("store: clear managed generations for session %s before representation replacement: %w; the transaction was refused and the prior representation is preserved", sessionID, err)
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
		return fmt.Errorf("store: clear the active generation for session %s before representation replacement: %w; the transaction was refused and the prior representation is preserved", sessionID, err)
	}
	return nil
}

// generationIsHarmonized reports whether the named generation of the
// session lives in the harmonized catalog (as opposed to the file-backed
// one Release N still reads).
func generationIsHarmonized(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) bool {
	harmonized := false
	_ = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { harmonized = true; return nil },
	})
	return harmonized
}

// refuseReusedGenerationIdentity enforces the immutable identity (design
// §4.1): a generation identifier that already has a harmonized row commits
// only if the stored candidate digest equals the candidate's (the retry is
// idempotent); any other reuse refuses, because immutable identifiers
// cannot name two candidates. An identifier present in the file-backed
// table is always refused, because only the migration moves it.
// candidate_digest is NOT NULL, so a binding is never unknown.
func refuseReusedGenerationIdentity(conn *sqlite.Conn, sessionID schema.SessionID, generationID, binding string) error {
	stored := ""
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			stored = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: check generation identity for session %s: %w; no generation was activated", sessionID, err)
	}
	if found {
		if stored != binding {
			return fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is already installed with a different candidate binding; immutable identifiers cannot be reused; the installed generation is unchanged", generationID, sessionID)
		}
		return nil
	}
	fileBacked := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_projection_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { fileBacked = true; return nil },
	}); err != nil {
		return fmt.Errorf("store: check the file-backed catalog for session %s: %w; no generation was activated", sessionID, err)
	}
	if fileBacked {
		return fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is installed in the file-backed catalog, which only the migration moves; the installed generation is unchanged", generationID, sessionID)
	}
	return nil
}

// bindNullableInt64Value binds a nullable *int64 parameter. A nil bound value
// stays NULL, matching the value sqlitex.ExecuteTransient wrote before the
// statements were reused.
func bindNullableInt64Value(stmt *sqlite.Stmt, param int, value *int64) {
	if value == nil {
		stmt.BindNull(param)
		return
	}
	stmt.BindInt64(param, *value)
}

func pointSessionAtGenerationOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generation indexformat.Generation) error {
	var root any
	if generation.Metadata.RootSessionID != nil {
		root = string(*generation.Metadata.RootSessionID)
	}
	var purpose any
	if generation.Metadata.Purpose != "" {
		purpose = string(generation.Metadata.Purpose)
	}
	var inputCount any
	if generation.Metadata.Stats.InputSubmissionCount != nil {
		inputCount = *generation.Metadata.Stats.InputSubmissionCount
	}
	var adapter any
	if generation.Metadata.AdapterVersion != nil {
		if *generation.Metadata.AdapterVersion <= 0 {
			return fmt.Errorf("store: managed generation %s for session %s carries non-positive adapter revision; no generation was activated; omit unknown provenance instead", generation.ID, sessionID)
		}
		adapter = *generation.Metadata.AdapterVersion
	}
	// The logical parent is durable relationship evidence, not the
	// availability cache. sessions.parent_id is the FK-safe cache: the durable
	// started_by target when it names a stored session, else NULL so an
	// admitted child survives an absent, unselected or cyclic parent.
	logicalParent := durableParentTarget(generation.Metadata.Relationships)
	var parent any
	if logicalParent != nil {
		exists := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
			Args:       []any{string(*logicalParent)},
			ResultFunc: func(*sqlite.Stmt) error { exists = true; return nil },
		}); err != nil {
			return fmt.Errorf("store: resolve logical parent for session %s generation %s: %w; no generation was activated", sessionID, generation.ID, err)
		}
		if exists {
			parent = string(*logicalParent)
		}
	}
	// The pointing UPDATE names session facts the row-version trigger watches
	// (parent_id, model_harness, start_ms, end_ms), so the trigger clears the
	// recorded publication provenance. Read it first and re-state it right
	// after the UPDATE: pointing a session at a generation is not a new
	// capture, so it must not itself invalidate the session's recorded capture
	// agreement. The capture revision is deliberately NOT re-stated here; the
	// index writer's success stamp remains its only evidence.
	var provenance string
	if err := sqlitex.ExecuteTransient(conn, `SELECT cwd_provenance_kind FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			provenance = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read recorded publication provenance before pointing session %s at generation %s: %w; no generation was activated; restore database access and retry activation", sessionID, generation.ID, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = ?, root_session_id = ?, session_purpose = ?, input_submission_count = ?, adapter_version = ?, start_ms = ?, end_ms = ?, model_harness = ?, parent_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{
		generation.ID, root, purpose, inputCount, adapter, generation.Metadata.Timestamp.Start, generation.Metadata.Timestamp.End, string(generation.Metadata.ModelHarness), parent, string(sessionID),
	}}); err != nil {
		return fmt.Errorf("store: point session %s at generation %s: %w; no generation was activated", sessionID, generation.ID, err)
	}
	if provenance != "" {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET cwd_provenance_kind = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{provenance, string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: retain recorded publication provenance after pointing session %s at generation %s: %w; the activation transaction rolled back and the prior generation is visible; repair database access and retry activation", sessionID, generation.ID, err)
		}
	}
	// session_metrics.turn_count and tool_calls are derived mirrors of
	// Metadata.Stats, never a second authority. The title is derived from the
	// generation's TitleRefs so ordinary list reads agree with the snapshot
	// without a separate metadata upsert. INSERT ... ON CONFLICT keeps a
	// session without a metrics row valid.
	title := deriveGenerationTitle(generation)
	var titleValue any
	if title != "" {
		titleValue = title
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_metrics (session_id, turn_count, tool_calls, title) VALUES (?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET turn_count = excluded.turn_count, tool_calls = excluded.tool_calls, title = excluded.title`, &sqlitex.ExecOptions{Args: []any{
		string(sessionID), generation.Metadata.Stats.TurnCount, generation.Metadata.Stats.ToolCallCount, titleValue,
	}}); err != nil {
		return fmt.Errorf("store: derive turn_count mirror for session %s generation %s: %w; no generation was activated", sessionID, generation.ID, err)
	}
	return nil
}

// refuseIncompleteWhenCompleteExists implements the completeness transition:
// incomplete_new is the one first-discovery exception and is allowed only when
// no complete generation exists for the session, in either catalog. Retrying
// the same incomplete identifier is idempotent and allowed; replacing any
// other generation while a complete last-good exists is refused
// transactionally.
func refuseIncompleteWhenCompleteExists(conn *sqlite.Conn, sessionID schema.SessionID, candidateID string) error {
	completeID := ""
	collect := func(statement string) error {
		return sqlitex.ExecuteTransient(conn, statement, &sqlitex.ExecOptions{
			Args: []any{string(sessionID)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				completeID = stmt.ColumnText(0)
				return nil
			},
		})
	}
	if err := collect(`SELECT generation_id FROM session_generations WHERE session_id = ? AND completeness = 'complete' LIMIT 1`); err != nil {
		return fmt.Errorf("store: check completeness transition for session %s: %w; no generation was activated", sessionID, err)
	}
	if completeID == "" {
		if err := collect(`SELECT generation_id FROM session_projection_generations WHERE session_id = ? AND completeness = 'complete' LIMIT 1`); err != nil {
			return fmt.Errorf("store: check completeness transition for session %s: %w; no generation was activated", sessionID, err)
		}
	}
	if completeID != "" && completeID != candidateID {
		return fmt.Errorf("store: refuse incomplete generation %s for session %s: a complete generation %s is the last-good read authority; incomplete_new is the first-discovery exception only and never replaces it", candidateID, sessionID, completeID)
	}
	return nil
}

// durableParentTarget returns the durable started_by target when the
// relationship evidence names a known retained session. It mirrors the
// snapshot's logical-parent derivation so activation and reads agree.
func durableParentTarget(relationships []schema.SessionRelationship) *schema.SessionID {
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

// deriveGenerationTitle derives the display title from the generation's
// TitleRefs: the first ref whose main entry carries usable prose, reduced to
// its first non-empty line. It never invents a title when no ref yields one.
func deriveGenerationTitle(generation indexformat.Generation) string {
	if len(generation.TitleRefs) == 0 {
		return ""
	}
	byRef := make(map[schema.SourceEntryRef]schema.SessionEntry, len(generation.Main.Entries))
	for _, entry := range generation.Main.Entries {
		if entry.SourceEntryRef != "" {
			byRef[entry.SourceEntryRef] = entry
		}
	}
	for _, ref := range generation.TitleRefs {
		entry, ok := byRef[ref]
		if !ok {
			continue
		}
		var text string
		if entry.ContentPreview != nil && *entry.ContentPreview != "" {
			text = *entry.ContentPreview
		} else if entry.ToolInput != nil && *entry.ToolInput != "" {
			text = *entry.ToolInput
		} else if entry.ToolOutput != nil && *entry.ToolOutput != "" {
			text = *entry.ToolOutput
		}
		if title := firstTitleLine(text); title != "" {
			return title
		}
	}
	return ""
}

func firstTitleLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if runes := []rune(trimmed); len(runes) > 80 {
				return string(runes[:77]) + "..."
			}
			return trimmed
		}
	}
	return ""
}
