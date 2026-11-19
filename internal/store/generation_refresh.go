package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// A metadata-only refresh of a managed generation installs a NEW immutable
// format-2 generation that reuses the active generation's parsed state by
// reference. The derivation rule is the inverse of the one stated with the
// pipeline's bind rule (ingest.Pipeline.capturedInputBindsWithoutReparse):
//
//   - The PARSE owns the generation's main and earlier entries, its native
//     metadata, its non-emitted content descriptors, its aliases, its captured
//     context segments and segment refs, its title refs, and the session-scoped
//     entry bodies and content bytes. Those rows are copied (or referenced)
//     unchanged, so the write cost is O(entry references + structural rows),
//     never O(transcript bytes).
//   - The METADATA CAPTURE owns the flattened catalog columns (project, git,
//     host, model, source, timestamps, redaction context, adapter revision,
//     diagnostics flag), mutable measured stats other than the three
//     parse-derived turn and submission counts, and the metadata-derived
//     children: subagents, commits, associations, and diagnostics. Those are
//     re-derived from the refreshed capture and become the new generation's
//     values.
//   - DURABLE GRAPH IDENTITY is not refreshed. The relationship evidence is
//     copied, and a refreshed capture whose relationships differ from the
//     stored evidence is refused: a metadata-only refresh may not move a
//     session's parent or context graph without a parse.
//   - CONTENT IDENTITY is not refreshed. A changed content hash, harness, or
//     source format is refused for the same reason.
//
// The generation row is immutable. A metadata-only refresh therefore inserts a
// new row and a new set of generation-scoped rows; the old generation stays
// the active read authority until the single commit moves the pointer, so a
// crash or cancellation before the commit leaves the old authority readable.
// The committed transaction flags the session for sweep, and the existing
// reclaim pass removes the superseded rows later. No new table, column, or
// reader is introduced.
//
// A managed generation has no session_projection_generations row: that table is
// the file-backed catalog the migration drains, and its metadata_json is dead
// state for a format-2 generation. The durable metadata document here is the
// flattened session_generations row plus its ordered children plus the
// session_captured_stats row; the refresh rewrites those and never writes the
// file-backed catalog. The operation also deliberately does not enter
// IndexSessionEntryBatch: that path re-encodes and compares every body, which is
// the parse-sized cost this operation exists to avoid. It uses the same
// exclusive session lock, the same one-transaction activation commit, and the
// same generation insert, pointer, stats and binding helpers.

// ManagedGenerationRefreshDisposition is the closed set of outcomes a
// metadata-only managed-generation refresh reports.
type ManagedGenerationRefreshDisposition string

const (
	// ManagedGenerationRefreshInstalled reports that a new generation is now
	// the active read authority and the source generation is superseded.
	ManagedGenerationRefreshInstalled ManagedGenerationRefreshDisposition = "installed"
	// ManagedGenerationRefreshUnchanged reports that the refreshed capture
	// reproduces the active generation's catalog values and children. No new
	// generation is installed; only the mutable stats and the index binding
	// are settled.
	ManagedGenerationRefreshUnchanged ManagedGenerationRefreshDisposition = "unchanged"
)

// ManagedGenerationRefresh is one metadata-only refresh of the active managed
// generation of SessionID. Expected is the stored capture/index proof read
// before the refresh; it proves the entries the source generation holds are
// still the current index and names the publication capture the refreshed
// metadata recorded. SourceGenerationID must be the session's active
// generation at commit time.
type ManagedGenerationRefresh struct {
	SessionID          schema.SessionID
	SourceGenerationID string
	Metadata           schema.UnifiedMetadata
	Expected           *ingest.SessionIndexState
}

// ManagedGenerationRefreshResult reports the committed disposition and the
// generation that is the active read authority afterwards. For
// ManagedGenerationRefreshUnchanged, GenerationID is the source generation.
type ManagedGenerationRefreshResult struct {
	GenerationID string
	Disposition  ManagedGenerationRefreshDisposition
}

// ManagedGenerationRefreshRefusalReason is the closed set of reasons a
// metadata-only refresh is refused without changing any row. A caller can
// branch on it and fall back to a parse.
type ManagedGenerationRefreshRefusalReason string

const (
	// ManagedGenerationRefreshSourceNotActive: the session has no active
	// generation, or the requested source generation is not it.
	ManagedGenerationRefreshSourceNotActive ManagedGenerationRefreshRefusalReason = "source-not-active"
	// ManagedGenerationRefreshSourceUnsupported: the source generation is not a
	// completed format-2 harmonized generation with a proven index, or it is a
	// file-backed generation only the migration may move.
	ManagedGenerationRefreshSourceUnsupported ManagedGenerationRefreshRefusalReason = "source-unsupported"
	// ManagedGenerationRefreshLatestAttemptHeld: the session holds a latest
	// attempt beside its active authority; its retry is the parse's to make.
	ManagedGenerationRefreshLatestAttemptHeld ManagedGenerationRefreshRefusalReason = "latest-attempt-held"
	// ManagedGenerationRefreshIdentityChanged: the refreshed capture changes
	// content identity or durable graph identity, which a metadata-only refresh
	// may not do.
	ManagedGenerationRefreshIdentityChanged ManagedGenerationRefreshRefusalReason = "identity-changed"
)

