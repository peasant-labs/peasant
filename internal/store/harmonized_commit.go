package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// resolveStoreWriteConfig resolves the store lanes' budgets: the configured
// value when the opener passed one, otherwise the shipped defaults at the
// host's worker count. Every staging and activation batch asks the one
// splitter over these knobs.
func resolveStoreWriteConfig(configured *ingest.WriteConfig) ingest.WriteConfig {
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if configured == nil {
		return ingest.DefaultWriteConfig(workers)
	}
	return configured.WithDefaults(workers)
}

// writeConfigForLane returns the store lanes' resolved budgets.
func (s *Store) writeConfigForLane() ingest.WriteConfig {
	return s.writeConfig
}

// Writer seam stages for the crash-injection family: production leaves the
// hook nil and every stage runs to completion; a test sets it to fail a
// named stage and prove the seam leaves exactly the old or the new
// generation behind.
const (
	// harmonizedSeamAfterPrepare fires after P1–P4 prepared the candidate
	// and before any object is staged.
	harmonizedSeamAfterPrepare = "after-prepare"
	// harmonizedSeamBetweenStageTxns fires after each committed staging
	// transaction except the last, so a budget-split stage can stop with
	// the flag set and partial objects staged.
	harmonizedSeamBetweenStageTxns = "between-stage-txns"
	// harmonizedSeamBeforeCommit fires after staging verified and before
	// the activation commit opens.
	harmonizedSeamBeforeCommit = "before-commit"
	// harmonizedSeamAtCommit fires inside the activation commit before it
	// releases its savepoint: an error rolls the whole commit back.
	harmonizedSeamAtCommit = "at-commit"
	// harmonizedSeamAfterCommit fires after the activation commit and
	// before the post-commit delete pass.
	harmonizedSeamAfterCommit = "after-commit"
	// harmonizedSeamMidActivationBatch fires in the batch loop after each
	// committed session write, so a batch can stop with the first sessions
	// committed and the rest unattempted.
	harmonizedSeamMidActivationBatch = "mid-activation-batch"
)

// harmonizedWriterSeam is the nil production hook a crash-injection test
// sets to stop the writer at a named stage. It mirrors contentSweepSeam and
// reclaimSeam: set it only in tests, and reset it to nil when the test
// finishes.
var harmonizedWriterSeam func(stage string) error

// reportHarmonizedWriterSeam reports one writer stage to the hook, if set.
// A nil hook is success: production writes run to completion with no
// observer.
func reportHarmonizedWriterSeam(stage string) error {
	if harmonizedWriterSeam == nil {
		return nil
	}
	return harmonizedWriterSeam(stage)
}

// errSkipPointerMoved reports that the active pointer moved between the skip
// comparison and the bookkeeping transaction: the comparison is stale, so
// the caller falls back to the ordinary write path instead of stamping
// bookkeeping for a generation it did not compare.
var errSkipPointerMoved = errors.New("store: the active generation moved between the skip comparison and the bookkeeping transaction; the comparison is stale and no bookkeeping was stamped; retry through the ordinary write path")

// stageHarmonizedSession stages one prepared candidate's objects without any
// lock (design §4.1 S0–S2): S0 revalidates at the boundary, S1 sets the
// crash flag in the first staging transaction, and S2 inserts bodies and
// blob objects idempotently, grouped by the one splitter. A giant session
// stages alone in budget-sized transactions; an ordinary session stages in
// exactly one commit. Every object is digest-addressed and written ON
// CONFLICT DO NOTHING, so concurrent same-session staging is idempotent,
// and the digest-presence check at the end proves every staged object is
// really there: a sweep that deleted a staged-but-uncommitted object, or a
// body_id allocation race the conflict target refused, surfaces here as an
// actionable error before the commit, never as a silent loss.
func (s *Store) stageHarmonizedSession(ctx context.Context, prepared *preparedHarmonized) error {
	return s.stageHarmonizedBatch(ctx, []*preparedHarmonized{prepared})
}

// stagedSessionObjects is one session's share of a staging batch: its
// staging units plus their staged byte size, the splitter's byte operand.
type stagedSessionObjects struct {
	session schema.SessionID
	units   []stageUnit
	bytes   int64
}

// stageHarmonizedBatch stages a batch of prepared candidates in
// budget-sized transactions through the one splitter. Sessions accumulate
// into a transaction while the splitter admits the next session's bytes; a
// session that exceeds the whole budget stages alone in budget-sized
// transactions of its own. Ordinary batches commit once no matter how many
// sessions they hold; one WAL commit serves the whole batch.
func (s *Store) stageHarmonizedBatch(ctx context.Context, prepared []*preparedHarmonized) error {
	if len(prepared) == 0 {
		return nil
	}
	cfg := s.writeConfigForLane()
	var sessions []stagedSessionObjects
	for _, candidate := range prepared {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateEntriesForStorage(candidate.sessionID, allGenerationEntries(candidate.generation)); err != nil {
			return err
		}
		units := stageUnits(candidate)
		var bytes int64
		for _, unit := range units {
			if unit.blob != nil && unit.blob.data == nil {
				return fmt.Errorf("store: blob %s of session %s carries no bytes to stage; stage the candidate with its content bytes instead", unit.blob.digest, candidate.sessionID)
			}
			bytes += unit.bytes
		}
		sessions = append(sessions, stagedSessionObjects{session: candidate.sessionID, units: units, bytes: bytes})
	}
	for _, txn := range splitStageBatch(cfg, sessions) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := s.pool.Take(ctx)
		if err != nil {
			return fmt.Errorf("store: take connection to stage harmonized objects: %w; nothing was staged", err)
		}
		txnErr := s.stageBatchTxn(conn, txn)
		s.pool.Put(conn)
		if txnErr != nil {
			return txnErr
		}
		if !txn.last {
			if err := reportHarmonizedWriterSeam(harmonizedSeamBetweenStageTxns); err != nil {
				return err
			}
		}
	}
	for _, candidate := range prepared {
		if err := s.verifyStagedObjects(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

// stageUnit is one idempotently insertable object: a body row or a whole
// blob (header plus all chunks, which share one transaction by
// construction).
type stageUnit struct {
	bytes int64
	body  *EntryRecord
	blob  *preparedBlob
}

// stageUnits flattens one candidate's bodies and blobs into staging units.
// Bodies come first so small sessions commit bodies and blobs together.
func stageUnits(prepared *preparedHarmonized) []stageUnit {
	units := make([]stageUnit, 0, len(prepared.bodies)+len(prepared.blobs))
	for i := range prepared.bodies {
		body := prepared.bodies[i]
		bodyCopy := body
		units = append(units, stageUnit{bytes: int64(len(serializeEntry(body))), body: &bodyCopy})
	}
	for i := range prepared.blobs {
		blob := prepared.blobs[i]
		blobCopy := blob
		units = append(units, stageUnit{bytes: int64(len(blob.data)), blob: &blobCopy})
	}
	return units
}

// stageBatch is one staging transaction: the sessions whose flags it sets
// (S1 rides every session's first staging transaction) and their units.
// last marks the batch no between-txn seam follows.
type stageBatch struct {
	sessions []schema.SessionID
	units    []batchUnit
	last     bool
}

// batchUnit is one idempotently insertable object bound to its session: a
// body row or a whole blob (header plus all chunks, which share one
// transaction by construction).
type batchUnit struct {
	session schema.SessionID
	unit    stageUnit
}

// splitStageBatch groups sessions' staging units into transactions through
// the one splitter. An ordinary batch fits one transaction (one commit);
// an oversized session's units split across budget-sized transactions of
// their own, with the flag riding the first.
func splitStageBatch(cfg ingest.WriteConfig, sessions []stagedSessionObjects) []stageBatch {
	var batches []stageBatch
	var current []batchUnit
	var currentSessions []schema.SessionID
	var currentSessionSet map[schema.SessionID]bool
	var currentBytes int64
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, stageBatch{sessions: currentSessions, units: current})
			current = nil
			currentSessions = nil
			currentSessionSet = nil
			currentBytes = 0
		}
	}
	for _, session := range sessions {
		if ingest.ExceedsWriteBudget(cfg, len(currentSessions), currentBytes, session.bytes) {
			flush()
		}
		if session.bytes > cfg.BatchBytes {
			for _, unit := range session.units {
				if len(current) > 0 && ingest.ExceedsWriteBudget(cfg, 1, currentBytes, unit.bytes) {
					flush()
				}
				current = append(current, batchUnit{session: session.session, unit: unit})
				if !currentSessionSet[session.session] {
					if currentSessionSet == nil {
						currentSessionSet = map[schema.SessionID]bool{}
					}
					currentSessionSet[session.session] = true
					currentSessions = append(currentSessions, session.session)
				}
				currentBytes += unit.bytes
			}
			continue
		}
		for _, unit := range session.units {
			current = append(current, batchUnit{session: session.session, unit: unit})
			currentBytes += unit.bytes
		}
		if !currentSessionSet[session.session] {
			if currentSessionSet == nil {
				currentSessionSet = map[schema.SessionID]bool{}
			}
			currentSessionSet[session.session] = true
			currentSessions = append(currentSessions, session.session)
		}
	}
	flush()
	for i := range batches {
		batches[i].last = i == len(batches)-1
	}
	return batches
}

