package ingest

import "context"

// ManagedGenerationRefreshDisposition is the closed set of outcomes a
// metadata-only managed-generation refresh reports.
type ManagedGenerationRefreshDisposition string

const (
	// ManagedGenerationRefreshInstalled reports that a new immutable generation
	// is now the active read authority and the refreshed metadata capture is
	// its context.
	ManagedGenerationRefreshInstalled ManagedGenerationRefreshDisposition = "installed"
	// ManagedGenerationRefreshUnchanged reports that the refreshed capture
	// reproduces the active generation: no new generation was installed and the
	// mutable bookkeeping was settled against the refreshed capture.
	ManagedGenerationRefreshUnchanged ManagedGenerationRefreshDisposition = "unchanged"
)

// AllManagedGenerationRefreshDispositions lists every outcome the pipeline
// understands. A store that reports anything else is refused at the adapter
// boundary rather than read as a successful settle.
var AllManagedGenerationRefreshDispositions = []ManagedGenerationRefreshDisposition{
	ManagedGenerationRefreshInstalled,
	ManagedGenerationRefreshUnchanged,
}

// ManagedGenerationRefreshRequest is the pipeline-owned request for a
// metadata-only refresh of a managed generation. The pipeline cannot name the
// store's request type directly: ingest must stay free of a store import. The
// store adapter translates this request to its own immutable request.
//
// The source generation is deliberately absent. The store resolves the
// session's active generation inside the refresh and refuses when it is no
// longer the active authority, so a caller never captures a generation
// identity that could silently become stale before the commit.
type ManagedGenerationRefreshRequest struct {
	SessionID SessionID
	// Metadata is the refreshed capture installed by the pair write and
	// recorded by the managed mirror. It owns the generation's flattened
	// context, stats and metadata-derived children; the parse still owns the
	// entries, content and structural rows.
	Metadata UnifiedMetadata
	// ExpectedState is the stored index proof captured AFTER the pair install
	// and the managed mirror committed. It proves the entries the active
	// generation holds are still the current index and names the publication
	// capture the refreshed metadata recorded. A pre-mirror proof is not a
	// substitute: the mirror moves the artifact identity and the capture
	// revision the refresh must agree with.
	ExpectedState *SessionIndexState
}

// ManagedGenerationRefreshOutcome is the pipeline-owned result of a
// metadata-only managed-generation refresh.
type ManagedGenerationRefreshOutcome struct {
	// GenerationID is the generation that is the active read authority
	// afterwards. For an unchanged outcome it is the source generation.
	GenerationID string
	// Disposition says whether a new generation was installed or the refreshed
	// capture reproduced the active one.
	Disposition ManagedGenerationRefreshDisposition
}

// ManagedGenerationRefresher is the store capability that installs a
// metadata-only refresh of a managed generation without parsing the
// transcript. The store reuses body and content bytes, but loads the active
// view and clones generation-scoped entry references and structural rows as
// well as rewriting refreshed context. Cost grows with those rows and context,
// rather than with the size of the reused bodies.
//
// Refusals are not failures of the session. The implementation returns the
// store's actionable refusal (a *store.ManagedGenerationRefreshError matching
// store.ErrManagedGenerationRefreshRefused) when a metadata-only refresh must
// not run, and a *StaleIndexWorkError when the captured proof no longer names
// the stored index. The caller falls back to the ordinary parse path for both,
// and the active generation stays the read authority until the parse commits.
type ManagedGenerationRefresher interface {
	RefreshManagedGenerationWithoutParse(context.Context, ManagedGenerationRefreshRequest) (ManagedGenerationRefreshOutcome, error)
}

// managedGenerationRefreshLane reports whether a stored-metadata result is a
// candidate for the metadata-only managed-generation refresh. It is the
// adapter-behind / adapter-stamp-absent lane ONLY: an indexer bump reads the
// stored transcript and belongs to the stored-index lane, and --force or
// --reindex are deliberate full rebuilds. A native-only adapter never reaches
// the stored-metadata lane because decideRefresh routes it native, and a
// strict format-1 target has no managed generation to refresh.
//
// The marker only says the refresh is worth attempting. The index stage checks
// the captured proof and the store is the final authority; either can send the
// session back to the ordinary parse path.
func (p *Pipeline) managedGenerationRefreshLane(session DiscoveredSession, decision RefreshDecision) bool {
	if decision.Reason != RefreshReasonAdapterBehind && decision.Reason != RefreshReasonAdapterStampAbsent {
		return false
	}
	if p.config.Force || p.config.Reindex {
		return false
	}
	if _, ok := p.metricsStore.(ManagedGenerationRefresher); !ok {
		return false
	}
	return p.sessionVersionTarget(session).IndexVersion != strictIndexFormat
}

// managedGenerationRefreshRequest builds the store request from the captured
// index input. The input's expected state was read after the pair install and
// mirror, so it carries the refreshed artifact identity and publication
// capture revision.
func managedGenerationRefreshRequest(input *CapturedIndexInput) ManagedGenerationRefreshRequest {
	return ManagedGenerationRefreshRequest{
		SessionID:     input.session.SessionID,
		Metadata:      *input.metadata,
		ExpectedState: input.expected,
	}
}