// ErrManagedGenerationRefreshRefused is the sentinel every
// ManagedGenerationRefreshError matches, so a caller can classify the refusal
// without matching message text.
var ErrManagedGenerationRefreshRefused = errors.New("managed generation metadata refresh refused")

// ManagedGenerationRefreshError is the actionable refusal of one metadata-only
// refresh. It names what was refused, why, the impact, and the caller's next
// step; nothing changed.
type ManagedGenerationRefreshError struct {
	SessionID          schema.SessionID
	SourceGenerationID string
	Reason             ManagedGenerationRefreshRefusalReason
	Detail             string
}

func (e *ManagedGenerationRefreshError) Error() string {
	return fmt.Sprintf(
		"store: refuse the metadata-only refresh of session %s generation %s: %s; where: store.RefreshManagedGeneration, before any row changed; impact: no generation was installed, the active authority is unchanged, and the session stays refresh work; fix: %s",
		e.SessionID, e.SourceGenerationID, e.Reason, e.Detail)
}

// Is reports the refusal class so a caller can test errors.Is against the
// sentinel.
func (e *ManagedGenerationRefreshError) Is(target error) bool {
	return target == ErrManagedGenerationRefreshRefused
}

// managedGenerationRefreshSeamAfterInsert is the crash-injection seam a test
// sets to fail the refresh after the new generation row exists and before any
// parse-owned row is copied. Production leaves the hook nil.
const managedGenerationRefreshSeamAfterInsert = "after-generation-insert"

// managedGenerationRefreshSeam is the nil production hook the rollback test
// sets. It mirrors the harmonizedWriterSeam and reclaimSeam pattern.
var managedGenerationRefreshSeam func(stage string) error

func reportManagedGenerationRefreshSeam(stage string) error {
	if managedGenerationRefreshSeam == nil {
		return nil
	}
	return managedGenerationRefreshSeam(stage)
}

// managedGenerationRefreshAction is what a metadata-only refresh does with one
// generation-scoped table. Every table in reclaimTableNames must carry one of
// these entries; a table with no entry refuses the refresh before any write, so
// a future generation-scoped table cannot be silently dropped from the clone.
type managedGenerationRefreshAction string

const (
	// managedGenerationRefreshClone copies every row of the source generation
	// under the new generation id.
	managedGenerationRefreshClone managedGenerationRefreshAction = "clone"
	// managedGenerationRefreshRegenerate writes rows from the refreshed
	// metadata capture; insertHarmonizedGenerationOnConn already did so.
	managedGenerationRefreshRegenerate managedGenerationRefreshAction = "regenerate-from-metadata"
	// managedGenerationRefreshCatalogRow is the new session_generations row;
	// insertHarmonizedGenerationOnConn already wrote it.
	managedGenerationRefreshCatalogRow managedGenerationRefreshAction = "catalog-row"
	// managedGenerationRefreshRefuseFileBacked guards the file-backed catalog:
	// a managed source generation owns no row there, and a refresh may not
	// create one.
	managedGenerationRefreshRefuseFileBacked managedGenerationRefreshAction = "refuse-file-backed"
)

// managedGenerationRefreshStep is one table's plan entry.
type managedGenerationRefreshStep struct {
	action    managedGenerationRefreshAction
	statement string
}

// generationCloneStatement builds the INSERT ... SELECT that copies one
// generation-scoped table under a new generation id. columns is every column
// except session_id and generation_id, in schema order; the same list feeds the
// INSERT and the SELECT so the two sides cannot drift.
func generationCloneStatement(table string, columns ...string) string {
	list := strings.Join(columns, ", ")
	return `INSERT INTO ` + table + `(session_id, generation_id, ` + list + `) SELECT session_id, ?, ` + list + ` FROM ` + table + ` WHERE session_id = ? AND generation_id = ?`
}

