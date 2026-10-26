package store

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// Every managed-generation write entry point must refuse through the view
// OpenReadOnlyWithOptions wraps artifact stores in. The refusals happen before
// the inner store is touched, so no file is created even when a future caller
// reaches a write path from a read-only store.
func TestReadOnlyGenerationArtifactsRefuseWrites(t *testing.T) {
	inner, err := NewOSGenerationArtifactStoreExisting(t.TempDir())
	if err != nil {
		t.Fatalf("open existing artifact root: %v", err)
	}
	view := readOnlyGenerationArtifacts{inner: inner}
	ctx := context.Background()

	if _, err := view.Stage(ctx, indexformat.Generation{}, nil); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("Stage error = %v, want the read-only refusal", err)
	}
	if err := view.WriteIntent(ctx, GenerationIntent{}); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("WriteIntent error = %v, want the read-only refusal", err)
	}
	if err := view.ClearIntent(ctx, "ses_planning"); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("ClearIntent error = %v, want the read-only refusal", err)
	}
	if err := view.RepairMetadata(ctx, "ses_planning", nil); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("RepairMetadata error = %v, want the read-only refusal", err)
	}
	if err := view.RemoveGeneration(ctx, "ses_planning", "g_planning"); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("RemoveGeneration error = %v, want the read-only refusal", err)
	}
	if err := view.WritePriorEvidence(ctx, "ses_planning", "g_planning", nil); !errors.Is(err, errReadOnlyGenerationWrite) {
		t.Fatalf("WritePriorEvidence error = %v, want the read-only refusal", err)
	}
}

// A dry run takes no lock file: shared acquisition proceeds without creating
// one and exclusive acquisition refuses, so a read-only store can never leave a
// lock behind or reach an activation path.
func TestLockFreeSessionLockerTakesNoLockFile(t *testing.T) {
	view := lockFreeSessionLocker{}
	ctx := context.Background()
	release, err := view.LockShared(ctx, "ses_planning")
	if err != nil {
		t.Fatalf("LockShared error = %v, want no-op acquisition", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release shared lock: %v", err)
	}
	if _, err := view.LockExclusive(ctx, "ses_planning"); !errors.Is(err, errReadOnlySessionLock) {
		t.Fatalf("LockExclusive error = %v, want the read-only refusal", err)
	}
}
