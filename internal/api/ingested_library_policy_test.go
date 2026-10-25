package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/ingested_library_policy.yaml
var ingestedLibraryPolicyYAML []byte

var libraryPolicyRequiredNames = []string{
	"mode_all",
	"selected_restrictive",
	"selected_empty",
	"selected_excluded",
	"selected_stale",
}

type libraryPolicyCase struct {
	Name      string                 `yaml:"name"`
	Selection config.SelectionConfig `yaml:"selection"`
	Outside   []string               `yaml:"outside_selection"`
}

type libraryPolicyFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []libraryPolicyCase `yaml:"cases"`
}

func loadLibraryPolicyFixture(t *testing.T) libraryPolicyFixture {
	t.Helper()
	var fixture libraryPolicyFixture
	if err := testutil.DecodeNamedFixtureYAML(ingestedLibraryPolicyYAML, &fixture); err != nil {
		t.Fatalf("decode ingested library policy fixture: %v", err)
	}
	if len(fixture.RequiredNames) != len(libraryPolicyRequiredNames) {
		t.Fatalf("ingested library policy fixture declares %d required names, want %d", len(fixture.RequiredNames), len(libraryPolicyRequiredNames))
	}
	for index, name := range libraryPolicyRequiredNames {
		if fixture.RequiredNames[index] != name {
			t.Fatalf("ingested library policy fixture requiredNames[%d] = %q, want %q", index, fixture.RequiredNames[index], name)
		}
	}
	if len(fixture.Cases) != len(libraryPolicyRequiredNames) {
		t.Fatalf("ingested library policy fixture has %d cases, want %d", len(fixture.Cases), len(libraryPolicyRequiredNames))
	}
	for _, fixtureCase := range fixture.Cases {
		if !fixtureCase.Selection.Mode.IsValid() {
			t.Fatalf("ingested library policy case %q has invalid selection mode %q", fixtureCase.Name, fixtureCase.Selection.Mode)
		}
	}
	return fixture
}

// The seeded identities. The saved selector varies per fixture case; these
// stored rows never do, so every browsing surface must return the same set in
// every case.
const (
	libraryInsideSession  = "lib-a"
	libraryOutsideSession = "lib-b"
	libraryAgentSession   = "lib-c"
	libraryHelperSession  = "lib-h"
)

var (
	libraryAlphaHash  = schema.ProjectHash(strings.Repeat("a", 64))
	libraryBetaHash   = schema.ProjectHash(strings.Repeat("b", 64))
	libraryGammaHash  = schema.ProjectHash(strings.Repeat("c", 64))
	libraryHelperHash = schema.ProjectHash(strings.Repeat("d", 64))
)

// TestIngestedLibraryBrowsingIgnoresSavedSelection is the policy regression
// guard for the ratified rule "ingestion selection only controls ingestion: the
// library shows everything already ingested". Every case seeds the same stored
// rows (two person-driven sessions, one agent-driven session, one helper) and
// applies a different saved selector: mode all, a restrictive project list, an
// empty selection, an exact exclusion, and a stale selection naming projects and
// sessions that no longer exist. Every browsing surface must return the same
// ingested library, the retained selection metadata must stay inactive/zero, and
// the publication metadata must still describe the ingest/push selector honestly
// without hiding the row.
func TestIngestedLibraryBrowsingIgnoresSavedSelection(t *testing.T) {
	for _, fixtureCase := range loadLibraryPolicyFixture(t).Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedIngestedLibrary(t, db)

			cfg := config.BaseConfig()
			cfg.Selection = fixtureCase.Selection
			policy := sessionvisibility.New(fixtureCase.Selection)
			provider := NewStoreDataProvider(db, policy)
			base := startHelperGroupServer(t, ServerConfig{
				Store:                      db,
				Config:                     cfg,
				Provider:                   provider,
				ConfigHome:                 t.TempDir(),
				RepositoryIdentityResolver: fixtureRepositoryResolver{},
			})

			assertLibraryProviderViews(t, provider)
			assertLibraryFlatSessions(t, base)
			assertLibraryGroupedSessions(t, base)
			assertLibraryGroupedSearch(t, base)
			assertLibraryGroupedProjectScope(t, base)
			assertLibraryHomeAndParent(t, fixtureCase, db, cfg, provider, base)
		})
	}
}