// stageBatchTxn writes one staging batch in one transaction. Every session
// in the batch gets its crash flag first (S1): any crash from here to the
// sweep leaves the flag set with the old generation active, and the next
// harvest re-selects the session while the flagged sweep clears the
// orphans.
func (s *Store) stageBatchTxn(conn *sqlite.Conn, batch stageBatch) error {
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	for _, sessionID := range batch.sessions {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
			txnErr = fmt.Errorf("store: set the sweep flag for session %s before staging: %w; nothing was staged", sessionID, err)
			return txnErr
		}
	}
	for i := range batch.units {
		entry := batch.units[i]
		var err error
		if entry.unit.body != nil {
			err = insertStagedBodyOnConn(conn, entry.session, entry.unit.body)
		} else {
			err = insertStagedBlobOnConn(conn, entry.session, entry.unit.blob)
		}
		if err != nil {
			txnErr = err
			return txnErr
		}
	}
	return nil
}

// verifyStagedObjects proves every staged digest is really stored: bodies
// and blob headers are re-read by digest, so a sweep that deleted a
// staged-but-uncommitted object surfaces here — before the commit whose
// foreign keys would otherwise refuse it — with the session and the
// recovery (re-run harvest) attached.
func (s *Store) verifyStagedObjects(ctx context.Context, prepared *preparedHarmonized) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to verify staged objects for session %s: %w", prepared.sessionID, err)
	}
	defer s.pool.Put(conn)
	for _, digest := range prepared.bodyDigests {
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_entry_bodies WHERE session_id = ? AND body_digest = ? LIMIT 1`, &sqlitex.ExecOptions{
			Args:       []any{string(prepared.sessionID), string(digest)},
			ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
		}); err != nil {
			return fmt.Errorf("store: verify staged body %s for session %s: %w", digest, prepared.sessionID, err)
		}
		if !found {
			return fmt.Errorf("store: staged body %s for session %s is missing after staging (a sweep deleted a staged-but-uncommitted object, or a concurrent staging won the body_id allocation); nothing was committed and the old generation is active; re-run harvest to stage and commit again", digest, prepared.sessionID)
		}
	}
	for _, blob := range prepared.blobs {
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_content WHERE session_id = ? AND digest = ? LIMIT 1`, &sqlitex.ExecOptions{
			Args:       []any{string(prepared.sessionID), string(blob.digest)},
			ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
		}); err != nil {
			return fmt.Errorf("store: verify staged blob %s for session %s: %w", blob.digest, prepared.sessionID, err)
		}
		if !found {
			return fmt.Errorf("store: staged blob %s for session %s is missing after staging (a sweep deleted a staged-but-uncommitted object); nothing was committed and the old generation is active; re-run harvest to stage and commit again", blob.digest, prepared.sessionID)
		}
	}
	return nil
}

// insertStagedBodyOnConn writes one body row idempotently: the only insert
// form (design §4.1 S2). The body_id allocates explicitly from max(body_id)
// so fresh and migrated stores behave the same; a reused id after the
// maximum row was deleted is safe because a clean delete removed its
// postings through the BEFORE DELETE trigger, and an untrusted delete set
// needs_rebuild first.
func insertStagedBodyOnConn(conn *sqlite.Conn, sessionID schema.SessionID, record *EntryRecord) error {
	digest := string(bodyDigestForRecord(*record))
	args := bodyInsertArgs(sessionID, digest, record)
	if err := sqlitex.ExecuteTransient(conn, sqlInsertStagedBody, &sqlitex.ExecOptions{Args: args}); err != nil {
		return fmt.Errorf("store: stage body %s (entry %d) for session %s: %w; nothing in this batch was committed", digest, record.EntryIndex, sessionID, err)
	}
	return nil
}

const sqlInsertStagedBodyTemplate = `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, timestamp_ms, content_preview, tokens_in, tokens_out, has_tool_use, tool_kind, tool_names_csv, has_thinking, is_error, stop_reason, raw_byte_length, tool_call_id, entry_id, parent_entry_id, depth, parent_index, tool_input, tool_output, model_id, tokens_reasoning, cache_read, cache_write, extra, extra_verbatim, part_type, source_entry_ref, prov_origin, prov_actor, prov_delivery, prov_ownership, prov_evidence, prov_input_modality, prov_submission_ref)
VALUES ((SELECT coalesce(max(body_id), %d) + 1 FROM session_entry_bodies), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, body_digest) DO NOTHING`

// sqlInsertStagedBody allocates explicitly from max(body_id) so fresh and
// migrated stores behave the same; the seed is BodyRowIDBase - 1, derived
// here rather than spelled out, so the constant stays the one home.
var sqlInsertStagedBody = fmt.Sprintf(sqlInsertStagedBodyTemplate, BodyRowIDBase-1)

// bodyInsertArgs binds one body row: named closed sets as their strings,
// NULLs for absent pointers, and the provenance struct fanned out to its
// seven columns. An empty submission ref reads as NULL, which reconstructs
// to the same absent value.
func bodyInsertArgs(sessionID schema.SessionID, digest string, r *EntryRecord) []any {
	var provenance [7]any
	if r.Provenance != nil {
		provenance[0] = string(r.Provenance.Origin)
		provenance[1] = string(r.Provenance.Actor)
		provenance[2] = string(r.Provenance.Delivery)
		provenance[3] = string(r.Provenance.Ownership)
		provenance[4] = string(r.Provenance.Evidence)
		provenance[5] = string(r.Provenance.InputModality)
		if r.Provenance.SubmissionRef != "" {
			provenance[6] = string(r.Provenance.SubmissionRef)
		}
	}
	var sourceRef any
	if r.SourceEntryRef != "" {
		sourceRef = string(r.SourceEntryRef)
	}
	return []any{
		string(sessionID), digest, int64(r.EntryIndex),
		string(r.Harness), string(r.EntryType), string(r.Role),
		optInt64(r.TimestampMs), optString(r.ContentPreview),
		optInt(r.TokensIn), optInt(r.TokensOut),
		boolToInt64(r.HasToolUse), optToolKind(r.ToolKind), optString(r.ToolNamesCSV),
		boolToInt64(r.HasThinking), boolToInt64(r.IsError),
		optStopReason(r.StopReason), optInt(r.RawByteLength),
		optString(r.ToolCallID), optString(r.EntryID), optString(r.ParentEntryID),
		int64(r.Depth), optInt(r.ParentIndex),
		optString(r.ToolInput), optString(r.ToolOutput),
		optString(r.ModelID), optInt(r.TokensReasoning),
		optInt(r.CacheRead), optInt(r.CacheWrite),
		optString(r.Extra), optString(r.ExtraVerbatim),
		optString(r.PartType), sourceRef,
		provenance[0], provenance[1], provenance[2], provenance[3],
		provenance[4], provenance[5], provenance[6],
	}
}

func optInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func optString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func optInt(v *int) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func optToolKind(v *schema.ToolCallKind) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func optStopReason(v *schema.StopReason) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func boolToInt64(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// insertStagedBlobOnConn writes one blob object idempotently: the header
// plus every 64 KiB chunk share the caller's transaction, so a blob is
// whole or absent, never torn. A re-stage of the same digest writes
// nothing.
func insertStagedBlobOnConn(conn *sqlite.Conn, sessionID schema.SessionID, blob *preparedBlob) error {
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_content(session_id, digest, byte_length) VALUES (?, ?, ?) ON CONFLICT(session_id, digest) DO NOTHING`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), string(blob.digest), int64(len(blob.data))},
	}); err != nil {
		return fmt.Errorf("store: stage blob %s for session %s: %w; nothing in this batch was committed", blob.digest, sessionID, err)
	}
	for index := 0; index*fullContentChunkBytes < len(blob.data) || (len(blob.data) == 0 && index == 0); index++ {
		if len(blob.data) == 0 {
			break
		}
		end := (index + 1) * fullContentChunkBytes
		if end > len(blob.data) {
			end = len(blob.data)
		}
		chunk := blob.data[index*fullContentChunkBytes : end]
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_content_chunks(session_id, digest, chunk_index, data) VALUES (?, ?, ?, ?) ON CONFLICT(session_id, digest, chunk_index) DO NOTHING`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), string(blob.digest), int64(index), chunk},
		}); err != nil {
			return fmt.Errorf("store: stage chunk %d of blob %s for session %s: %w; nothing in this batch was committed", index, blob.digest, sessionID, err)
		}
		if end == len(blob.data) {
			break
		}
	}
	return nil
}

