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
	RequiredNames     []string                    `yaml:"requiredNames"`
	HomeRootIDs       []string                    `yaml:"homeRootIds"`
	HomeChildIDs      []string                    `yaml:"homeChildIds"`
	HomeParentID      string                      `yaml:"homeParentId"`
	HomeProjectHashes []string                    `yaml:"homeProjectHashes"`
	Sessions          []helperGroupListingSession `yaml:"sessions"`
	Cases             []libraryPolicyCase         `yaml:"cases"`
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
	if len(fixture.HomeRootIDs) == 0 || len(fixture.HomeChildIDs) == 0 || fixture.HomeParentID == "" || len(fixture.HomeProjectHashes) == 0 {
		t.Fatal("ingested library policy fixture must declare its Home root, child, parent and project-option identities")
	}
	seedIDs := make([]string, 0, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		seedIDs = append(seedIDs, session.ID)
		if session.ProjectHash == "" || session.Origin == "" ||
			session.Text == "" || !session.Pushable {
			t.Fatalf("ingested library seed %q must declare its project, origin, text and push eligibility", session.ID)
		}
	}
	if !equalIDSet(seedIDs, []string{libraryInsideSession, libraryOutsideSession, libraryAgentSession, libraryHelperSession}) {
		t.Fatalf("ingested library seed IDs = %v; must retain the person, agent and helper identities", seedIDs)
	}
	for _, fixtureCase := range fixture.Cases {
		if err := config.ValidateSelection(fixtureCase.Selection); err != nil {
			t.Fatalf("ingested library policy case %q must supply valid saved selection: %v", fixtureCase.Name, err)
		}
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
	libraryInsideSession  = "ses_liba"
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
	fixture := loadLibraryPolicyFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			db := storetest.Open(t)
			seedIngestedLibrary(t, db, fixture.Sessions)

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
			assertLibraryHomeAndParent(t, fixture, fixtureCase, base)
		})
	}
}

// seedIngestedLibrary writes the stored rows and the projects their selector
// rules name. The helper belongs to the person's inside session so group
// expansion has a real owner.
func seedIngestedLibrary(t *testing.T, db *store.Store, sessions []helperGroupListingSession) {
	t.Helper()
	for index, session := range sessions {
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

func assertLibraryHomeAndParent(t *testing.T, fixture libraryPolicyFixture, fixtureCase libraryPolicyCase, base string) {
	t.Helper()

	rootBody, rootStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home")
	if rootStatus != http.StatusOK {
		t.Fatalf("Home root page status = %d (body %s)", rootStatus, rootBody)
	}
	var roots schema.LocalSyncSessionPage
	if err := json.Unmarshal(rootBody, &roots); err != nil {
		t.Fatalf("decode Home root page: %v (body %s)", err, rootBody)
	}
	if err := roots.Validate(); err != nil {
		t.Fatalf("Home root page breaks the canonical wire contract: %v", err)
	}
	rootIDs := make([]string, 0, len(roots.Items))
	for _, item := range roots.Items {
		rootIDs = append(rootIDs, item.Sync.ID)
		assertHomePublicationMetadata(t, item, fixtureCase.Outside)
	}
	if !equalIDSequence(rootIDs, fixture.HomeRootIDs) {
		t.Fatalf("Home root IDs = %v, want exact roots in start-desc-id-asc order %v", rootIDs, fixture.HomeRootIDs)
	}

	childBody, childStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home&parentSessionId="+fixture.HomeParentID)
	if childStatus != http.StatusOK {
		t.Fatalf("Home child page status = %d (body %s)", childStatus, childBody)
	}
	var children schema.LocalSyncSessionPage
	if err := json.Unmarshal(childBody, &children); err != nil {
		t.Fatalf("decode Home child page: %v (body %s)", err, childBody)
	}
	if err := children.Validate(); err != nil {
		t.Fatalf("Home child page breaks the canonical wire contract: %v", err)
	}
	childIDs := make([]string, 0, len(children.Items))
	for _, item := range children.Items {
		childIDs = append(childIDs, item.Sync.ID)
		assertHomePublicationMetadata(t, item, fixtureCase.Outside)
	}
	if !equalIDSequence(childIDs, fixture.HomeChildIDs) {
		t.Fatalf("Home child IDs = %v, want exact children in start-desc-id-asc order %v", childIDs, fixture.HomeChildIDs)
	}
	combined := append(append([]string(nil), rootIDs...), childIDs...)
	allStored := []string{libraryInsideSession, libraryOutsideSession, libraryAgentSession, libraryHelperSession}
	if !equalIDSet(combined, allStored) {
		t.Fatalf("Home root+child traversal = %v, want every stored session %v", combined, allStored)
	}

	optionsBody, optionsStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=project-options")
	if optionsStatus != http.StatusOK {
		t.Fatalf("Home project-options status = %d (body %s)", optionsStatus, optionsBody)
	}
	var options schema.LocalSyncProjectOptions
	if err := json.Unmarshal(optionsBody, &options); err != nil {
		t.Fatalf("decode Home project options: %v (body %s)", err, optionsBody)
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("Home project options break the canonical wire contract: %v", err)
	}
	optionHashes := make([]string, 0, len(options.Projects))
	for _, option := range options.Projects {
		optionHashes = append(optionHashes, option.ProjectHash.String())
	}
	if !equalIDSequence(optionHashes, fixture.HomeProjectHashes) {
		t.Fatalf("Home project options = %v, want project-name then hash order %v", optionHashes, fixture.HomeProjectHashes)
	}

	firstBody, firstStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home&maxItems=1")
	if firstStatus != http.StatusOK {
		t.Fatalf("Home first page status = %d (body %s)", firstStatus, firstBody)
	}
	var first schema.LocalSyncSessionPage
	if err := json.Unmarshal(firstBody, &first); err != nil {
		t.Fatalf("decode Home first page: %v (body %s)", err, firstBody)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("Home first page breaks the canonical wire contract: %v", err)
	}
	if first.Next == "" {
		t.Fatal("Home first page has no continuation token")
	}
	if _, replayStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home&maxItems=1&pageToken="+string(first.Next)); replayStatus != http.StatusOK {
		t.Fatalf("Home continuation replay status = %d, want 200", replayStatus)
	}
	if _, filteredStatus := helperGroupGET(t, base+"/api/v1/sync/sessions?view=home&maxItems=1&pageToken="+string(first.Next)+"&projectHash="+libraryAlphaHash.String()); filteredStatus != http.StatusBadRequest {
		t.Fatalf("Home continuation under a changed project filter status = %d, want 400", filteredStatus)
	}
}

func assertHomePublicationMetadata(t *testing.T, item schema.LocalSyncSessionRow, outsideIDs []string) {
	t.Helper()
	if item.Publication.SessionID != item.Sync.ID {
		t.Fatalf("Home row %q carries publication for %q", item.Sync.ID, item.Publication.SessionID)
	}
	wantOutside := false
	for _, id := range outsideIDs {
		if id == item.Sync.ID {
			wantOutside = true
			break
		}
	}
	if item.Publication.OutsideSelection != wantOutside {
		t.Fatalf("Home row %q outsideSelection=%t, want %t", item.Sync.ID, item.Publication.OutsideSelection, wantOutside)
	}
	if wantOutside && item.Publication.AutoPublish {
		t.Fatalf("Home row %q reports autoPublish=true while outsideSelection=true", item.Sync.ID)
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

func equalIDSequence(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
