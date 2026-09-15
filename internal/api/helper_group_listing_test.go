package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/helper_group_listing.yaml
var helperGroupListingYAML []byte

const helperGroupListingFixturePath = "internal/api/testdata/helper_group_listing.yaml"

var helperGroupFixtureProjectHash = schema.ProjectHash(strings.Repeat("a", 64))

// helperGroupFixtureSiblingProjectHash is a second stored project. A
// project-scoped grouped case uses it to prove an in-project request never sees
// a sibling project's ordinary session or saved helper.
var helperGroupFixtureSiblingProjectHash = schema.ProjectHash(strings.Repeat("b", 64))

type helperGroupListingFixture struct {
	RequiredNames []string                                                             `yaml:"requiredNames"`
	NotApplicable []helperGroupListingNotApplicableCase                                `yaml:"notApplicableInThisRouteSet"`
	Cases         []testcase.Case[helperGroupListingInput, helperGroupListingExpected] `yaml:"cases"`
}

type helperGroupListingNotApplicableCase struct {
	Name   string `yaml:"name"`
	Reason string `yaml:"reason"`
}

type helperGroupListingInput struct {
	Route       string                      `yaml:"route"`
	SearchQuery string                      `yaml:"searchQuery"`
	SearchLimit int                         `yaml:"searchLimit"`
	Project     string                      `yaml:"project"`
	Selection   string                      `yaml:"selection"`
	SelectedIDs []string                    `yaml:"selectedIds"`
	Sessions    []helperGroupListingSession `yaml:"sessions"`
}

type helperGroupListingSession struct {
	ID          string `yaml:"id"`
	StartMs     int64  `yaml:"startMs"`
	Purpose     string `yaml:"purpose"`
	Origin      string `yaml:"origin"`
	Owner       string `yaml:"owner"`
	OwnerState  string `yaml:"ownerState"`
	Text        string `yaml:"text"`
	ProjectHash string `yaml:"projectHash"`
}

type helperGroupListingExpected struct {
	StatusCode           int                              `yaml:"statusCode"`
	ErrorCode            string                           `yaml:"errorCode"`
	BadScope             bool                             `yaml:"badScope"`
	ReListIdentical      bool                             `yaml:"reListIdentical"`
	DistinctGroupIDs     bool                             `yaml:"distinctGroupIDs"`
	Page                 int                              `yaml:"page"`
	Limit                int                              `yaml:"limit"`
	TotalItems           int                              `yaml:"totalItems"`
	OrdinarySessionTotal int                              `yaml:"ordinarySessionTotal"`
	HelperThreadTotal    int                              `yaml:"helperThreadTotal"`
	Items                []helperGroupListingItem         `yaml:"items"`
	TopLevelPages        []helperGroupListingTopLevelPage `yaml:"topLevelPages"`
	MemberPages          []helperGroupListingMemberPages  `yaml:"memberPages"`
	AfterList            *helperGroupListingAfterList     `yaml:"afterList"`
}

type helperGroupListingItem struct {
	Kind               string                          `yaml:"kind"`
	SessionID          string                          `yaml:"sessionId"`
	OwnerStatus        string                          `yaml:"ownerStatus"`
	GroupCount         int                             `yaml:"groupCount"`
	HelperThreadCount  int                             `yaml:"helperThreadCount"`
	MemberIDs          []string                        `yaml:"memberIds"`
	NestedMemberGroups []helperGroupListingNestedGroup `yaml:"nestedMemberGroups"`
}

// helperGroupListingNestedGroup asserts a helper that itself owns saved helpers:
// its immediate group must render under the member row, with the same exact
// count and member page the top-level group gets.
type helperGroupListingNestedGroup struct {
	SessionID         string   `yaml:"sessionId"`
	GroupCount        int      `yaml:"groupCount"`
	HelperThreadCount int      `yaml:"helperThreadCount"`
	MemberIDs         []string `yaml:"memberIds"`
}

type helperGroupListingTopLevelPage struct {
	Page       int      `yaml:"page"`
	Limit      int      `yaml:"limit"`
	SessionIDs []string `yaml:"sessionIds"`
}

type helperGroupListingMemberPages struct {
	ItemIndex int                            `yaml:"itemIndex"`
	Pages     []helperGroupListingMemberPage `yaml:"pages"`
}

type helperGroupListingMemberPage struct {
	Page      int      `yaml:"page"`
	Limit     int      `yaml:"limit"`
	Total     int      `yaml:"total"`
	MemberIDs []string `yaml:"memberIds"`
}

