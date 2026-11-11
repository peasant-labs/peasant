package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	sqlite "github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const (
	ownerTestVillageURL = "https://village.example/team"
	ownerTestUserOne    = "11111111-1111-4111-8111-111111111111"
	ownerTestUserTwo    = "22222222-2222-4222-8222-222222222222"
)

func ownerTestSnapshot(t *testing.T, home, userID, username string) auth.CredentialSnapshot {
	t.Helper()
	creds := auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     userID,
		Username:   username,
		VillageURL: ownerTestVillageURL,
	}
	if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatalf("save fixture credentials: %v", err)
	}
	snapshot, err := auth.CaptureCredentials(t.Context(), home)
	if err != nil {
		t.Fatalf("capture fixture credentials: %v", err)
	}
	return snapshot
}

func ownerTestSeedSessions(t *testing.T, ctx context.Context, s *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		seedTestSessionV13(t, ctx, s, id)
	}
}

func ownerTestCreateSessionAnnotation(t *testing.T, ctx context.Context, s *store.Store, sessionID string) string {
	t.Helper()
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)
	id, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("create fixture annotation: %v", err)
	}
	return id
}

func ownerTestCount(t *testing.T, s *store.Store, query string) int64 {
	t.Helper()
	conn, err := s.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	var n int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func ownerTestExec(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	conn, err := s.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestLocalOwnerAndClaims(t *testing.T) {
	t.Run("owner-created-without-village-login", func(t *testing.T) {
		s := openTestStore(t)
		home := t.TempDir()
		first, err := s.EnsureLocalOwner(t.Context(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.EnsureLocalOwner(t.Context(), 2000)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID == "" || first.ID != second.ID || first.CreatedAt != 1000 {
			t.Fatalf("owner = %+v then %+v, want one stable owner", first, second)
		}
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM local_owner`); got != 1 {
			t.Fatalf("local_owner rows = %d, want 1", got)
		}
		if _, err := os.Stat(filepath.Join(home, "credentials.json")); !os.IsNotExist(err) {
			t.Fatalf("credentials file exists without a login: %v", err)
		}
	})

	t.Run("owner-survives-sign-out", func(t *testing.T) {
		s := openTestStore(t)
		home := t.TempDir()
		owner, err := s.EnsureLocalOwner(t.Context(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(t.Context(), snapshot, false, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		if err := auth.ClearCredentialsFrom(home); err != nil {
			t.Fatal(err)
		}
		again, err := s.EnsureLocalOwner(t.Context(), 3000)
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != owner.ID {
			t.Fatalf("owner id changed across sign-out: %q -> %q", owner.ID, again.ID)
		}
	})

	t.Run("owner-second-row-refused", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.EnsureLocalOwner(t.Context(), 1000); err != nil {
			t.Fatal(err)
		}
		conn, err := s.PoolForTest().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer s.PoolForTest().Put(conn)
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO local_owner(id, singleton, created_at) VALUES('lo_second', 1, 2000)`, nil); err == nil {
			t.Fatal("a second local_owner row was accepted")
		}
	})

	t.Run("claim-default-unclaimed-only", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a", "owner-session-b", "owner-session-c")
		ownerTestCreateSessionAnnotation(t, ctx, s, "owner-session-a")
		ownerTestCreateSessionAnnotation(t, ctx, s, "owner-session-b")
		home := t.TempDir()
		snapshot := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimSessionIDs(ctx, snapshot, []string{"owner-session-a"}, store.ClaimSourceAutomatic, 2000); err != nil {
			t.Fatal(err)
		}
		result, err := s.ClaimArtifacts(ctx, snapshot, false, store.ClaimSourceManual, 3000)
		if err != nil {
			t.Fatal(err)
		}
		if result.AlreadyClaimed == 0 {
			t.Fatal("default claim did not report the already-claimed session/annotation")
		}
		counts, err := s.OwnershipCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if counts.UnclaimedSessions != 0 || counts.UnclaimedAnnotations != 0 {
			t.Fatalf("counts after default claim = %+v, want nothing unclaimed", counts)
		}
	})

	t.Run("claim-all-retains-earlier-entries", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a", "owner-session-b")
		home := t.TempDir()
		first := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(ctx, first, false, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		before, err := s.OwnerClaims(ctx)
		if err != nil || len(before) != 1 {
			t.Fatalf("claims before = %+v, %v; want one entry", before, err)
		}
		if _, err := s.ClaimArtifacts(ctx, first, true, store.ClaimSourceManual, 3000); err != nil {
			t.Fatal(err)
		}
		after, err := s.OwnerClaims(ctx)
		if err != nil || len(after) != 2 {
			t.Fatalf("claims after = %+v, %v; want two entries", after, err)
		}
		if before[0] != after[0] {
			t.Fatalf("earlier claim changed: %+v -> %+v", before[0], after[0])
		}
	})

	t.Run("claim-username-snapshot-only", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a")
		home := t.TempDir()
		first := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(ctx, first, false, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		renamed := ownerTestSnapshot(t, home, ownerTestUserOne, "alice-renamed")
		if _, err := s.ClaimArtifacts(ctx, renamed, true, store.ClaimSourceManual, 3000); err != nil {
			t.Fatal(err)
		}
		claims, err := s.OwnerClaims(ctx)
		if err != nil || len(claims) != 2 {
			t.Fatalf("claims = %+v, %v; want two entries", claims, err)
		}
		if claims[0].UserID != claims[1].UserID || claims[0].UsernameSnapshot != "alice" || claims[1].UsernameSnapshot != "alice-renamed" {
			t.Fatalf("username snapshots = %+v, want two snapshots with one UUID", claims)
		}
	})

	t.Run("claim-username-reuse-different-uuid", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a")
		home := t.TempDir()
		one := ownerTestSnapshot(t, home, ownerTestUserOne, "shared-name")
		if _, err := s.ClaimArtifacts(ctx, one, false, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		two := ownerTestSnapshot(t, home, ownerTestUserTwo, "shared-name")
		if _, err := s.ClaimArtifacts(ctx, two, true, store.ClaimSourceManual, 3000); err != nil {
			t.Fatal(err)
		}
		claims, err := s.OwnerClaims(ctx)
		if err != nil || len(claims) != 2 {
			t.Fatalf("claims = %+v, %v; want two entries", claims, err)
		}
		if claims[0].UserID == claims[1].UserID {
			t.Fatalf("two UUIDs collapsed into one account: %+v", claims)
		}
	})

	t.Run("claim-unlink-and-reclaim-other-account", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a")
		home := t.TempDir()
		first := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(ctx, first, false, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		second := ownerTestSnapshot(t, home, ownerTestUserTwo, "bob")
		if _, err := s.ClaimArtifacts(ctx, second, true, store.ClaimSourceManual, 3000); err != nil {
			t.Fatal(err)
		}
		claims, err := s.OwnerClaims(ctx)
		if err != nil || len(claims) != 2 {
			t.Fatalf("claims = %+v, %v; want two entries", claims, err)
		}
		if claims[0].UserID != ownerTestUserOne || claims[1].UserID != ownerTestUserTwo || claims[0].ClaimedAt > claims[1].ClaimedAt {
			t.Fatalf("claim order = %+v, want account one then account two", claims)
		}
	})

	t.Run("claim-existing-install-prompted-once", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a")
		if claims, err := s.OwnerClaims(ctx); err != nil || len(claims) != 0 {
			t.Fatalf("fresh installation claims = %+v, %v; want none", claims, err)
		}
		home := t.TempDir()
		snapshot := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(ctx, snapshot, false, store.ClaimSourceSignInPrompt, 2000); err != nil {
			t.Fatal(err)
		}
		claims, err := s.OwnerClaims(ctx)
		if err != nil || len(claims) != 1 || claims[0].Source != store.ClaimSourceSignInPrompt {
			t.Fatalf("claims after first prompt = %+v, %v; want one sign-in claim", claims, err)
		}
	})

	t.Run("claim-never-touches-pulled-transcripts", func(t *testing.T) {
		s := openTestStore(t)
		ctx := t.Context()
		ownerTestSeedSessions(t, ctx, s, "owner-session-a")
		ownerTestExec(t, s, `INSERT INTO pulled_transcripts(village_host, transcript_id, owner_user_id, owner_username, content_hash, visibility, pull_dir, first_pulled_at, last_pulled_at)
VALUES('village.example', 'pulled-a', 'remote-user', 'remote-user', 'hash-a', 'private', '/pulls/a', 10, 11)`)
		ownerTestExec(t, s, `INSERT INTO pulled_annotations(village_host, content_hash, transcript_id, author_user_id, author_username, payload, pulled_at)
VALUES('village.example', 'ann-hash-a', 'pulled-a', 'remote-author', 'remote-author', '{"value":"foreign"}', 14)`)
		beforeTranscripts := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_transcripts WHERE owner_user_id='remote-user' AND owner_username='remote-user'`)
		beforeAnnotations := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_annotations WHERE author_user_id='remote-author' AND author_username='remote-author'`)
		home := t.TempDir()
		snapshot := ownerTestSnapshot(t, home, ownerTestUserOne, "alice")
		if _, err := s.ClaimArtifacts(ctx, snapshot, true, store.ClaimSourceManual, 2000); err != nil {
			t.Fatal(err)
		}
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_transcripts WHERE owner_user_id='remote-user' AND owner_username='remote-user'`); got != beforeTranscripts {
			t.Fatalf("pulled transcript rows changed: %d -> %d", beforeTranscripts, got)
		}
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_annotations WHERE author_user_id='remote-author' AND author_username='remote-author'`); got != beforeAnnotations {
			t.Fatalf("pulled annotation rows changed: %d -> %d", beforeAnnotations, got)
		}
	})

	t.Run("migration-forward-only", func(t *testing.T) {
		// The migration test owns the forward-only refusal. Here the current
		// store must simply expose the new tables.
		s := openTestStore(t)
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM local_owner`); got != 0 {
			t.Fatalf("fresh local_owner rows = %d, want 0", got)
		}
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM owner_claims`); got != 0 {
			t.Fatalf("fresh owner_claims rows = %d, want 0", got)
		}
	})

	t.Run("user-show-no-network", func(t *testing.T) {
		s := openTestStore(t)
		home := t.TempDir()
		if _, err := s.EnsureLocalOwner(t.Context(), 1000); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LocalOwner(t.Context()); err != nil {
			t.Fatalf("LocalOwner without credentials: %v", err)
		}
		if _, err := s.OwnerClaims(t.Context()); err != nil {
			t.Fatalf("OwnerClaims without credentials: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, "credentials.json")); !os.IsNotExist(err) {
			t.Fatalf("credentials file exists for a no-network read: %v", err)
		}
	})
}