// managedGenerationRefreshPlan is the single copy plan for a metadata-only
// refresh, keyed by the reclaim inventory. Every table reclaimTableNames may
// delete for a superseded generation is either copied, regenerated from the
// refreshed metadata, written as the new catalog row, or refused. The inverse
// order of reclaimTableNames is the copy order: it puts foreign-key parents
// (the generation row, sections, segments) before their children.
var managedGenerationRefreshPlan = map[string]managedGenerationRefreshStep{
	"session_context_segment_refs": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_context_segment_refs",
			"segment_ordinal", "ordinal", "source_entry_ref"),
	},
	"session_section_native_metadata": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_section_native_metadata",
			"partition_id", "ordinal", "native_id", "kind", "source_entry_ref", "source_type",
			"source_message_role", "attachment_turn_index", "attachment_tool_call_id", "custom_type", "data"),
	},
	"session_generation_entries": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_generation_entries",
			"partition_id", "entry_index", "source_entry_ref", "body_digest"),
	},
	"session_generation_content": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_generation_content",
			"source_entry_ref", "digest"),
	},
	"session_generation_subagents": {
		action: managedGenerationRefreshRegenerate,
	},
	"session_generation_commits": {
		action: managedGenerationRefreshRegenerate,
	},
	"session_generation_associations": {
		action: managedGenerationRefreshRegenerate,
	},
	"session_generation_diagnostics": {
		action: managedGenerationRefreshRegenerate,
	},
	"session_generation_title_refs": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_generation_title_refs",
			"ordinal", "source_entry_ref"),
	},
	"session_projection_entries": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_projection_entries",
			"partition_id", "entry_index", "source_entry_ref", "entry_json"),
	},
	"session_projection_content": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_projection_content",
			"source_entry_ref", "relative_blob", "byte_length", "integrity_digest"),
	},
	"session_projection_aliases": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_projection_aliases",
			"native_key", "source_entry_ref"),
	},
	"session_projection_sections": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_projection_sections",
			"partition_id", "earlier_state"),
	},
	"session_context_segments": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_context_segments",
			"segment_ordinal", "logical_session_id", "physical_source_id", "coordinate_kind",
			"start_coordinate", "end_exclusive", "decoded_byte_start", "decoded_byte_end_exclusive",
			"inclusion"),
	},
	"session_relationship_evidence": {
		action: managedGenerationRefreshClone,
		statement: generationCloneStatement("session_relationship_evidence",
			"ordinal", "kind", "target_state", "target_local_id", "evidence",
			"anchor_kind", "anchor_source_entry_ref", "anchor_source_revision_ref"),
	},
	"session_generations": {
		action: managedGenerationRefreshCatalogRow,
	},
	"session_projection_generations": {
		action: managedGenerationRefreshRefuseFileBacked,
	},
}

// RefreshManagedGeneration installs a metadata-only refresh of the session's
// active managed generation as a new immutable generation. It takes the
// exclusive session lock and writes one transaction: the new catalog row, the
// metadata-derived children from the refreshed capture, every parse-owned row
// copied from the source generation, the session's pointer and metadata
// mirrors, the mutable stats, and the index/publication binding. The source
// generation stays active until that commit. No transcript is parsed and no
// entry body or content byte is written.
//
// The pair install and the managed mirror run BEFORE this operation: the
// capture-owned session columns (schema version, project identity, source path,
// session cwd, artifact hash, publication capture revision) and the
// publication metadata row are theirs. The refresh owns the
// generation-derived pointer mirrors, the mutable stats, and the index binding.
// Expected is captured after that mirror, so the artifact hash it names is the
// refreshed pair and the publication revision it names is the capture this
// generation's index is bound to.
//
// The new generation identity is derived deterministically from the source
// generation and the refreshed state, so an identical retry that still names
// the active source is idempotent, while a changed refresh gets a distinct
// generation. A refresh whose metadata reproduces the active generation installs
// nothing and reports ManagedGenerationRefreshUnchanged after settling the
// mutable stats and the binding.
func (s *Store) RefreshManagedGeneration(ctx context.Context, refresh ManagedGenerationRefresh) (result ManagedGenerationRefreshResult, err error) {
	if err := validateManagedGenerationRefresh(refresh); err != nil {
		return result, err
	}
	if err := s.requireGenerationSupport(); err != nil {
		return result, err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, refresh.SessionID)
	if err != nil {
		return result, fmt.Errorf("store: refresh managed generation %s for session %s: acquire the exclusive session lock: %w; nothing changed and the active generation is unchanged", refresh.SourceGenerationID, refresh.SessionID, err)
	}
	defer func() {
		_ = release()
	}()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, fmt.Errorf("store: refresh managed generation %s for session %s: take a database connection: %w; nothing changed and the active generation is unchanged", refresh.SourceGenerationID, refresh.SessionID, err)
	}
	defer s.pool.Put(conn)
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return result, fmt.Errorf("store: refresh managed generation %s for session %s: begin the activation transaction: %w; nothing changed and the active generation is unchanged", refresh.SourceGenerationID, refresh.SessionID, err)
	}
	defer func() {
		end(&err)
		if err != nil {
			result = ManagedGenerationRefreshResult{}
		}
	}()
	return s.refreshManagedGenerationOnConn(ctx, conn, refresh)
}

