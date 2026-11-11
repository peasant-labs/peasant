package store

import (
	"context"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// AutomaticSessionClaimer returns the ingest claim seam for a signed-in run.
// It appends at most one ClaimSourceAutomatic entry per batch and points only
// the named sessions, plus their local annotations, at it.
//
// The caller captures the credential snapshot and attaches this only when the
// run is signed in. A signed-out, dry, or file-only run attaches nothing.
func (s *Store) AutomaticSessionClaimer(snapshot auth.CredentialSnapshot) ingest.SessionClaimer {
	return ingest.SessionClaimerFunc(func(ctx context.Context, sessionIDs []string) error {
		_, err := s.ClaimSessionIDs(ctx, snapshot, sessionIDs, ClaimSourceAutomatic, time.Now().UnixMilli())
		return err
	})
}

// Compile-time guard: the automatic claimer satisfies the ingest seam.
var _ ingest.SessionClaimer = (ingest.SessionClaimerFunc)(nil)
