package store_test

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	sqlite "github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
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

func ownerTestCount(t *testing.T, s *store.Store, query string, args ...any) int64 {
	t.Helper()
	conn, err := s.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	var n int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args:       args,
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

func ownerTestExecError(t *testing.T, s *store.Store, query string, args ...any) error {
	t.Helper()
	conn, err := s.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	return sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args})
}

func ownerTestString(t *testing.T, s *store.Store, query string, args ...any) string {
	t.Helper()
	conn, err := s.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	var value string
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args:       args,
		ResultFunc: func(stmt *sqlite.Stmt) error { value = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

//go:embed testdata/local_owner_claim_cases.yaml
var localOwnerClaimCasesYAML []byte

type localOwnerClaimFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []localOwnerClaimCase `yaml:"cases"`
}

type localOwnerClaimCase struct {
	Name   string                  `yaml:"name"`
	Steps  []localOwnerClaimStep   `yaml:"steps"`
	Expect localOwnerClaimExpected `yaml:"expect"`
}

type localOwnerClaimStep struct {
	Kind             string   `yaml:"kind"`
	NowMs            int64    `yaml:"nowMs"`
	User             string   `yaml:"user"`
	Username         string   `yaml:"username"`
	Source           string   `yaml:"source"`
	Sessions         []string `yaml:"sessions"`
	Annotations      []string `yaml:"annotations"`
	EntryAnnotations []string `yaml:"entryAnnotations"`
	WantError        string   `yaml:"wantError"`
}

type localOwnerClaimExpected struct {
	OwnerStable       *bool             `yaml:"ownerStable"`
	OwnerRows         *int              `yaml:"ownerRows"`
	OwnerFile         *bool             `yaml:"ownerFile"`
	CredentialsAbsent *bool             `yaml:"credentialsAbsent"`
	PulledUnchanged   *bool             `yaml:"pulledUnchanged"`
	ClaimCount        *int              `yaml:"claimCount"`
	ClaimSources      []string          `yaml:"claimSources"`
	ClaimUsernames    []string          `yaml:"claimUsernames"`
	ClaimUserIDs      []string          `yaml:"claimUserIDs"`
	Sessions          map[string]string `yaml:"sessions"`
	Annotations       map[string]string `yaml:"annotations"`
}

func loadLocalOwnerClaimFixture(t *testing.T) localOwnerClaimFixture {
	t.Helper()
	var fixture localOwnerClaimFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(localOwnerClaimCasesYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode local_owner_claim_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			t.Fatal("local_owner_claim_cases.yaml has an unnamed case")
		}
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "local_owner_claim_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func ownerTestOpenStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dbPath
}

func TestLocalOwnerAndClaims(t *testing.T) {
	fixture := loadLocalOwnerClaimFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runLocalOwnerClaimCase(t, testCase)
		})
	}
}