// validateManagedGenerationRefresh validates the request shape before any
// lock or transaction. These are caller errors, not refusals: the caller did
// not name the evidence a metadata-only refresh requires.
func validateManagedGenerationRefresh(refresh ManagedGenerationRefresh) error {
	if _, err := schema.NewSessionID(string(refresh.SessionID)); err != nil {
		return fmt.Errorf("store: refresh a managed generation with a malformed session identity: %w; nothing changed; supply a canonical session ID", err)
	}
	if err := validateGenerationID(refresh.SourceGenerationID); err != nil {
		return fmt.Errorf("store: refresh of session %s names an unusable source generation: %w; nothing changed", refresh.SessionID, err)
	}
	if refresh.Expected == nil {
		return fmt.Errorf("store: refresh of session %s names no captured index proof; nothing changed; supply the SessionIndexState the run captured before the refresh, including the indexed input hash, so the store can prove the copied entries are the current index", refresh.SessionID)
	}
	if refresh.Expected.SessionID != refresh.SessionID {
		return fmt.Errorf("store: refresh of session %s carries the index proof of session %s; nothing changed; supply the proof captured for this session", refresh.SessionID, refresh.Expected.SessionID)
	}
	if !refresh.Expected.Completed() {
		return fmt.Errorf("store: refresh of session %s carries an incomplete index proof (producer revision %d, completion time present: %t); nothing changed; a metadata-only refresh requires the completed index the source generation's entries were installed with, so finish the index write or re-parse the source", refresh.SessionID, refresh.Expected.IndexerVersion, refresh.Expected.IndexedAt != nil)
	}
	if refresh.Expected.IndexedInputHash == nil || !validIndexInputHash(*refresh.Expected.IndexedInputHash) {
		return fmt.Errorf("store: refresh of session %s carries no valid indexed input digest; nothing changed; supply the digest of the input the stored index consumed so the refresh cannot overwrite a stale index", refresh.SessionID)
	}
	return nil
}

