package ingest_test

import (
	"context"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/locate_steps.yaml
var locateStepsYAML []byte

type locateFileCase struct {
	Path    string `yaml:"path"`
	ModTime string `yaml:"modTime"`
}

type locateStoredCase struct {
	ID      string `yaml:"id"`
	Harness string `yaml:"harness"`
	Path    string `yaml:"path"`
	Parent  string `yaml:"parent"`
}

type locateProbeCase struct {
	Kind    string `yaml:"kind"`
	Harness string `yaml:"harness"`
	ID      string `yaml:"id"`
	Path    string `yaml:"path"`
}

type locateChildCase struct {
	ID         string `yaml:"id"`
	Harness    string `yaml:"harness"`
	Path       string `yaml:"path"`
	ModTime    string `yaml:"modTime"`
	Unreadable string `yaml:"unreadable"`

	modTime time.Time
}

type locateBuildCase struct {
	Harness   string            `yaml:"harness"`
	Path      string            `yaml:"path"`
	Result    string            `yaml:"result"`
	ModTime   string            `yaml:"modTime"`
	CreatedAt string            `yaml:"createdAt"`
	Probe     *locateProbeCase  `yaml:"probe"`
	Children  []locateChildCase `yaml:"children"`

	modTime   time.Time
	createdAt time.Time
}

type locateLookupCase struct {
	Harness          string   `yaml:"harness"`
	Hits             []string `yaml:"hits"`
	Fail             string   `yaml:"fail"`
	Location         string   `yaml:"location"`
	SearchedLocation string   `yaml:"searchedLocation"`
	Unsupported      string   `yaml:"unsupported"`
}

type locateCatalogCase struct {
	Path string `yaml:"path"`
	ID   string `yaml:"id"`
}

type locateStepCase struct {
	Name     string              `yaml:"name"`
	Selected []string            `yaml:"selected"`
	Related  string              `yaml:"related"`
	Harness  string              `yaml:"harness"`
	Since    string              `yaml:"since"`
	Disabled []string            `yaml:"disabled"`
	Files    []locateFileCase    `yaml:"files"`
	Stored   []locateStoredCase  `yaml:"stored"`
	Build    []locateBuildCase   `yaml:"build"`
	Lookup   []locateLookupCase  `yaml:"lookup"`
	Catalog  []locateCatalogCase `yaml:"catalog"`
	Want     struct {
		Misses []struct {
			ID      string `yaml:"id"`
			Reason  string `yaml:"reason"`
			Harness string `yaml:"harness"`
		} `yaml:"misses"`
		ReasonContains []string `yaml:"reasonContains"`
		Members        []struct {
			ID             string `yaml:"id"`
			Role           string `yaml:"role"`
			Outcome        string `yaml:"outcome"`
			Unreadable     string `yaml:"unreadable"`
			DetailContains string `yaml:"detailContains"`
		} `yaml:"members"`
		Processed       []string       `yaml:"processed"`
		Upkeep          []string       `yaml:"upkeep"`
		Refused         []string       `yaml:"refused"`
		RefusedContains []string       `yaml:"refusedContains"`
		LookupOrder     []string       `yaml:"lookupOrder"`
		NoLookup        bool           `yaml:"noLookup"`
		NoWalk          bool           `yaml:"noWalk"`
		ReadDirs        map[string]int `yaml:"readDirs"`
		StatExactly     map[string]int `yaml:"statExactly"`
		BuildCount      int            `yaml:"buildCount"`
		FixContains     []string       `yaml:"fixContains"`
	} `yaml:"want"`
}

type locateStepsFixture struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Cases         []locateStepCase `yaml:"cases"`
}

func loadLocateStepFixture(t *testing.T) locateStepsFixture {
	t.Helper()
	var fixture locateStepsFixture
	if err := testutil.DecodeNamedFixtureYAML(locateStepsYAML, &fixture); err != nil {
		t.Fatalf("decode locate steps fixture: %v", err)
	}
	return fixture
}

// TestLocateSteps drives the real pipeline's LOCATE through one fixture case per
// behavior: the stored path first, the by-name fan-out, unreadable recorded
// members, the duplicate refusal, the ancestor walk, and --no-related.
func TestLocateSteps(t *testing.T) {
	fixture := loadLocateStepFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			runLocateStepCase(t, testCase)
		})
	}
}

