package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// An observation write that outlasts the writer hold target is a deadline,
// not a local persistence failure, and the hold target never bounds a read.
// A hold target that is already spent makes the interruption deterministic.
func TestCollectiveObservationWriteHoldTargetIsDeadline(t *testing.T) {
	db := storetest.OpenWith(t, store.WithWriteConfig(ingest.WriteConfig{HoldTarget: -time.Nanosecond}))
	home := t.TempDir()
	creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture-key", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.invalid/installation"}
	if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	snapshot, err := auth.CaptureCredentials(t.Context(), home)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.BeginCollectiveObservation(t.Context(), snapshot)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "writer hold target") {
		t.Fatalf("held write past the hold target: err=%v, want a deadline naming the writer hold target", err)
	}
	if _, err := db.CollectiveTrackingIDs(t.Context(), snapshot); err != nil {
		t.Fatalf("read was bounded by the writer hold target: %v", err)
	}
}