// scanEntryRecord reads one body row into its record: the FTS rowid plus
// every column in SELECT order, with the provenance columns folded back
// into the struct. Storage identity (BodyID, BodyDigest) travels with the
// record but never reaches the wire struct.
func scanEntryRecord(stmt *sqlite.Stmt) EntryRecord {
	record := EntryRecord{
		BodyID:          stmt.ColumnInt64(0),
		SessionID:       schema.SessionID(stmt.ColumnText(1)),
		BodyDigest:      stmt.ColumnText(2),
		EntryIndex:      int(stmt.ColumnInt64(3)),
		TimestampMs:     nullableColumnInt64(stmt, 7),
		ContentPreview:  nullableColumnText(stmt, 8),
		TokensIn:        nullableColumnInt(stmt, 9),
		TokensOut:       nullableColumnInt(stmt, 10),
		HasToolUse:      stmt.ColumnInt64(11) == 1,
		ToolNamesCSV:    nullableColumnText(stmt, 13),
		HasThinking:     stmt.ColumnInt64(14) == 1,
		IsError:         stmt.ColumnInt64(15) == 1,
		RawByteLength:   nullableColumnInt(stmt, 17),
		ToolCallID:      nullableColumnText(stmt, 18),
		EntryID:         nullableColumnText(stmt, 19),
		ParentEntryID:   nullableColumnText(stmt, 20),
		Depth:           int(stmt.ColumnInt64(21)),
		ParentIndex:     nullableColumnInt(stmt, 22),
		ToolInput:       nullableColumnText(stmt, 23),
		ToolOutput:      nullableColumnText(stmt, 24),
		ModelID:         nullableColumnText(stmt, 25),
		TokensReasoning: nullableColumnInt(stmt, 26),
		CacheRead:       nullableColumnInt(stmt, 27),
		CacheWrite:      nullableColumnInt(stmt, 28),
		Extra:           nullableColumnText(stmt, 29),
		ExtraVerbatim:   nullableColumnText(stmt, 30),
		PartType:        nullableColumnText(stmt, 31),
	}
	if text := stmt.ColumnText(4); text != "" {
		harness := schema.Harness(text)
		record.Harness = harness
	}
	if text := stmt.ColumnText(5); text != "" {
		record.EntryType = schema.EntryType(text)
	}
	if text := stmt.ColumnText(6); text != "" {
		record.Role = schema.Role(text)
	}
	if stmt.ColumnType(12) != sqlite.TypeNull {
		kind := schema.ToolCallKind(stmt.ColumnText(12))
		record.ToolKind = &kind
	}
	if stmt.ColumnType(16) != sqlite.TypeNull {
		reason := schema.StopReason(stmt.ColumnText(16))
		record.StopReason = &reason
	}
	if stmt.ColumnType(32) != sqlite.TypeNull {
		record.SourceEntryRef = schema.SourceEntryRef(stmt.ColumnText(32))
	}
	if hasProvenanceColumns(stmt) {
		record.Provenance = &schema.ContentProvenance{
			Origin:        schema.ContentOrigin(stmt.ColumnText(33)),
			Actor:         schema.ActorOrigin(stmt.ColumnText(34)),
			Delivery:      schema.DeliveryOrigin(stmt.ColumnText(35)),
			Ownership:     schema.ContentOwnership(stmt.ColumnText(36)),
			Evidence:      schema.EvidenceKind(stmt.ColumnText(37)),
			InputModality: schema.InputModality(stmt.ColumnText(38)),
		}
		if stmt.ColumnType(39) != sqlite.TypeNull {
			record.Provenance.SubmissionRef = schema.SubmissionRef(stmt.ColumnText(39))
		}
	}
	return record
}

const sqlSelectBodyColumns = `body_id, session_id, body_digest, entry_index, harness, entry_type, role, timestamp_ms, content_preview, tokens_in, tokens_out, has_tool_use, tool_kind, tool_names_csv, has_thinking, is_error, stop_reason, raw_byte_length, tool_call_id, entry_id, parent_entry_id, depth, parent_index, tool_input, tool_output, model_id, tokens_reasoning, cache_read, cache_write, extra, extra_verbatim, part_type, source_entry_ref, prov_origin, prov_actor, prov_delivery, prov_ownership, prov_evidence, prov_input_modality, prov_submission_ref`

// sqlSelectBodyColumnsJoined is the same column list qualified for the
// body/mapping join both tables expose session_id under: the unqualified
// list is ambiguous there, so join sites read through this derived form
// and the column home stays single.
var sqlSelectBodyColumnsJoined = "b." + strings.ReplaceAll(sqlSelectBodyColumns, ", ", ", b.")

// hasProvenanceColumns reports whether the row carries any provenance: the
// six required dimensions are non-NULL together, or all NULL together. A
// half-present provenance cannot reconstruct a valid struct and reads as
// absent; the full-read digest check refuses the corruption before anything
// is served.
func hasProvenanceColumns(stmt *sqlite.Stmt) bool {
	for col := 33; col <= 38; col++ {
		if stmt.ColumnType(col) == sqlite.TypeNull {
			return false
		}
	}
	return true
}

// activeHarmonizedView is the structured read of one session's active
// harmonized generation: the catalog row, its ordered children, the entry
// mapping in (partition, index) order, the non-emitted descriptors, and the
// aliases. It is the skip comparison's operand and the repair transaction's
// anchor; it is not a wire surface.
type activeHarmonizedView struct {
	found        bool
	generationID string
	record       GenerationRecord
	children     GenerationChildren
	mapping      []mappedBody
	descriptors  map[schema.SourceEntryRef]string
	aliases      map[string]schema.SourceEntryRef
}

// mappedBody is one session_generation_entries row: the mapping from a
// (partition, index) position to its body digest.
type mappedBody struct {
	partition int
	index     int
	digest    string
}

// readActiveHarmonizedView loads the active harmonized generation's
// structured rows on the caller's connection: no lock is taken, so the
// prepare phase and the crash seams read one snapshot. A session whose
// active generation row is file-backed (or which has none) reads as not
// found, and the caller takes the write path so harvest converts it.
func readActiveHarmonizedView(conn *sqlite.Conn, sessionID schema.SessionID) (activeHarmonizedView, error) {
	var view activeHarmonizedView
	view.descriptors = map[schema.SourceEntryRef]string{}
	view.aliases = map[string]schema.SourceEntryRef{}
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return view, err
	}
	if active == nil || *active == "" {
		return view, nil
	}
	view.generationID = *active
	if err := loadGenerationRecordOnConn(conn, sessionID, *active, &view); err != nil {
		return view, err
	}
	if !view.found {
		return view, nil
	}
	if err := loadGenerationChildrenOnConn(conn, sessionID, *active, &view); err != nil {
		return view, err
	}
	return view, nil
}

