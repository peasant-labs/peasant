package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// A reference installs a context change as a new generation without a parse.
// The new generation has its own catalog row and context rows, written from
// the builder's metadata half, and reads the parsed rows of the generation its
// mapping_set_id names. Its seal is computed over its own catalog row and the
// owner's body and blob digests, which is the seal a parse of the same input
// computes, so a later identical parse takes the ordinary skip.
//
// The plan reads on a reader connection and writes nothing. The install runs
// on the write lane under the exclusive session lock in one transaction. It
// reads no parse table: it checks the catalog rows and the index state the
// plan read, then writes the catalog row, the context rows, the stats, the
// session's mirror columns and the publication binding.

var (
	_ ingest.ManagedGenerationReferencer    = (*Store)(nil)
	_ ingest.ManagedGenerationReferencePlan = (*ManagedGenerationReferencePlan)(nil)
	_ ingest.ReferenceRefusal               = (*ManagedGenerationReferenceError)(nil)
)

// ManagedGenerationReferencePlan is one planned reference. It is opaque
// outside the store, and only the store that made it installs it.
type ManagedGenerationReferencePlan struct {
	store        *Store
	sessionID    schema.SessionID
	source       string
	owner        string
	sourceSeal   string
	ownerSeal    string
	generationID string
	// record is the new catalog row with its seal; children holds its
	// context rows.
	record   GenerationRecord
	children GenerationChildren
	// metadata is the candidate's metadata; its stats become the session's
	// stats with the stored parse counts.
	metadata ingest.UnifiedMetadata
	expected *ingest.SessionIndexState
}

// SessionID names the session the plan is for.
func (p *ManagedGenerationReferencePlan) SessionID() ingest.SessionID { return p.sessionID }

// SourceGenerationID names the active generation the plan read.
func (p *ManagedGenerationReferencePlan) SourceGenerationID() string { return p.source }

// OwnerGenerationID names the generation whose parsed rows the new
// generation reads.
func (p *ManagedGenerationReferencePlan) OwnerGenerationID() string { return p.owner }

// ManagedGenerationReferenceError is one refused reference. Nothing was
// written; the session parses.
type ManagedGenerationReferenceError struct {
	SessionID          schema.SessionID
	SourceGenerationID string
	Reason             ingest.ReferenceRefusalReason
	Detail             string
	// Where names the step that refused: the plan or the install.
	Where string
}

func (e *ManagedGenerationReferenceError) Error() string {
	return fmt.Sprintf(
		"store: refuse to reference the parsed rows of generation %s for session %s: %s (%s); where: %s; when: before any row changed; impact: the active generation is unchanged and the session parses its transcript instead; fix: %s",
		e.SourceGenerationID, e.SessionID, e.Reason, e.Detail, e.Where, referenceRefusalFix(e.Reason))
}

// RefusalReason reports the refusal's reason.
func (e *ManagedGenerationReferenceError) RefusalReason() ingest.ReferenceRefusalReason {
	return e.Reason
}

// referenceRefusalFix is the action a refusal leaves to the caller.
func referenceRefusalFix(reason ingest.ReferenceRefusalReason) string {
	switch reason {
	case ingest.ReferenceRefusalContextUnchanged:
		return "nothing to do; the validated settle applies to an unchanged context"
	case ingest.ReferenceRefusalSourceMoved:
		return "nothing to do; the next harvest captures the new state and retries"
	case ingest.ReferenceRefusalStatsIncomplete, ingest.ReferenceRefusalOwnerSeal:
		return "let the parse run; it rewrites the parsed rows, their seal and the counts"
	default:
		return "let the parse run; it installs the change"
	}
}

const (
	referenceWherePlan    = "store.PlanManagedGenerationReference"
	referenceWhereInstall = "store.InstallManagedGenerationReference"
)

// referenceInstallSeam is the install's test seam: conn is called with the
// install connection before the transaction starts, and read is called inside
// it before any write. Production leaves both nil.
type referenceInstallSeam struct {
	conn func(*sqlite.Conn)
	read func(*sqlite.Conn) error
}

