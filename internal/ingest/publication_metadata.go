package ingest

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// CWDProvenanceKind records what the source inspection established, not a
// directory guessed from the project, the managed file, or the current process.
type CWDProvenanceKind string

const (
	CWDSourceExact     CWDProvenanceKind = "source_exact"
	CWDSourceWorkspace CWDProvenanceKind = "source_workspace"
	CWDSourceWorktree  CWDProvenanceKind = "source_worktree"
	CWDSourceAbsent    CWDProvenanceKind = "source_absent"
	CWDNotRecovered    CWDProvenanceKind = "not_recovered"
)

func NewCWDProvenanceKind(raw string) (CWDProvenanceKind, error) {
	v := CWDProvenanceKind(raw)
	switch v {
	case CWDSourceExact, CWDSourceWorkspace, CWDSourceWorktree, CWDSourceAbsent, CWDNotRecovered:
		return v, nil
	default:
		return "", fmt.Errorf("ingest: unknown CWD provenance during metadata capture; publication cannot use unverified evidence; inspect the source with peasant ingest and retry")
	}
}

// PublicationReadiness describes capture/index consistency, not wire validation.
// In particular, model validation remains the publication consumer's obligation.
type PublicationReadiness string

const (
	PublicationReady       PublicationReadiness = "ready"
	PublicationNeedsIngest PublicationReadiness = "needs_ingest"
)

// PublicationInputBundle is read from one database snapshot. A committed read
// includes the selected generation and composes its count and graph facts into
// Metadata; capture-only reads leave Generation nil. Stored capture evidence is
// never rewritten. Generation blobs may only be hydrated during the callback.
type PublicationInputBundle struct {
	Generation         *indexformat.ReadSnapshot
	ProjectPath        string
	ContentCapture     SessionContentCapture
	Metadata           schema.UnifiedMetadata
	Entries            []schema.SessionEntry
	Quality            *schema.QualityMetrics
	Associations       []schema.PublishedAssociation
	SessionOrigin      sessionorigin.Origin
	ReceiptProjectHash schema.ProjectHash
	CaptureRevision    int64
	Readiness          PublicationReadiness
}

// PublicationInputReader keeps managed content alive through the
// callback. Consumers hydrate the generation there, then release the snapshot
// before performing network I/O. An error is a refusal, not a legacy fallback.
type PublicationInputReader interface {
	indexformat.ContentResolver
	WithCommittedPublicationInput(context.Context, SessionID, func(PublicationInputBundle) error) error
}

// PublicationCaptureStore is optional so existing SessionStore callers retain
// their API. Only an explicitly source-proven capture receives a revision.
type PublicationCaptureStore interface {
	InsertSessionsWithRevisions(context.Context, []StoreEntry) (map[SessionID]int64, error)
}

// SessionCommitMergeStore retains prior bindings when source recovery observes
// only a subset of historical Git evidence (or Git is no longer available).
type SessionCommitMergeStore interface {
	MergeSessionCommits(context.Context, SessionID, []CommitInfo) error
}