func runLocalOwnerClaimCase(t *testing.T, testCase localOwnerClaimCase) {
	t.Helper()
	s, dbPath := ownerTestOpenStore(t)
	ctx := t.Context()
	home := t.TempDir()
	state := struct {
		snapshot    auth.CredentialSnapshot
		owner       *store.LocalOwner
		annotations map[string]string
		pulled      map[string]int64
	}{annotations: map[string]string{}, pulled: map[string]int64{}}

	for _, step := range testCase.Steps {
		switch step.Kind {
		case "ensureOwner":
			owner, err := s.EnsureLocalOwner(ctx, step.NowMs)
			if err != nil {
				t.Fatalf("ensure owner: %v", err)
			}
			if state.owner == nil {
				state.owner = &owner
			}
		case "signIn":
			userID := ownerTestUserID(t, step.User)
			state.snapshot = ownerTestSnapshot(t, home, userID, step.Username)
		case "signOut":
			if err := auth.ClearCredentialsFrom(home); err != nil {
				t.Fatalf("sign out fixture credentials: %v", err)
			}
			state.snapshot = auth.CredentialSnapshot{}
		case "seed":
			ownerTestSeedSessions(t, ctx, s, step.Sessions...)
			for _, sessionID := range step.Annotations {
				state.annotations[sessionID] = ownerTestCreateSessionAnnotation(t, ctx, s, sessionID)
			}
			for _, sessionID := range step.EntryAnnotations {
				key := "entry:" + sessionID
				id := "owner-entry-" + sessionID
				ownerTestExec(t, s, `INSERT INTO annotations(id, target_kind_id, annotation_type_id, annotator_id, value, created_at)
SELECT ?, (SELECT id FROM target_kinds WHERE name = 'entry'), annotation_type_id, annotator_id, 'entry-target', created_at
FROM annotations WHERE id = ?`, id, state.annotations[sessionID])
				ownerTestExec(t, s, `INSERT INTO annotation_target_entries(annotation_id, session_id, entry_index, end_index) VALUES(?, ?, 0, 1)`, id, sessionID)
				state.annotations[key] = id
			}
		case "seedPulled":
			ownerTestExec(t, s, `INSERT INTO pulled_transcripts(village_host, transcript_id, owner_user_id, owner_username, content_hash, visibility, pull_dir, first_pulled_at, last_pulled_at)
VALUES('village.example', 'pulled-a', 'remote-user', 'remote-user', 'hash-a', 'private', '/pulls/a', 10, 11)`)
			ownerTestExec(t, s, `INSERT INTO pulled_annotations(village_host, content_hash, transcript_id, author_user_id, author_username, payload, pulled_at)
VALUES('village.example', 'ann-hash-a', 'pulled-a', 'remote-author', 'remote-author', '{"value":"foreign"}', 14)`)
			state.pulled["transcripts"] = ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_transcripts WHERE owner_user_id='remote-user' AND owner_username='remote-user'`)
			state.pulled["annotations"] = ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_annotations WHERE author_user_id='remote-author' AND author_username='remote-author'`)
		case "claimDefault", "claimAll", "claimSessions":
			if state.snapshot.Credentials().UserID == "" {
				t.Fatalf("%s requires a signed-in account", step.Kind)
			}
			source := store.ClaimSource(step.Source)
			if source == "" {
				source = store.ClaimSourceManual
			}
			var err error
			switch step.Kind {
			case "claimDefault":
				_, err = s.ClaimArtifacts(ctx, state.snapshot, false, source, step.NowMs)
			case "claimAll":
				_, err = s.ClaimArtifacts(ctx, state.snapshot, true, source, step.NowMs)
			case "claimSessions":
				_, err = s.ClaimSessionIDs(ctx, state.snapshot, step.Sessions, source, step.NowMs)
			}
			if err != nil {
				t.Fatalf("%s: %v", step.Kind, err)
			}
		case "insertSecondOwner":
			err := ownerTestExecError(t, s, `INSERT INTO local_owner(id, singleton, created_at) VALUES('lo_second', 1, 2000)`)
			if err == nil || !strings.Contains(err.Error(), step.WantError) {
				t.Fatalf("second local_owner insert error = %v, want %q", err, step.WantError)
			}
		default:
			t.Fatalf("unknown fixture step %q", step.Kind)
		}
	}

	claims, err := s.OwnerClaims(ctx)
	if err != nil {
		t.Fatalf("read claims: %v", err)
	}
	expect := testCase.Expect
	if expect.OwnerStable != nil && *expect.OwnerStable {
		owner, err := s.LocalOwner(ctx)
		if err != nil || owner == nil || state.owner == nil {
			t.Fatalf("read final owner = %+v, %v, want %+v", owner, err, state.owner)
		}
		if owner.ID != state.owner.ID || owner.CreatedAt != state.owner.CreatedAt {
			t.Fatalf("owner changed: %+v -> %+v", *state.owner, *owner)
		}
	}
	if expect.OwnerRows != nil {
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM local_owner`); got != int64(*expect.OwnerRows) {
			t.Fatalf("local_owner rows = %d, want %d", got, *expect.OwnerRows)
		}
	}
	if expect.OwnerFile != nil && *expect.OwnerFile {
		if _, err := os.Stat(store.LocalOwnerFilePath(dbPath)); err != nil {
			t.Fatalf("owner permanence file: %v", err)
		}
	}
	if expect.CredentialsAbsent != nil && *expect.CredentialsAbsent {
		if _, err := os.Stat(filepath.Join(home, "credentials.json")); !os.IsNotExist(err) {
			t.Fatalf("credentials file exists without a login: %v", err)
		}
	}
	if expect.PulledUnchanged != nil && *expect.PulledUnchanged {
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_transcripts WHERE owner_user_id='remote-user' AND owner_username='remote-user'`); got != state.pulled["transcripts"] {
			t.Fatalf("pulled transcripts changed: %d -> %d", state.pulled["transcripts"], got)
		}
		if got := ownerTestCount(t, s, `SELECT COUNT(*) FROM pulled_annotations WHERE author_user_id='remote-author' AND author_username='remote-author'`); got != state.pulled["annotations"] {
			t.Fatalf("pulled annotations changed: %d -> %d", state.pulled["annotations"], got)
		}
	}
	if expect.ClaimCount != nil && len(claims) != *expect.ClaimCount {
		t.Fatalf("claim entries = %d (%+v), want %d", len(claims), claims, *expect.ClaimCount)
	}
	if expect.ClaimSources != nil {
		got := make([]string, len(claims))
		for i, claim := range claims {
			got[i] = string(claim.Source)
		}
		if strings.Join(got, ",") != strings.Join(expect.ClaimSources, ",") {
			t.Fatalf("claim sources = %v, want %v", got, expect.ClaimSources)
		}
	}
	if expect.ClaimUsernames != nil {
		got := make([]string, len(claims))
		for i, claim := range claims {
			got[i] = claim.UsernameSnapshot
		}
		if strings.Join(got, ",") != strings.Join(expect.ClaimUsernames, ",") {
			t.Fatalf("claim usernames = %v, want %v", got, expect.ClaimUsernames)
		}
	}
	if expect.ClaimUserIDs != nil {
		got := make([]string, len(claims))
		for i, claim := range claims {
			got[i] = ownerTestUserToken(t, claim.UserID)
		}
		if strings.Join(got, ",") != strings.Join(expect.ClaimUserIDs, ",") {
			t.Fatalf("claim user ids = %v, want %v", got, expect.ClaimUserIDs)
		}
	}
	for sessionID, want := range expect.Sessions {
		got := ownerTestPointerOrdinal(t, s, claims, "sessions", "session_id", sessionID)
		if got != want {
			t.Fatalf("session %s pointer = %s, want %s", sessionID, got, want)
		}
	}
	for key, want := range expect.Annotations {
		annotationID, ok := state.annotations[key]
		if !ok {
			t.Fatalf("fixture expected annotation %q but none was seeded", key)
		}
		got := ownerTestPointerOrdinal(t, s, claims, "annotations", "id", annotationID)
		if got != want {
			t.Fatalf("annotation %s pointer = %s, want %s", key, got, want)
		}
	}
}

