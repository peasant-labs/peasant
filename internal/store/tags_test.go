package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestTags_RoundTrip verifies SetTags / GetTags round-trip.
func TestTags_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, s, string(sid))

	// Initially no tags.
	tags, err := s.GetTags(ctx, sid)
	if err != nil {
		t.Fatalf("GetTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected no tags initially, got %v", tags)
	}

	// Set tags.
	if err := s.SetTags(ctx, sid, []string{"bug", "p0"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	tags, err = s.GetTags(ctx, sid)
	if err != nil {
		t.Fatalf("GetTags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "bug" || tags[1] != "p0" {
		t.Errorf("expected [bug p0], got %v", tags)
	}
}

// TestTags_AddIdempotent verifies AddTag is idempotent.
func TestTags_AddIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, s, string(sid))

	// Add same tag twice — should be idempotent.
	if err := s.AddTag(ctx, sid, "feature"); err != nil {
		t.Fatalf("AddTag: %v", err)
	}
	if err := s.AddTag(ctx, sid, "feature"); err != nil {
		t.Fatalf("AddTag (2nd): %v", err)
	}

	tags, _ := s.GetTags(ctx, sid)
	if len(tags) != 1 || tags[0] != "feature" {
		t.Errorf("expected [feature], got %v", tags)
	}
}

// TestTags_RemoveAbsent verifies RemoveTag on absent tag is a no-op.
func TestTags_RemoveAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, s, string(sid))

	// Remove from empty tags — no error.
	if err := s.RemoveTag(ctx, sid, "nonexistent"); err != nil {
		t.Fatalf("RemoveTag on empty: %v", err)
	}

	// Add, then remove.
	s.AddTag(ctx, sid, "a")
	s.AddTag(ctx, sid, "b")
	s.RemoveTag(ctx, sid, "a")

	tags, _ := s.GetTags(ctx, sid)
	if len(tags) != 1 || tags[0] != "b" {
		t.Errorf("expected [b], got %v", tags)
	}
}

// TestTags_ClearAll verifies SetTags with nil clears all tags.
func TestTags_ClearAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, s, string(sid))

	s.SetTags(ctx, sid, []string{"x", "y"})
	s.SetTags(ctx, sid, nil) // clear

	tags, _ := s.GetTags(ctx, sid)
	if len(tags) != 0 {
		t.Errorf("expected no tags after clear, got %v", tags)
	}
}