// PlanManagedGenerationReference checks that a context change can be
// installed as a reference and computes the new generation. It holds one
// refresh read slot and reads on one reader connection, in one read
// transaction. It writes nothing.
func (s *Store) PlanManagedGenerationReference(ctx context.Context, request ingest.ManagedGenerationReferenceRequest) (plan ingest.ManagedGenerationReferencePlan, err error) {
	proof := request.Proof
	if proof.Expected == nil || proof.Expected.ActiveGenerationID == "" {
		return nil, fmt.Errorf("store: plan a reference for session %s: the request carries no captured index state; nothing was read; capture the index state before the refresh", proof.SessionID)
	}
	if err := validateGenerationID(request.GenerationID); err != nil {
		return nil, fmt.Errorf("store: plan a reference for session %s: %w; nothing was read", proof.SessionID, err)
	}
	release, err := s.acquireRefreshReadSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take a reader connection to plan a reference for session %s: %w; nothing was read; the session parses", proof.SessionID, err)
	}
	defer s.pool.Put(conn)
	end := sqlitex.Transaction(conn)
	defer end(&err)
	planned, err := s.planReferenceOnConn(conn, request)
	if err != nil {
		return nil, err
	}
	return planned, nil
}

func (s *Store) planReferenceOnConn(conn *sqlite.Conn, request ingest.ManagedGenerationReferenceRequest) (*ManagedGenerationReferencePlan, error) {
	proof := request.Proof
	sid := proof.SessionID
	source := proof.Expected.ActiveGenerationID
	candidate := request.Candidate
	refuse := func(reason ingest.ReferenceRefusalReason, detail string) (*ManagedGenerationReferencePlan, error) {
		return nil, &ManagedGenerationReferenceError{SessionID: sid, SourceGenerationID: source, Reason: reason, Detail: detail, Where: referenceWherePlan}
	}
	terms, err := evaluateUnchangedTermsOnConn(conn, ManagedGenerationUnchangedProof{
		SessionID: sid, GenerationID: source, Metadata: proof.Metadata,
		InputDigest: proof.InputDigest, IndexerTarget: proof.IndexerTarget, FormatTarget: proof.FormatTarget,
		Expected:  proof.Expected,
		Validated: &ingest.ValidatedContext{GenerationID: source, Candidate: candidate},
	}, termModeValidateContext)
	if err != nil {
		return nil, err
	}
	if terms.Proven {
		return refuse(ingest.ReferenceRefusalContextUnchanged, "the candidate's context equals the stored context")
	}
	switch terms.Mismatch {
	case ingest.UnchangedNotProvenMetadata:
	case ingest.UnchangedNotProvenActive:
		return refuse(ingest.ReferenceRefusalSourceMoved, "the active generation is not the one the captured state names")
	case ingest.UnchangedNotProvenLatestAttempt:
		return refuse(ingest.ReferenceRefusalLatestAttempt, "the session holds a latest attempt beside its active generation")
	case ingest.UnchangedNotProvenContent, ingest.UnchangedNotProvenSourceFormat, ingest.UnchangedNotProvenHarness,
		ingest.UnchangedNotProvenCompleteness, ingest.UnchangedNotProvenSourceEvidence:
		return refuse(ingest.ReferenceRefusalContentIdentity, fmt.Sprintf("the %s term does not hold", terms.Mismatch))
	default:
		return refuse(ingest.ReferenceRefusalNotContextOnly, fmt.Sprintf("the %s term does not hold", terms.Mismatch))
	}
	var sourceView activeHarmonizedView
	if err := loadGenerationRecordOnConn(conn, sid, source, &sourceView); err != nil {
		return nil, err
	}
	if !sourceView.found {
		return refuse(ingest.ReferenceRefusalSourceMoved, "the active generation has no harmonized catalog row")
	}
	stored := sourceView.record
	if detail := contentIdentityChange(stored, candidate); detail != "" {
		return refuse(ingest.ReferenceRefusalContentIdentity, detail)
	}
	if len(request.ParseInputContextFields) > 0 {
		changed, err := parseInputContextChange(conn, sid, source, stored, candidate.Metadata, request.ParseInputContextFields)
		if err != nil {
			return nil, err
		}
		if changed != "" {
			return refuse(ingest.ReferenceRefusalParseInputContext, fmt.Sprintf("the parsed rows depend on %s, and it changed", changed))
		}
	}
	stats, err := readCapturedStatsOnConn(conn, sid)
	if err != nil && !errors.Is(err, ErrNoCapturedStats) {
		return nil, fmt.Errorf("store: read the parse counts of session %s to plan a reference: %w; nothing was written; the session parses", sid, err)
	}
	countsStored := err == nil && stats.TurnCount != nil && stats.ConversationTurnCount != nil
	if !countsStored || stats.InputSubmissionCount == nil {
		return refuse(ingest.ReferenceRefusalStatsIncomplete, "the stored turn, conversation-turn or input-submission count is missing")
	}
	owner, err := rowsGenerationIDOnConn(conn, sid, source)
	if err != nil {
		return nil, err
	}
	ownerSeal := stored.CandidateDigest
	if owner != source {
		ownerSeal, err = ownerSealOnConn(conn, sid, owner)
		if err != nil {
			return nil, err
		}
		if ownerSeal == "" {
			return refuse(ingest.ReferenceRefusalOwnerSeal, "the generation the source references is missing or is itself a reference")
		}
	}
	bodies, blobs, err := ownerDigestsOnConn(conn, sid, owner)
	if err != nil {
		return nil, err
	}
	if sealOf(stored, bodies, blobs) != stored.CandidateDigest {
		return refuse(ingest.ReferenceRefusalOwnerSeal, "the stored seal of the active generation does not match its catalog row and its parsed rows")
	}
	record, children, err := mapGenerationRecord(sid, indexformat.Generation{
		ID:                   request.GenerationID,
		Metadata:             candidate.Metadata,
		Completeness:         candidate.Completeness,
		SourceEvidenceDigest: candidate.SourceEvidenceDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("store: map the metadata candidate of session %s for a reference: %w; nothing was written; the session parses", sid, err)
	}
	record.CandidateDigest = computeActivationBinding(record, bodies, blobs)
	// The new generation reads the owner's parsed rows, so it carries the
	// same prior evidence the owner's parse wrote.
	record.PriorEvidence = append([]byte(nil), stored.PriorEvidence...)
	metadata := candidate.Metadata
	metadata.Stats.TurnCount = *stats.TurnCount
	metadata.Stats.InputSubmissionCount = stats.InputSubmissionCount
	conversation := int64(*stats.ConversationTurnCount)
	metadata.Stats.ConversationTurnCount = &conversation
	return &ManagedGenerationReferencePlan{
		store: s, sessionID: sid, source: source, owner: owner,
		sourceSeal: stored.CandidateDigest, ownerSeal: ownerSeal,
		generationID: request.GenerationID, record: record,
		children: GenerationChildren{
			Subagents: children.Subagents, Commits: children.Commits, Associations: children.Associations,
			Diagnostics: children.Diagnostics, Relationships: children.Relationships,
		},
		metadata: metadata, expected: proof.Expected,
	}, nil
}

// sealOf recomputes a stored generation's seal from its catalog row and the
// digests of the parsed rows it reads. The seal was computed before the
// prior evidence was attached, so the evidence is left out here too.
func sealOf(stored GenerationRecord, bodies []BodyDigest, blobs []BlobDigest) string {
	stored.PriorEvidence = nil
	return computeActivationBinding(stored, bodies, blobs)
}

// contentIdentityChange reports how the candidate's content identity differs
// from the stored generation's, or the empty string when it does not.
func contentIdentityChange(stored GenerationRecord, candidate ingest.GenerationMetadataCandidate) string {
	switch {
	case candidate.Metadata.ModelHarness != stored.Harness:
		return "the harness changed"
	case candidate.Metadata.ContentHash != stored.ContentHash:
		return "the content hash changed"
	case candidate.Metadata.Source.Format != stored.SourceFormat:
		return "the source format changed"
	case candidate.Completeness != stored.Completeness:
		return "the completeness changed"
	case candidate.SourceEvidenceDigest != stored.SourceEvidenceDigest:
		return "the source-evidence digest changed"
	}
	return ""
}

// parseInputContextChange returns the first listed field whose value differs
// between the candidate metadata and the stored generation's metadata, or the
// empty string. Fields are dotted paths of JSON names.
func parseInputContextChange(conn *sqlite.Conn, sid schema.SessionID, generationID string, stored GenerationRecord, candidate ingest.UnifiedMetadata, fields []string) (string, error) {
	var children GenerationChildren
	if err := loadGenerationContextChildrenOnConn(conn, sid, generationID, &children); err != nil {
		return "", err
	}
	storedJSON := serializeMetadata(stored, children, CapturedStats{SessionID: sid})
	candidateJSON, err := json.Marshal(candidate)
	if err != nil || storedJSON == nil {
		return "", fmt.Errorf("store: encode the metadata of session %s to compare the parse-input fields: %v; nothing was written; the session parses", sid, err)
	}
	var left, right map[string]any
	if err := json.Unmarshal(storedJSON, &left); err != nil {
		return "", err
	}
	if err := json.Unmarshal(candidateJSON, &right); err != nil {
		return "", err
	}
	for _, field := range fields {
		if !reflect.DeepEqual(jsonPathValue(left, field), jsonPathValue(right, field)) {
			return field, nil
		}
	}
	return "", nil
}

// jsonPathValue walks a dotted path of JSON names; a missing step is nil.
func jsonPathValue(document map[string]any, path string) any {
	var value any = document
	for _, step := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[step]
	}
	return value
}

