package api

import (
	_ "embed"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/sync_push_typed_request.yaml
var syncPushTypedRequestYAML []byte

// TestSyncPushRefusesARequestOutsideTheTypedContract runs
// testdata/sync_push_typed_request.yaml: a body the typed push contract does not
// declare is refused before the credential is read, so nothing is published.
func TestSyncPushRefusesARequestOutsideTheTypedContract(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name          string `yaml:"name"`
			Body          string `yaml:"body"`
			ErrorContains string `yaml:"errorContains"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(syncPushTypedRequestYAML, &fixture); err != nil {
		t.Fatalf("testdata/sync_push_typed_request.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if strings.TrimSpace(c.Body) == "" || strings.TrimSpace(c.ErrorContains) == "" {
				t.Fatal("a case needs a body and the reason it is refused")
			}
			// An empty config home holds no credential: a request that got past
			// validation would answer 401, not the refusal asserted here.
			handler := newTestXDGHomes(t).handler(new(store.Store), config.BaseConfig())
			response := httptest.NewRecorder()
			handler.handleSyncPush(response, httptest.NewRequest(http.MethodPost, defaults.RouteSyncPush.String(), strings.NewReader(c.Body)))
			refusal := decodeRefusal(t, response.Code, response.Body.Bytes(), http.StatusBadRequest, syncPushInvalidRequestCode)
			if !strings.Contains(refusal.Error, c.ErrorContains) || !strings.Contains(refusal.Error, "nothing was scanned, published, shared, or taken back") {
				t.Errorf("reason %q does not say %q and that nothing ran", refusal.Error, c.ErrorContains)
			}
		})
	}
}

//go:embed testdata/village_collectives.yaml
var villageCollectivesYAML []byte

// TestSuggestCollectiveComparesRemoteLabels runs testdata/village_collectives.yaml
// through the suggestion the collectives route computes, from the label the
// route computes with schema.RemoteLabel.
func TestSuggestCollectiveComparesRemoteLabels(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string   `yaml:"name"`
			Remote       string   `yaml:"remote"`
			Organization string   `yaml:"organization"`
			Repositories []string `yaml:"repositories"`
			Expect       struct {
				Reason schema.LocalCollectiveSuggestionReason `yaml:"reason"`
				Match  string                                 `yaml:"match"`
			} `yaml:"expect"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(villageCollectivesYAML, &fixture); err != nil {
		t.Fatalf("testdata/village_collectives.yaml: %v", err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			label, _ := schema.RemoteLabel(c.Remote)
			group := schema.VillageUserGroup{ID: "11111111-1111-4111-8111-111111111111"}
			if c.Organization != "" {
				organization := c.Organization
				group.LinkedGithubOrg = &organization
			}
			repositories := make([]schema.VillageLinkedRepository, 0, len(c.Repositories))
			for _, fullName := range c.Repositories {
				owner, name, _ := strings.Cut(fullName, "/")
				repositories = append(repositories, schema.VillageLinkedRepository{Owner: owner, Name: name})
			}
			got := suggestCollective(label, group, repositories)
			if c.Expect.Reason == "" {
				if got != nil {
					t.Fatalf("suggestion = %+v, want none", *got)
				}
				return
			}
			if got == nil || got.Reason != c.Expect.Reason || got.Match != c.Expect.Match {
				t.Fatalf("suggestion = %+v, want %+v", got, c.Expect)
			}
		})
	}
}