type helperGroupListingAfterList struct {
	Purposes map[string]string `yaml:"purposes"`
}

// groupedScopeIssuerFunc adapts a function to the grouping issuer seam so a
// test can assert the core pager without minting real opaque scopes.
type groupedScopeIssuerFunc func(groupID string, variant GroupedRouteVariant, filters GroupedFilters) (string, error)

func (fn groupedScopeIssuerFunc) issueMemberScope(groupID string, variant GroupedRouteVariant, filters GroupedFilters) (string, error) {
	return fn(groupID, variant, filters)
}

func loadHelperGroupListingFixture(t *testing.T) helperGroupListingFixture {
	t.Helper()
	var fixture helperGroupListingFixture
	decoder := yaml.NewDecoder(bytes.NewReader(helperGroupListingYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", helperGroupListingFixturePath, err)
	}
	if err := (testcase.Corpus[helperGroupListingInput, helperGroupListingExpected]{Cases: fixture.Cases}).Validate(); err != nil {
		t.Fatalf("validate %s corpus: %v", helperGroupListingFixturePath, err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames(helperGroupListingFixturePath, "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	if len(fixture.NotApplicable) == 0 {
		t.Fatalf("%s must record why a required case in the shared corpus is not applicable to this route set", helperGroupListingFixturePath)
	}
	return fixture
}

// TestHelperGroupListingThroughRegisteredRoutes drives every local corpus case
// through the real registered routes: the opt-in grouped list/search view and
// the scoped member operation. Expectations include each rendered group's
// member expansion, so counts, order and scope replay are asserted together.
func TestHelperGroupListingThroughRegisteredRoutes(t *testing.T) {
	fixture := loadHelperGroupListingFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			runHelperGroupListingCase(t, tc)
		})
	}
}