// loadGenerationRecordOnConn reads one session_generations row into the
// view's record. A row in the file-backed catalog reads as not found: only
// the harmonized catalog participates in the skip comparison.
func loadGenerationRecordOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, view *activeHarmonizedView) error {
	err := sqlitex.ExecuteTransient(conn, `SELECT schema_version, harness, model, version, ts_start, ts_end, ts_ingested, source_file_path, source_format, git_branch, git_remote, git_worktree, git_tracking, project_hash, project_file_path, project_name, host_slug, root_session_id, purpose, cwd, derived_at, content_hash, metadata_hash, redaction_applied, redaction_level, redaction_rule_set_version, redaction_at_ms, redaction_content_hash_at_redact, adapter_version, diagnostics_partial, completeness, source_evidence_digest, index_format_version, candidate_digest, prior_evidence, installed_at_ms, activated_at_ms FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			view.found = true
			record := GenerationRecord{SessionID: sessionID, GenerationID: generationID}
			record.SchemaVersion = int(stmt.ColumnInt64(0))
			record.Harness = schema.Harness(stmt.ColumnText(1))
			record.Model = schema.ModelID(stmt.ColumnText(2))
			record.Version = stmt.ColumnText(3)
			record.TimestampStartMs = stmt.ColumnInt64(4)
			record.TimestampEndMs = stmt.ColumnInt64(5)
			if stmt.ColumnType(6) != sqlite.TypeNull {
				ingested := stmt.ColumnInt64(6)
				record.TimestampIngestedMs = &ingested
			}
			if stmt.ColumnType(7) != sqlite.TypeNull {
				path := stmt.ColumnText(7)
				record.SourceFilePath = &path
			}
			record.SourceFormat = schema.SourceFormat(stmt.ColumnText(8))
			if stmt.ColumnType(9) != sqlite.TypeNull {
				branch := stmt.ColumnText(9)
				record.GitBranch = &branch
			}
			if stmt.ColumnType(10) != sqlite.TypeNull {
				remote := stmt.ColumnText(10)
				record.GitRemote = &remote
			}
			if stmt.ColumnType(11) != sqlite.TypeNull {
				worktree := stmt.ColumnText(11)
				record.GitWorktree = &worktree
			}
			if stmt.ColumnType(12) != sqlite.TypeNull {
				tracking := stmt.ColumnText(12)
				record.GitTracking = &tracking
			}
			record.ProjectHash = schema.ProjectHash(stmt.ColumnText(13))
			if stmt.ColumnType(14) != sqlite.TypeNull {
				path := stmt.ColumnText(14)
				record.ProjectFilePath = &path
			}
			record.ProjectName = stmt.ColumnText(15)
			record.HostSlug = schema.HostSlug(stmt.ColumnText(16))
			if stmt.ColumnType(17) != sqlite.TypeNull {
				root := schema.SessionID(stmt.ColumnText(17))
				record.RootSessionID = &root
			}
			if stmt.ColumnType(18) != sqlite.TypeNull {
				purpose := schema.SessionPurpose(stmt.ColumnText(18))
				record.Purpose = &purpose
			}
			if stmt.ColumnType(19) != sqlite.TypeNull {
				cwd := stmt.ColumnText(19)
				record.CWD = &cwd
			}
			if stmt.ColumnType(20) != sqlite.TypeNull {
				derived := stmt.ColumnInt64(20)
				record.DerivedAtMs = &derived
			}
			record.ContentHash = stmt.ColumnText(21)
			record.MetadataHash = stmt.ColumnText(22)
			record.RedactionApplied = stmt.ColumnInt64(23) == 1
			if stmt.ColumnType(24) != sqlite.TypeNull {
				level := stmt.ColumnText(24)
				record.RedactionLevel = &level
			}
			if stmt.ColumnType(25) != sqlite.TypeNull {
				version := stmt.ColumnText(25)
				record.RedactionRuleSetVersion = &version
			}
			if stmt.ColumnType(26) != sqlite.TypeNull {
				at := stmt.ColumnInt64(26)
				record.RedactionAtMs = &at
			}
			if stmt.ColumnType(27) != sqlite.TypeNull {
				hash := stmt.ColumnText(27)
				record.RedactionContentHashAtRedact = &hash
			}
			if stmt.ColumnType(28) != sqlite.TypeNull {
				adapter := int(stmt.ColumnInt64(28))
				record.AdapterVersion = &adapter
			}
			if stmt.ColumnType(29) != sqlite.TypeNull {
				partial := stmt.ColumnInt64(29) == 1
				record.DiagnosticsPartial = &partial
			}
			record.Completeness = indexFormatCompleteness(stmt.ColumnText(30))
			record.SourceEvidenceDigest = stmt.ColumnText(31)
			record.IndexFormatVersion = int(stmt.ColumnInt64(32))
			record.CandidateDigest = stmt.ColumnText(33)
			if stmt.ColumnType(34) != sqlite.TypeNull {
				buf := make([]byte, stmt.ColumnLen(34))
				n := stmt.ColumnBytes(34, buf)
				record.PriorEvidence = append([]byte(nil), buf[:n]...)
			}
			record.InstalledAtMs = stmt.ColumnInt64(35)
			if stmt.ColumnType(36) != sqlite.TypeNull {
				activated := stmt.ColumnInt64(36)
				record.ActivatedAtMs = &activated
			}
			view.record = record
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("store: read harmonized generation %s for session %s: %w", generationID, sessionID, err)
	}
	return nil
}

func indexFormatCompleteness(text string) indexformat.GenerationCompleteness {
	return indexformat.GenerationCompleteness(text)
}

// loadGenerationChildrenOnConn reads the ordered 1:N rows, the entry
// mapping, the non-emitted descriptors, and the aliases into the view.
// Every collection loads in key order so the comparison is order-exact
// where order is identity (children, mapping) and order-free where it is
// not (descriptors, aliases).
func loadGenerationChildrenOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string, view *activeHarmonizedView) error {
	query := func(sql string, fn func(*sqlite.Stmt) error) error {
		return sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
			Args:       []any{string(sessionID), generationID},
			ResultFunc: fn,
		})
	}
	if err := query(`SELECT ordinal, subagent_session_id, parent_uuid FROM session_generation_subagents WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.Subagents = append(view.children.Subagents, GenerationSubagent{
			Ordinal: int(stmt.ColumnInt64(0)), SubagentSessionID: schema.SessionID(stmt.ColumnText(1)), ParentUUID: schema.SessionID(stmt.ColumnText(2)),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation subagents for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, hash, message, author_name, author_email, commit_time, author_time FROM session_generation_commits WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.Commits = append(view.children.Commits, GenerationCommit{
			Ordinal: int(stmt.ColumnInt64(0)), Hash: stmt.ColumnText(1), Message: stmt.ColumnText(2),
			AuthorName: stmt.ColumnText(3), AuthorEmail: stmt.ColumnText(4),
			CommitTime: stmt.ColumnInt64(5), AuthorTime: stmt.ColumnInt64(6),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation commits for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, association_id, observed_commit_hash FROM session_generation_associations WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.Associations = append(view.children.Associations, GenerationAssociation{
			Ordinal: int(stmt.ColumnInt64(0)), AssociationID: stmt.ColumnText(1), ObservedCommitHash: stmt.ColumnText(2),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation associations for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, error_type, location, message, remediation FROM session_generation_diagnostics WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.Diagnostics = append(view.children.Diagnostics, GenerationDiagnostic{
			Ordinal: int(stmt.ColumnInt64(0)), ErrorType: stmt.ColumnText(1), Location: stmt.ColumnText(2),
			Message: stmt.ColumnText(3), Remediation: stmt.ColumnText(4),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation diagnostics for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT ordinal, source_entry_ref FROM session_generation_title_refs WHERE session_id = ? AND generation_id = ? ORDER BY ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.TitleRefs = append(view.children.TitleRefs, schema.SourceEntryRef(stmt.ColumnText(1)))
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation title refs for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? ORDER BY kind`, func(stmt *sqlite.Stmt) error {
		relationship := GenerationRelationship{
			Kind:        schema.SessionRelationshipKind(stmt.ColumnText(0)),
			TargetState: schema.RelationshipTargetState(stmt.ColumnText(1)),
		}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			target := schema.SessionID(stmt.ColumnText(2))
			relationship.TargetLocalID = &target
		}
		if stmt.ColumnType(3) != sqlite.TypeNull {
			evidence := stmt.ColumnText(3)
			relationship.Evidence = &evidence
		}
		if stmt.ColumnType(4) != sqlite.TypeNull {
			kind := schema.PublicSourceAnchorKind(stmt.ColumnText(4))
			relationship.AnchorKind = &kind
		}
		if stmt.ColumnType(5) != sqlite.TypeNull {
			ref := schema.SourceEntryRef(stmt.ColumnText(5))
			relationship.AnchorSourceEntryRef = &ref
		}
		if stmt.ColumnType(6) != sqlite.TypeNull {
			revision := stmt.ColumnText(6)
			relationship.AnchorSourceRevisionRef = &revision
		}
		view.children.Relationships = append(view.children.Relationships, relationship)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read relationship evidence for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT segment_ordinal, logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion FROM session_context_segments WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal`, func(stmt *sqlite.Stmt) error {
		segment := GenerationSegment{
			Ordinal:          int(stmt.ColumnInt64(0)),
			PhysicalSourceID: stmt.ColumnText(2),
			CoordinateKind:   indexformat.CoordinateKind(stmt.ColumnText(3)),
			Inclusion:        indexformat.SegmentInclusion(stmt.ColumnText(8)),
		}
		if stmt.ColumnType(1) != sqlite.TypeNull {
			logical := schema.SessionID(stmt.ColumnText(1))
			segment.LogicalSessionID = &logical
		}
		if stmt.ColumnType(4) != sqlite.TypeNull {
			start := stmt.ColumnInt64(4)
			segment.StartCoordinate = &start
		}
		if stmt.ColumnType(5) != sqlite.TypeNull {
			end := stmt.ColumnInt64(5)
			segment.EndExclusive = &end
		}
		if stmt.ColumnType(6) != sqlite.TypeNull {
			start := stmt.ColumnInt64(6)
			segment.DecodedByteStart = &start
		}
		if stmt.ColumnType(7) != sqlite.TypeNull {
			end := stmt.ColumnInt64(7)
			segment.DecodedByteEndExclusive = &end
		}
		view.children.Segments = append(view.children.Segments, segment)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read context segments for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT segment_ordinal, ordinal, source_entry_ref FROM session_context_segment_refs WHERE session_id = ? AND generation_id = ? ORDER BY segment_ordinal, ordinal`, func(stmt *sqlite.Stmt) error {
		view.children.SegmentRefs = append(view.children.SegmentRefs, GenerationSegmentRef{
			SegmentOrdinal: int(stmt.ColumnInt64(0)), Ordinal: int(stmt.ColumnInt64(1)),
			SourceEntryRef: schema.SourceEntryRef(stmt.ColumnText(2)),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read segment refs for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT partition_id, earlier_state FROM session_projection_sections WHERE session_id = ? AND generation_id = ? ORDER BY partition_id`, func(stmt *sqlite.Stmt) error {
		section := GenerationSection{PartitionID: int(stmt.ColumnInt64(0))}
		if stmt.ColumnType(1) != sqlite.TypeNull {
			state := schema.EarlierHistoryState(stmt.ColumnText(1))
			section.EarlierState = &state
		}
		view.children.Sections = append(view.children.Sections, section)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read projection sections for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT partition_id, ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data FROM session_section_native_metadata WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, ordinal`, func(stmt *sqlite.Stmt) error {
		record := GenerationNativeMetadata{
			PartitionID: int(stmt.ColumnInt64(0)), Ordinal: int(stmt.ColumnInt64(1)),
			NativeID: stmt.ColumnText(2), Kind: schema.NativeMetadataKind(stmt.ColumnText(3)),
			SourceEntryRef: schema.SourceEntryRef(stmt.ColumnText(4)),
			SourceType:     schema.NativeMetadataSourceType(stmt.ColumnText(5)),
		}
		if stmt.ColumnType(6) != sqlite.TypeNull {
			role := schema.NativePiMessageRole(stmt.ColumnText(6))
			record.SourceMessageRole = &role
		}
		if stmt.ColumnType(7) != sqlite.TypeNull {
			turn := int(stmt.ColumnInt64(7))
			record.AttachmentTurnIndex = &turn
		}
		if stmt.ColumnType(8) != sqlite.TypeNull {
			toolCallID := stmt.ColumnText(8)
			record.AttachmentToolCallID = &toolCallID
		}
		if stmt.ColumnType(9) != sqlite.TypeNull {
			customType := stmt.ColumnText(9)
			record.CustomType = &customType
		}
		if stmt.ColumnType(10) != sqlite.TypeNull {
			record.Data = stmt.ColumnText(10)
		}
		view.children.NativeMetadata = append(view.children.NativeMetadata, record)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read native metadata for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT partition_id, entry_index, body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, func(stmt *sqlite.Stmt) error {
		view.mapping = append(view.mapping, mappedBody{
			partition: int(stmt.ColumnInt64(0)), index: int(stmt.ColumnInt64(1)), digest: stmt.ColumnText(2),
		})
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation entry mapping for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT source_entry_ref, digest FROM session_generation_content WHERE session_id = ? AND generation_id = ?`, func(stmt *sqlite.Stmt) error {
		view.descriptors[schema.SourceEntryRef(stmt.ColumnText(0))] = stmt.ColumnText(1)
		return nil
	}); err != nil {
		return fmt.Errorf("store: read generation content descriptors for session %s: %w", sessionID, err)
	}
	if err := query(`SELECT native_key, source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ?`, func(stmt *sqlite.Stmt) error {
		view.aliases[stmt.ColumnText(0)] = schema.SourceEntryRef(stmt.ColumnText(1))
		return nil
	}); err != nil {
		return fmt.Errorf("store: read projection aliases for session %s: %w", sessionID, err)
	}
	return nil
}

// refreshEqualsActive is the P5 skip comparison (design §4.2): it reports
// whether the candidate reproduces the active harmonized generation exactly.
// It compares the main and earlier entries by (partition, index, digest),
// the generation's 1:1 metadata columns and its ordered children, the
// non-emitted content refs and digests, the aliases as a map, the segments
// and evidence, and the completeness, title refs, and source-evidence
// digest. It never compares the mutable stats row (a stats-only change is
// an in-place update, never a new generation), the immutable
// stats-excluded metadata_hash anchor, or the read-invisible adapter
// revision (which the bookkeeping stamps forward on a skip). An
// incomplete_new candidate never equals a complete active generation,
// because completeness participates. Only a harmonized active generation
// can compare equal: a file-backed active generation always takes the
// write path so harvest converts it.
func refreshEqualsActive(prepared *preparedHarmonized, active activeHarmonizedView) bool {
	if !active.found {
		return false
	}
	if !generationRecordsEqual(prepared.record, active.record) {
		return false
	}
	if !reflect.DeepEqual(prepared.children, active.children) {
		return false
	}
	if len(prepared.bodyDigests) != len(active.mapping) {
		return false
	}
	for i := range prepared.bodyDigests {
		mapped := active.mapping[i]
		if mapped.partition != prepared.partitions[i] {
			return false
		}
		entryIndex := prepared.bodies[i].EntryIndex
		if mapped.index != entryIndex {
			return false
		}
		if mapped.digest != string(prepared.bodyDigests[i]) {
			return false
		}
	}
	if len(prepared.blobs) != len(active.descriptors) {
		return false
	}
	for _, blob := range prepared.blobs {
		activeDigest, ok := active.descriptors[blob.ref]
		if !ok || activeDigest != string(blob.digest) {
			return false
		}
	}
	return reflect.DeepEqual(prepared.aliases, active.aliases)
}

// generationRecordsEqual compares two catalog rows field by field, excluding
// the generation identity (the comparison is across two identifiers by
// construction), the stats-excluded metadata_hash anchor (never re-derived),
// the read-invisible adapter revision (stamped forward on a skip), and the
// staging-derived stamps (installed/activated). The candidate digest
// participates: equal content binds equally, so a mismatch there is a
// binding bug failing safe toward a new generation.
func generationRecordsEqual(candidate, active GenerationRecord) bool {
	normalize := func(r GenerationRecord) GenerationRecord {
		r.GenerationID = ""
		r.MetadataHash = ""
		r.AdapterVersion = nil
		r.InstalledAtMs = 0
		r.ActivatedAtMs = nil
		return r
	}
	return reflect.DeepEqual(normalize(candidate), normalize(active))
}

// insertHarmonizedGenerationOnConn runs C2: the session_generations row
// (never delete-then-insert; the flattened metadata columns, the
// stats-excluded metadata_hash, the candidate binding, and the opaque prior
// evidence) plus its metadata children (3b), the entry mapping, the
// non-emitted descriptors, and the reshaped table-6 rows (aliases,
// sections, segments, evidence). The foreign keys prove every referenced
// object was staged: a sweep that deleted a staged-but-uncommitted object
// refuses the commit, and the session retries on the next harvest.
func insertHarmonizedGenerationOnConn(conn *sqlite.Conn, prepared *preparedHarmonized, priorEvidence []byte, installedAtMs int64) error {
	sessionID := prepared.sessionID
	generationID := prepared.generation.ID
	record := prepared.record
	record.CandidateDigest = prepared.binding
	record.PriorEvidence = append([]byte(nil), priorEvidence...)
	record.InstalledAtMs = installedAtMs
	record.ActivatedAtMs = &installedAtMs
	r := record
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generations(session_id, generation_id, schema_version, harness, model, version, ts_start, ts_end, ts_ingested, source_file_path, source_format, git_branch, git_remote, git_worktree, git_tracking, project_hash, project_file_path, project_name, host_slug, root_session_id, purpose, cwd, derived_at, content_hash, metadata_hash, redaction_applied, redaction_level, redaction_rule_set_version, redaction_at_ms, redaction_content_hash_at_redact, adapter_version, diagnostics_partial, completeness, source_evidence_digest, index_format_version, candidate_digest, prior_evidence, installed_at_ms, activated_at_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
		string(sessionID), generationID,
		int64(r.SchemaVersion), string(r.Harness), string(r.Model), r.Version,
		r.TimestampStartMs, r.TimestampEndMs, optInt64(r.TimestampIngestedMs),
		optString(r.SourceFilePath), string(r.SourceFormat),
		optString(r.GitBranch), optString(r.GitRemote), optString(r.GitWorktree), optString(r.GitTracking),
		string(r.ProjectHash), optString(r.ProjectFilePath), r.ProjectName, string(r.HostSlug),
		optSessionID(r.RootSessionID), optPurpose(r.Purpose), optString(r.CWD), optInt64(r.DerivedAtMs),
		r.ContentHash, r.MetadataHash,
		boolToInt64(r.RedactionApplied), optString(r.RedactionLevel), optString(r.RedactionRuleSetVersion),
		optInt64(r.RedactionAtMs), optString(r.RedactionContentHashAtRedact),
		optInt(r.AdapterVersion), optDiagnosticsPartial(r.DiagnosticsPartial),
		string(r.Completeness), r.SourceEvidenceDigest, int64(r.IndexFormatVersion), r.CandidateDigest,
		optBytes(r.PriorEvidence), r.InstalledAtMs, optInt64(r.ActivatedAtMs),
	}}); err != nil {
		return fmt.Errorf("store: install harmonized generation %s for session %s: %w; no generation was activated", generationID, sessionID, err)
	}
	children := prepared.children
	for _, subagent := range children.Subagents {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_subagents(session_id, generation_id, ordinal, subagent_session_id, parent_uuid) VALUES (?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(subagent.Ordinal), string(subagent.SubagentSessionID), string(subagent.ParentUUID),
		}}); err != nil {
			return fmt.Errorf("store: install subagent %d for generation %s of session %s: %w; no generation was activated", subagent.Ordinal, generationID, sessionID, err)
		}
	}
	for _, commit := range children.Commits {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_commits(session_id, generation_id, ordinal, hash, message, author_name, author_email, commit_time, author_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(commit.Ordinal), commit.Hash, commit.Message, commit.AuthorName, commit.AuthorEmail, commit.CommitTime, commit.AuthorTime,
		}}); err != nil {
			return fmt.Errorf("store: install commit %d for generation %s of session %s: %w; no generation was activated", commit.Ordinal, generationID, sessionID, err)
		}
	}
	for _, association := range children.Associations {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_associations(session_id, generation_id, ordinal, association_id, observed_commit_hash) VALUES (?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(association.Ordinal), association.AssociationID, association.ObservedCommitHash,
		}}); err != nil {
			return fmt.Errorf("store: install association %d for generation %s of session %s: %w; no generation was activated", association.Ordinal, generationID, sessionID, err)
		}
	}
	for _, diagnostic := range children.Diagnostics {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_diagnostics(session_id, generation_id, ordinal, error_type, location, message, remediation) VALUES (?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(diagnostic.Ordinal), diagnostic.ErrorType, diagnostic.Location, diagnostic.Message, diagnostic.Remediation,
		}}); err != nil {
			return fmt.Errorf("store: install diagnostic %d for generation %s of session %s: %w; no generation was activated", diagnostic.Ordinal, generationID, sessionID, err)
		}
	}
	for i, ref := range children.TitleRefs {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_title_refs(session_id, generation_id, ordinal, source_entry_ref) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(i), string(ref),
		}}); err != nil {
			return fmt.Errorf("store: install title ref %d for generation %s of session %s: %w; no generation was activated", i, generationID, sessionID, err)
		}
	}
	for i := range prepared.bodies {
		var sourceRef any
		if ref := prepared.bodies[i].SourceEntryRef; ref != "" {
			sourceRef = string(ref)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_entries(session_id, generation_id, partition_id, entry_index, source_entry_ref, body_digest) VALUES (?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(prepared.partitions[i]), int64(prepared.bodies[i].EntryIndex), sourceRef, string(prepared.bodyDigests[i]),
		}}); err != nil {
			return fmt.Errorf("store: map entry %d of partition %d for generation %s of session %s: %w; no generation was activated", prepared.bodies[i].EntryIndex, prepared.partitions[i], generationID, sessionID, err)
		}
	}
	for _, blob := range prepared.blobs {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_content(session_id, generation_id, source_entry_ref, digest) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, string(blob.ref), string(blob.digest),
		}}); err != nil {
			return fmt.Errorf("store: install content descriptor %q for generation %s of session %s: %w; no generation was activated", blob.ref, generationID, sessionID, err)
		}
	}
	aliasKeys := make([]string, 0, len(prepared.aliases))
	for key := range prepared.aliases {
		aliasKeys = append(aliasKeys, key)
	}
	sort.Strings(aliasKeys)
	for _, key := range aliasKeys {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_aliases(session_id, generation_id, native_key, source_entry_ref) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, key, string(prepared.aliases[key]),
		}}); err != nil {
			return fmt.Errorf("store: install native alias %q for generation %s of session %s: %w; no generation was activated", key, generationID, sessionID, err)
		}
	}
	for _, section := range children.Sections {
		var earlierState any
		if section.EarlierState != nil {
			earlierState = string(*section.EarlierState)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_sections(session_id, generation_id, partition_id, earlier_state) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(section.PartitionID), earlierState,
		}}); err != nil {
			return fmt.Errorf("store: install partition %d for generation %s of session %s: %w; no generation was activated", section.PartitionID, generationID, sessionID, err)
		}
	}
	for _, record := range children.NativeMetadata {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_section_native_metadata(session_id, generation_id, partition_id, ordinal, native_id, kind, source_entry_ref, source_type, source_message_role, attachment_turn_index, attachment_tool_call_id, custom_type, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(record.PartitionID), int64(record.Ordinal),
			record.NativeID, string(record.Kind), string(record.SourceEntryRef), string(record.SourceType),
			optNativeRole(record.SourceMessageRole), optInt(record.AttachmentTurnIndex),
			optString(record.AttachmentToolCallID), optString(record.CustomType), record.Data,
		}}); err != nil {
			return fmt.Errorf("store: install native metadata %d of partition %d for generation %s of session %s: %w; no generation was activated", record.Ordinal, record.PartitionID, generationID, sessionID, err)
		}
	}
	for _, segment := range children.Segments {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_context_segments(session_id, generation_id, segment_ordinal, logical_session_id, physical_source_id, coordinate_kind, start_coordinate, end_exclusive, decoded_byte_start, decoded_byte_end_exclusive, inclusion) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(segment.Ordinal),
			optSessionID(segment.LogicalSessionID), segment.PhysicalSourceID, string(segment.CoordinateKind),
			optInt64(segment.StartCoordinate), optInt64(segment.EndExclusive),
			optInt64(segment.DecodedByteStart), optInt64(segment.DecodedByteEndExclusive),
			string(segment.Inclusion),
		}}); err != nil {
			return fmt.Errorf("store: install context segment %d for generation %s of session %s: %w; no generation was activated", segment.Ordinal, generationID, sessionID, err)
		}
	}
	for _, ref := range children.SegmentRefs {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_context_segment_refs(session_id, generation_id, segment_ordinal, ordinal, source_entry_ref) VALUES (?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, int64(ref.SegmentOrdinal), int64(ref.Ordinal), string(ref.SourceEntryRef),
		}}); err != nil {
			return fmt.Errorf("store: install segment ref %d of segment %d for generation %s of session %s: %w; no generation was activated", ref.Ordinal, ref.SegmentOrdinal, generationID, sessionID, err)
		}
	}
	for _, relationship := range children.Relationships {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{Args: []any{
			string(sessionID), generationID, string(relationship.Kind), string(relationship.TargetState),
			optSessionID(relationship.TargetLocalID), optString(relationship.Evidence),
			optAnchorKind(relationship.AnchorKind), optEntryRef(relationship.AnchorSourceEntryRef),
			optString(relationship.AnchorSourceRevisionRef),
		}}); err != nil {
			return fmt.Errorf("store: install %s relationship evidence for generation %s of session %s: %w; no generation was activated", relationship.Kind, generationID, sessionID, err)
		}
	}
	return nil
}