func runLocateStepCase(t *testing.T, testCase locateStepCase) {
	t.Helper()
	ctx := context.Background()

	mfs := testutil.NewMemFS()
	for _, file := range testCase.Files {
		if err := mfs.WriteFile(file.Path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write fixture file %q: %v", file.Path, err)
		}
		if file.ModTime != "" {
			mfs.ModTimes[file.Path] = mustParseTime(t, file.ModTime)
		}
	}

	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	seedLocateStored(t, db, testCase.Stored)
	if len(testCase.Catalog) > 0 {
		catalog := make([]ingest.RolloutFile, 0, len(testCase.Catalog))
		for _, row := range testCase.Catalog {
			catalog = append(catalog, ingest.RolloutFile{Path: ingest.ResolvedPath(row.Path), SessionID: ingest.SessionID(row.ID)})
		}
		if err := db.RebuildDerivedLookups(ctx, ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete, Files: catalog}); err != nil {
			t.Fatalf("seed derived lookups: %v", err)
		}
	}

	behavior := newLocateFakeBehavior(t, testCase)
	adapters := make(map[ingest.Harness]ingest.AdapterFactory, len(ingest.DefaultAdapterRegistry))
	sources := make(map[ingest.Harness]ingest.SourceConfig, len(ingest.DefaultAdapterRegistry))
	disabled := make(map[string]bool, len(testCase.Disabled))
	for _, harness := range testCase.Disabled {
		disabled[harness] = true
	}
	for harness := range ingest.DefaultAdapterRegistry {
		bound := harness
		adapters[harness] = func(_ ingest.FileSystem, _ ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
			return &locateFakeAdapter{harness: bound, behavior: behavior}
		}
		sources[harness] = ingest.SourceConfig{
			Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join("/src", string(harness)))},
			Enabled: !disabled[string(harness)],
		}
	}

	related := ingest.RelatedInclude
	if testCase.Related == "exclude" {
		related = ingest.RelatedExclude
	}
	ids := make([]ingest.SessionID, 0, len(testCase.Selected))
	for _, raw := range testCase.Selected {
		id, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("fixture session id %q: %v", raw, err)
		}
		ids = append(ids, id)
	}
	scope, err := ingest.NewSessionScope(ids, related)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}

	cfg := ingest.PipelineConfig{
		Sources:     sources,
		OutputDir:   ingest.ResolvedPath("/out"),
		Scope:       &scope,
		Command:     "harvest",
		DryRun:      true,
		Parallelism: 1,
	}
	if testCase.Since != "" {
		since := mustParseTime(t, testCase.Since)
		cfg.Since = &since
	}
	if testCase.Harness != "" {
		harness := ingest.Harness(testCase.Harness)
		cfg.Harness = &harness
	}

	before := snapshotStoreTables(t, dbPath)

	counting := testutil.NewCountingFS(mfs)
	pipeline, err := ingest.NewPipeline(counting, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithStore(db))
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	result, runErr := pipeline.Run(ctx)

	assertStoreUnchanged(t, dbPath, before)
	assertLocateMisses(t, testCase, runErr)
	if runErr != nil {
		return
	}
	if result == nil || result.ScopeReport == nil {
		t.Fatalf("a scoped run returned no scope report")
	}
	assertLocateMembers(t, testCase, result.ScopeReport)
	assertLocateRefusals(t, testCase, result)
	if testCase.Want.NoLookup && len(behavior.lookupOrder) != 0 {
		t.Errorf("LOCATE ran a name lookup for a stored id: %v", behavior.lookupOrder)
	}
	if len(testCase.Want.LookupOrder) > 0 {
		got := make([]string, len(behavior.lookupOrder))
		for i, harness := range behavior.lookupOrder {
			got[i] = string(harness)
		}
		if !locateEqualStrings(got, testCase.Want.LookupOrder) {
			t.Errorf("lookup order = %v, want %v", got, testCase.Want.LookupOrder)
		}
	}
	if testCase.Want.NoWalk {
		if walked := counting.CountUnder(testutil.FSOpWalkDir, "/"); walked != 0 {
			t.Errorf("LOCATE walked the tree %d time(s)", walked)
		}
	}
	for path, want := range testCase.Want.ReadDirs {
		if got := counting.Count(testutil.FSOpReadDir, path); got != want {
			t.Errorf("ReadDir(%q) ran %d time(s), want %d", path, got, want)
		}
	}
	for path, want := range testCase.Want.StatExactly {
		if got := counting.Count(testutil.FSOpStat, path); got != want {
			t.Errorf("Stat(%q) ran %d time(s), want exactly %d", path, got, want)
		}
	}
	if testCase.Want.BuildCount > 0 && behavior.buildCalls != testCase.Want.BuildCount {
		t.Errorf("DiscoverSessionAt ran %d time(s), want %d", behavior.buildCalls, testCase.Want.BuildCount)
	}
}