// ownerSealOnConn returns the stored seal of an owner generation, or the empty
// string when the generation is missing or is itself a reference.
func ownerSealOnConn(conn *sqlite.Conn, sid schema.SessionID, owner string) (string, error) {
	seal := ""
	err := sqlitex.Execute(conn, `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ? AND mapping_set_id IS NULL`, &sqlitex.ExecOptions{
		Args: []any{string(sid), owner},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			seal = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: read the seal of owner generation %s for session %s: %w; nothing was written; the session parses", owner, sid, err)
	}
	return seal, nil
}

// ownerDigestsOnConn reads the owner's body digests in (partition, index)
// order and its non-emitted blob digests in ref order: the operands of the
// seal, in the order the parse's prepare computes them.
//
// Both slices are non-nil, as the prepare's are: the seal hashes their JSON
// encoding, in which an empty list and a missing one differ.
func ownerDigestsOnConn(conn *sqlite.Conn, sid schema.SessionID, owner string) ([]BodyDigest, []BlobDigest, error) {
	bodies := []BodyDigest{}
	if err := sqlitex.Execute(conn, `SELECT body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sid), owner},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bodies = append(bodies, BodyDigest(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("store: read the body digests of owner generation %s for session %s: %w; nothing was written; the session parses", owner, sid, err)
	}
	blobs := []BlobDigest{}
	if err := sqlitex.Execute(conn, `SELECT digest FROM session_generation_content WHERE session_id = ? AND generation_id = ? ORDER BY source_entry_ref`, &sqlitex.ExecOptions{
		Args: []any{string(sid), owner},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			blobs = append(blobs, BlobDigest(stmt.ColumnText(0)))
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("store: read the blob digests of owner generation %s for session %s: %w; nothing was written; the session parses", owner, sid, err)
	}
	return bodies, blobs, nil
}

// InstallManagedGenerationReference installs a planned reference under the
// exclusive session lock in one transaction. It first checks that nothing the
// plan read has moved: the active generation, the latest attempt, the index
// state, the source's seal, and the owner's seal and ownership. Then it writes
// the new catalog row with mapping_set_id naming the owner, its context rows,
// the stats, the session's mirror columns and the publication binding, moves
// the active generation, and flags the session for sweep when the source
// becomes collectable. It writes no parsed row and reads no parse table.
func (s *Store) InstallManagedGenerationReference(ctx context.Context, planned ingest.ManagedGenerationReferencePlan) (outcome ingest.ManagedGenerationReferenceOutcome, err error) {
	plan, ok := planned.(*ManagedGenerationReferencePlan)
	if !ok || plan == nil || plan.store != s {
		return outcome, fmt.Errorf("store: install a reference: the plan was not made by this store; nothing was written; plan the reference on the store that installs it")
	}
	if s.sessionLocker == nil {
		return outcome, fmt.Errorf("store: install a reference for session %s: session locking is not configured; nothing was written; open the store with managed generation support", plan.sessionID)
	}
	release, err := s.sessionLocker.LockExclusive(ctx, plan.sessionID)
	if err != nil {
		return outcome, fmt.Errorf("store: lock session %s to install a reference: %w; nothing was written; the session parses", plan.sessionID, err)
	}
	defer func() { _ = release() }()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return outcome, fmt.Errorf("store: take a connection to install a reference for session %s: %w; nothing was written; the session parses", plan.sessionID, err)
	}
	defer s.pool.Put(conn)
	if seam := s.referenceInstallSeam.conn; seam != nil {
		seam(conn)
	}
	end, err := beginWrite(conn, "install the reference generation of session "+string(plan.sessionID))
	if err != nil {
		return outcome, err
	}
	defer end(&err)
	if seam := s.referenceInstallSeam.read; seam != nil {
		if err = seam(conn); err != nil {
			return outcome, err
		}
	}
	if err = s.installReferenceOnConn(conn, plan, time.Now().UnixMilli()); err != nil {
		return outcome, err
	}
	return ingest.ManagedGenerationReferenceOutcome{GenerationID: plan.generationID, OwnerGenerationID: plan.owner}, nil
}

func (s *Store) installReferenceOnConn(conn *sqlite.Conn, plan *ManagedGenerationReferencePlan, nowMs int64) error {
	sid := plan.sessionID
	moved := func(detail string) error {
		return &ManagedGenerationReferenceError{SessionID: sid, SourceGenerationID: plan.source, Reason: ingest.ReferenceRefusalSourceMoved, Detail: detail, Where: referenceWhereInstall}
	}
	keep, err := generationKeepSetOnConn(conn, sid)
	if err != nil {
		return err
	}
	if keep.active != plan.source {
		return moved("the active generation changed after the plan")
	}
	if keep.latest != "" {
		return &ManagedGenerationReferenceError{SessionID: sid, SourceGenerationID: plan.source, Reason: ingest.ReferenceRefusalLatestAttempt, Detail: "a latest attempt appeared after the plan", Where: referenceWhereInstall}
	}
	if keep.activeOwner != plan.owner {
		return moved("the generation the source reads changed after the plan")
	}
	state, err := readIndexStateOnConn(conn, sid)
	if err != nil {
		return err
	}
	if !indexStateStillExpected(plan.expected, state) {
		return moved("the index state changed after the plan")
	}
	sourceSeal, err := generationSealOnConn(conn, sid, plan.source)
	if err != nil {
		return err
	}
	if sourceSeal != plan.sourceSeal {
		return moved("the source's seal changed after the plan")
	}
	ownerSeal, err := ownerSealOnConn(conn, sid, plan.owner)
	if err != nil {
		return err
	}
	if ownerSeal == "" || ownerSeal != plan.ownerSeal {
		return moved("the owner's seal or ownership changed after the plan")
	}
	if existing, err := generationSealOnConn(conn, sid, plan.generationID); err != nil {
		return err
	} else if existing != "" {
		return moved("the new generation identity is already installed")
	}
	if err := writeReferenceStatsOnConn(conn, sid, plan.metadata.Stats, nowMs); err != nil {
		return err
	}
	// The sweep flag marks a session that holds collectable rows. A source
	// that owns its rows becomes the new generation's owner and stays live,
	// so nothing becomes collectable; a source that was itself a reference
	// becomes superseded and is collected by the next sweep.
	if plan.source != plan.owner {
		if err := sqlitex.Execute(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
			return fmt.Errorf("store: flag session %s for sweep before installing a reference: %w; the transaction rolled back and the active generation is unchanged", sid, err)
		}
	}
	record := plan.record
	record.InstalledAtMs = nowMs
	record.ActivatedAtMs = &nowMs
	if err := insertGenerationCatalogOnConn(conn, sid, plan.generationID, record, plan.children, plan.owner); err != nil {
		return err
	}
	if err := pointSessionAtReferenceOnConn(conn, sid, indexformat.Generation{ID: plan.generationID, Metadata: plan.metadata}); err != nil {
		return err
	}
	if err := syncInputSubmissionMirrorOnConn(conn, sid); err != nil {
		return err
	}
	if revision := plan.expected.PublicationCaptureRevision; revision > 0 {
		if err := checkPublicationIndexRevision(conn, sid, revision); err != nil {
			return err
		}
		if err := sqlitex.Execute(conn, `UPDATE session_content_captures SET publication_capture_revision = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{revision, string(sid)}}); err != nil {
			return fmt.Errorf("store: move the content capture of session %s to metadata capture revision %d: %w; the transaction rolled back and the active generation is unchanged", sid, revision, err)
		}
		if err := stampPublicationIndex(conn, sid, revision); err != nil {
			return err
		}
	}
	return nil
}