func optSessionID(v *schema.SessionID) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func optPurpose(v *schema.SessionPurpose) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func optDiagnosticsPartial(v *bool) any {
	if v == nil {
		return nil
	}
	return boolToInt64(*v)
}

func optBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func optAnchorKind(v *schema.PublicSourceAnchorKind) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func optEntryRef(v *schema.SourceEntryRef) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

func optNativeRole(v *schema.NativePiMessageRole) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

// untypedInt64 binds a nullable count: NULL when absent, so the driver
// never stores a typed nil pointer as TEXT.
func untypedInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// seedJSONForStats renders the capture's stats document: the harness-only
// seed home's next value, replaced wholesale on every harness update.
func seedJSONForStats(stats schema.SessionStats) string {
	encoded, err := json.Marshal(stats)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// remapHarmonizedAnnotations runs C5: the carried entry targets are
// re-attached to the new main entries inside the activation commit, after
// the mapping rows and the active pointer are written. The old session
// rows are deleted first and re-inserted against the new generation, so a
// remap failure rolls everything back and the prior targets survive. When
// the previous active generation was harmonized, its body rows supply the
// old anchors (entry type, role, and part type from the entry row's
// columns); otherwise the carried anchors stand as read. The anchor match
// itself runs through the same matcher the mirror path uses.
func remapHarmonizedAnnotations(conn *sqlite.Conn, sessionID schema.SessionID, entries []schema.SessionEntry, previousHarmonized *string, stats *ingest.SessionEntryWriteStats) error {
	carried, err := readEntryAnnotationTargets(conn, string(sessionID))
	if err != nil {
		return fmt.Errorf("store: read annotation targets for session %s before the harmonized remap: %w; the prior targets are preserved", sessionID, err)
	}
	if len(carried) == 0 {
		return nil
	}
	if previousHarmonized != nil {
		anchors, err := readHarmonizedBodyAnchors(conn, sessionID, *previousHarmonized)
		if err != nil {
			return err
		}
		byIndex := make(map[int]entryTargetAnchor, len(anchors))
		for _, anchor := range anchors {
			byIndex[anchor.entryIndex] = anchor
		}
		for i := range carried {
			carried[i].anchors = carried[i].anchors[:0]
			for index := carried[i].entryIndex; index < carried[i].endIndex; index++ {
				if anchor, ok := byIndex[index]; ok {
					carried[i].anchors = append(carried[i].anchors, anchor)
				}
			}
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM annotation_target_entries WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: clear annotation targets for session %s before the harmonized remap: %w; the prior targets are preserved", sessionID, err)
	}
	if _, err := restoreEntryAnnotationTargets(conn, string(sessionID), carried, entries, stats); err != nil {
		return err
	}
	return nil
}

// readHarmonizedBodyAnchors reads the previous active generation's main
// entries as anchors for the C5 remap: the mapping rows joined to their
// body rows, reconstructed through the one row-to-struct function.
func readHarmonizedBodyAnchors(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]entryTargetAnchor, error) {
	var anchors []entryTargetAnchor
	err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumnsJoined+` FROM session_entry_bodies b JOIN session_generation_entries m ON m.session_id = b.session_id AND m.body_digest = b.body_digest WHERE m.session_id = ? AND m.generation_id = ? AND m.partition_id = 0 ORDER BY m.entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entry := entryFromRow(scanEntryRecord(stmt))
			anchors = append(anchors, entryTargetAnchorFromEntry(&entry))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read body anchors for session %s generation %s: %w", sessionID, generationID, err)
	}
	return anchors, nil
}

// skipStamps carries the bookkeeping inputs both the skip path and the
// repair transaction stamp (design §4.1 C4): the publication capture, the
// producer stamps, the input proof, and the pair identity.
type skipStamps struct {
	publicationCapture *ingest.PublicationCaptureWrite
	captureRevision    int64
	indexerVersion     int
	indexedAtMs        int64
	indexedInputHash   *string
	artifactIdentity   *string
	adapterVersion     *int
	stats              schema.SessionStats
	seedJSON           string
	updatedAtMs        int64
}

// stampHarmonizedBookkeeping runs the C4 bookkeeping inside the caller's
// transaction: the publication capture, the index state over the entries
// hash, the stats merge with its mirror, the pair identity, the adapter
// revision, and the publication index stamp. It is the same stamp list for
// the ordinary commit, the skip, and the repair.
func stampHarmonizedBookkeeping(conn *sqlite.Conn, sessionID schema.SessionID, stamps skipStamps, sessionEntriesHash string) error {
	if stamps.publicationCapture != nil {
		revision, recorded, err := persistActivationPublicationCapture(conn, sessionID, stamps.publicationCapture)
		if err != nil {
			return err
		}
		if recorded {
			stamps.captureRevision = revision
		}
	}
	if err := checkPublicationIndexRevision(conn, sessionID, stamps.captureRevision); err != nil {
		return err
	}
	if _, err := upsertCapturedStatsOnConn(conn, capturedStatsForHarnessWrite(sessionID, stamps.stats, stamps.seedJSON, stamps.updatedAtMs)); err != nil {
		return err
	}
	if stamps.artifactIdentity != nil {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ? AND artifact_hash IS NULL`, &sqlitex.ExecOptions{
			Args: []any{*stamps.artifactIdentity, string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: record the pair identity for session %s: %w", sessionID, err)
		}
	}
	if stamps.adapterVersion != nil {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET adapter_version = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{*stamps.adapterVersion, string(sessionID)},
		}); err != nil {
			return fmt.Errorf("store: stamp the adapter revision for session %s: %w", sessionID, err)
		}
	}
	if stamps.indexerVersion > 0 {
		if err := updateIndexStateWithSessionEntriesHashOnConn(conn, sessionID, stamps.indexerVersion, stamps.indexedAtMs, sessionEntriesHash, stamps.indexedInputHash); err != nil {
			return fmt.Errorf("store: update index state for session %s: %w", sessionID, err)
		}
	}
	if err := stampPublicationIndex(conn, sessionID, stamps.captureRevision); err != nil {
		return err
	}
	return nil
}