func runHelperGroupListingCase(t *testing.T, tc testcase.Case[helperGroupListingInput, helperGroupListingExpected]) {
	t.Helper()
	db := storetest.Open(t)
	for i, session := range tc.Input.Sessions {
		seedHelperGroupSession(t, db, session, int64(i))
	}
	selection := helperGroupSelection(tc.Input)
	policy, err := sessionvisibility.New(selection)
	if err != nil {
		t.Fatalf("selection policy: %v", err)
	}
	provider := NewStoreDataProvider(db, policy)
	base := startHelperGroupServer(t, ServerConfig{
		Port:     0,
		Store:    db,
		Config:   &config.Config{Selection: selection},
		Provider: provider,
	})

	if tc.Expected.BadScope {
		membersURL := base + "/api/v1/session-groups/hg_bogus/members?scope=deadbeef"
		body, status := helperGroupGET(t, membersURL)
		if status != tc.Expected.StatusCode {
			t.Fatalf("member status = %d, want %d (body %s)", status, tc.Expected.StatusCode, body)
		}
		if code := helperGroupErrorCode(body); code != tc.Expected.ErrorCode {
			t.Fatalf("member error code = %q, want %q (body %s)", code, tc.Expected.ErrorCode, body)
		}
		return
	}

	listURL := helperGroupListURL(t, base, tc.Input)
	firstBody, firstStatus := helperGroupGET(t, listURL)
	if firstStatus != tc.Expected.StatusCode {
		t.Fatalf("list status = %d, want %d (body %s)", firstStatus, tc.Expected.StatusCode, firstBody)
	}
	if code := helperGroupErrorCode(firstBody); code != tc.Expected.ErrorCode {
		t.Fatalf("list error code = %q, want %q (body %s)", code, tc.Expected.ErrorCode, firstBody)
	}
	if firstStatus != http.StatusOK {
		return
	}

	var payload schema.LocalSessionListPayload
	if err := json.Unmarshal(firstBody, &payload); err != nil {
		t.Fatalf("decode grouped list: %v (body %s)", err, firstBody)
	}
	if tc.Expected.Page > 0 {
		if payload.Page != tc.Expected.Page || payload.Limit != tc.Expected.Limit {
			t.Fatalf("list page/limit = (%d,%d), want (%d,%d)", payload.Page, payload.Limit, tc.Expected.Page, tc.Expected.Limit)
		}
	}
	if payload.TotalItems != tc.Expected.TotalItems || payload.OrdinarySessionTotal != tc.Expected.OrdinarySessionTotal || payload.HelperThreadTotal != tc.Expected.HelperThreadTotal {
		t.Fatalf("list totals = (total %d, ordinary %d, helper %d), want (%d,%d,%d)",
			payload.TotalItems, payload.OrdinarySessionTotal, payload.HelperThreadTotal,
			tc.Expected.TotalItems, tc.Expected.OrdinarySessionTotal, tc.Expected.HelperThreadTotal)
	}
	if len(payload.Items) != len(tc.Expected.Items) {
		t.Fatalf("list items = %d, want %d (%+v)", len(payload.Items), len(tc.Expected.Items), payload.Items)
	}

	containerGroupIDs := make([]string, 0, len(payload.Items))
	for i, expected := range tc.Expected.Items {
		item := payload.Items[i]
		switch expected.Kind {
		case string(schema.SessionListItemTranscript):
			if item.Kind != schema.SessionListItemTranscript || item.Transcript == nil {
				t.Fatalf("item[%d] kind = %q transcript nil=%v, want transcript", i, item.Kind, item.Transcript == nil)
			}
			if item.Transcript.Session.ID != expected.SessionID {
				t.Fatalf("item[%d] session = %q, want %q", i, item.Transcript.Session.ID, expected.SessionID)
			}
		case string(schema.SessionListItemContextContainer):
			if item.Kind != schema.SessionListItemContextContainer || item.Context == nil {
				t.Fatalf("item[%d] kind = %q context nil=%v, want context_container", i, item.Kind, item.Context == nil)
			}
			if string(item.Context.OwnerStatus) != expected.OwnerStatus {
				t.Fatalf("item[%d] ownerStatus = %q, want %q", i, item.Context.OwnerStatus, expected.OwnerStatus)
			}
			containerGroupIDs = append(containerGroupIDs, item.Context.GroupID)
		default:
			t.Fatalf("case names unknown expected kind %q", expected.Kind)
		}
		if len(item.HelperGroups) != expected.GroupCount {
			t.Fatalf("item[%d] helper groups = %d, want %d", i, len(item.HelperGroups), expected.GroupCount)
		}
		if expected.GroupCount > 0 {
			group := item.HelperGroups[0]
			if group.HelperThreadCount != expected.HelperThreadCount {
				t.Fatalf("item[%d] group count = %d, want %d", i, group.HelperThreadCount, expected.HelperThreadCount)
			}
			members := helperGroupMembers(t, base, group.GroupID, group.MemberScope, 1, 50)
			if members.Total != expected.HelperThreadCount {
				t.Fatalf("item[%d] member total = %d, want %d", i, members.Total, expected.HelperThreadCount)
			}
			if got := helperGroupMemberIDs(members); !reflect.DeepEqual(got, expected.MemberIDs) {
				t.Fatalf("item[%d] members = %v, want %v", i, got, expected.MemberIDs)
			}
			assertNestedMemberGroups(t, base, i, members, expected.NestedMemberGroups)
		}
		if expected.GroupCount == 0 && len(expected.NestedMemberGroups) > 0 {
			t.Fatalf("item[%d] declares nested member groups but renders no group to expand", i)
		}
	}
	if tc.Expected.DistinctGroupIDs {
		if len(containerGroupIDs) < 2 {
			t.Fatalf("distinct group identity check needs at least two containers, got %v", containerGroupIDs)
		}
		seen := map[string]bool{}
		for _, id := range containerGroupIDs {
			if seen[id] {
				t.Fatalf("two independent containers share group ID %q; independent helpers coalesced", id)
			}
			seen[id] = true
		}
	}

	if tc.Expected.ReListIdentical {
		secondBody, secondStatus := helperGroupGET(t, listURL)
		if secondStatus != http.StatusOK {
			t.Fatalf("repeat list status = %d, want 200", secondStatus)
		}
		var repeated schema.LocalSessionListPayload
		if err := json.Unmarshal(secondBody, &repeated); err != nil {
			t.Fatalf("decode repeated grouped list: %v", err)
		}
		if !reflect.DeepEqual(helperGroupListShape(payload), helperGroupListShape(repeated)) {
			t.Fatalf("repeat grouped list changed identity/order: %+v vs %+v", helperGroupListShape(payload), helperGroupListShape(repeated))
		}
	}

	if tc.Expected.AfterList != nil {
		for id, purpose := range tc.Expected.AfterList.Purposes {
			helperGroupSetPurpose(t, db, id, purpose)
		}
	}

	for _, memberPages := range tc.Expected.MemberPages {
		item := payload.Items[memberPages.ItemIndex]
		if len(item.HelperGroups) != 1 {
			t.Fatalf("member pages item[%d] has %d groups, want 1", memberPages.ItemIndex, len(item.HelperGroups))
		}
		group := item.HelperGroups[0]
		for _, page := range memberPages.Pages {
			members := helperGroupMembers(t, base, group.GroupID, group.MemberScope, page.Page, page.Limit)
			if members.Total != page.Total {
				t.Fatalf("member page (%d,%d) total = %d, want %d", page.Page, page.Limit, members.Total, page.Total)
			}
			if got := helperGroupMemberIDs(members); !reflect.DeepEqual(got, page.MemberIDs) {
				t.Fatalf("member page (%d,%d) members = %v, want %v", page.Page, page.Limit, got, page.MemberIDs)
			}
		}
	}

	if len(tc.Expected.TopLevelPages) > 0 {
		filters := GroupedFilters{Variant: GroupedRouteSessions}
		candidates, err := provider.GroupedCandidates(context.Background(), filters)
		if err != nil {
			t.Fatalf("provider grouped candidates: %v", err)
		}
		issuer := groupedScopeIssuerFunc(func(string, GroupedRouteVariant, GroupedFilters) (string, error) { return "scope-stub", nil })
		for _, page := range tc.Expected.TopLevelPages {
			paged, err := buildGroupedListPayload(filters, candidates, page.Page, page.Limit, issuer)
			if err != nil {
				t.Fatalf("build page (%d,%d): %v", page.Page, page.Limit, err)
			}
			got := make([]string, 0, len(paged.Items))
			for _, item := range paged.Items {
				if item.Transcript != nil {
					got = append(got, item.Transcript.Session.ID)
				}
			}
			if !reflect.DeepEqual(got, page.SessionIDs) {
				t.Fatalf("page (%d,%d) items = %v, want %v", page.Page, page.Limit, got, page.SessionIDs)
			}
		}
	}
}

