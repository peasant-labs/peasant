package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

func openUserCommandStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	dataDir := string(defaults.ResolveDataDirPathWith(dir))
	dbPath := string(defaults.ResolveDBFilePathWith(dir))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	storetest.CopyGoldenTo(t, dbPath)
	db, err := openPreparedStore(t, dbPath)
	if err != nil {
		t.Fatalf("open prepared store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func saveUserCommandCredentials(t *testing.T, dir string) auth.Credentials {
	t.Helper()
	creds := auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     "11111111-1111-4111-8111-111111111111",
		Username:   "alice",
		VillageURL: "https://village.example/team",
	}
	if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
		t.Fatalf("save fixture credentials: %v", err)
	}
	return creds
}

func TestUserShowReportsLocalOwnerWithoutLogin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	owner, err := db.EnsureLocalOwner(t.Context(), 1700000000000)
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedSession(t, db, "user-show-session-a")
	storetest.SeedSession(t, db, "user-show-session-b")

	stdout, stderr, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show: %v\n%s%s", err, stdout, stderr)
	}
	var report struct {
		Owner struct {
			ID        string `json:"id"`
			CreatedAt string `json:"createdAt"`
		} `json:"owner"`
		Claims      []any `json:"claims"`
		TotalClaims int   `json:"totalClaims"`
		Ownership   struct {
			Sessions struct {
				Total     int `json:"total"`
				Claimed   int `json:"claimed"`
				Unclaimed int `json:"unclaimed"`
			} `json:"sessions"`
		} `json:"ownership"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode user show json: %v\n%s", err, stdout)
	}
	if report.Owner.ID != owner.ID || report.Owner.CreatedAt == "" {
		t.Fatalf("local owner = %+v, want %s with a created time", report.Owner, owner.ID)
	}
	if len(report.Claims) != 0 || report.Claims == nil || report.TotalClaims != 0 {
		t.Fatalf("claims = %#v total %d, want an empty array", report.Claims, report.TotalClaims)
	}
	if report.Ownership.Sessions.Total != 2 || report.Ownership.Sessions.Claimed != 0 || report.Ownership.Sessions.Unclaimed != 2 {
		t.Fatalf("session counts = %+v, want 2 total and 2 unclaimed", report.Ownership.Sessions)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "peasant", "credentials.json")); !os.IsNotExist(statErr) {
		t.Fatalf("user show wrote or required credentials: %v", statErr)
	}
}

func TestUserShowPrintsClaimLogInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "user-show-session-a")
	creds := saveUserCommandCredentials(t, dir)
	snapshot, err := auth.CaptureCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimArtifacts(t.Context(), snapshot, false, store.ClaimSourceManual, 1700000001000); err != nil {
		t.Fatal(err)
	}
	// Reassign to a second scope so the log has two ordered entries.
	creds.UserID = "22222222-2222-4222-8222-222222222222"
	creds.Username = "bob"
	if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
		t.Fatal(err)
	}
	second, err := auth.CaptureCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimArtifacts(t.Context(), second, true, store.ClaimSourceManual, 1700000002000); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show: %v\n%s", err, stdout)
	}
	var report struct {
		Claims []struct {
			ID           string `json:"id"`
			LocalOwnerID string `json:"localOwnerId"`
			VillageURL   string `json:"villageUrl"`
			UserID       string `json:"userId"`
			Username     string `json:"username"`
			ClaimedAt    string `json:"claimedAt"`
			Source       string `json:"source"`
		} `json:"claims"`
		TotalClaims int `json:"totalClaims"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode user show json: %v\n%s", err, stdout)
	}
	if len(report.Claims) != 2 || report.TotalClaims != 2 {
		t.Fatalf("claims = %+v total %d, want two ordered entries", report.Claims, report.TotalClaims)
	}
	if report.Claims[0].UserID != "11111111-1111-4111-8111-111111111111" || report.Claims[0].Username != "alice" ||
		report.Claims[1].UserID != "22222222-2222-4222-8222-222222222222" || report.Claims[1].Username != "bob" {
		t.Fatalf("claim order = %+v, want alice then bob", report.Claims)
	}
	for _, claim := range report.Claims {
		if claim.ID == "" || claim.LocalOwnerID == "" || claim.VillageURL != "https://village.example/team" || claim.ClaimedAt == "" || claim.Source != string(store.ClaimSourceManual) {
			t.Fatalf("claim entry = %+v, want ids, URL, time, and manual source", claim)
		}
	}
}

func TestUserClaimRequiresSavedCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = openUserCommandStore(t, dir)
	stdout, stderr, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim"})
	if err == nil {
		t.Fatalf("user claim without credentials succeeded\n%s%s", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "no valid saved peasant online credential") {
		t.Fatalf("error = %v, want the sign-in remedy", err)
	}
}

func TestUserClaimDefaultThenAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "user-claim-session-a")
	storetest.SeedSession(t, db, "user-claim-session-b")
	saveUserCommandCredentials(t, dir)

	stdout, stderr, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--json"})
	if err != nil {
		t.Fatalf("user claim: %v\n%s%s", err, stdout, stderr)
	}
	var first struct {
		Claimed        int    `json:"claimed"`
		AlreadyClaimed int    `json:"alreadyClaimed"`
		ClaimID        string `json:"claimId"`
	}
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatalf("decode claim json: %v\n%s", err, stdout)
	}
	if first.Claimed != 2 || first.ClaimID == "" {
		t.Fatalf("first claim = %+v, want two claimed sessions with an entry", first)
	}
	if !strings.Contains(stderr, "Claiming unclaimed artifacts") {
		t.Fatalf("stderr = %q, want progress on stderr", stderr)
	}

	stdout, _, err = executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--json"})
	if err != nil {
		t.Fatalf("second user claim: %v\n%s", err, stdout)
	}
	var second struct {
		Claimed int    `json:"claimed"`
		ClaimID string `json:"claimId"`
	}
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatalf("decode second claim json: %v\n%s", err, stdout)
	}
	if second.Claimed != 0 || second.ClaimID != "" {
		t.Fatalf("second claim = %+v, want nothing to claim and no entry", second)
	}

	storetest.SeedSession(t, db, "user-claim-session-c")
	stdout, _, err = executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--all", "--json"})
	if err != nil {
		t.Fatalf("claim all: %v\n%s", err, stdout)
	}
	var all struct {
		All     bool   `json:"all"`
		Claimed int    `json:"claimed"`
		ClaimID string `json:"claimId"`
	}
	if err := json.Unmarshal([]byte(stdout), &all); err != nil {
		t.Fatalf("decode claim all json: %v\n%s", err, stdout)
	}
	if !all.All || all.Claimed != 3 || all.ClaimID == "" {
		t.Fatalf("claim all = %+v, want three reassigned sessions with an entry", all)
	}
	claims, err := db.OwnerClaims(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claim log entries = %d, want two (default then all)", len(claims))
	}
}

func TestUserCommandJSONWritesOneDocument(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = openUserCommandStore(t, dir)
	stdout, stderr, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show: %v\n%s%s", err, stdout, stderr)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(stdout))
	var document any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		t.Fatalf("stdout holds more than one JSON document:\n%s", stdout)
	}
}

func TestUserClaimJSONParity(t *testing.T) {
	t.Parallel()
	jsonDir := t.TempDir()
	textDir := t.TempDir()
	for _, dir := range []string{jsonDir, textDir} {
		db := openUserCommandStore(t, dir)
		storetest.SeedSession(t, db, "user-parity-session-a")
		storetest.SeedSession(t, db, "user-parity-session-b")
		saveUserCommandCredentials(t, dir)
	}

	jsonOut, _, err := executeWithDataDirStreams(t, BuildUserCommand(), jsonDir, []string{"claim", "--json"})
	if err != nil {
		t.Fatalf("user claim --json: %v\n%s", err, jsonOut)
	}
	var report struct {
		Status  string `json:"status"`
		Claimed int    `json:"claimed"`
		ClaimID string `json:"claimId"`
		URL     string `json:"villageUrl"`
		UserID  string `json:"userId"`
		User    string `json:"username"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("decode claim json: %v\n%s", err, jsonOut)
	}
	if report.Status != "claimed" || report.Claimed != 2 || report.ClaimID == "" || report.User != "alice" {
		t.Fatalf("claim json = %+v, want two claimed artifacts for alice", report)
	}
	if report.URL != "https://village.example/team" || report.UserID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("claim scope = %s %s, want the normalized URL and user id", report.URL, report.UserID)
	}

	textOut, _, err := executeWithDataDirStreams(t, BuildUserCommand(), textDir, []string{"claim"})
	if err != nil {
		t.Fatalf("user claim text: %v\n%s", err, textOut)
	}
	if !strings.Contains(textOut, "Claimed 2 local artifacts") || !strings.Contains(textOut, "alice") {
		t.Fatalf("claim text = %q, want the same count and username as the JSON facts", textOut)
	}
}

func TestUserClaimEmptyStoreSaysNothingToClaimYet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = openUserCommandStore(t, dir)
	saveUserCommandCredentials(t, dir)
	stdout, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim"})
	if err != nil {
		t.Fatalf("user claim: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "nothing to claim yet") {
		t.Fatalf("claim output = %q, want the empty-store wording", stdout)
	}
}

func TestUserClaimNormalizesVillageURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "user-normalize-session")
	creds := auth.Credentials{
		APIKey:     "fixture-api-key",
		KeyID:      "fixture-key-id",
		UserID:     "11111111-1111-4111-8111-111111111111",
		Username:   "alice",
		VillageURL: "HTTPS://Village.Example/team/",
	}
	if err := auth.SaveCredentialsFrom(&creds, dir); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--json"})
	if err != nil {
		t.Fatalf("user claim: %v\n%s", err, stdout)
	}
	var report struct {
		URL string `json:"villageUrl"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode claim json: %v\n%s", err, stdout)
	}
	if report.URL != "https://village.example/team" {
		t.Fatalf("claim URL = %q, want the normalized scope", report.URL)
	}
	show, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show: %v\n%s", err, show)
	}
	var shown struct {
		Claims []struct {
			URL string `json:"villageUrl"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(show), &shown); err != nil {
		t.Fatalf("decode show json: %v\n%s", err, show)
	}
	if len(shown.Claims) != 1 || shown.Claims[0].URL != report.URL {
		t.Fatalf("show claims = %+v, want the same normalized URL as claim output", shown.Claims)
	}
}
