package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// TestSessionPublicationsFollowTheSessionAcrossProjects pins the session-keyed
// receipt and attempt reads the local publication status is served from. They
// answer for the signed-in account only, and they still find a session's
// receipt after a harvest moved the session to another project hash, where the
// project-keyed read no longer does.
func TestSessionPublicationsFollowTheSessionAcrossProjects(t *testing.T) {
	t.Parallel()
	fixture := loadPublicationFixture(t)
	primary := publicationRecordFromFixture(t, fixture.Records[0])
	moved := publicationRecordFromFixture(t, fixture.Records[1])
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	storetest.SeedSessionInProject(t, s, primary.SessionID, primary.ProjectHash)
	storetest.SeedSessionInProject(t, s, fixture.UnpublishedSibling, primary.ProjectHash)
	if err := s.SavePublication(ctx, primary); err != nil {
		t.Fatalf("save receipt: %v", err)
	}
	for index, attempt := range fixture.Attempts[:2] {
		diagnostic := store.PublicationAttemptDiagnostic{VillageOrigin: primary.VillageOrigin, OwnerUserID: primary.OwnerUserID, SessionID: primary.SessionID, ProjectHash: primary.ProjectHash, AttemptedAt: int64(10 + index), Stage: attempt.Stage, Message: attempt.Message}
		if err := s.RecordPublicationAttempt(ctx, diagnostic); err != nil {
			t.Fatalf("record attempt %q: %v", attempt.Name, err)
		}
	}
	// A later harvest attributes the session to another project.
	storetest.SeedSessionInProject(t, s, primary.SessionID, moved.ProjectHash)
	if got, err := s.Publication(ctx, primary.VillageOrigin, primary.OwnerUserID, moved.ProjectHash, primary.SessionID); err != nil || got != nil {
		t.Fatalf("precondition: the project-keyed read under the new project = %+v, %v; want no receipt", got, err)
	}

	ids := []string{primary.SessionID, fixture.UnpublishedSibling}
	receipts, err := s.SessionPublications(ctx, primary.VillageOrigin, primary.OwnerUserID, ids)
	if err != nil {
		t.Fatalf("session receipts: %v", err)
	}
	got, ok := receipts[primary.SessionID]
	if !ok || got.Receipt.TranscriptID != primary.Receipt.TranscriptID || got.ProjectHash != primary.ProjectHash {
		t.Fatalf("receipt after the move = %+v (found %v); want the receipt published under %s", got, ok, primary.ProjectHash)
	}
	if _, ok := receipts[fixture.UnpublishedSibling]; ok || len(receipts) != 1 {
		t.Fatalf("receipts = %+v; a session with no receipt must be absent", receipts)
	}
	attempts, err := s.SessionPublicationAttempts(ctx, primary.VillageOrigin, primary.OwnerUserID, ids)
	if err != nil {
		t.Fatalf("session attempts: %v", err)
	}
	latest, ok := attempts[primary.SessionID]
	if !ok || latest.Message != fixture.Attempts[1].Message || latest.AttemptedAt != 11 || len(attempts) != 1 {
		t.Fatalf("attempts = %+v; want only the latest attempt of the published session", attempts)
	}

	// Publishing the session under its new project writes a second receipt,
	// which Village updated later; that one wins.
	if err := s.SavePublication(ctx, moved); err != nil {
		t.Fatalf("save the receipt under the new project: %v", err)
	}
	receipts, err = s.SessionPublications(ctx, primary.VillageOrigin, primary.OwnerUserID, ids)
	if err != nil {
		t.Fatalf("session receipts after the second publish: %v", err)
	}
	if got := receipts[primary.SessionID]; got.Receipt.TranscriptID != moved.Receipt.TranscriptID || got.ProjectHash != moved.ProjectHash {
		t.Fatalf("receipt after the second publish = %+v; want the later receipt under %s", got, moved.ProjectHash)
	}

	assertNoPublicationState(t, s, "another owner", primary.VillageOrigin, primary.OwnerUserID+"-other", ids)
	assertNoPublicationState(t, s, "another Village", primary.VillageOrigin+"/other", primary.OwnerUserID, ids)
}

// assertNoPublicationState checks that an account holds neither a receipt nor
// an attempt for the named sessions.
func assertNoPublicationState(t *testing.T, s *store.Store, label, origin, owner string, ids []string) {
	t.Helper()
	receipts, err := s.SessionPublications(context.Background(), origin, owner, ids)
	if err != nil || len(receipts) != 0 {
		t.Errorf("%s reads receipts %+v (err %v); a receipt answers for its own account only", label, receipts, err)
	}
	attempts, err := s.SessionPublicationAttempts(context.Background(), origin, owner, ids)
	if err != nil || len(attempts) != 0 {
		t.Errorf("%s reads attempts %+v (err %v); an attempt answers for its own account only", label, attempts, err)
	}
}