func assertLocateMisses(t *testing.T, testCase locateStepCase, runErr error) {
	t.Helper()
	if len(testCase.Want.Misses) == 0 {
		if runErr != nil {
			t.Fatalf("run returned an unexpected error: %v", runErr)
		}
		return
	}
	var scopeErr *ingest.SessionScopeError
	if !errors.As(runErr, &scopeErr) {
		t.Fatalf("run error = %v, want a *SessionScopeError", runErr)
	}
	if len(scopeErr.Misses) != len(testCase.Want.Misses) {
		t.Fatalf("run reported %d miss(es), want %d: %+v", len(scopeErr.Misses), len(testCase.Want.Misses), scopeErr.Misses)
	}
	for index, want := range testCase.Want.Misses {
		got := scopeErr.Misses[index]
		if string(got.ID) != want.ID {
			t.Errorf("miss[%d] id = %q, want %q (one block per id, in id order)", index, got.ID, want.ID)
		}
		if gotReason := locateMissReasonName(got.Reason); gotReason != want.Reason {
			t.Errorf("miss[%d] reason = %q, want %q", index, gotReason, want.Reason)
		}
		if want.Harness != "" && string(got.StoredHarness) != want.Harness {
			t.Errorf("miss[%d] stored harness = %q, want %q", index, got.StoredHarness, want.Harness)
		}
	}
	reason := scopeErr.Reason()
	for _, substring := range testCase.Want.ReasonContains {
		if !strings.Contains(reason, substring) {
			t.Errorf("Reason() = %q, missing %q", reason, substring)
		}
	}
	fix := scopeErr.FixLine()
	for _, substring := range testCase.Want.FixContains {
		if !strings.Contains(fix, substring) {
			t.Errorf("FixLine() = %q, missing %q", fix, substring)
		}
	}
	block := scopeErr.Error()
	if got := strings.Count(block, "peasant: harvest failed:"); got != 1 {
		t.Errorf("block carries %d headline(s), want 1:\n%s", got, block)
	}
	if got := strings.Count(block, "  impact:"); got != 1 {
		t.Errorf("block carries %d impact block(s), want 1:\n%s", got, block)
	}
	for _, want := range testCase.Want.Misses {
		if !strings.Contains(block, want.ID[:4]) {
			t.Errorf("block omits the miss for %s:\n%s", want.ID, block)
		}
	}
}

func assertLocateMembers(t *testing.T, testCase locateStepCase, report *ingest.ScopeReport) {
	t.Helper()
	got := make(map[string]ingest.LocatedMember, len(report.Members))
	for _, member := range report.Members {
		got[string(member.ID)] = member
	}
	if len(report.Members) != len(testCase.Want.Members) {
		t.Fatalf("report carries %d member(s), want %d: %+v", len(report.Members), len(testCase.Want.Members), report.Members)
	}
	for _, want := range testCase.Want.Members {
		member, ok := got[want.ID]
		if !ok {
			t.Errorf("report omits member %s", want.ID)
			continue
		}
		if gotRole := locateRoleName(member.Role); gotRole != want.Role {
			t.Errorf("member %s role = %q, want %q", want.ID, gotRole, want.Role)
		}
		if gotOutcome := locateOutcomeName(member.Outcome); gotOutcome != want.Outcome {
			t.Errorf("member %s outcome = %q, want %q", want.ID, gotOutcome, want.Outcome)
		}
		if want.Unreadable != "" && member.Unreadable.String() != want.Unreadable {
			t.Errorf("member %s reason = %q, want %q", want.ID, member.Unreadable, want.Unreadable)
		}
		if want.DetailContains != "" && !strings.Contains(member.Detail, want.DetailContains) {
			t.Errorf("member %s detail = %q, missing %q", want.ID, member.Detail, want.DetailContains)
		}
	}
	assertIDSet(t, "Processed()", report.Processed(), testCase.Want.Processed)
	assertIDSet(t, "Upkeep()", report.Upkeep(), testCase.Want.Upkeep)
}