// seedIngestedLibrary writes the stored rows and the projects their selector
// rules name. The helper belongs to the person's inside session so group
// expansion has a real owner.
func seedIngestedLibrary(t *testing.T, db *store.Store) {
	t.Helper()
	seeded := []helperGroupListingSession{
		{ID: libraryInsideSession, ProjectHash: libraryAlphaHash.String(), Origin: "user", Pushable: true, Text: "libraryneedle"},
		{ID: libraryOutsideSession, ProjectHash: libraryBetaHash.String(), Origin: "user", Pushable: true, Text: "libraryneedle"},
		{ID: libraryAgentSession, ProjectHash: libraryGammaHash.String(), Origin: "agent", Pushable: true, Text: "libraryneedle"},
		{ID: libraryHelperSession, ProjectHash: libraryHelperHash.String(), Origin: "agent", Purpose: "helper_review", Owner: libraryInsideSession, OwnerState: "target_known", Pushable: true, Text: "libraryneedle"},
	}
	for index, session := range seeded {
		seedHelperGroupSession(t, db, session, int64(index+1))
	}
	homeExec(t, db, `UPDATE projects SET canonical_cwd = ?, canonical_remote = ? WHERE project_hash = ?`,
		"/library/alpha-project", "git@example.test:alpha/repo.git", libraryAlphaHash.String())
	homeExec(t, db, `UPDATE projects SET canonical_cwd = ?, canonical_remote = ? WHERE project_hash = ?`,
		"/library/beta-project", "git@example.test:beta/repo.git", libraryBetaHash.String())
	homeExec(t, db, `UPDATE projects SET canonical_cwd = ?, canonical_remote = ? WHERE project_hash = ?`,
		"/library/gamma-project", "git@example.test:gamma/repo.git", libraryGammaHash.String())
	homeExec(t, db, `UPDATE projects SET canonical_cwd = ?, canonical_remote = ? WHERE project_hash = ?`,
		"/library/helper-project", "git@example.test:helper/repo.git", libraryHelperHash.String())
}

func assertLibraryProviderViews(t *testing.T, provider *StoreDataProvider) {
	t.Helper()
	ctx := context.Background()

	sessions, err := provider.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got := sessionIDSet(sessions); !equalIDSet(got, []string{libraryInsideSession, libraryOutsideSession}) {
		t.Fatalf("Sessions = %v, want every person-driven ingested session", got)
	}

	summaries, err := provider.SessionSummaries(ctx)
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}
	if got := summaryIDSet(summaries); !equalIDSet(got, []string{libraryInsideSession, libraryOutsideSession}) {
		t.Fatalf("SessionSummaries = %v, want every indexed person-driven session", got)
	}

	dashboard, err := provider.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("DashboardMetrics: %v", err)
	}
	if dashboard.TotalSessions != 4 {
		t.Fatalf("DashboardMetrics.TotalSessions = %d, want all 4 stored sessions regardless of selection", dashboard.TotalSessions)
	}

	trends, err := provider.TrendsData(ctx)
	if err != nil {
		t.Fatalf("TrendsData: %v", err)
	}
	if trends.TotalSessions != 4 {
		t.Fatalf("TrendsData.TotalSessions = %d, want all 4 stored sessions", trends.TotalSessions)
	}

	quality, err := provider.QualitySessions(ctx, QualityFilter{})
	if err != nil {
		t.Fatalf("QualitySessions: %v", err)
	}
	if len(quality) != 4 {
		t.Fatalf("QualitySessions = %d rows, want all 4 stored sessions", len(quality))
	}

	projects, err := provider.ProjectSummaries(ctx)
	if err != nil {
		t.Fatalf("ProjectSummaries: %v", err)
	}
	if len(projects.Projects) != 4 {
		t.Fatalf("ProjectSummaries = %+v, want all 4 stored projects", projects.Projects)
	}
	totalSessions := 0
	for _, project := range projects.Projects {
		totalSessions += project.Sessions
	}
	if totalSessions != 4 {
		t.Fatalf("ProjectSummaries sessions = %d, want all 4 stored sessions", totalSessions)
	}
	if projects.Selection.Active || projects.Selection.HiddenProjects != 0 || projects.Selection.HiddenSessions != 0 {
		t.Fatalf("ProjectSummaries reported an active selection filter: %+v", projects.Selection)
	}
}

