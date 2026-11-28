package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/claimprompt"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
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

func runUserShowNoNetwork(t *testing.T) {
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
		Claims      []any   `json:"claims"`
		TotalClaims int     `json:"totalClaims"`
		NextBefore  *string `json:"nextBefore"`
		Prompt      struct {
			State   string `json:"state"`
			Account *struct {
				VillageURL string `json:"villageUrl"`
				UserID     string `json:"userId"`
			} `json:"account"`
		} `json:"prompt"`
		Ownership struct {
			Sessions struct {
				Total     int `json:"total"`
				Unclaimed int `json:"unclaimed"`
				Mine      int `json:"mine"`
				Others    int `json:"others"`
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
	if report.NextBefore != nil {
		t.Fatalf("nextBefore = %q, want none on a short page", *report.NextBefore)
	}
	if report.Prompt.State != string(userPromptStateNotApplicable) || report.Prompt.Account != nil {
		t.Fatalf("prompt = %+v, want not_applicable without an account", report.Prompt)
	}
	if report.Ownership.Sessions.Total != 2 || report.Ownership.Sessions.Unclaimed != 2 || report.Ownership.Sessions.Mine != 0 || report.Ownership.Sessions.Others != 0 {
		t.Fatalf("session counts = %+v, want 2 total, 2 unclaimed, and nothing owned", report.Ownership.Sessions)
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
	if report.Claims[0].UserID != "22222222-2222-4222-8222-222222222222" || report.Claims[0].Username != "bob" ||
		report.Claims[1].UserID != "11111111-1111-4111-8111-111111111111" || report.Claims[1].Username != "alice" {
		t.Fatalf("claim order = %+v, want newest first: bob then alice", report.Claims)
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
		Outcome struct {
			Kind string `json:"kind"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatalf("decode second claim json: %v\n%s", err, stdout)
	}
	if second.Claimed != 0 || second.ClaimID == "" || second.Outcome.Kind != string(userClaimOutcomeNothingToMove) {
		t.Fatalf("second claim = %+v, want nothing moved but a recorded entry", second)
	}

	storetest.SeedSession(t, db, "user-claim-session-c")
	stdout, _, err = executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--all", "--json"})
	if err != nil {
		t.Fatalf("claim all: %v\n%s", err, stdout)
	}
	var all struct {
		Claimed        int    `json:"claimed"`
		AlreadyClaimed int    `json:"alreadyClaimed"`
		ClaimID        string `json:"claimId"`
		Outcome        struct {
			All  bool   `json:"all"`
			Kind string `json:"kind"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(stdout), &all); err != nil {
		t.Fatalf("decode claim all json: %v\n%s", err, stdout)
	}
	if !all.Outcome.All || all.Claimed != 1 || all.AlreadyClaimed != 2 || all.ClaimID == "" || all.Outcome.Kind != string(userClaimOutcomeClaimed) {
		t.Fatalf("claim all = %+v, want one moved session, two already claimed, and an entry", all)
	}
	page, err := db.OwnerClaims(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Claims) != 3 {
		t.Fatalf("claim log entries = %d, want three (default, no-op decision, all)", len(page.Claims))
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

func runUserClaimJSONParity(t *testing.T) {
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
		Status struct {
			Owner struct {
				ID        string `json:"id"`
				CreatedAt string `json:"createdAt"`
			} `json:"owner"`
			Claims      []any `json:"claims"`
			TotalClaims int   `json:"totalClaims"`
			Prompt      struct {
				State     string  `json:"state"`
				DecidedAt *string `json:"decidedAt"`
				Account   *struct {
					VillageURL string `json:"villageUrl"`
					UserID     string `json:"userId"`
				} `json:"account"`
			} `json:"prompt"`
			Ownership struct {
				Sessions struct {
					Total     int `json:"total"`
					Unclaimed int `json:"unclaimed"`
					Mine      int `json:"mine"`
					Others    int `json:"others"`
				} `json:"sessions"`
				Annotations struct {
					Total     int `json:"total"`
					Unclaimed int `json:"unclaimed"`
					Mine      int `json:"mine"`
					Others    int `json:"others"`
				} `json:"annotations"`
			} `json:"ownership"`
		} `json:"status"`
		Claimed        int    `json:"claimed"`
		AlreadyClaimed int    `json:"alreadyClaimed"`
		ClaimID        string `json:"claimId"`
		Outcome        struct {
			Kind       string `json:"kind"`
			All        bool   `json:"all"`
			Stored     int    `json:"stored"`
			VillageURL string `json:"villageUrl"`
			UserID     string `json:"userId"`
			Username   string `json:"username"`
		} `json:"outcome"`
	}
	decoder := json.NewDecoder(strings.NewReader(jsonOut))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		t.Fatalf("decode claim json with unknown fields refused: %v\n%s", err, jsonOut)
	}
	if report.Outcome.Kind != string(userClaimOutcomeClaimed) || report.Claimed != 2 || report.ClaimID == "" || report.Outcome.Username != "alice" {
		t.Fatalf("claim json = %+v, want two claimed artifacts for alice", report)
	}
	if report.Outcome.VillageURL != "https://village.example/team" || report.Outcome.UserID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("claim scope = %s %s, want the normalized URL and user id", report.Outcome.VillageURL, report.Outcome.UserID)
	}
	if report.Status.Owner.ID == "" || report.Status.TotalClaims != 1 || len(report.Status.Claims) != 1 {
		t.Fatalf("nested status = %+v, want the owner and the appended claim", report.Status)
	}

	textOut, _, err := executeWithDataDirStreams(t, BuildUserCommand(), textDir, []string{"claim"})
	if err != nil {
		t.Fatalf("user claim text: %v\n%s", err, textOut)
	}
	for _, want := range []string{"Claimed 2 local artifacts", "alice", report.Outcome.VillageURL, report.Outcome.UserID, "Claim entry: oc_"} {
		if !strings.Contains(textOut, want) {
			t.Fatalf("claim text = %q, want the JSON fact %q", textOut, want)
		}
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
		Outcome struct {
			URL string `json:"villageUrl"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode claim json: %v\n%s", err, stdout)
	}
	if report.Outcome.URL != "https://village.example/team" {
		t.Fatalf("claim URL = %q, want the normalized scope", report.Outcome.URL)
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
	if len(shown.Claims) != 1 || shown.Claims[0].URL != report.Outcome.URL {
		t.Fatalf("show claims = %+v, want the same normalized URL as claim output", shown.Claims)
	}
}

type userArtifactOwnershipJSON struct {
	Total     int `json:"total"`
	Unclaimed int `json:"unclaimed"`
	Mine      int `json:"mine"`
	Others    int `json:"others"`
}

type userOwnershipJSON struct {
	Sessions    userArtifactOwnershipJSON `json:"sessions"`
	Annotations userArtifactOwnershipJSON `json:"annotations"`
}

func decodeUserOwnership(t *testing.T, document string) userOwnershipJSON {
	t.Helper()
	var report struct {
		Ownership userOwnershipJSON `json:"ownership"`
	}
	if err := json.Unmarshal([]byte(document), &report); err != nil {
		t.Fatalf("decode user ownership json: %v\n%s", err, document)
	}
	return report.Ownership
}

// runUserOwnershipParity proves the JSON and text surfaces report the same
// three ownership partitions for two accounts and while signed out.
func runUserOwnershipParity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	for _, id := range []string{"user-ownership-a", "user-ownership-b", "user-ownership-c"} {
		storetest.SeedSession(t, db, id)
	}
	alice := saveUserCommandCredentials(t, dir)
	aliceSnapshot, err := auth.CaptureCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimSessionIDs(t.Context(), aliceSnapshot, []string{"user-ownership-a"}, store.ClaimSourceAutomatic, 1700000001000); err != nil {
		t.Fatal(err)
	}
	bob := alice
	bob.UserID = "22222222-2222-4222-8222-222222222222"
	bob.Username = "bob"
	if err := auth.SaveCredentialsFrom(&bob, dir); err != nil {
		t.Fatal(err)
	}
	bobSnapshot, err := auth.CaptureCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimSessionIDs(t.Context(), bobSnapshot, []string{"user-ownership-b"}, store.ClaimSourceAutomatic, 1700000002000); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveCredentialsFrom(&alice, dir); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show --json: %v\n%s", err, stdout)
	}
	if got, want := decodeUserOwnership(t, stdout), (userOwnershipJSON{
		Sessions: userArtifactOwnershipJSON{Total: 3, Unclaimed: 1, Mine: 1, Others: 1},
	}); got != want {
		t.Fatalf("ownership json = %+v, want %+v", got, want)
	}

	text, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show"})
	if err != nil {
		t.Fatalf("user show: %v\n%s", err, text)
	}
	for _, want := range []string{"3 total, 1 unclaimed, 1 mine, 1 other accounts", "0 total, 0 unclaimed, 0 mine, 0 other accounts"} {
		if !strings.Contains(text, want) {
			t.Fatalf("user show text = %q, want %q", text, want)
		}
	}

	if err := auth.ClearCredentialsFrom(dir); err != nil {
		t.Fatal(err)
	}
	signedOut, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("signed-out user show --json: %v\n%s", err, signedOut)
	}
	got := decodeUserOwnership(t, signedOut)
	if got.Sessions.Total != 3 || got.Sessions.Unclaimed != 1 || got.Sessions.Mine != 0 || got.Sessions.Others != 2 {
		t.Fatalf("signed-out ownership = %+v, want 3 total, 1 unclaimed, 0 mine, 2 others", got.Sessions)
	}
}

// runClaimCommandCompletesPrompt proves a successful explicit claim settles the
// prompt for the local owner plus account, so user show reports completed and a
// later sign-in does not ask again.
func runClaimCommandCompletesPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openUserCommandStore(t, dir)
	storetest.SeedSession(t, db, "user-claim-completes-session")
	creds := saveUserCommandCredentials(t, dir)
	if _, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"claim", "--json"}); err != nil {
		t.Fatalf("user claim: %v", err)
	}
	owner, err := db.LocalOwner(t.Context())
	if err != nil || owner == nil {
		t.Fatalf("read local owner = %+v, %v", owner, err)
	}
	show, _, err := executeWithDataDirStreams(t, BuildUserCommand(), dir, []string{"show", "--json"})
	if err != nil {
		t.Fatalf("user show --json: %v\n%s", err, show)
	}
	var report struct {
		Prompt struct {
			State string `json:"state"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(show), &report); err != nil {
		t.Fatalf("decode user show json: %v\n%s", err, show)
	}
	if report.Prompt.State != string(userPromptStateCompleted) {
		t.Fatalf("prompt state after an explicit claim = %q, want completed", report.Prompt.State)
	}

	statePath := claimprompt.DefaultPath(defaults.ResolveStateDirPathWith(dir).String())
	state, err := claimprompt.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := auth.NormalizeVillageURL(creds.VillageURL)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Decided(owner.ID, base, creds.UserID) {
		t.Fatal("the claim did not record a prompt decision for the local owner and account")
	}
	var out bytes.Buffer
	claimed := false
	if err := promptSignInClaim(context.Background(), &creds, signInClaimDeps{
		in:           strings.NewReader(""),
		out:          &out,
		interactive:  true,
		statePath:    statePath,
		localOwnerID: owner.ID,
		now:          func() int64 { return 1700000003000 },
		capture: func(context.Context) (auth.CredentialSnapshot, error) {
			return auth.CaptureCredentials(t.Context(), dir)
		},
		claim: func(context.Context, auth.CredentialSnapshot, bool) (store.ClaimResult, error) {
			claimed = true
			return store.ClaimResult{}, nil
		},
	}); err != nil {
		t.Fatalf("later sign-in prompt: %v", err)
	}
	if claimed || out.Len() != 0 {
		t.Fatalf("a later sign-in prompted again (claimed=%v, output=%q)", claimed, out.String())
	}
}

//go:embed testdata/user_command_cases.yaml
var userCommandCasesYAML []byte

type userCommandFixture struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []userCommandCase `yaml:"cases"`
}

type userCommandCase struct {
	Name string `yaml:"name"`
}

// TestUserCommandNamedCases keeps the required-name manifest over the user
// command cases and dispatches each named case to its runner.
func TestUserCommandNamedCases(t *testing.T) {
	t.Parallel()
	var fixture userCommandFixture
	decoder := yaml.NewDecoder(bytes.NewReader(userCommandCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode user_command_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: fixture.RequiredNames}, names, "user_command_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	runners := map[string]func(*testing.T){
		"user-show-no-network":           runUserShowNoNetwork,
		"user-claim-json-parity":         runUserClaimJSONParity,
		"user-ownership-parity":          runUserOwnershipParity,
		"claim-command-completes-prompt": runClaimCommandCompletesPrompt,
	}
	for _, testCase := range fixture.Cases {
		run, ok := runners[testCase.Name]
		if !ok {
			t.Fatalf("user_command_cases.yaml case %q has no runner", testCase.Name)
		}
		t.Run(testCase.Name, run)
	}
}