func assertIDSet(t *testing.T, label string, got []ingest.SessionID, want []string) {
	t.Helper()
	gotStrings := make([]string, len(got))
	for i, id := range got {
		gotStrings[i] = string(id)
	}
	sort.Strings(gotStrings)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !locateEqualStrings(gotStrings, wantSorted) {
		t.Errorf("%s = %v, want %v", label, gotStrings, wantSorted)
	}
}

func assertLocateRefusals(t *testing.T, testCase locateStepCase, result *ingest.PipelineResult) {
	t.Helper()
	if len(testCase.Want.Refused) == 0 {
		return
	}
	refused := make(map[string]bool)
	var texts []string
	for _, session := range result.Sessions {
		if session.Error == nil {
			continue
		}
		refused[string(session.SessionID)] = true
		texts = append(texts, session.Error.Error())
	}
	for _, id := range testCase.Want.Refused {
		if !refused[id] {
			t.Errorf("run did not report refused id %s: %+v", id, result.Sessions)
		}
	}
	joined := strings.Join(texts, "\n")
	for _, substring := range testCase.Want.RefusedContains {
		if !strings.Contains(joined, substring) {
			t.Errorf("refusal text missing %q:\n%s", substring, joined)
		}
	}
}

func locateEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustParseTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse fixture time %q: %v", raw, err)
	}
	return parsed
}

// seedLocateStored writes the recorded rows the case declares. Parents and
// children go in one batch, which the writer sorts parents-first.
func seedLocateStored(t *testing.T, db *store.Store, rows []locateStoredCase) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	ingested := int64(3)
	entries := make([]ingest.StoreEntry, 0, len(rows))
	for _, row := range rows {
		var parent *schema.SessionID
		if row.Parent != "" {
			value := schema.SessionID(row.Parent)
			parent = &value
		}
		entries = append(entries, ingest.StoreEntry{
			Metadata: &schema.UnifiedMetadata{
				SessionID:    schema.SessionID(row.ID),
				ModelHarness: ingest.Harness(row.Harness),
				Model:        schema.ModelID("test-model"),
				HostSlug:     schema.HostSlug("testslug"),
				Project: schema.ProjectContext{
					Hash:     schema.ProjectHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
					Name:     "locate",
					FilePath: "/locate",
				},
				Timestamp:  schema.TimestampInfo{Start: 1, End: 2, Ingested: &ingested},
				Source:     schema.SourceInfo{FilePath: row.Path, Format: schema.SourceFormatJSONL},
				ParentUUID: parent,
			},
		})
	}
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed stored sessions: %v", err)
	}
}

// locateFakeBehavior is the per-case script the fake adapters read.
type locateFakeBehavior struct {
	builds      map[string]locateBuildCase
	lookups     map[ingest.Harness]locateLookupCase
	lookupOrder []ingest.Harness
	buildCalls  int
}

func newLocateFakeBehavior(t *testing.T, testCase locateStepCase) *locateFakeBehavior {
	t.Helper()
	behavior := &locateFakeBehavior{
		builds:  make(map[string]locateBuildCase, len(testCase.Build)),
		lookups: make(map[ingest.Harness]locateLookupCase, len(testCase.Lookup)),
	}
	for _, build := range testCase.Build {
		if build.ModTime != "" {
			build.modTime = mustParseTime(t, build.ModTime)
		}
		if build.CreatedAt != "" {
			build.createdAt = mustParseTime(t, build.CreatedAt)
		}
		for index := range build.Children {
			if build.Children[index].ModTime != "" {
				build.Children[index].modTime = mustParseTime(t, build.Children[index].ModTime)
			}
		}
		behavior.builds[locateBuildKey(ingest.Harness(build.Harness), build.Path)] = build
	}
	for _, lookup := range testCase.Lookup {
		behavior.lookups[ingest.Harness(lookup.Harness)] = lookup
	}
	return behavior
}

