package ingest

import (
	"context"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// GenerationCandidate is one validated managed generation plus the full
// captured bytes its content records address and the opaque prior evidence a
// reopen needs. A harness indexer produces it; only the maintenance activation
// stages, persists and activates it, so no format target is enabled here.
type GenerationCandidate struct {
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

// GenerationBuilder is implemented by a harness indexer that can produce a
// managed generation with its captured content. The maintenance composition
// asserts it at dispatch instead of switching on concrete indexer types, and
// calls it instead of the retained entry exit when the harness's declared
// output format declares a managed generation.
type GenerationBuilder interface {
	BuildGeneration(ctx context.Context, in GenerationBuildInput) (GenerationCandidate, error)
}

// GenerationBuildInput carries the per-attempt evidence, identity and access
// seams one BuildGeneration call needs. It is built per attempt by the
// composition, so a builder never reaches back into pipeline state and no
// dependency bag is shared between attempts.
type GenerationBuildInput struct {
	// Session is the attempt's session. Codex and OpenCode acquire their own
	// source from it; the file-sourced builders consume CapturedTranscript.
	Session DiscoveredSession
	// GenerationID is the installed identity activation owns. It is empty when
	// no identity source is wired, and a builder refuses rather than inventing
	// one.
	GenerationID string
	// RecordedMetadata is the committed, identity-validated metadata of the
	// attempt. It is the publication-capture snapshot and the agreement the
	// generation indexes; nil only for an attempt built without a committed
	// artifact.
	RecordedMetadata *schema.UnifiedMetadata
	// CapturedTranscript holds the attempt's exact verified bytes. It is
	// normalized non-nil for file-sourced attempts, so a captured empty
	// transcript is distinguishable from one that was never captured; it stays
	// nil when the attempt has no captured bytes.
	CapturedTranscript []byte
	// Prior is the per-attempt evidence-access façade. It is never nil when
	// built by the composition; a builder tolerates nil for a direct caller.
	Prior GenerationPriorSource
	// Allocator mints or replays the opaque refs the projection allocates. A
	// fixture injects a deterministic one; production uses RandomRefAllocator.
	Allocator RefAllocator
	// OpenCode is the per-attempt recorded-source access for the OpenCode
	// builder. It performs no I/O until Snapshot is called and stays nil for a
	// builder that does not consume it.
	OpenCode OpenCodeSourceAccess
}

// GenerationPriorSource is the evidence-access façade for one attempt. It
// exposes the active generation's reusable evidence and the stored
// content-capture certificate that stands in for it when there is none, with
// the mode and capability gates applied by the implementation, so a builder
// consumes evidence without knowing the store or the run's mode.
type GenerationPriorSource interface {
	// ReadGenerationPrior returns the active generation's reusable evidence. It
	// returns nil only when the session has no active generation AND no latest
	// attempt. The certificate-only state (no active generation, a kept latest
	// attempt) returns the attempt's aliases with no metadata, no evidence
	// document and no complete-generation claim: the attempt contributes
	// identity, never proof.
	ReadGenerationPrior(ctx context.Context) (*GenerationPrior, error)
	// ReadStoredCaptureAuthority returns the stored capture certificate that
	// holds last-good full authority, or nil. It is consulted for a nil prior
	// AND for the certificate-only prior (the attempt's aliases with no
	// evidence document), so a candidate is still judged against the stored
	// certificate. The implementation returns (nil, nil) under an
	// operator-initiated rebuild and when no authority reader is configured.
	ReadStoredCaptureAuthority(ctx context.Context) (*StoredCaptureAuthority, error)
}

// OpenCodeSourceAccess snapshots one attempt's recorded OpenCode source. It is
// bound to the attempt's session; a non-empty locator is required, and the
// implementation issues SELECTs only.
type OpenCodeSourceAccess interface {
	Snapshot(ctx context.Context, locator string) (OpenCodeHistorySnapshot, error)
}

// generationPriorState normalizes a loaded prior's reusable projection state,
// so an absent alias table rebuilds an empty state instead of reusing another
// generation's identities.
func generationPriorState(prior *GenerationPrior) ProjectionPriorState {
	if prior == nil || prior.Aliases.Entries == nil {
		return NewProjectionPriorState()
	}
	return prior.Aliases
}

// GenerationActivation is the ingest-owned activation envelope the store
// turns into one immutable managed generation. It mirrors the store's own
// preconditions so the composition can reuse the expected-state compare and
// the success-stamp rules without importing the store package.
type GenerationActivation struct {
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

// GenerationActivator stages and activates one managed generation in ONE
// transaction after staging its files (content blobs written, the manifest and
// the directory fsynced), repairs the exported metadata, and
// reconciles any interrupted activation. The production store implements it.
// The outcome distinguishes newly-committed, already-committed, and
// not-committed invocations so per-invocation counts stay truthful; a
// post-commit repair failure returns CommittedNow or AlreadyCommitted with a
// GenerationRepairPendingError, never a rollback.
type GenerationActivator interface {
	ActivateGenerationCandidate(context.Context, GenerationActivation) (ActivationOutcome, error)
}

// GenerationStager prepares one managed generation's files ahead of the
// activation, so independent sessions can write their content in
// parallel while only the manifest fsync, the install and the database commit
// stay on the serialized
// writer. Preparation records no activation intent and installs nothing: the
// returned handle is inert until passed to a GenerationPreparedActivator,
// so a candidate the pipeline later refuses can never be recovered into
// authority. It is optional: without a stager, activation stages inline. A
// preparation error is not reported as the candidate's outcome; the activation
// that follows stages inline and owns the authoritative disposition.
type GenerationStager interface {
	StageGenerationCandidate(context.Context, GenerationActivation) (GenerationStaged, error)
}

// GenerationStaged is the opaque store-owned handle for one prepared
// candidate. It names the candidate it prepared and carries nothing the
// pipeline may inspect or persist.
type GenerationStaged interface {
	GenerationCandidateID() string
}

// GenerationPreparedActivator activates one candidate using the files a
// GenerationStager prepared for it. It applies exactly the guarded
// activation of GenerationActivator; the handle only saves the file
// writes.
type GenerationPreparedActivator interface {
	ActivateStagedGenerationCandidate(context.Context, GenerationActivation, GenerationStaged) (ActivationOutcome, error)
}

// GenerationBatchStager stages independent candidates in bounded shared
// transactions. Results retain input order, including refused candidates.
type GenerationBatchStager interface {
	StageGenerationCandidates(context.Context, []GenerationActivation) ([]GenerationStaged, []error)
}

// NativeActivationResult is one request's guarded activation outcome.
type NativeActivationResult struct {
	Outcome ActivationOutcome
	Err     error
}

// GenerationBatchActivator commits staged independent candidates together
// with per-session savepoints. A nil handle stages that candidate inline.
type GenerationBatchActivator interface {
	ActivateStagedGenerationCandidates(context.Context, []GenerationActivation, []GenerationStaged) []NativeActivationResult
}

// GenerationPrior is the last-good evidence a prior activation left for
// one session. Metadata and Aliases are derived from the committed generation;
// PriorEvidence is the exact persisted harness document, or nil when none was
// written. A nil reader result means the session has no active generation.
type GenerationPrior struct {
	GenerationID          string
	Metadata              *schema.UnifiedMetadata
	Aliases               ProjectionPriorState
	HasCompleteGeneration bool
	PriorEvidence         []byte
}

// GenerationPriorReader loads the active generation's prior evidence for
// one session. A session with no active generation returns (nil, nil): a first
// discovery must build an empty prior state rather than reuse another
// session's aliases.
type GenerationPriorReader interface {
	ReadGenerationPrior(context.Context, SessionID) (*GenerationPrior, error)
}

// StoredCaptureAuthority names the stored content-capture certificate that
// stands in for an active managed generation as last-good full authority. It
// preserves the certificate's own state in full: its status, source authority,
// transcript origin, capture format, and failure code. An accounted
// incomplete/full certificate therefore reads as what it is rather than being
// flattened into a complete one. It carries no alias table and no
// captured-prefix proof.
type StoredCaptureAuthority struct {
	Status           ContentCaptureStatus
	SourceAuthority  ContentSourceAuthority
	TranscriptOrigin TranscriptOrigin
	CaptureFormat    ContentCaptureFormat
	FailureCode      ContentCaptureFailureCode
}

// NativePriorCaptureAuthorityReader reports the stored content-capture
// certificate that still holds last-good full authority for a session with no
// active managed generation. It applies the writer's own publishable-capture
// predicate to the stored certificate; a publication revision is never proof.
// A session with no publishable certificate returns (nil, nil), so a genuine
// first discovery keeps its empty prior.
type NativePriorCaptureAuthorityReader interface {
	ReadStoredCaptureAuthority(context.Context, SessionID) (*StoredCaptureAuthority, error)
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
	// ActivationLatestAttemptPreview reports the requested candidate was an
	// unaccounted source-records omission preview over a publishable full
	// authority: the active generation is unchanged and the candidate is kept
	// as the latest attempt. It counts zero, like AlreadyCommitted.
	ActivationLatestAttemptPreview
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