// refreshManagedGenerationOnConn runs the refresh inside the caller's
// transaction. Every refusal is decided from stored state before the first
// mutation.
func (s *Store) refreshManagedGenerationOnConn(ctx context.Context, conn *sqlite.Conn, refresh ManagedGenerationRefresh) (ManagedGenerationRefreshResult, error) {
	var result ManagedGenerationRefreshResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	active, latestAttempt, found, err := readRefreshKeepSetOnConn(conn, refresh.SessionID)
	if err != nil {
		return result, err
	}
	if !found {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceNotActive,
			Detail: "the session has no metadata row, so it has no active generation; import the session first and retry; nothing changed",
		}
	}
	if active == "" || active != refresh.SourceGenerationID {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceNotActive,
			Detail: fmt.Sprintf("the session's active generation is %q, not the requested source; the requested generation is not the read authority; re-read the active generation and retry against it, or re-parse if it is no longer the representation you captured; nothing changed", active),
		}
	}
	if latestAttempt != "" {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshLatestAttemptHeld,
			Detail: fmt.Sprintf("the session holds latest attempt %q beside its active authority; the attempt's retry is the parse's to make and a metadata-only refresh must not replace the authority it is compared against; finalize or clear the latest attempt first; nothing changed", latestAttempt),
		}
	}
	source, err := readActiveHarmonizedView(conn, refresh.SessionID)
	if err != nil {
		return result, err
	}
	if !source.found {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceUnsupported,
			Detail: "the source generation has no format-2 catalog row; only a harmonized managed generation can be refreshed without a parse; re-parse the source; nothing changed",
		}
	}
	if source.record.IndexFormatVersion != 2 {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceUnsupported,
			Detail: fmt.Sprintf("the source generation records index format %d; only format 2 carries the managed body and content rows a metadata-only refresh reuses; re-parse the source; nothing changed", source.record.IndexFormatVersion),
		}
	}
	fileBacked, err := generationIDIsFileBacked(conn, refresh.SessionID, refresh.SourceGenerationID)
	if err != nil {
		return result, err
	}
	if fileBacked {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceUnsupported,
			Detail: "the source generation is installed in the file-backed catalog, which only the migration moves; convert it through peasant migrate, then retry; nothing changed",
		}
	}
	state, err := readIndexStateOnConn(conn, refresh.SessionID)
	if err != nil {
		return result, err
	}
	if !sameIndexState(refresh.Expected, state) {
		return result, &ingest.StaleIndexWorkError{SessionID: ingest.SessionID(refresh.SessionID)}
	}

	// Rebuild the metadata-derived portion of the new generation and check the
	// refreshed capture's identity before touching any row.
	if refresh.Metadata.SessionID != refresh.SessionID {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshIdentityChanged,
			Detail: fmt.Sprintf("the refreshed capture names session %q but the source generation belongs to session %q; a metadata-only refresh cannot relabel a generation; rebuild the capture for this session or re-parse the source; nothing changed", refresh.Metadata.SessionID, refresh.SessionID),
		}
	}
	// These three counts belong to the stored parse, not the adapter capture.
	stats, statsErr := readCapturedStatsOnConn(conn, refresh.SessionID)
	if statsErr != nil && !errors.Is(statsErr, ErrNoCapturedStats) {
		return result, fmt.Errorf("store: read the stored counts of session %s before the metadata-only refresh: %w; nothing changed; restore database access and retry", refresh.SessionID, statsErr)
	}
	if statsErr != nil || stats.TurnCount == nil || stats.InputSubmissionCount == nil || stats.ConversationTurnCount == nil {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshSourceUnsupported,
			Detail: "the stored parse counts are incomplete; re-parse the source to restore authoritative counts",
		}
	}
	refresh.Metadata.Stats.TurnCount = *stats.TurnCount
	refresh.Metadata.Stats.InputSubmissionCount = stats.InputSubmissionCount
	conversation := int64(*stats.ConversationTurnCount)
	refresh.Metadata.Stats.ConversationTurnCount = &conversation
	probe := indexformat.Generation{
		ID:                   refresh.SourceGenerationID,
		Completeness:         source.record.Completeness,
		Metadata:             refresh.Metadata,
		SourceEvidenceDigest: source.record.SourceEvidenceDigest,
	}
	if err := probe.Validate(); err != nil {
		return result, fmt.Errorf("store: refuse the metadata-only refresh of session %s: the refreshed metadata failed managed validation; nothing changed and the active generation is unchanged; re-extract the metadata or re-parse the source: %w", refresh.SessionID, err)
	}
	candidateRecord, candidateChildren, err := mapGenerationRecord(refresh.SessionID, probe)
	if err != nil {
		return result, fmt.Errorf("store: map the refreshed metadata of session %s: %w; nothing changed and the active generation is unchanged", refresh.SessionID, err)
	}
	if reason := managedRefreshIdentityChange(source.record, candidateRecord, source.children.Relationships, candidateChildren.Relationships); reason != "" {
		return result, &ManagedGenerationRefreshError{
			SessionID: refresh.SessionID, SourceGenerationID: refresh.SourceGenerationID,
			Reason: ManagedGenerationRefreshIdentityChanged,
			Detail: reason + "; a metadata-only refresh reuses the stored parse, so it may not change content or graph identity; re-parse the source so the changed identity is indexed, or capture the unchanged identity; nothing changed",
		}
	}
	metadataChildren := GenerationChildren{
		Subagents:    candidateChildren.Subagents,
		Commits:      candidateChildren.Commits,
		Associations: candidateChildren.Associations,
		Diagnostics:  candidateChildren.Diagnostics,
	}
	if managedRefreshRecordEqual(candidateRecord, source.record) &&
		managedRefreshChildrenEqual(metadataChildren, candidateChildren, source.children) {
		// The refreshed capture reproduces the active generation. A stats-only
		// change is an in-place update in the writer's own rule; settle it and
		// the binding without installing a generation.
		if err := s.settleManagedRefreshOnConn(conn, refresh, refresh.Metadata, source); err != nil {
			return result, err
		}
		return ManagedGenerationRefreshResult{GenerationID: source.generationID, Disposition: ManagedGenerationRefreshUnchanged}, nil
	}

	bodies := make([]BodyDigest, len(source.mapping))
	for i := range source.mapping {
		bodies[i] = BodyDigest(source.mapping[i].digest)
	}
	blobs := make([]BlobDigest, 0, len(source.descriptors))
	refs := make([]string, 0, len(source.descriptors))
	for ref := range source.descriptors {
		refs = append(refs, string(ref))
	}
	sort.Strings(refs)
	for _, ref := range refs {
		blobs = append(blobs, BlobDigest(source.descriptors[schema.SourceEntryRef(ref)]))
	}
	candidateRecord.CandidateDigest = computeActivationBinding(candidateRecord, bodies, blobs)
	generationID, err := derivedManagedRefreshGenerationID(refresh.SourceGenerationID, candidateRecord, metadataChildren, bodies, blobs)
	if err != nil {
		return result, err
	}
	candidateRecord.GenerationID = generationID
	if stored, installed, err := readStoredCandidateDigest(conn, refresh.SessionID, generationID); err != nil {
		return result, err
	} else if installed {
		if stored != candidateRecord.CandidateDigest {
			return result, fmt.Errorf("store: refuse the metadata-only refresh of session %s: derived generation identity %s is already installed with a different candidate binding; immutable identities cannot be reused; nothing changed and the active generation is unchanged; re-read the active generation and retry", refresh.SessionID, generationID)
		}
		if generationID != active {
			return result, fmt.Errorf("store: the metadata-only refresh of session %s derives generation identity %s, which is already installed but is not the active authority; the active pointer moved since the request was captured; re-read the active generation and retry; nothing changed", refresh.SessionID, generationID)
		}
	}
	if conflict, err := generationIDIsFileBacked(conn, refresh.SessionID, generationID); err != nil {
		return result, err
	} else if conflict {
		return result, fmt.Errorf("store: refuse the metadata-only refresh of session %s: derived generation identity %s is installed in the file-backed catalog, which only the migration moves; nothing changed; re-read the active generation and retry with a fresh capture", refresh.SessionID, generationID)
	}

	// Every inventory table must have a plan entry BEFORE the first write, so a
	// future generation-scoped table cannot be forgotten halfway through a clone.
	for _, table := range reclaimTableNames {
		if _, ok := managedGenerationRefreshPlan[table]; !ok {
			return result, fmt.Errorf("store: refuse the metadata-only refresh of session %s: generation-scoped table %s has no refresh copy plan; nothing changed and the active generation is unchanged; add the table to managedGenerationRefreshPlan (clone it, or regenerate it from the refreshed metadata) before retrying", refresh.SessionID, table)
		}
	}

	if err := sqlitex.Execute(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(refresh.SessionID)}}); err != nil {
		return result, fmt.Errorf("store: flag session %s for sweep before installing the refreshed generation: %w; the transaction rolled back and the active generation is unchanged", refresh.SessionID, err)
	}
	prepared := &preparedHarmonized{
		sessionID:  refresh.SessionID,
		generation: indexformat.Generation{ID: generationID},
		record:     candidateRecord,
		children:   metadataChildren,
		binding:    candidateRecord.CandidateDigest,
	}
	now := time.Now().UnixMilli()
	if err := insertHarmonizedGenerationOnConn(conn, prepared, source.record.PriorEvidence, now, &now); err != nil {
		return result, err
	}
	if err := reportManagedGenerationRefreshSeam(managedGenerationRefreshSeamAfterInsert); err != nil {
		return result, err
	}
	if err := cloneManagedGenerationRowsOnConn(conn, refresh.SessionID, refresh.SourceGenerationID, generationID); err != nil {
		return result, err
	}
	titleEntries, err := readRefreshTitleEntriesOnConn(conn, refresh.SessionID, refresh.SourceGenerationID)
	if err != nil {
		return result, err
	}
	pointed := indexformat.Generation{
		ID:        generationID,
		Metadata:  refresh.Metadata,
		TitleRefs: source.children.TitleRefs,
		Main:      indexformat.Partition{Entries: titleEntries},
	}
	if err := pointSessionAtGenerationOnConn(conn, refresh.SessionID, pointed); err != nil {
		return result, err
	}
	if err := s.settleManagedRefreshStatsOnConn(conn, refresh, refresh.Metadata, now); err != nil {
		return result, err
	}
	return ManagedGenerationRefreshResult{GenerationID: generationID, Disposition: ManagedGenerationRefreshInstalled}, nil
}