// TestVillageCollectivesRouteSuggestsForTheSession reads the collectives through
// the mounted route: the collectives the user belongs to, each suggested for
// the session's repository or organization, and the refusals a signed-out
// computer and an unreachable Village get.
func TestVillageCollectivesRouteSuggestsForTheSession(t *testing.T) {
	t.Parallel()
	world := newPublishingWorld(t)
	read := func(sessionID string) map[string]*schema.LocalCollectiveSuggestion {
		t.Helper()
		status, body := world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String()+"?sessionId="+sessionID, nil)
		var response schema.LocalVillageCollectivesResponse
		decodeContract(t, status, body, &response)
		suggestions := map[string]*schema.LocalCollectiveSuggestion{}
		for _, collective := range response.Collectives {
			suggestions[collective.Group.Name] = collective.Suggestion
		}
		return suggestions
	}

	inside := read(insideSessionID)
	if len(inside) != 3 {
		t.Fatalf("collectives = %v; want the three the user belongs to, and not the one they are not a member of", inside)
	}
	if s := inside["platform"]; s == nil || s.Reason != schema.LocalCollectiveSuggestionLinkedRepository || s.Match != "github.com:acme/tools" {
		t.Errorf("platform suggestion = %+v; it links the session's repository", s)
	}
	if s := inside["research"]; s == nil || s.Reason != schema.LocalCollectiveSuggestionLinkedGithubOrg || s.Match != acmeOrg {
		t.Errorf("research suggestion = %+v; it links the session's organization", s)
	}
	if s := inside["review"]; s != nil {
		t.Errorf("review suggestion = %+v; it links nothing of the session", s)
	}
	for name, s := range read(outsideSessionID) {
		if s != nil {
			t.Errorf("%s is suggested for a session of another repository: %+v", name, s)
		}
	}

	// A collective whose repositories cannot be read gets no suggestion, and
	// the list is still served; a refused credential fails it.
	world.village.AnswerRepositories(http.StatusInternalServerError)
	for name, s := range read(insideSessionID) {
		if s != nil && s.Reason == schema.LocalCollectiveSuggestionLinkedRepository {
			t.Errorf("%s is suggested by a repository Village could not list: %+v", name, s)
		}
	}
	world.village.AnswerRepositories(http.StatusUnauthorized)
	status, body := world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String()+"?sessionId="+insideSessionID, nil)
	decodeRefusal(t, status, body, http.StatusUnauthorized, villageSignedOutCode)
	world.village.AnswerRepositories(0)

	world.village.AnswerCollectives(http.StatusUnauthorized)
	status, body = world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String(), nil)
	decodeRefusal(t, status, body, http.StatusUnauthorized, villageSignedOutCode)
	world.village.AnswerCollectives(0)

	world.village.Close()
	status, body = world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String(), nil)
	decodeRefusal(t, status, body, http.StatusBadGateway, villageUnreachableCode)

	if err := os.Remove(filepath.Join(string(defaults.ResolveConfigDirPathWith(world.hs.Config)), string(defaults.CredentialsFile))); err != nil {
		t.Fatal(err)
	}
	status, body = world.request(t, http.MethodGet, defaults.RouteVillageCollectives.String(), nil)
	decodeRefusal(t, status, body, http.StatusUnauthorized, villageSignedOutCode)
}