// TestMemberScopeCacheExpiresAndBounds proves the scope cache is a bounded
// TTL lookup: an expired entry is refused and the oldest entry is evicted at
// capacity so a scope never silently outlives its originating list.
func TestMemberScopeCacheExpiresAndBounds(t *testing.T) {
	expiring := newMemberScopeCache(time.Minute, 8)
	now := time.Unix(0, 0)
	expiring.now = func() time.Time { return now }
	scope, err := expiring.issue("hg_a", GroupedRouteSessions, GroupedFilters{Variant: GroupedRouteSessions}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := expiring.lookup(scope); !ok {
		t.Fatal("freshly issued scope did not resolve")
	}
	now = now.Add(time.Minute + time.Second)
	if _, ok := expiring.lookup(scope); ok {
		t.Fatal("expired scope still resolved")
	}

	bounded := newMemberScopeCache(time.Hour, 2)
	bounded.now = func() time.Time { return now }
	first, _ := bounded.issue("hg_a", GroupedRouteSessions, GroupedFilters{Variant: GroupedRouteSessions}, "r1")
	now = now.Add(time.Second)
	second, _ := bounded.issue("hg_b", GroupedRouteSessions, GroupedFilters{Variant: GroupedRouteSessions}, "r1")
	now = now.Add(time.Second)
	third, _ := bounded.issue("hg_c", GroupedRouteSessions, GroupedFilters{Variant: GroupedRouteSessions}, "r1")
	if _, ok := bounded.lookup(first); ok {
		t.Fatal("oldest scope survived capacity eviction")
	}
	if _, ok := bounded.lookup(second); !ok {
		t.Fatal("middle scope was evicted before the oldest")
	}
	entry, ok := bounded.lookup(third)
	if !ok || entry.GroupID != "hg_c" {
		t.Fatalf("newest scope = %+v ok=%v, want hg_c", entry, ok)
	}
}

func helperGroupSelection(input helperGroupListingInput) config.SelectionConfig {
	if input.Selection != "selected" {
		return config.SelectionConfig{Mode: config.SelectionModeAll}
	}
	return config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			defaults.HarnessCodex.String(): {Sessions: input.SelectedIDs},
		},
	}
}