func locateBuildKey(harness ingest.Harness, path string) string {
	return string(harness) + "\x00" + path
}

func locateRoleName(role ingest.CohortRole) string {
	switch role {
	case ingest.CohortSelected:
		return "selected"
	case ingest.CohortChild:
		return "child"
	case ingest.CohortAncestor:
		return "ancestor"
	case ingest.CohortContext:
		return "context"
	default:
		return "unset"
	}
}

func locateOutcomeName(outcome ingest.LocateOutcome) string {
	switch outcome {
	case ingest.LocateStoredPath:
		return "stored_path"
	case ingest.LocateDirectLookup:
		return "direct_lookup"
	case ingest.LocateStoredUnreadable:
		return "stored_unreadable"
	default:
		return "unset"
	}
}

func locateMissReasonName(reason ingest.MissReason) string {
	switch reason {
	case ingest.MissNotFound:
		return "not_found"
	case ingest.MissHarnessFiltered:
		return "harness_filtered"
	case ingest.MissSinceFiltered:
		return "since_filtered"
	default:
		return "unset"
	}
}

// locateFakeAdapter implements the two capabilities LOCATE uses. Its Discover
// fails loudly so a scoped run that falls back to a full walk is caught.
type locateFakeAdapter struct {
	harness  ingest.Harness
	behavior *locateFakeBehavior
}

func (a *locateFakeAdapter) Harness() ingest.Harness { return a.harness }

func (a *locateFakeAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return nil, fmt.Errorf("scoped LOCATE must not run a full discovery for %s", a.harness)
}

func (a *locateFakeAdapter) ExtractMetadata(_ context.Context, session ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	return &ingest.UnifiedMetadata{SessionID: session.SessionID, ModelHarness: session.Harness}, nil
}

func (a *locateFakeAdapter) LookupSessionSources(_ context.Context, _ ingest.SourceConfig, _ ingest.SessionID) (ingest.SourceLookup, error) {
	a.behavior.lookupOrder = append(a.behavior.lookupOrder, a.harness)
	entry, ok := a.behavior.lookups[a.harness]
	if !ok {
		return ingest.SourceLookup{}, nil
	}
	if entry.Unsupported != "" {
		reason := ingest.IDOnlyInFileHeader
		if entry.Unsupported == "file_body" {
			reason = ingest.IDOnlyInFileBody
		}
		return ingest.SourceLookup{Searched: []ingest.SearchedSource{{
			Harness:  a.harness,
			Method:   ingest.SearchUnsupported,
			Location: entry.Location,
			Reason:   reason,
		}}}, nil
	}
	searchedLocation := entry.SearchedLocation
	if searchedLocation == "" {
		searchedLocation = entry.Location
	}
	searched := []ingest.SearchedSource{{Harness: a.harness, Method: ingest.SearchNamePattern, Location: searchedLocation}}
	if entry.Fail != "" {
		return ingest.SourceLookup{Searched: searched}, &ingest.SourceLookupFailure{Location: entry.Location, Err: errors.New(entry.Fail)}
	}
	hits := make([]ingest.ResolvedPath, 0, len(entry.Hits))
	for _, hit := range entry.Hits {
		hits = append(hits, ingest.ResolvedPath(hit))
	}
	return ingest.SourceLookup{Sources: hits, Searched: searched}, nil
}

