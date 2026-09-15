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
	// Capture is the publication-capture agreement this activation records in
	// the same transaction as the generation install, from the metadata
	// snapshot and the provenance kind the pipeline certifies. Nil records no
	// capture and leaves the stored provenance exactly as it was.
	Capture *PublicationCaptureWrite
}

// NativeGenerationActivator stages and activates one managed generation in ONE
// transaction after fsyncing its files, repairs the exported metadata, and
// reconciles any interrupted activation. The production store implements it.
type NativeGenerationActivator interface {
	ActivateNativeGeneration(context.Context, NativeGenerationActivation) error
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
