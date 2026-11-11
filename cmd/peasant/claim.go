package main

import (
	"context"
	"errors"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
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