// settleManagedRefreshOnConn applies the bookkeeping half of an unchanged
// refresh: the refreshed stats, the session metadata mirrors, and the
// publication index binding. No generation row and no sweep flag are written.
func (s *Store) settleManagedRefreshOnConn(conn *sqlite.Conn, refresh ManagedGenerationRefresh, metadata schema.UnifiedMetadata, source activeHarmonizedView) error {
	titleEntries, err := readRefreshTitleEntriesOnConn(conn, refresh.SessionID, source.generationID)
	if err != nil {
		return err
	}
	pointed := indexformat.Generation{
		ID:        source.generationID,
		Metadata:  metadata,
		TitleRefs: source.children.TitleRefs,
		Main:      indexformat.Partition{Entries: titleEntries},
	}
	if err := pointSessionAtGenerationOnConn(conn, refresh.SessionID, pointed); err != nil {
		return err
	}
	return s.settleManagedRefreshStatsOnConn(conn, refresh, metadata, time.Now().UnixMilli())
}

// settleManagedRefreshStatsOnConn writes the mutable stats and the publication
// index binding in the caller's transaction. The index producer revision, the
// input proof, and the entries hash are deliberately untouched: no parse ran, so
// the stored index still describes the copied entries.
func (s *Store) settleManagedRefreshStatsOnConn(conn *sqlite.Conn, refresh ManagedGenerationRefresh, metadata schema.UnifiedMetadata, now int64) error {
	if _, err := upsertCapturedStatsOnConn(conn, capturedStatsForHarnessWrite(refresh.SessionID, metadata.Stats, seedJSONForStats(metadata.Stats), now)); err != nil {
		return err
	}
	revision := refresh.Expected.PublicationCaptureRevision
	if revision <= 0 {
		return nil
	}
	// The metadata capture the refreshed pair recorded already moved the session
	// counter in the mirror. Move the content capture with it, and stamp the
	// index binding, exactly as the bind-only index path does. The unchanged
	// capture branch of the mirror re-states the same revision.
	if err := sqlitex.Execute(conn, `UPDATE session_content_captures SET publication_capture_revision = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{revision, string(refresh.SessionID)}}); err != nil {
		return fmt.Errorf("store: move the content capture of session %s to metadata capture revision %d: %w; the transaction rolled back and the active generation is unchanged; restore database access and retry", refresh.SessionID, revision, err)
	}
	if err := stampPublicationIndex(conn, refresh.SessionID, revision); err != nil {
		return fmt.Errorf("store: bind the index of session %s to metadata capture revision %d: %w; the transaction rolled back and the active generation is unchanged; restore database access and retry", refresh.SessionID, revision, err)
	}
	return nil
}

// readRefreshKeepSetOnConn reads the active generation and the latest-attempt
// pointer in one snapshot, distinguishing an absent session row from a session
// with no active generation.
func readRefreshKeepSetOnConn(conn *sqlite.Conn, sessionID schema.SessionID) (active, latestAttempt string, found bool, err error) {
	if err := sqlitex.Execute(conn, `SELECT active_generation_id, latest_attempt_generation_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if stmt.ColumnType(0) != sqlite.TypeNull {
				active = stmt.ColumnText(0)
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				latestAttempt = stmt.ColumnText(1)
			}
			return nil
		},
	}); err != nil {
		return "", "", false, fmt.Errorf("store: read the active generation and latest attempt for session %s: %w; nothing changed", sessionID, err)
	}
	return active, latestAttempt, found, nil
}

