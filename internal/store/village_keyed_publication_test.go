package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// reattributedProjectHash is the project identity a later harvest moves the
// session to.
const reattributedProjectHash = schema.ProjectHash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

// TestVillageKeyedPublicationReadsFollowTheVillageIdentity checks the two reads
// the push pipeline uses to tell an update from a first publication. The
// Village keys a transcript by its account and the local session, so both
// reads match on that and ignore the project identity: a receipt or an attempt
// recorded before a harvest re-attributed the session still counts, while one
// from another Village or account does not.
func TestVillageKeyedPublicationReadsFollowTheVillageIdentity(t *testing.T) {
	t.Parallel()
	fixture := loadPublicationFixture(t)
	s := openTestStore(t)
	defer s.Close()
	row := fixture.Records[0]
	record := publicationRecordFromFixture(t, row)
	storetest.SeedSessionInProject(t, s, row.SessionID, record.ProjectHash)
	storetest.SeedSessionInProject(t, s, fixture.UnpublishedSibling, record.ProjectHash)
	ctx := context.Background()

	published := func(origin, owner, sessionID string) bool {
		t.Helper()
		found, err := s.PublishedToVillage(ctx, origin, owner, sessionID)
		if err != nil {
			t.Fatalf("check village receipt: %v", err)
		}
		return found
	}
	if published(row.Origin, row.Owner, row.SessionID) {
		t.Fatal("a session with no receipt reads as published")
	}
	if err := s.SavePublication(ctx, record); err != nil {
		t.Fatalf("save receipt: %v", err)
	}
	storetest.SeedSessionInProject(t, s, row.SessionID, reattributedProjectHash)
	if !published(row.Origin, row.Owner, row.SessionID) {
		t.Fatal("a receipt recorded before the session was re-attributed must still count for this Village account")
	}
	if published(row.Origin, "another-account", row.SessionID) || published("https://another.village.example", row.Owner, row.SessionID) {
		t.Fatal("a receipt from one Village account must not count for another")
	}
	if published(row.Origin, row.Owner, fixture.UnpublishedSibling) {
		t.Fatal("a receipt of one session must not count for another session")
	}

	latest := func(owner, sessionID string) *store.PublicationAttemptDiagnostic {
		t.Helper()
		attempt, err := s.LatestSessionPublicationAttempt(ctx, row.Origin, owner, sessionID)
		if err != nil {
			t.Fatalf("read latest attempt: %v", err)
		}
		return attempt
	}
	if latest(row.Owner, fixture.UnpublishedSibling) != nil {
		t.Fatal("a session with no recorded attempt must read none")
	}
	if err := s.RecordPublicationAttempt(ctx, store.PublicationAttemptDiagnostic{VillageOrigin: row.Origin, OwnerUserID: row.Owner, SessionID: fixture.UnpublishedSibling, ProjectHash: record.ProjectHash, AttemptedAt: 1, Stage: store.PublicationAttemptStagePublish, Message: "the upload answer was lost"}); err != nil {
		t.Fatal(err)
	}
	storetest.SeedSessionInProject(t, s, fixture.UnpublishedSibling, reattributedProjectHash)
	if attempt := latest(row.Owner, fixture.UnpublishedSibling); attempt == nil || attempt.Stage != store.PublicationAttemptStagePublish || attempt.ProjectHash != record.ProjectHash {
		t.Fatalf("an attempt recorded before the session was re-attributed must still be found; got %+v", attempt)
	}
	if err := s.RecordPublicationAttempt(ctx, store.PublicationAttemptDiagnostic{VillageOrigin: row.Origin, OwnerUserID: row.Owner, SessionID: fixture.UnpublishedSibling, ProjectHash: reattributedProjectHash, AttemptedAt: 2, Stage: store.PublicationAttemptStageVisibility, Message: "the owner update failed"}); err != nil {
		t.Fatal(err)
	}
	if attempt := latest(row.Owner, fixture.UnpublishedSibling); attempt == nil || attempt.Stage != store.PublicationAttemptStageVisibility {
		t.Fatalf("the latest attempt across project identities must win; got %+v", attempt)
	}
	if latest("another-account", fixture.UnpublishedSibling) != nil {
		t.Fatal("an attempt recorded for one Village account must not be found for another")
	}
}