func ownerTestUserID(t *testing.T, token string) string {
	t.Helper()
	switch token {
	case "one":
		return ownerTestUserOne
	case "two":
		return ownerTestUserTwo
	default:
		t.Fatalf("unknown fixture user %q", token)
		return ""
	}
}

func ownerTestUserToken(t *testing.T, userID string) string {
	t.Helper()
	switch userID {
	case ownerTestUserOne:
		return "one"
	case ownerTestUserTwo:
		return "two"
	default:
		t.Fatalf("unknown fixture user id %q", userID)
		return ""
	}
}

func ownerTestPointerOrdinal(t *testing.T, s *store.Store, claims []store.OwnerClaim, table, keyColumn, key string) string {
	t.Helper()
	value := ownerTestString(t, s, `SELECT COALESCE(owner_claim_id, '') FROM `+table+` WHERE `+keyColumn+` = ?`, key)
	if value == "" {
		return "null"
	}
	for i, claim := range claims {
		if claim.ID == value {
			return []string{"first", "second", "third", "fourth"}[i]
		}
	}
	return "unknown:" + value
}

func TestLocalOwnerFileIsAdoptedAfterDatabaseRebuild(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "peasant.db")
	// ast-grep-ignore: no-migrating-store-open-in-tests -- this test deletes the database and reopens the real migration path to prove owner adoption.
	first, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	owner, err := first.LocalOwner(t.Context())
	if err != nil || owner == nil {
		t.Fatalf("read owner = %+v, %v", owner, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove rebuilt database %s: %v", dbPath+suffix, err)
		}
	}
	// ast-grep-ignore: no-migrating-store-open-in-tests -- the reopen after deleting the database is the subject: it must adopt the owner file.
	second, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open rebuilt store: %v", err)
	}
	defer second.Close()
	adopted, err := second.LocalOwner(t.Context())
	if err != nil || adopted == nil {
		t.Fatalf("read adopted owner = %+v, %v", adopted, err)
	}
	if adopted.ID != owner.ID || adopted.CreatedAt != owner.CreatedAt {
		t.Fatalf("owner after rebuild = %+v, want %+v", *adopted, *owner)
	}
	claims, err := second.OwnerClaims(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("rebuilt store claims = %+v, want none (claims do not survive a rebuild)", claims)
	}
}