// cloneManagedGenerationRowsOnConn copies every parse-owned generation-scoped
// row under the new generation id. The plan is keyed by reclaimTableNames, so a
// future table either has an explicit plan or the refresh refused before any
// write. Reverse reclaim order puts foreign-key parents before children.
func cloneManagedGenerationRowsOnConn(conn *sqlite.Conn, sessionID schema.SessionID, sourceGenerationID, generationID string) error {
	for i := len(reclaimTableNames) - 1; i >= 0; i-- {
		table := reclaimTableNames[i]
		step, ok := managedGenerationRefreshPlan[table]
		if !ok {
			return fmt.Errorf("store: metadata-only refresh of session %s found no plan for generation-scoped table %s; the transaction rolled back and the active generation is unchanged", sessionID, table)
		}
		switch step.action {
		case managedGenerationRefreshClone:
			if err := sqlitex.Execute(conn, step.statement, &sqlitex.ExecOptions{Args: []any{generationID, string(sessionID), sourceGenerationID}}); err != nil {
				return fmt.Errorf("store: copy generation-scoped rows of session %s in %s to generation %s: %w; the transaction rolled back and the active generation is unchanged", sessionID, table, generationID, err)
			}
		case managedGenerationRefreshRegenerate, managedGenerationRefreshCatalogRow:
			// insertHarmonizedGenerationOnConn wrote the catalog row and the
			// metadata-derived children from the refreshed capture.
		case managedGenerationRefreshRefuseFileBacked:
			// The source check already refused a file-backed generation. Guard
			// the invariant again so a managed refresh can never create a
			// file-backed catalog row.
			backed, err := generationIDIsFileBacked(conn, sessionID, sourceGenerationID)
			if err != nil {
				return err
			}
			if backed {
				return fmt.Errorf("store: metadata-only refresh of session %s found file-backed generation %s during the copy; the transaction rolled back and the active generation is unchanged; convert the generation through peasant migrate and retry", sessionID, sourceGenerationID)
			}
		default:
			return fmt.Errorf("store: metadata-only refresh of session %s has no handling for table %s action %q; the transaction rolled back and the active generation is unchanged", sessionID, table, step.action)
		}
	}
	return nil
}

// readRefreshTitleEntriesOnConn reads the source generation's main entries that
// its ordered title refs name. Only those bodies are decoded, because the title
// derivation reads no other entry; the rest of the mapping is copied in SQL.
func readRefreshTitleEntriesOnConn(conn *sqlite.Conn, sessionID schema.SessionID, generationID string) ([]schema.SessionEntry, error) {
	var entries []schema.SessionEntry
	err := sqlitex.Execute(conn, `SELECT `+sqlSelectBodyColumnsJoined+`
FROM session_generation_title_refs t
JOIN session_generation_entries m
  ON m.session_id = t.session_id AND m.generation_id = t.generation_id
 AND m.partition_id = 0 AND m.source_entry_ref = t.source_entry_ref
JOIN session_entry_bodies b
  ON b.session_id = m.session_id AND b.body_digest = m.body_digest
WHERE t.session_id = ? AND t.generation_id = ?
ORDER BY t.ordinal`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, entryFromRow(scanEntryRecord(stmt)))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read the title entries of generation %s for session %s: %w; the transaction rolled back and the active generation is unchanged", generationID, sessionID, err)
	}
	return entries, nil
}

