package ingest

import (
	"context"
	"fmt"

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

// PublicationInputBundle is read from one database snapshot, without file I/O.
type PublicationInputBundle struct {
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

type PublicationInputReader interface {
	LoadPublicationInput(context.Context, SessionID) (PublicationInputBundle, error)
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
