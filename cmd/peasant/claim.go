package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// automaticSessionClaimer builds the optional claim seam for a writing run.
// It captures the saved credential snapshot locally: a signed-out run attaches
// no claimer and is not an error, because local ingest must keep working
// without a Village account.
func automaticSessionClaimer(ctx context.Context, configHome string, db *store.Store) (ingest.SessionClaimer, error) {
	snapshot, err := auth.CaptureCredentials(ctx, configHome)
	if errors.Is(err, auth.ErrSignedOut) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.AutomaticSessionClaimer(snapshot), nil
}

// claimNewAnnotations points just-created human annotations at the signed-in
// account's latest claim entry. It is best-effort: the annotation is already
// stored, and signed-out use is not an error.
func claimNewAnnotations(ctx context.Context, cmd *cobra.Command, db *store.Store, annotationIDs ...string) {
	if db == nil || len(annotationIDs) == 0 {
		return
	}
	snapshot, err := auth.CaptureCredentials(ctx, configDirOverride(cmd))
	if errors.Is(err, auth.ErrSignedOut) {
		return
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: local ownership claim is off for this annotation: %v\n", err)
		return
	}
	if _, err := db.ClaimAnnotationIDs(ctx, snapshot, annotationIDs, store.ClaimSourceAutomatic, time.Now().UnixMilli()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: the annotation was saved but could not be claimed: %v\n", err)
	}
}
