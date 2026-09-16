package mock

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/schema"
)

// TestProviderReadServesStoredLinksAndRetainedHistory is the focused
// production-exit check for the mounted current-parent fixture: the mock store
// serves the flat local read through the same decoration boundary the
// generation-backed store uses, so the child's context/source and parent links
// resolve to their exact stored sessions while the retained earlier partition
// and the separate input submission count stay intact.
func TestProviderReadServesStoredLinksAndRetainedHistory(t *testing.T) {
	fixture := canonicalContextNavigationFixture
	read, err := api.SessionDetailReadForProvider(context.Background(), NewProvider(), fixture.Child.ID)
	if err != nil {
		t.Fatalf("session detail read for %s failed: %v", fixture.Child.ID, err)
	}

	if read.ID != fixture.Child.ID {
		t.Errorf("payload id = %q, want %q", read.ID, fixture.Child.ID)
	}
	if read.TurnCount != 5 || len(read.Turns) != 5 {
		t.Errorf("main turn count = %d/%d, want 5/5", read.TurnCount, len(read.Turns))
	}
	if read.InputSubmissionCount == nil || *read.InputSubmissionCount != 1 {
		t.Errorf("inputSubmissionCount = %v, want the measured single submission", read.InputSubmissionCount)
	}
	if len(read.EarlierHistory) != 1 || len(read.EarlierHistory[0].Turns) != 2 {
		t.Fatalf("earlier history = %#v, want one partition with two retained turns", read.EarlierHistory)
	}
	if read.EarlierHistory[0].State != schema.EarlierHistoryUncertainMigrated {
		t.Errorf("earlier history state = %q, want %q", read.EarlierHistory[0].State, schema.EarlierHistoryUncertainMigrated)
	}
	for _, turn := range read.Turns {
		for _, earlier := range read.EarlierHistory[0].Turns {
			if turn.SourceEntryRef != "" && turn.SourceEntryRef == earlier.SourceEntryRef {
				t.Errorf("main and earlier history share retained ref %q", turn.SourceEntryRef)
			}
		}
	}

	if len(read.Relationships) != 2 {
		t.Fatalf("relationships = %#v, want the context_from source and the started_by parent", read.Relationships)
	}
	if len(read.RelationshipNavigation) != 2 {
		t.Fatalf("relationshipNavigation = %+v, want one entry per stored relationship", read.RelationshipNavigation)
	}
	navByKind := map[schema.SessionRelationshipKind]schema.SessionRelationshipNavigation{}
	for _, entry := range read.RelationshipNavigation {
		navByKind[entry.Kind] = entry
	}
	for kind, want := range map[schema.SessionRelationshipKind]string{
		schema.SessionRelationshipContextFrom: fixture.Source.ID,
		schema.SessionRelationshipStartedBy:   fixture.Parent.ID,
	} {
		entry, ok := navByKind[kind]
		if !ok {
			t.Fatalf("relationshipNavigation is missing the %q entry: %#v", kind, read.RelationshipNavigation)
		}
		if entry.Status != schema.RelationshipNavigationResolved {
			t.Errorf("%q navigation status = %q, want %q", kind, entry.Status, schema.RelationshipNavigationResolved)
		}
		if entry.LocalID == nil || string(*entry.LocalID) != want {
			t.Errorf("%q navigation localId = %v, want %q", kind, entry.LocalID, want)
		}
	}
}

// TestProviderReadReportsAbsentTargetHonestly keeps the unavailable case
// honest: a stored child whose current-parent target is no longer stored reads
// as known_unavailable with NO identifier, and the child's own turns stay
// readable, so one stale link can never route to the wrong session or hide the
// child.
func TestProviderReadReportsAbsentTargetHonestly(t *testing.T) {
	fixture := canonicalContextNavigationFixture
	read, err := api.SessionDetailReadForProvider(context.Background(), NewProvider(), fixture.Unresolved.ID)
	if err != nil {
		t.Fatalf("session detail read for %s failed: %v", fixture.Unresolved.ID, err)
	}
	if len(read.RelationshipNavigation) != 1 {
		t.Fatalf("relationshipNavigation = %+v, want one entry for the known target", read.RelationshipNavigation)
	}
	entry := read.RelationshipNavigation[0]
	if entry.Status != schema.RelationshipNavigationKnownUnavailable {
		t.Fatalf("navigation status = %q, want known_unavailable for a stored-absent target", entry.Status)
	}
	if entry.LocalID != nil || entry.TranscriptID != nil {
		t.Fatalf("unavailable navigation = %+v, want no identifier that could route to another session", entry)
	}
	if len(read.Turns) == 0 {
		t.Fatal("the child must stay readable when its current-parent target is unavailable")
	}
}

// TestProviderResolveStoredTargetsReportsMissingHonestly covers the mock's
// stored-target resolver directly: a requested identifier that names no stored
// session resolves to an explicit miss (no target) instead of an error, and a
// repeated identifier is collapsed, so one stale link cannot hide the sessions
// that do exist.
func TestProviderResolveStoredTargetsReportsMissingHonestly(t *testing.T) {
	provider := NewProvider()
	targets, err := provider.ResolveStoredTargets(context.Background(), []string{
		canonicalContextNavigationFixture.Child.ID,
		"sess_notstored",
		canonicalContextNavigationFixture.Child.ID,
	})
	if err != nil {
		t.Fatalf("ResolveStoredTargets failed: %v", err)
	}
	want := []string{canonicalContextNavigationFixture.Child.ID, "sess_notstored"}
	if len(targets) != len(want) {
		t.Fatalf("targets = %#v, want %d entries with duplicates collapsed", targets, len(want))
	}
	if !targets[0].Found {
		t.Errorf("stored child target reported missing")
	}
	if targets[1].Found {
		t.Errorf("unknown target %q reported found", targets[1].ID)
	}
}
