package mock

import (
	"context"
	"testing"

	"github.com/peasant-labs/schema"
)

// TestProviderDetailReadPayloadResolvesCurrentParentNavigation is the focused
// production-exit check for the mounted current-parent fixture: the mock store
// serves the flat local read through the same decoration boundary the
// generation-backed store uses, so the child's context/source and parent links
// resolve to their exact stored sessions while the retained earlier partition
// and the separate input submission count stay intact.
func TestProviderDetailReadPayloadResolvesCurrentParentNavigation(t *testing.T) {
	fixture := canonicalContextNavigationFixture
	provider := NewProvider()
	ctx := context.Background()

	payload, err := provider.DetailReadPayload(ctx, fixture.Child.ID)
	if err != nil {
		t.Fatalf("DetailReadPayload(%s) failed: %v", fixture.Child.ID, err)
	}

	if payload.ID != fixture.Child.ID {
		t.Errorf("payload id = %q, want %q", payload.ID, fixture.Child.ID)
	}
	if payload.TurnCount != 5 || len(payload.Turns) != 5 {
		t.Errorf("main turn count = %d/%d, want 5/5", payload.TurnCount, len(payload.Turns))
	}
	if payload.InputSubmissionCount == nil || *payload.InputSubmissionCount != 1 {
		t.Errorf("inputSubmissionCount = %v, want present 1", payload.InputSubmissionCount)
	}
	if len(payload.EarlierHistory) != 1 || len(payload.EarlierHistory[0].Turns) != 2 {
		t.Fatalf("earlier history = %#v, want one partition with two retained turns", payload.EarlierHistory)
	}
	if payload.EarlierHistory[0].State != schema.EarlierHistoryUncertainMigrated {
		t.Errorf("earlier history state = %q, want %q", payload.EarlierHistory[0].State, schema.EarlierHistoryUncertainMigrated)
	}
	for _, turn := range payload.Turns {
		for _, earlier := range payload.EarlierHistory[0].Turns {
			if turn.SourceEntryRef != "" && turn.SourceEntryRef == earlier.SourceEntryRef {
				t.Errorf("main and earlier history share retained ref %q", turn.SourceEntryRef)
			}
		}
	}

	if len(payload.Relationships) != 2 {
		t.Fatalf("relationships = %#v, want the context_from source and the started_by parent", payload.Relationships)
	}
	navByKind := map[schema.SessionRelationshipKind]schema.SessionRelationshipNavigation{}
	for _, entry := range payload.RelationshipNavigation {
		navByKind[entry.Kind] = entry
	}
	for kind, want := range map[schema.SessionRelationshipKind]string{
		schema.SessionRelationshipContextFrom: fixture.Source.ID,
		schema.SessionRelationshipStartedBy:   fixture.Parent.ID,
	} {
		entry, ok := navByKind[kind]
		if !ok {
			t.Fatalf("relationshipNavigation is missing the %q entry: %#v", kind, payload.RelationshipNavigation)
		}
		if entry.Status != schema.RelationshipNavigationResolved {
			t.Errorf("%q navigation status = %q, want %q", kind, entry.Status, schema.RelationshipNavigationResolved)
		}
		if entry.LocalID == nil || string(*entry.LocalID) != want {
			t.Errorf("%q navigation localId = %v, want %q", kind, entry.LocalID, want)
		}
	}
}

// TestProviderResolveStoredTargetsReportsMissingHonestly keeps the unavailable
// case honest: an identifier that names no stored session resolves to an
// explicit miss (no target) instead of an error, so one stale link cannot hide
// the sessions that do exist.
func TestProviderResolveStoredTargetsReportsMissingHonestly(t *testing.T) {
	provider := NewProvider()
	targets, err := provider.resolveStoredTargets(context.Background(), []string{
		canonicalContextNavigationFixture.Child.ID,
		"sess_notstored",
		canonicalContextNavigationFixture.Child.ID,
	})
	if err != nil {
		t.Fatalf("resolveStoredTargets failed: %v", err)
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