func assertLibraryFlatSessions(t *testing.T, base string) {
	t.Helper()
	body, status := helperGroupGET(t, base+"/api/v1/sessions")
	if status != http.StatusOK {
		t.Fatalf("flat sessions status = %d (body %s)", status, body)
	}
	var envelope struct {
		Sessions []SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode flat sessions: %v (body %s)", err, body)
	}
	got := make([]string, 0, len(envelope.Sessions))
	for _, session := range envelope.Sessions {
		got = append(got, session.ID)
	}
	if !equalIDSet(got, []string{libraryInsideSession, libraryOutsideSession}) {
		t.Fatalf("flat session list = %v, want both person-driven ingested sessions", got)
	}
}

func assertLibraryGroupedSessions(t *testing.T, base string) {
	t.Helper()
	body, status := helperGroupGET(t, base+"/api/v1/sessions?view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped sessions status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode grouped sessions: %v (body %s)", err, body)
	}
	if payload.OrdinarySessionTotal != 2 || payload.HelperThreadTotal != 1 {
		t.Fatalf("grouped totals = ordinary %d helper %d, want 2 and 1; payload %+v", payload.OrdinarySessionTotal, payload.HelperThreadTotal, payload)
	}
	ordinary := make([]string, 0, len(payload.Items))
	memberIDs := make([]string, 0, 1)
	for _, item := range payload.Items {
		if item.Transcript != nil {
			ordinary = append(ordinary, string(item.Transcript.Session.ID))
		}
		for _, group := range item.HelperGroups {
			members := helperGroupMembers(t, base, group.GroupID, group.MemberScope, 1, 50)
			memberIDs = append(memberIDs, helperGroupMemberIDs(members)...)
		}
	}
	if !equalIDSet(ordinary, []string{libraryInsideSession, libraryOutsideSession}) {
		t.Fatalf("grouped ordinary sessions = %v, want both person-driven ingested sessions", ordinary)
	}
	if !equalIDSet(memberIDs, []string{libraryHelperSession}) {
		t.Fatalf("grouped helper members = %v, want %q", memberIDs, libraryHelperSession)
	}
}

func assertLibraryGroupedSearch(t *testing.T, base string) {
	t.Helper()
	body, status := helperGroupGET(t, base+"/api/v1/search?q=libraryneedle&view=grouped")
	if status != http.StatusOK {
		t.Fatalf("grouped search status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode grouped search: %v (body %s)", err, body)
	}
	got := make([]string, 0, len(payload.Items))
	for _, item := range payload.Items {
		if item.Transcript != nil {
			got = append(got, string(item.Transcript.Session.ID))
		}
		for _, group := range item.HelperGroups {
			members := helperGroupMembers(t, base, group.GroupID, group.MemberScope, 1, 50)
			got = append(got, helperGroupMemberIDs(members)...)
		}
	}
	// Search keeps its own policy: every stored match is returned, including the
	// outside-selection row and the agent-driven row, with the helper as a member.
	want := []string{libraryInsideSession, libraryOutsideSession, libraryAgentSession, libraryHelperSession}
	if !equalIDSet(got, want) {
		t.Fatalf("grouped search sessions = %v, want every stored match %v", got, want)
	}
}

func assertLibraryGroupedProjectScope(t *testing.T, base string) {
	t.Helper()
	body, status := helperGroupGET(t, base+"/api/v1/sessions?view=grouped&project="+libraryBetaHash.String())
	if status != http.StatusOK {
		t.Fatalf("project-scoped grouped status = %d (body %s)", status, body)
	}
	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode project-scoped grouped list: %v (body %s)", err, body)
	}
	got := make([]string, 0, len(payload.Items))
	for _, item := range payload.Items {
		if item.Transcript != nil {
			got = append(got, string(item.Transcript.Session.ID))
		}
	}
	if !equalIDSet(got, []string{libraryOutsideSession}) {
		t.Fatalf("project-scoped grouped sessions = %v, want only the named project's ingested row %q", got, libraryOutsideSession)
	}
}