func (a *locateFakeAdapter) DiscoverSessionAt(_ context.Context, _ ingest.SourceConfig, at ingest.LocatedSource) (ingest.LocatedBuild, error) {
	a.behavior.buildCalls++
	build, ok := a.behavior.builds[locateBuildKey(a.harness, string(at.Path))]
	if !ok {
		return ingest.LocatedBuild{}, fmt.Errorf("no source at %q: %w", at.Path, ingest.ErrSourceGone)
	}
	switch build.Result {
	case "gone":
		return ingest.LocatedBuild{}, fmt.Errorf("%q no longer holds the session: %w", at.Path, ingest.ErrSourceGone)
	case "fail":
		return ingest.LocatedBuild{}, fmt.Errorf("cannot read %q", at.Path)
	case "noMember":
		return ingest.LocatedBuild{Deferred: ingest.NoDeferredWrites{}}, nil
	}
	source := ingest.DiscoveredSession{
		SessionID:    at.ID,
		Harness:      a.harness,
		SourcePath:   at.Path,
		SourceFormat: ingest.SourceFormatJSONL,
		OriginalRoot: ingest.ResolvedPath(filepath.Join("/src", string(a.harness))),
		ModTime:      build.modTime,
		CreatedAt:    build.createdAt,
	}
	members := []ingest.LocatedMember{{
		ID:          at.ID,
		Harness:     a.harness,
		Source:      &source,
		ParentProbe: locateProbe(build.Probe),
	}}
	for _, child := range build.Children {
		if child.Unreadable != "" {
			members = append(members, ingest.LocatedMember{
				ID:         ingest.SessionID(child.ID),
				Harness:    ingest.Harness(child.Harness),
				Unreadable: locateUnreadableReason(child.Unreadable),
			})
			continue
		}
		parent := at.ID
		childSource := ingest.DiscoveredSession{
			SessionID:    ingest.SessionID(child.ID),
			Harness:      ingest.Harness(child.Harness),
			SourcePath:   ingest.ResolvedPath(child.Path),
			SourceFormat: ingest.SourceFormatJSONL,
			ModTime:      child.modTime,
			ParentUUID:   &parent,
		}
		members = append(members, ingest.LocatedMember{
			ID:      ingest.SessionID(child.ID),
			Harness: ingest.Harness(child.Harness),
			Source:  &childSource,
		})
	}
	return ingest.LocatedBuild{Members: members, Deferred: ingest.NoDeferredWrites{}}, nil
}

func locateUnreadableReason(name string) ingest.UnreadableReason {
	switch name {
	case "gone":
		return ingest.UnreadableSourceGone
	case "disabled":
		return ingest.UnreadableHarnessDisabled
	default:
		return ingest.UnreadableSourceFailed
	}
}

func locateProbe(probe *locateProbeCase) *ingest.AncestorProbe {
	if probe == nil {
		return nil
	}
	result := &ingest.AncestorProbe{Harness: ingest.Harness(probe.Harness)}
	switch probe.Kind {
	case "path":
		result.Kind = ingest.ProbePath
		result.Path = ingest.ResolvedPath(probe.Path)
	case "row":
		result.Kind = ingest.ProbeRow
		result.ID = ingest.SessionID(probe.ID)
	case "catalog_window":
		result.Kind = ingest.ProbeCatalogWindow
		result.ID = ingest.SessionID(probe.ID)
	}
	return result
}

var _ ingest.SessionLocator = (*locateFakeAdapter)(nil)
var _ ingest.SourceAdapter = (*locateFakeAdapter)(nil)

// snapshotStoreTables reads every table's normalized rows so a test can prove a
// pass wrote nothing. Row order is not part of the snapshot, so a VACUUM or a
// b-tree rebalance cannot look like a write.
func snapshotStoreTables(t *testing.T, dbPath string) map[string][]string {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatalf("open snapshot connection: %v", err)
	}
	defer conn.Close()

	var names []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			names = append(names, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables := make(map[string][]string, len(names))
	for _, name := range names {
		query := `SELECT * FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`
		var rows []string
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				var row strings.Builder
				for column := 0; column < stmt.ColumnCount(); column++ {
					row.WriteString(strconv.Itoa(column))
					row.WriteByte('=')
					switch stmt.ColumnType(column) {
					case sqlite.TypeNull:
						row.WriteString("null")
					case sqlite.TypeInteger:
						row.WriteString(strconv.FormatInt(stmt.ColumnInt64(column), 10))
					case sqlite.TypeFloat:
						row.WriteString(strconv.FormatFloat(stmt.ColumnFloat(column), 'g', -1, 64))
					case sqlite.TypeBlob:
						data, _ := io.ReadAll(stmt.ColumnReader(column))
						row.WriteString(hex.EncodeToString(data))
					default:
						row.WriteString(strconv.Quote(stmt.ColumnText(column)))
					}
					row.WriteByte('|')
				}
				rows = append(rows, row.String())
				return nil
			},
		}); err != nil {
			t.Fatalf("read table %q: %v", name, err)
		}
		sort.Strings(rows)
		tables[name] = rows
	}
	return tables
}