func helperGroupListURL(t *testing.T, base string, input helperGroupListingInput) string {
	t.Helper()
	switch input.Route {
	case "sessions":
		values := url.Values{"view": {groupedViewValue}}
		if input.Project != "" {
			values.Set("project", input.Project)
		}
		return base + "/api/v1/sessions?" + values.Encode()
	case "search":
		values := url.Values{"q": {input.SearchQuery}, "view": {groupedViewValue}}
		if input.SearchLimit > 0 {
			values.Set("limit", fmt.Sprintf("%d", input.SearchLimit))
		}
		return base + "/api/v1/search?" + values.Encode()
	default:
		t.Fatalf("case names unknown route %q", input.Route)
		return ""
	}
}

func helperGroupGET(t *testing.T, rawURL string) ([]byte, int) {
	t.Helper()
	response, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	return body, response.StatusCode
}

func helperGroupErrorCode(body []byte) string {
	var envelope struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Code
}

func helperGroupMembers(t *testing.T, base, groupID, scope string, page, limit int) schema.LocalHelperMembersPayload {
	t.Helper()
	values := url.Values{"scope": {scope}, "page": {fmt.Sprintf("%d", page)}, "limit": {fmt.Sprintf("%d", limit)}}
	rawURL := base + "/api/v1/session-groups/" + groupID + "/members?" + values.Encode()
	body, status := helperGroupGET(t, rawURL)
	if status != http.StatusOK {
		t.Fatalf("member request for %s status = %d (body %s)", groupID, status, body)
	}
	var payload schema.LocalHelperMembersPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode members for %s: %v (body %s)", groupID, err, body)
	}
	return payload
}

func helperGroupMemberIDs(payload schema.LocalHelperMembersPayload) []string {
	ids := make([]string, 0, len(payload.Members))
	for _, member := range payload.Members {
		if member.Transcript != nil {
			ids = append(ids, member.Transcript.Session.ID)
		}
	}
	return ids
}

// assertNestedMemberGroups asserts a helper that itself owns saved helpers: its
// row on the member page carries exactly the declared immediate group, and that
// group's own member page returns the declared exact members. It is the control
// that keeps an owner chain rendered where the owner's row is instead of being
// root-flattened.
func assertNestedMemberGroups(t *testing.T, base string, itemIndex int, members schema.LocalHelperMembersPayload, expected []helperGroupListingNestedGroup) {
	t.Helper()
	for _, nested := range expected {
		var row *schema.LocalSessionListItem
		for i := range members.Members {
			member := members.Members[i]
			if member.Transcript != nil && member.Transcript.Session.ID == nested.SessionID {
				row = &members.Members[i]
				break
			}
		}
		if row == nil {
			t.Fatalf("item[%d] member page has no row for %q; a helper that owns helpers lost its disclosure", itemIndex, nested.SessionID)
		}
		if len(row.HelperGroups) != nested.GroupCount {
			t.Fatalf("item[%d] member %q groups = %d, want %d", itemIndex, nested.SessionID, len(row.HelperGroups), nested.GroupCount)
		}
		if nested.GroupCount == 0 {
			continue
		}
		group := row.HelperGroups[0]
		if group.HelperThreadCount != nested.HelperThreadCount {
			t.Fatalf("item[%d] member %q group count = %d, want %d", itemIndex, nested.SessionID, group.HelperThreadCount, nested.HelperThreadCount)
		}
		child := helperGroupMembers(t, base, group.GroupID, group.MemberScope, 1, 50)
		if got := helperGroupMemberIDs(child); !reflect.DeepEqual(got, nested.MemberIDs) {
			t.Fatalf("item[%d] member %q nested members = %v, want %v", itemIndex, nested.SessionID, got, nested.MemberIDs)
		}
	}
}

// helperGroupListShape extracts the identity/order/count projection that must be
// stable across repeated identical queries. Opaque member scopes are excluded:
// they are fresh lookups each time, not identity.
type helperGroupListShapeItem struct {
	Kind        string
	SessionID   string
	OwnerStatus string
	GroupIDs    []string
	GroupCounts []int
}

func helperGroupListShape(payload schema.LocalSessionListPayload) []helperGroupListShapeItem {
	shape := make([]helperGroupListShapeItem, 0, len(payload.Items))
	for _, item := range payload.Items {
		entry := helperGroupListShapeItem{Kind: string(item.Kind)}
		if item.Transcript != nil {
			entry.SessionID = item.Transcript.Session.ID
		}
		if item.Context != nil {
			entry.OwnerStatus = string(item.Context.OwnerStatus)
		}
		for _, group := range item.HelperGroups {
			entry.GroupIDs = append(entry.GroupIDs, group.GroupID)
			entry.GroupCounts = append(entry.GroupCounts, group.HelperThreadCount)
		}
		shape = append(shape, entry)
	}
	return shape
}

