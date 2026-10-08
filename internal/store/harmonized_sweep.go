package store

import (
	"context"

	"github.com/peasant-labs/schema"
)

// SweepResult is the per-session sweep report (design §4.5): the bounded,
// idempotent pass that deletes unreferenced entry rows and blobs, removes
// orphan directories, and rebuilds the search index when a delete could not
// trust its values.
type SweepResult struct {
	RowsDeleted        int64
	DirectoriesRemoved int64
	BodiesDeleted      int64
	BlobsDeleted       int64
	Rebuilt            bool
}

// SweepSession runs the per-session sweep (design §4.5): bounded, idempotent,
// selected by the content_sweep_pending flag. Stub: returns
// ErrHarmonizedNotImplemented until the sweep lands it.
func SweepSession(ctx context.Context, sessionID schema.SessionID) (SweepResult, error) {
	return SweepResult{}, ErrHarmonizedNotImplemented
}