// stampSkippedHarmonized runs the P5 skip's one bookkeeping transaction
// under the exclusive lock (design §4.2): it re-checks that the active
// pointer still names the compared generation (a move falls back to the
// ordinary write path), then calls the same stamping functions the
// ordinary commit uses. The generation row keeps its older adapter_version
// column, because generation rows are immutable; the session row advances.
func stampSkippedHarmonized(ctx context.Context, s *Store, prepared *preparedHarmonized, comparedID string, stamps skipStamps) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for the skip bookkeeping of session %s: %w", prepared.sessionID, err)
	}
	defer s.pool.Put(conn)
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	active, err := readActiveGenerationOnConn(conn, prepared.sessionID)
	if err != nil {
		txnErr = err
		return txnErr
	}
	if active == nil || *active != comparedID {
		txnErr = errSkipPointerMoved
		return txnErr
	}
	hash, err := computeSessionEntriesHash(prepared.mainEntries)
	if err != nil {
		txnErr = fmt.Errorf("store: hash main entries for the skip bookkeeping of session %s: %w", prepared.sessionID, err)
		return txnErr
	}
	if txnErr = stampHarmonizedBookkeeping(conn, prepared.sessionID, stamps, hash); txnErr != nil {
		return txnErr
	}
	return nil
}