func assertLibraryHomeAndParent(t *testing.T, fixtureCase libraryPolicyCase, db *store.Store, cfg *config.Config, provider *StoreDataProvider, base string) {
	t.Helper()
	publishing := &publishingHandler{store: db, configHome: t.TempDir()}
	publishing.selection = provider
	h := &homeHandler{reader: db, publishing: publishing}
	homeQuery := schema.LocalSyncSessionsQuery{View: "home"}

	page, err := h.newReader(homeQuery).ReadPage(context.Background(), schema.PageRequest{MaxItems: 100})
	if err != nil {
		t.Fatalf("Home ReadPage: %v", err)
	}
	if len(page.Items) != 4 {
		t.Fatalf("Home items = %d, want all 4 pushable stored sessions", len(page.Items))
	}
	gotIDs := make([]string, 0, len(page.Items))
	outside := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		gotIDs = append(gotIDs, item.Sync.ID)
		outside[item.Sync.ID] = item.Publication.OutsideSelection
		if item.Publication.OutsideSelection && item.Publication.AutoPublish {
			t.Fatalf("Home publication metadata for %q reports autoPublish=true while outsideSelection=true; a saved selector never auto-publishes a row it does not admit", item.Sync.ID)
		}
	}
	wantHome := []string{libraryInsideSession, libraryOutsideSession, libraryAgentSession, libraryHelperSession}
	if !equalIDSet(gotIDs, wantHome) {
		t.Fatalf("Home sessions = %v, want every stored pushable session %v", gotIDs, wantHome)
	}
	for _, id := range fixtureCase.Outside {
		if !outside[id] {
			t.Fatalf("Home publication metadata for %q says insideSelection, want outsideSelection=true from the fixture's saved selector", id)
		}
	}
	for id, isOutside := range outside {
		if isOutside && !containsString(fixtureCase.Outside, id) {
			t.Fatalf("Home publication metadata for %q says outsideSelection=true, but the saved selector admits it", id)
		}
	}

	// A parent the saved selector excludes must still open. The page is empty,
	// so it also round-trips through the published validation on the mounted
	// route.
	body, status := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home&parentSessionId="+libraryOutsideSession)
	if status != http.StatusOK {
		t.Fatalf("Home parent %q status = %d, want 200 (body %s); an ingested parent's children must remain reachable", libraryOutsideSession, status, body)
	}
	parentQuery := schema.LocalSyncSessionsQuery{View: "home"}
	parentID := libraryOutsideSession
	parentQuery.ParentSessionID = &parentID
	if _, err := h.newReader(parentQuery).ReadPage(context.Background(), schema.PageRequest{MaxItems: 100}); err != nil {
		t.Fatalf("Home ReadPage for an outside-selection parent: %v", err)
	}

	options, err := h.newReader(schema.LocalSyncSessionsQuery{View: "project-options"}).projectOptions(context.Background())
	if err != nil {
		t.Fatalf("Home project options: %v", err)
	}
	optionHashes := make([]string, 0, len(options.Projects))
	for _, option := range options.Projects {
		optionHashes = append(optionHashes, option.ProjectHash.String())
	}
	if !equalIDSet(optionHashes, []string{libraryAlphaHash.String(), libraryBetaHash.String(), libraryGammaHash.String(), libraryHelperHash.String()}) {
		t.Fatalf("Home project options = %v, want every stored project", optionHashes)
	}

	// A continuation issued before the selection changed must keep working, and
	// a real filter change must still invalidate it.
	token := func() schema.PageToken {
		first, readErr := h.newReader(homeQuery).ReadPage(context.Background(), schema.PageRequest{MaxItems: 1})
		if readErr != nil {
			t.Fatalf("Home first page: %v", readErr)
		}
		if first.Next == "" {
			t.Fatal("Home first page has no continuation token")
		}
		return first.Next
	}()
	cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeAll}
	if _, err := h.newReader(homeQuery).boundary(token); err != nil {
		t.Fatalf("browsing continuation was invalidated by an ingest-selection change: %v", err)
	}
	filtered := schema.LocalSyncSessionsQuery{View: "home", ProjectHash: &libraryAlphaHash}
	if _, err := h.newReader(filtered).boundary(token); err == nil {
		t.Fatal("continuation survived a real project-filter change; explicit query bounds must still be enforced")
	}
}

func summaryIDSet(summaries []SessionSummary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}

func sessionIDSet(sessions []ingest.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID.String())
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	left := append([]string(nil), got...)
	right := append([]string(nil), want...)
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}