func seedHelperGroupSession(t *testing.T, db *store.Store, spec helperGroupListingSession, sequence int64) {
	t.Helper()
	start := spec.StartMs
	if start == 0 {
		start = 1000 + sequence
	}
	end := start + 10
	ingested := end + 1
	origin := sessionorigin.User
	if spec.Origin != "" {
		origin = sessionorigin.Origin(spec.Origin)
	}
	projectHash := helperGroupFixtureProjectHash
	projectName := "fixture-project"
	projectFilePath := "/fixture/project"
	if spec.ProjectHash == helperGroupFixtureSiblingProjectHash.String() {
		projectHash = helperGroupFixtureSiblingProjectHash
		projectName = "fixture-sibling-project"
		projectFilePath = "/fixture/sibling-project"
	} else if spec.ProjectHash != "" {
		projectHash = schema.ProjectHash(spec.ProjectHash)
	}
	metadata := &schema.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     schema.SessionID(spec.ID),
		ModelHarness:  defaults.HarnessCodex,
		Model:         schema.ModelID("fixture-model"),
		HostSlug:      schema.HostSlug("fixture-host"),
		Project: schema.ProjectContext{
			Hash:     projectHash,
			Name:     projectName,
			FilePath: projectFilePath,
		},
		Timestamp: schema.TimestampInfo{Start: start, End: end, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: "/fixture/session.jsonl", Format: schema.SourceFormatJSONL},
		Stats:     schema.SessionStats{TurnCount: 1},
	}
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{{
		Metadata: metadata,
		Session:  ingest.DiscoveredSession{Origin: origin},
	}}); err != nil {
		t.Fatalf("seed session %q: %v", spec.ID, err)
	}
	if spec.Purpose != "" || spec.Owner != "" || spec.OwnerState != "" {
		seedHelperGroupingEvidence(t, db, spec)
	}
	MarkStoredSessionsIndexed(t, db)
	if spec.Text != "" {
		preview := spec.Text
		entry := schema.SessionEntry{
			SessionID:      schema.SessionID(spec.ID),
			EntryIndex:     0,
			Harness:        defaults.HarnessCodex,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: &preview,
		}
		if err := testutil.WriteFullEntries(context.Background(), db, ingest.SessionID(spec.ID), []schema.SessionEntry{entry}); err != nil {
			t.Fatalf("index search text for %q: %v", spec.ID, err)
		}
	}
}

// seedHelperGroupingEvidence writes the EXISTING V2 read-model columns the
// grouped query reads: the durable purpose mirror and the active generation's
// started_by evidence. It seeds the same rows the activation writer produces so
// the test exercises the real read path rather than a parallel one.
func seedHelperGroupingEvidence(t *testing.T, db *store.Store, spec helperGroupListingSession) {
	t.Helper()
	var purpose any
	if spec.Purpose != "" {
		purpose = spec.Purpose
	}
	generationID := "gen-" + spec.ID
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection for %q grouping evidence: %v", spec.ID, err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE sessions SET session_purpose = ?, active_generation_id = COALESCE(active_generation_id, ?) WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{purpose, generationID, spec.ID}}); err != nil {
		t.Fatalf("seed purpose for %q: %v", spec.ID, err)
	}
	if spec.Owner == "" && spec.OwnerState == "" {
		return
	}
	state := spec.OwnerState
	if state == "" {
		state = string(schema.RelationshipTargetKnown)
	}
	var target any
	if spec.Owner != "" {
		target = spec.Owner
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state, target_local_id) VALUES (?, ?, 'started_by', ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{spec.ID, generationID, state, target}}); err != nil {
		t.Fatalf("seed started_by evidence for %q: %v", spec.ID, err)
	}
}

func helperGroupSetPurpose(t *testing.T, db *store.Store, id, purpose string) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("take connection to change purpose for %q: %v", id, err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_purpose = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{purpose, id}}); err != nil {
		t.Fatalf("change purpose for %q: %v", id, err)
	}
}

func startHelperGroupServer(t *testing.T, cfg ServerConfig) string {
	t.Helper()
	_, base := startHelperGroupServerHandle(t, cfg)
	return base
}

func startHelperGroupServerHandle(t *testing.T, cfg ServerConfig) (*Server, string) {
	t.Helper()
	server := NewServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen helper group server: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop helper group server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("helper group server did not stop")
		}
	})
	return server, "http://" + server.Addr().String()
}