// repairNeedsHarmonized reports whether the session needs repair-mode
// activation: it matches the repair predicate (an artifact identity with a
// cleared input proof) while its active generation is harmonized.
// Corruption changes stored bytes, not canonical digests, so a repair
// re-index of unchanged input would compare equal and skip, leaving the
// corrupt object in place. Repair bypasses the skip, inserts no new
// generation row, and rewrites the failing objects from the candidate.
func repairNeedsHarmonized(conn *sqlite.Conn, sessionID schema.SessionID) (bool, error) {
	active, err := readActiveGenerationOnConn(conn, sessionID)
	if err != nil {
		return false, err
	}
	if active == nil || *active == "" {
		return false, nil
	}
	harmonized := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), *active},
		ResultFunc: func(*sqlite.Stmt) error { harmonized = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("store: check the active generation for session %s: %w", sessionID, err)
	}
	if !harmonized {
		return false, nil
	}
	predicate := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions WHERE session_id = ? AND artifact_hash IS NOT NULL AND indexed_input_hash IS NULL LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID)},
		ResultFunc: func(*sqlite.Stmt) error { predicate = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("store: check the repair predicate for session %s: %w", sessionID, err)
	}
	if !predicate {
		return false, nil
	}
	// The predicate alone cannot select repair: the mirror clears the input
	// proof on every artifact-hash change, so it also matches an ordinary
	// changed-input refresh whose stored objects are healthy — and repair
	// inserts no new generation row, so it would swallow the refresh. Only
	// failing objects take the in-place rewrite; a healthy store takes the
	// ordinary skip-or-commit path, which restores the proof itself.
	return activeObjectsNeedRepair(conn, sessionID, *active)
}

// activeObjectsNeedRepair reports whether the active generation's mapped
// objects fail self-verification: a mapped body missing or recomputing a
// different digest than its stored anchor, or a descriptor whose blob
// bytes do not verify. Blob verification hashes the concatenated chunk
// bytes in chunk order against the descriptor digest and checks the summed
// chunk lengths against the header byte length — the same proof a full
// read demands of blob bytes — so a flipped chunk byte, a byte-length
// error inside one chunk count, or a torn chunk row all count as failing.
// This gate and the read path can never disagree about corruption.
func activeObjectsNeedRepair(conn *sqlite.Conn, sessionID schema.SessionID, activeID string) (bool, error) {
	var digests []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), activeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: list active mapped bodies for session %s: %w; the repair decision could not be made", sessionID, err)
	}
	for _, digest := range digests {
		found := false
		failing := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				record := scanEntryRecord(stmt)
				if string(bodyDigestForRecord(record)) != record.BodyDigest {
					failing = true
				}
				return nil
			},
		}); err != nil {
			return false, fmt.Errorf("store: verify active body %s for session %s: %w; the repair decision could not be made", digest, sessionID, err)
		}
		if !found || failing {
			return true, nil
		}
	}
	var blobDigests []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT digest FROM session_generation_content WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), activeID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			blobDigests = append(blobDigests, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: list active descriptors for session %s: %w; the repair decision could not be made", sessionID, err)
	}
	for _, digest := range blobDigests {
		var byteLength int64
		header := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT byte_length FROM session_content WHERE session_id = ? AND digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				header = true
				byteLength = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			return false, fmt.Errorf("store: verify active blob %s for session %s: %w; the repair decision could not be made", digest, sessionID, err)
		}
		if !header {
			return true, nil
		}
		// Hash the stored bytes in chunk order: the schema keeps no
		// per-chunk digest, so the concatenation hash against the blob
		// digest is the byte proof, and the summed lengths against the
		// header length is the truncation proof. Both run here because the
		// gate only runs for repair-predicate sessions.
		hasher := sha256.New()
		var total, chunks int64
		contiguous := true
		if err := sqlitex.ExecuteTransient(conn, `SELECT chunk_index, data FROM session_content_chunks WHERE session_id = ? AND digest = ? ORDER BY chunk_index`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnInt64(0) != chunks {
					contiguous = false
				}
				n := stmt.ColumnLen(1)
				data := make([]byte, n)
				stmt.ColumnBytes(1, data)
				hasher.Write(data)
				total += int64(n)
				chunks++
				return nil
			},
		}); err != nil {
			return false, fmt.Errorf("store: read active blob %s chunks for session %s: %w; the repair decision could not be made", digest, sessionID, err)
		}
		wantChunks := (byteLength + 65535) / 65536
		if !contiguous || chunks != wantChunks || total != byteLength {
			return true, nil
		}
		if hex.EncodeToString(hasher.Sum(nil)) != digest {
			return true, nil
		}
	}
	return false, nil
}