// assertStoreUnchanged proves the run wrote no row. LOCATE is a read-only
// stage, and a write here would record a session, a derived row, or an audit
// entry for a run that did not process anything yet.
func assertStoreUnchanged(t *testing.T, dbPath string, before map[string][]string) {
	t.Helper()
	after := snapshotStoreTables(t, dbPath)
	var changed []string
	for name, rows := range before {
		if !reflect.DeepEqual(rows, after[name]) {
			changed = append(changed, fmt.Sprintf("%s (before %d row(s), after %d)", name, len(rows), len(after[name])))
		}
	}
	for name, rows := range after {
		if _, existed := before[name]; !existed && len(rows) > 0 {
			changed = append(changed, name+" (appeared)")
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		t.Errorf("LOCATE wrote to the store; changed tables: %s", strings.Join(changed, ", "))
	}
}

// TestScopedReindexRequiresAStore pins the scoped reindex path's one hard
// requirement: it resolves each named session's saved pair from the recorded
// location, so it cannot run without the database. The old refusal of the
// combination is gone: the scope now narrows the reindex run instead of being
// silently ignored.
func TestScopedReindexRequiresAStore(t *testing.T) {
	scope, err := ingest.NewSessionScope([]ingest.SessionID{"11111111-1111-4111-8111-111111111111"}, ingest.RelatedInclude)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}
	pipeline, err := ingest.NewPipeline(nil, nil, ingest.DefaultAdapterRegistry, ingest.PipelineConfig{
		Reindex: true,
		Scope:   &scope,
	})
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	_, runErr := pipeline.Run(context.Background())
	if runErr == nil {
		t.Fatalf("a scoped reindex run without a store was accepted")
	}
	for _, substring := range []string{"session-scoped index run", "no store row was written", "fix:"} {
		if !strings.Contains(runErr.Error(), substring) {
			t.Errorf("refusal = %q, missing %q", runErr, substring)
		}
	}
}

// storeWithoutDerivedLookup is a store that answers the stored-relative reads
// but not the indexed derived lookups. It is the shape that must not silently
// skip the Codex duplicate check.
type storeWithoutDerivedLookup struct {
	ingest.SessionStore
}

func (s storeWithoutDerivedLookup) StoredSessionSources(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.StoredSessionSource, error) {
	return s.SessionStore.(ingest.SessionSourceReader).StoredSessionSources(ctx, ids)
}

func (s storeWithoutDerivedLookup) StoredChildren(ctx context.Context, parents []ingest.SessionID) (map[ingest.SessionID][]ingest.StoredSessionSource, error) {
	return s.SessionStore.(ingest.SessionSourceReader).StoredChildren(ctx, parents)
}