func TestClaimSourceSetMatchesSchema(t *testing.T) {
	s := openTestStore(t)
	tableSQL := ownerTestString(t, s, `SELECT sql FROM sqlite_master WHERE type='table' AND name='owner_claims'`)
	const marker = "source IN ("
	start := strings.Index(tableSQL, marker)
	if start < 0 {
		t.Fatalf("owner_claims DDL has no source CHECK: %s", tableSQL)
	}
	rest := tableSQL[start+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("owner_claims source CHECK is unterminated: %s", tableSQL)
	}
	fromSchema := map[string]bool{}
	for _, part := range strings.Split(rest[:end], ",") {
		fromSchema[strings.Trim(strings.TrimSpace(part), "'")] = true
	}
	fromGo := map[string]bool{}
	for _, source := range store.AllClaimSources() {
		fromGo[string(source)] = true
		if parsed, ok := store.ParseClaimSource(string(source)); !ok || parsed != source {
			t.Fatalf("ParseClaimSource(%q) = %q, %v", source, parsed, ok)
		}
	}
	if len(fromSchema) != len(fromGo) {
		t.Fatalf("schema claim sources = %v, Go set = %v", fromSchema, fromGo)
	}
	for source := range fromGo {
		if !fromSchema[source] {
			t.Fatalf("Go claim source %q is absent from the schema CHECK %v", source, fromSchema)
		}
	}
	for source := range fromSchema {
		if !fromGo[source] {
			t.Fatalf("schema claim source %q is absent from AllClaimSources %v", source, fromGo)
		}
	}
	if _, ok := store.ParseClaimSource("not-a-source"); ok {
		t.Fatal("ParseClaimSource accepted a value outside the closed set")
	}
}

func TestOwnerClaimsOrderedByClaimedAtThenInsertion(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	owner, err := s.EnsureLocalOwner(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ownerTestExec(t, s, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation) VALUES(?, ?, 'ordering-fixture-one')`, ownerTestVillageURL, ownerTestUserOne)
	ownerTestExec(t, s, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation) VALUES(?, ?, 'ordering-fixture-two')`, ownerTestVillageURL, ownerTestUserTwo)
	// Deliberately reverse the id order: a claimed_at,id sort returns oc_aaa
	// before oc_zzz, while claimed_at,rowid returns insertion order.
	ownerTestExec(t, s, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_zzz', ?, ?, ?, 'first-inserted', 4000, 'manual')`, owner.ID, ownerTestVillageURL, ownerTestUserOne)
	ownerTestExec(t, s, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES('oc_aaa', ?, ?, ?, 'second-inserted', 4000, 'manual')`, owner.ID, ownerTestVillageURL, ownerTestUserTwo)
	claims, err := s.OwnerClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].ID != "oc_zzz" || claims[1].ID != "oc_aaa" {
		t.Fatalf("same-millisecond claim order = %+v, want insertion order oc_zzz then oc_aaa", claims)
	}
}

func TestOwnerClaimsLogIsBounded(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	owner, err := s.EnsureLocalOwner(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ownerTestExec(t, s, `INSERT INTO collective_observation_scopes(village_url, user_id, incarnation) VALUES(?, ?, 'bounded-fixture')`, ownerTestVillageURL, ownerTestUserOne)
	total := store.OwnerClaimsPageSize + 5
	for i := 0; i < total; i++ {
		ownerTestExec(t, s, `INSERT INTO owner_claims(id, local_owner_id, village_url, user_id, username_snapshot, claimed_at, source)
VALUES(?, ?, ?, ?, ?, ?, 'manual')`, "oc_"+string(rune('a'+i%26))+"_"+string(rune('a'+i/26)), owner.ID, ownerTestVillageURL, ownerTestUserOne, "bounded", int64(1000+i))
	}
	claims, err := s.OwnerClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != store.OwnerClaimsPageSize {
		t.Fatalf("claim page = %d, want %d", len(claims), store.OwnerClaimsPageSize)
	}
	if claims[0].ClaimedAt != int64(1000+5) || claims[len(claims)-1].ClaimedAt != int64(1000+total-1) {
		t.Fatalf("claim page bounds = %d..%d, want %d..%d", claims[0].ClaimedAt, claims[len(claims)-1].ClaimedAt, 1000+5, 1000+total-1)
	}
	count, err := s.OwnerClaimCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != total {
		t.Fatalf("OwnerClaimCount = %d, want %d", count, total)
	}
}