// repairHarmonizedObjects rewrites the candidate's objects in place in one
// repair transaction under the exclusive lock (design §4.6): neither a
// staging sub-transaction nor a catalog commit, because there is no new
// generation. A referenced entry row is rewritten with PRAGMA
// defer_foreign_keys=ON — an explicit DELETE (the BEFORE DELETE trigger
// fires), then an INSERT of the corrected row under the same
// (session_id, body_digest) and a new body_id — so the mapping rows, which
// reference the digest, stay untouched. A referenced blob uses the same
// shape on its header (chunks cascade, then header and chunks re-inserted
// under the same digest). A wrong descriptor is deleted and re-inserted
// from the parsed input. INSERT OR REPLACE is never used on any of these
// paths: it skips the BEFORE DELETE trigger and leaves stale postings.
// Because a deleted entry row failed verification, the transaction sets
// needs_rebuild, which the whole-index rebuild clears; the ordinary
// bookkeeping runs, which clears the repair predicate.
func repairHarmonizedObjects(ctx context.Context, s *Store, prepared *preparedHarmonized, activeID string, stamps skipStamps) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for the repair of session %s: %w", prepared.sessionID, err)
	}
	defer s.pool.Put(conn)
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA defer_foreign_keys = ON`, nil); err != nil {
		txnErr = fmt.Errorf("store: defer foreign keys for the repair of session %s: %w", prepared.sessionID, err)
		return txnErr
	}
	defer func() {
		_ = sqlitex.ExecuteTransient(conn, `PRAGMA defer_foreign_keys = OFF`, nil)
	}()
	for i := range prepared.bodies {
		record := prepared.bodies[i]
		digest := string(prepared.bodyDigests[i])
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(prepared.sessionID), digest},
		}); err != nil {
			txnErr = fmt.Errorf("store: delete damaged body %s of session %s for repair: %w", digest, prepared.sessionID, err)
			return txnErr
		}
		if err := insertStagedBodyOnConn(conn, prepared.sessionID, &record); err != nil {
			txnErr = fmt.Errorf("store: rewrite body %s of session %s for repair: %w", digest, prepared.sessionID, err)
			return txnErr
		}
	}
	for _, blob := range prepared.blobs {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_content WHERE session_id = ? AND digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(prepared.sessionID), string(blob.digest)},
		}); err != nil {
			txnErr = fmt.Errorf("store: delete damaged blob %s of session %s for repair: %w", blob.digest, prepared.sessionID, err)
			return txnErr
		}
		blobCopy := blob
		if err := insertStagedBlobOnConn(conn, prepared.sessionID, &blobCopy); err != nil {
			txnErr = fmt.Errorf("store: rewrite blob %s of session %s for repair: %w", blob.digest, prepared.sessionID, err)
			return txnErr
		}
	}
	for _, blob := range prepared.blobs {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_generation_content WHERE session_id = ? AND generation_id = ? AND source_entry_ref = ?`, &sqlitex.ExecOptions{
			Args: []any{string(prepared.sessionID), activeID, string(blob.ref)},
		}); err != nil {
			txnErr = fmt.Errorf("store: delete descriptor %q of session %s for repair: %w", blob.ref, prepared.sessionID, err)
			return txnErr
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_generation_content(session_id, generation_id, source_entry_ref, digest) VALUES (?, ?, ?, ?)`, &sqlitex.ExecOptions{
			Args: []any{string(prepared.sessionID), activeID, string(blob.ref), string(blob.digest)},
		}); err != nil {
			txnErr = fmt.Errorf("store: rewrite descriptor %q of session %s for repair: %w", blob.ref, prepared.sessionID, err)
			return txnErr
		}
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 1 WHERE id = 1`, nil); err != nil {
		txnErr = fmt.Errorf("store: flag the search index for rebuild after the repair of session %s: %w", prepared.sessionID, err)
		return txnErr
	}
	hash, err := computeSessionEntriesHash(prepared.mainEntries)
	if err != nil {
		txnErr = fmt.Errorf("store: hash main entries for the repair of session %s: %w", prepared.sessionID, err)
		return txnErr
	}
	if txnErr = stampHarmonizedBookkeeping(conn, prepared.sessionID, stamps, hash); txnErr != nil {
		return txnErr
	}
	return nil
}

// deleteConvertedMirrorRows runs the C6 post-commit delete pass after the
// locks are released: a session whose active generation is harmonized
// holds no mirror rows, so any session_entries rows under its id are a
// converted predecessor's leftovers. They go in bounded batches (the
// configured sweep_rows); the short dual-representation window they leave
// is covered by the representation filter. The ext, command, and
// full-content rows cascade from session_entries, and the chunks cascade
// from the full-content manifest, so one batched delete clears the whole
// mirror. File-backed sessions never reach this pass: the guard returns
// them untouched.
func (s *Store) deleteConvertedMirrorRows(ctx context.Context, sessionID schema.SessionID) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for the converted-mirror delete of session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)
	harmonized := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions s JOIN session_generations g ON g.session_id = s.session_id AND g.generation_id = s.active_generation_id WHERE s.session_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID)},
		ResultFunc: func(*sqlite.Stmt) error { harmonized = true; return nil },
	}); err != nil {
		return fmt.Errorf("store: check the converted guard for session %s: %w", sessionID, err)
	}
	if !harmonized {
		return nil
	}
	cfg := s.writeConfigForLane()
	limit := cfg.SweepRows
	if limit < 1 {
		limit = 1
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries WHERE session_id = ? AND rowid IN (SELECT rowid FROM session_entries WHERE session_id = ? LIMIT ?)`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), string(sessionID), int64(limit)},
		}); err != nil {
			return fmt.Errorf("store: delete converted mirror rows for session %s: %w; the harmonized generation is active and the leftovers are retried on the next harvest", sessionID, err)
		}
		if conn.Changes() == 0 {
			return nil
		}
	}
}

// harmonizedBatchSkip is the batch path's precommit decision: commit
// proceeds to the catalog write, while a skip carries the idempotent
// retry's outcome (the hash and the stats) straight to the shared stamps.
type harmonizedBatchSkip struct {
	commit  bool
	outcome sessionEntryWriteOutcome
}

// harmonizedBatchPrecommit runs the batch path's precommit decision for one
// V2 write on the caller's connection: it prepares the candidate (P1–P4
// pure), refuses a format conversion (V2 conversions run through the
// migration, never through a parser-less batch write), honors the immutable
// identity (an identical retry advances only the bookkeeping and reports
// skipped; any other reuse refuses), and refuses repair-predicate sessions
// (they activate in repair mode through ActivateGeneration, which rewrites
// objects instead of comparing past them). The ordinary write returns the
// prepared candidate for the commit and the stats merge.
func (s *Store) harmonizedBatchPrecommit(conn *sqlite.Conn, write ingest.SessionEntryWrite) ([]schema.SessionEntry, *preparedHarmonized, harmonizedBatchSkip, error) {
	v2, ok := asV2Value(write.Result)
	if !ok {
		return nil, nil, harmonizedBatchSkip{}, generationIndexFormat{}.Validate(write.Result)
	}
	if write.Mode == ingest.SessionEntryWriteFormatConversion {
		return nil, nil, harmonizedBatchSkip{}, fmt.Errorf("store: session %s requests a format conversion to a managed generation; no V2 conversion is registered and a conversion may not invent entries; convert through peasant migrate instead", write.SessionID)
	}
	prepared, err := prepareHarmonizedCandidate(write.SessionID, v2.Generation, nil)
	if err != nil {
		return nil, nil, harmonizedBatchSkip{}, err
	}
	stored, found, err := readStoredCandidateDigest(conn, write.SessionID, v2.Generation.ID)
	if err != nil {
		return nil, nil, harmonizedBatchSkip{}, err
	}
	if found {
		if stored != prepared.binding {
			return nil, nil, harmonizedBatchSkip{}, fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is already installed with a different candidate binding; immutable identifiers cannot be reused; the installed generation is unchanged", v2.Generation.ID, write.SessionID)
		}
		hash, err := computeSessionEntriesHash(prepared.mainEntries)
		if err != nil {
			return nil, nil, harmonizedBatchSkip{}, fmt.Errorf("store: hash main entries for the idempotent retry of session %s: %w", write.SessionID, err)
		}
		if _, err := upsertCapturedStatsOnConn(conn, capturedStatsForHarnessWrite(write.SessionID, v2.Generation.Metadata.Stats, seedJSONForStats(v2.Generation.Metadata.Stats), write.IndexedAtMs)); err != nil {
			return nil, nil, harmonizedBatchSkip{}, err
		}
		outcome := sessionEntryWriteOutcome{sessionEntriesHash: hash, skipped: true}
		outcome.stats.SkippedByCompare++
		return prepared.mainEntries, prepared, harmonizedBatchSkip{commit: false, outcome: outcome}, nil
	}
	fileBacked, err := generationIDIsFileBacked(conn, write.SessionID, v2.Generation.ID)
	if err != nil {
		return nil, nil, harmonizedBatchSkip{}, err
	}
	if fileBacked {
		return nil, nil, harmonizedBatchSkip{}, fmt.Errorf("store: refuse to activate generation %s for session %s: the identifier is installed in the file-backed catalog, which only the migration moves; the installed generation is unchanged", v2.Generation.ID, write.SessionID)
	}
	repair, err := repairNeedsHarmonized(conn, write.SessionID)
	if err != nil {
		return nil, nil, harmonizedBatchSkip{}, err
	}
	if repair {
		return nil, nil, harmonizedBatchSkip{}, fmt.Errorf("store: session %s matches the repair predicate with a harmonized active generation; its stored bytes may be corrupt, so a comparing write could report unchanged and leave the damage in place; activate in repair mode, which rewrites the failing objects instead", write.SessionID)
	}
	return prepared.mainEntries, prepared, harmonizedBatchSkip{commit: true}, nil
}

// readStoredCandidateDigest reads the installed binding for one generation
// identifier, reporting whether a harmonized row names it.
func readStoredCandidateDigest(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (string, bool, error) {
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
		return "", false, fmt.Errorf("store: check generation identity for session %s: %w", sessionID, err)
	}
	return stored, found, nil
}

// generationIDIsFileBacked reports whether the identifier is installed in
// the file-backed catalog, which only the migration moves.
func generationIDIsFileBacked(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) (bool, error) {
	fileBacked := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_projection_generations WHERE session_id = ? AND generation_id = ? LIMIT 1`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { fileBacked = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("store: check the file-backed catalog for session %s: %w", sessionID, err)
	}
	return fileBacked, nil
}
