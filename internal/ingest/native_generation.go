package ingest

import (
	"context"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// NativeGenerationCandidate is one validated managed generation plus the full
// captured bytes its content records address and the opaque prior evidence a
// reopen needs. A native indexer produces it; only the maintenance activation
// stages, persists and activates it, so no format target is enabled here.
type NativeGenerationCandidate struct {
	Result indexformat.V2
	// Blobs holds the full bytes for every content record the generation
	// names, including retained inherited evidence, so activation can stage a
	// self-contained candidate without reopening the mutable native source.
	Blobs map[schema.SourceEntryRef][]byte
	// PriorEvidence is the harness-owned, activation-persisted document a
	// later candidate reloads to reuse identities and retained prefixes. Nil
	// means the generation alone carries the reusable evidence.
	PriorEvidence []byte
	Diagnostics   []DiagnosticEntry
}

// NativeGenerationBuilder is implemented by a native harness indexer that can
// produce a managed generation with its captured content. The maintenance
// composition calls it instead of the retained entry exit when the harness's
// declared output format is 2.
type NativeGenerationBuilder interface {
	BuildNativeGeneration(context.Context, DiscoveredSession) (NativeGenerationCandidate, error)
}

// NativeGenerationActivation is the ingest-owned activation envelope the store
// turns into one immutable managed generation. It mirrors the store's own
// preconditions so the composition can reuse the expected-state compare and
// the success-stamp rules without importing the store package.
type NativeGenerationActivation struct {
	Generation       indexformat.V2
	Blobs            map[schema.SourceEntryRef][]byte
	PriorEvidence    []byte
	IndexerVersion   int
	IndexedAtMs      int64
	ExpectedState    *SessionIndexState
	ContentCapture   SessionContentCaptureWrite
	CaptureRevision  int64
	IndexedInputHash *string
	ArtifactIdentity *string
	// ExplicitRebuild marks an operator-initiated rebuild (harvest index
	// --force, Reindex) that deliberately replaces full read authority with a
	// preview. It exempts the last-good preview-over-full refusal on the same
	// principle as a format conversion; accidental previews stay refused.
	ExplicitRebuild bool
	// Capture is the publication-capture agreement this activation records in
	// the same transaction as the generation install, from the metadata
	// snapshot and the provenance kind the pipeline certifies. Nil records no
	// capture and leaves the stored provenance exactly as it was.
	Capture *PublicationCaptureWrite
}

// NativeGenerationActivator stages and activates one managed generation in ONE
// transaction after staging its files (content blobs written, the manifest and
// the directory fsynced), repairs the exported metadata, and
// reconciles any interrupted activation. The production store implements it.
// The outcome distinguishes newly-committed, already-committed, and
// not-committed invocations so per-invocation counts stay truthful; a
// post-commit repair failure returns CommittedNow or AlreadyCommitted with a
// GenerationRepairPendingError, never a rollback.
type NativeGenerationActivator interface {
	ActivateNativeGeneration(context.Context, NativeGenerationActivation) (ActivationOutcome, error)
}

// NativeGenerationStager prepares one managed generation's files ahead of the
// activation, so independent sessions can write their content in
// parallel while only the manifest fsync, the install and the database commit
// stay on the serialized
// writer. Preparation records no activation intent and installs nothing: the
// returned handle is inert until passed to a NativeGenerationPreparedActivator,
// so a candidate the pipeline later refuses can never be recovered into
// authority. It is optional: without a stager, activation stages inline. A
// preparation error is not reported as the candidate's outcome; the activation
// that follows stages inline and owns the authoritative disposition.
type NativeGenerationStager interface {
	StageNativeGeneration(context.Context, NativeGenerationActivation) (NativeGenerationStaged, error)
}

// NativeGenerationStaged is the opaque store-owned handle for one prepared
// candidate. It names the candidate it prepared and carries nothing the
// pipeline may inspect or persist.
type NativeGenerationStaged interface {
	NativeGenerationCandidateID() string
}

// NativeGenerationPreparedActivator activates one candidate using the files a
// NativeGenerationStager prepared for it. It applies exactly the guarded
// activation of NativeGenerationActivator; the handle only saves the file
// writes.
type NativeGenerationPreparedActivator interface {
	ActivateStagedNativeGeneration(context.Context, NativeGenerationActivation, NativeGenerationStaged) (ActivationOutcome, error)
}

// NativeGenerationPrior is the last-good evidence a prior activation left for
// one session. Metadata and Aliases are derived from the committed generation;
// PriorEvidence is the exact persisted harness document, or nil when none was
// written. A nil reader result means the session has no active generation.
type NativeGenerationPrior struct {
	GenerationID          string
	Metadata              *schema.UnifiedMetadata
	Aliases               ProjectionPriorState
	HasCompleteGeneration bool
	PriorEvidence         []byte
}

// NativeGenerationPriorReader loads the active generation's prior evidence for
// one session. A session with no active generation returns (nil, nil): a first
// discovery must build an empty prior state rather than reuse another
// session's aliases.
type NativeGenerationPriorReader interface {
	ReadNativeGenerationPrior(context.Context, SessionID) (*NativeGenerationPrior, error)
}

// ActivationDisposition is the lock-derived disposition for the REQUESTED
// candidate, independent of repair state. It is never inferred from an
// unlocked read or error text. Counting is per-invocation, never
// crash-global exactly-once: only CommittedNow counts once, AlreadyCommitted
// and NotCommitted count zero.
type ActivationDisposition uint8

const (
	// ActivationNotCommitted reports the requested candidate was not committed
	// by this invocation, including pre-commit failures and unrelated
	// prior-intent repair failures in the preamble.
	ActivationNotCommitted ActivationDisposition = iota
	// ActivationCommittedNow reports the requested candidate was newly
	// committed during this invocation, including preamble-recovery commits.
	ActivationCommittedNow
	// ActivationAlreadyCommitted reports the requested candidate was already
	// committed before this invocation. The call re-repaired from the
	// committed row and changed no authority.
	ActivationAlreadyCommitted
	// ActivationSkipped reports the requested candidate reproduces the
	// active harmonized generation exactly: no new generation row was
	// written and no object was staged, and the call advanced only the
	// bookkeeping stamps. It counts zero, like AlreadyCommitted.
	ActivationSkipped
)

// ActivationOutcome is the request-scoped activation result. RepairPending
// reports a post-commit metadata-repair failure: authority already committed,
// never rollback. Repair carries the fixed repair category via
// GenerationRepairPendingError, never raw I/O text.
type ActivationOutcome struct {
	Disposition   ActivationDisposition
	CandidateID   string
	RepairPending bool
}

// GenerationRepairKind names the fixed post-commit repair categories a
// GenerationRepairPendingError may carry. The set is closed: only the store
// assigns these values after commit is known to have succeeded, and callers
// classify with errors.As, never by comparing category text.
type GenerationRepairKind string

const (
	// GenerationRepairMetadata reports the exported metadata repair failed
	// after the generation committed.
	GenerationRepairMetadata GenerationRepairKind = "metadata repair"
	// GenerationRepairIntentClear reports the pending-intent clear failed
	// after the generation committed.
	GenerationRepairIntentClear GenerationRepairKind = "intent clear"
	// GenerationRepairPriorPersist reports the prior-evidence persist failed
	// after the generation committed. The generation install commits before
	// its prior document is persisted, so a persist failure is post-commit
	// repair, never a pre-commit refusal.
	GenerationRepairPriorPersist GenerationRepairKind = "prior persist"
)

// GenerationRepairPendingError reports committed authority with pending repair.
// Only the store returns it, after commit is known to have succeeded. The
// wrapper exposes a fixed repair category and validated identity only, never
// raw I/O text or payload bytes. Callers classify with errors.As.
type GenerationRepairPendingError struct {
	SessionID   string
	CandidateID string
	Repair      GenerationRepairKind
}

func (e *GenerationRepairPendingError) Error() string {
	return "store: generation " + e.CandidateID + " for session " + e.SessionID + " is active but " + string(e.Repair) + " failed; the active generation is valid and the repair is retried on the next open or activation; no rollback was performed"
}