// derivedManagedRefreshGenerationID derives the new immutable generation
// identity from the source generation and the refreshed state. The derivation
// excludes only the staging-derived stamps, so an identical refresh from the
// same source derives the same identity and a retry cannot install a second
// copy; a changed capture derives a different identity.
func derivedManagedRefreshGenerationID(sourceGenerationID string, record GenerationRecord, children GenerationChildren, bodies []BodyDigest, blobs []BlobDigest) (string, error) {
	normalized := record
	normalized.GenerationID = ""
	normalized.CandidateDigest = ""
	normalized.InstalledAtMs = 0
	normalized.ActivatedAtMs = nil
	payload, err := json.Marshal(struct {
		Source       string
		Generation   GenerationRecord
		Subagents    []GenerationSubagent
		Commits      []GenerationCommit
		Associations []GenerationAssociation
		Diagnostics  []GenerationDiagnostic
		Bodies       []BodyDigest
		Blobs        []BlobDigest
	}{
		Source:       sourceGenerationID,
		Generation:   normalized,
		Subagents:    children.Subagents,
		Commits:      children.Commits,
		Associations: children.Associations,
		Diagnostics:  children.Diagnostics,
		Bodies:       bodies,
		Blobs:        blobs,
	})
	if err != nil {
		return "", fmt.Errorf("store: derive the refreshed generation identity for session %s: %w; nothing changed and the active generation is unchanged", record.SessionID, err)
	}
	sum := sha256.Sum256(payload)
	return "mgr-" + hex.EncodeToString(sum[:])[:32], nil
}

// managedRefreshIdentityChange reports why the refreshed capture may not replace
// the source generation's metadata, or the empty string when the identity is
// unchanged. Content identity and durable graph identity are the two sets a
// metadata-only refresh may not move; everything else in the capture is
// context the refresh exists to update.
func managedRefreshIdentityChange(source, candidate GenerationRecord, sourceRelationships, candidateRelationships []GenerationRelationship) string {
	switch {
	case candidate.Harness != source.Harness:
		return fmt.Sprintf("the refreshed capture names harness %q but the stored generation is %q", candidate.Harness, source.Harness)
	case candidate.ContentHash != source.ContentHash:
		return "the refreshed capture names a different content hash, so the stored parse no longer describes the captured bytes"
	case candidate.SourceFormat != source.SourceFormat:
		return fmt.Sprintf("the refreshed capture names source format %q but the stored generation is %q, so the stored parse may not describe the same input shape", candidate.SourceFormat, source.SourceFormat)
	case !managedRefreshRelationshipsEqual(sourceRelationships, candidateRelationships):
		return "the refreshed capture carries different relationship evidence, so the durable parent or context graph would move"
	}
	return ""
}

// managedRefreshRelationshipsEqual compares two relationship collections by
// their stored fields. The read path orders rows by ordinal but does not set the
// struct ordinal; the mapped collection does. Ordinal is therefore excluded.
func managedRefreshRelationshipsEqual(stored, refreshed []GenerationRelationship) bool {
	if len(stored) != len(refreshed) {
		return false
	}
	for i := range stored {
		a, b := stored[i], refreshed[i]
		a.Ordinal = 0
		b.Ordinal = 0
		if !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

// managedRefreshRecordEqual compares the refreshed catalog values with the
// source row, excluding the generation identity and the staging-derived fields.
// Unlike the skip comparison it keeps the adapter revision, because an adapter
// bump is the primary reason a metadata-only refresh runs.
func managedRefreshRecordEqual(candidate, source GenerationRecord) bool {
	normalize := func(r GenerationRecord) GenerationRecord {
		r.GenerationID = ""
		r.MetadataHash = ""
		r.CandidateDigest = ""
		r.PriorEvidence = nil
		r.InstalledAtMs = 0
		r.ActivatedAtMs = nil
		return r
	}
	return reflect.DeepEqual(normalize(candidate), normalize(source))
}

// managedRefreshChildrenEqual compares the metadata-derived children the
// refreshed capture would install with the source generation's rows.
// metadataChildren is the set written by the generation insert (relationships
// are cloned separately), while candidateChildren carries the mapped
// relationships for the identity comparison. The read path leaves relationship
// ordinals unset, so relationships are compared by field only.
func managedRefreshChildrenEqual(metadataChildren, candidateChildren, source GenerationChildren) bool {
	// The mutable stats upsert adds its own stats-overflow diagnostic to the
	// active generation; it is compatibility evidence, not a captured change, so
	// it cannot force a new generation. Producer diagnostics still participate.
	sourceDiagnostics := capturedGenerationChildren(source).Diagnostics
	candidateDiagnostics := capturedGenerationChildren(metadataChildren).Diagnostics
	return reflect.DeepEqual(metadataChildren.Subagents, source.Subagents) &&
		reflect.DeepEqual(metadataChildren.Commits, source.Commits) &&
		reflect.DeepEqual(metadataChildren.Associations, source.Associations) &&
		reflect.DeepEqual(candidateDiagnostics, sourceDiagnostics) &&
		managedRefreshRelationshipsEqual(source.Relationships, candidateChildren.Relationships)
}