// generationSealOnConn returns a generation's stored seal, or the empty
// string when the generation has no harmonized catalog row.
func generationSealOnConn(conn *sqlite.Conn, sid schema.SessionID, generationID string) (string, error) {
	seal := ""
	err := sqlitex.Execute(conn, `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			seal = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: read the seal of generation %s for session %s: %w; the transaction rolled back and the active generation is unchanged", generationID, sid, err)
	}
	return seal, nil
}

// writeReferenceStatsOnConn writes the session's stats the way the unchanged
// settle does: the metadata half's stats with the stored parse counts.
func writeReferenceStatsOnConn(conn *sqlite.Conn, sid schema.SessionID, metadataStats schema.SessionStats, nowMs int64) error {
	stats := capturedStatsForHarnessWrite(sid, metadataStats, seedJSONForStats(metadataStats), nowMs)
	overflow, warnings, err := retainPriorStatsOverflowOnConn(conn, stats)
	if err != nil {
		return err
	}
	if len(warnings) != 0 {
		return fmt.Errorf("store: install a reference for session %s: the stats overflow needs diagnostic bookkeeping outside this transaction; nothing was written; the session parses", sid)
	}
	stats.Overflow = overflow
	if err := sqlitex.Execute(conn, sqlUpsertCapturedStats, &sqlitex.ExecOptions{Args: []any{string(stats.SessionID), nullableArg(stats.TurnCount), nullableArg64(stats.InputSubmissionCount), nullableArg(stats.ToolCallCount), nullableArg(stats.SubagentCount), nullableArg64(stats.DurationMs), nullableArg(stats.TokensIn), nullableArg(stats.TokensOut), nullableArg(stats.ThoughtTokens), nullableArg(stats.CachedReadTokens), nullableArg(stats.CachedWriteTokens), nullableStr(stats.SeedJSON), string(stats.Source), stats.UpdatedAtMs, nullableStr(stats.Overflow), nullableArg(stats.ConversationTurnCount), nullableArg(stats.ConversationTurnCount)}}); err != nil {
		return fmt.Errorf("store: write the stats of session %s for a reference: %w; the transaction rolled back and the active generation is unchanged", sid, err)
	}
	return nil
}