// TestLocalPublishingRoutesAnswerWithTheSchemaTypes walks the publishing routes
// the local web calls, signed in and then signed out, and decodes every answer
// with the schema module's types, refusing unknown fields.
func TestLocalPublishingRoutesAnswerWithTheSchemaTypes(t *testing.T) {
	t.Parallel()
	world := newPublishingWorld(t)

	var auth schema.SyncAuthResponse
	world.decode(t, http.MethodGet, defaults.RouteSyncAuth.String(), &auth)
	if !auth.Authenticated || auth.Username != "tester" || auth.VillageURL != world.village.URL() {
		t.Fatalf("signed-in auth = %+v", auth)
	}
	var login schema.SyncLoginResponse
	world.decode(t, http.MethodPost, defaults.RouteSyncLogin.String(), &login)
	if login.Status != schema.SyncLoginAlreadyAuthenticated {
		t.Fatalf("login = %+v; a signed-in computer starts no sign-in", login)
	}
	var sessions schema.LocalSyncSessionsPayload
	world.decode(t, http.MethodGet, defaults.RouteSyncSessions.String(), &sessions)
	var redactions schema.SyncRedactionsResponse
	world.decode(t, http.MethodGet, defaults.RouteSyncRedactions.String()+"?session_id="+insideSessionID, &redactions)

	pushed := world.publish(t, insideSessionID, "platform", "review")
	steps := pushed.Sessions[0].Steps
	if len(steps) != 3 || steps[1].Outcome != schema.SyncPushStepSucceeded || steps[2].Outcome != schema.SyncPushStepPendingApproval || pushed.Sessions[0].TranscriptURL == "" {
		t.Fatalf("typed push through the mounted route = %+v", pushed)
	}
	for _, row := range sessions.Sessions {
		if row.ID == insideSessionID && (row.SyncStatus != schema.SyncStatusNew || row.PreviouslyPushed) {
			t.Fatalf("sync row before the publish = %+v", row)
		}
	}

	// Taking a collective back goes through the same route and revokes its
	// members' access on Village.
	status, body := world.request(t, http.MethodPost, defaults.RouteSyncPush.String(), schema.SyncPushRequest{
		SessionIDs:  []string{insideSessionID},
		Collectives: &schema.SyncPushCollectives{Remove: []schema.VillageUUID{publishingCollectives["platform"].ID}},
	})
	var removed schema.SyncPushResponse
	decodeContract(t, status, body, &removed)
	removal := removed.Sessions[0].Steps
	if removed.Skipped != 1 || len(removal) != 2 || removal[1].Step != schema.SyncPushStepRemoveCollective || removal[1].Outcome != schema.SyncPushStepSucceeded || *removal[1].CollectiveID != publishingCollectives["platform"].ID {
		t.Fatalf("removal through the mounted route = %s", body)
	}
	audience := world.village.Audience(schema.TranscriptID(insideSessionID))
	if _, stillReads := audience[publishingCollectives["platform"].ID]; stillReads || audience[publishingCollectives["review"].ID] != schema.VillageShareStatusPending {
		t.Fatalf("Village audience after the removal = %v; platform must be gone and review still pending", audience)
	}
	var published schema.LocalSyncSessionsPayload
	world.decode(t, http.MethodGet, defaults.RouteSyncSessions.String(), &published)
	for _, row := range published.Sessions {
		if row.ID == insideSessionID && (row.SyncStatus == schema.SyncStatusNew || !row.PreviouslyPushed) {
			t.Fatalf("sync row after the publish = %+v; it was published", row)
		}
	}

	var publications schema.LocalPublicationsResponse
	world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?include=audience&sessionIds="+insideSessionID, &publications)
	var collectives schema.LocalVillageCollectivesResponse
	world.decode(t, http.MethodGet, defaults.RouteVillageCollectives.String()+"?sessionId="+insideSessionID, &collectives)

	var logout schema.SyncLogoutResponse
	world.decode(t, http.MethodPost, defaults.RouteSyncLogout.String(), &logout)
	if logout.Status != schema.SyncLogoutLoggedOut {
		t.Fatalf("logout = %+v; the computer held a credential", logout)
	}
	if _, err := os.Stat(filepath.Join(string(defaults.ResolveConfigDirPathWith(world.hs.Config)), string(defaults.CredentialsFile))); !os.IsNotExist(err) {
		t.Fatalf("the credential file is still present after logout: %v", err)
	}
	world.decode(t, http.MethodPost, defaults.RouteSyncLogout.String(), &logout)
	if logout.Status != schema.SyncLogoutAlreadyLoggedOut {
		t.Fatalf("second logout = %+v; nothing was left to remove", logout)
	}
	auth = schema.SyncAuthResponse{}
	world.decode(t, http.MethodGet, defaults.RouteSyncAuth.String(), &auth)
	if auth.Authenticated {
		t.Fatalf("auth after logout = %+v", auth)
	}
	var signedOut schema.LocalPublicationsResponse
	world.decode(t, http.MethodGet, defaults.RoutePublications.String()+"?sessionIds="+insideSessionID, &signedOut)
	if signedOut.Publications[0].State != schema.LocalPublicationUnpublished {
		t.Fatalf("a signed-out computer reports %+v; it reports every session unpublished", signedOut.Publications[0])
	}
}

// TestCollectiveIDPatternIsTheContractPattern keeps the collective identifier
// check on the typed push equal to the pattern the contract declares, so a
// change to the contract cannot leave this server refusing valid collectives.
func TestCollectiveIDPatternIsTheContractPattern(t *testing.T) {
	t.Parallel()
	declared, err := schema.VillageUUID("").JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	if declared.Pattern == nil || *declared.Pattern != villageUUIDPattern.String() {
		t.Fatalf("the typed push checks collectives against %q, but the contract declares %v", villageUUIDPattern.String(), declared.Pattern)
	}
}