// TestLocateRefusesStoreWithoutDerivedLookup pins the Codex duplicate check: a
// missing reader is a refused run, never a run that reports duplicates as
// checked while it skipped them.
func TestLocateRefusesStoreWithoutDerivedLookup(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const path = "/src/codex/2026/11/09/rollout-2026-11-09T10-00-00-11111111-1111-4111-8111-111111111111.jsonl"
	ctx := context.Background()

	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedLocateStored(t, db, []locateStoredCase{{ID: id, Harness: string(ingest.HarnessCodex), Path: path}})

	behavior := &locateFakeBehavior{
		builds: map[string]locateBuildCase{
			locateBuildKey(ingest.HarnessCodex, path): {Harness: string(ingest.HarnessCodex), Path: path, Result: "ok"},
		},
		lookups: map[ingest.Harness]locateLookupCase{},
	}
	adapters := make(map[ingest.Harness]ingest.AdapterFactory, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		bound := harness
		adapters[harness] = func(_ ingest.FileSystem, _ ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
			return &locateFakeAdapter{harness: bound, behavior: behavior}
		}
	}
	sources := make(map[ingest.Harness]ingest.SourceConfig, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		sources[harness] = ingest.SourceConfig{
			Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join("/src", string(harness)))},
			Enabled: harness == ingest.HarnessCodex,
		}
	}
	scope, err := ingest.NewSessionScope([]ingest.SessionID{id}, ingest.RelatedInclude)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}
	pipeline, err := ingest.NewPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(), adapters, ingest.PipelineConfig{
		Sources:     sources,
		OutputDir:   ingest.ResolvedPath("/out"),
		Scope:       &scope,
		Command:     "harvest",
		DryRun:      true,
		Parallelism: 1,
	}, ingest.WithStore(storeWithoutDerivedLookup{SessionStore: db}))
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	_, runErr := pipeline.Run(ctx)
	if runErr == nil {
		t.Fatalf("LOCATE accepted a store without DerivedLookupReader and skipped the duplicate check")
	}
	for _, substring := range []string{"CountCodexRolloutsBySession", "DerivedLookupReader", "fix:"} {
		if !strings.Contains(runErr.Error(), substring) {
			t.Errorf("refusal = %q, missing %q", runErr, substring)
		}
	}
}

// TestLocateRefusesStoreWithoutDerivedLookupForClaudeCohort pins the other half
// of the derived-read requirement: a Claude build links teammates from the
// indexed identity reads, so a store that cannot answer them would turn every
// located Claude session into an unreadable member and the run would exit 0
// having processed nothing.
func TestLocateRefusesStoreWithoutDerivedLookupForClaudeCohort(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const path = "/src/claude-code/proj-a/11111111-1111-4111-8111-111111111111.jsonl"
	ctx := context.Background()

	mfs := testutil.NewMemFS()
	if err := mfs.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedLocateStored(t, db, []locateStoredCase{{ID: id, Harness: string(ingest.HarnessClaudeCode), Path: path}})

	behavior := &locateFakeBehavior{
		builds: map[string]locateBuildCase{
			locateBuildKey(ingest.HarnessClaudeCode, path): {Harness: string(ingest.HarnessClaudeCode), Path: path, Result: "ok"},
		},
		lookups: map[ingest.Harness]locateLookupCase{},
	}
	adapters := make(map[ingest.Harness]ingest.AdapterFactory, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		bound := harness
		adapters[harness] = func(_ ingest.FileSystem, _ ingest.GitResolver, _ salt.Salt) ingest.SourceAdapter {
			return &locateFakeAdapter{harness: bound, behavior: behavior}
		}
	}
	sources := make(map[ingest.Harness]ingest.SourceConfig, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		sources[harness] = ingest.SourceConfig{
			Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Join("/src", string(harness)))},
			Enabled: harness == ingest.HarnessClaudeCode,
		}
	}
	scope, err := ingest.NewSessionScope([]ingest.SessionID{id}, ingest.RelatedInclude)
	if err != nil {
		t.Fatalf("build scope: %v", err)
	}
	pipeline, err := ingest.NewPipeline(testutil.NewMemFS(), testutil.DefaultGitResolver(), adapters, ingest.PipelineConfig{
		Sources:     sources,
		OutputDir:   ingest.ResolvedPath("/out"),
		Scope:       &scope,
		Command:     "harvest",
		DryRun:      true,
		Parallelism: 1,
	}, ingest.WithStore(storeWithoutDerivedLookup{SessionStore: db}))
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	result, runErr := pipeline.Run(ctx)
	if runErr == nil {
		t.Fatalf("LOCATE degraded a Claude cohort to an unreadable member instead of refusing (report: %+v)", result.ScopeReport)
	}
	for _, substring := range []string{"indexed Claude identity reads", "DerivedLookupReader", "fix:"} {
		if !strings.Contains(runErr.Error(), substring) {
			t.Errorf("refusal = %q, missing %q", runErr, substring)
		}
	}
}
