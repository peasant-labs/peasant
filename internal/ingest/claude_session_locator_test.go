package ingest_test

// Parity between the full Claude discovery walk and the two paths a scoped run
// uses: the by-name lookup and the located-source build. Every case writes its
// own source tree, drives the production adapter over it, and asserts the exact
// set of filesystem operations the step performs, so a lookup that reads a
// fourth place, a build that walks the tree, or a probe that does I/O while it
// is built fails here rather than in review.

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_session_locator.yaml
var claudeSessionLocatorYAML []byte

type claudeLocatorFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Root          string              `yaml:"root"`
	Cases         []claudeLocatorCase `yaml:"cases"`
}

type claudeLocatorCase struct {
	Name    string                `yaml:"name"`
	Files   []claudeLocatorFile   `yaml:"files"`
	Lookup  *claudeLocatorLookup  `yaml:"lookup"`
	Located *claudeLocatorLocated `yaml:"located"`
}

type claudeLocatorFile struct {
	Path string `yaml:"path"`
	Body string `yaml:"body"`
}

type claudeLocatorLookup struct {
	ID       string                  `yaml:"id"`
	Sources  []string                `yaml:"sources"`
	Searched []claudeLocatorSearched `yaml:"searched"`
	Reads    []string                `yaml:"reads"`
	Stats    []string                `yaml:"stats"`
	Readdirs []string                `yaml:"readdirs"`
}

type claudeLocatorSearched struct {
	Harness  string `yaml:"harness"`
	Method   string `yaml:"method"`
	Location string `yaml:"location"`
}

type claudeLocatorDerived struct {
	Declarers    int      `yaml:"declarers"`
	Spawners     int      `yaml:"spawners"`
	SpawnerPaths []string `yaml:"spawnerPaths"`
}

type claudeLocatorLocated struct {
	ID             string                    `yaml:"id"`
	Path           string                    `yaml:"path"`
	StoredParent   string                    `yaml:"storedParent"`
	StoredSessions []string                  `yaml:"storedSessions"`
	StoredChildren []claudeLocatorChild      `yaml:"storedChildren"`
	StaleOwner     bool                      `yaml:"staleOwner"`
	Derived        *claudeLocatorDerived     `yaml:"derived"`
	WantMembers    []string                  `yaml:"wantMembers"`
	WantUnreadable []claudeLocatorUnreadable `yaml:"wantUnreadable"`
	WantProbe      *claudeLocatorProbe       `yaml:"wantProbe"`
	ContextPaths   []string                  `yaml:"contextPaths"`
	WantParent     string                    `yaml:"wantParent"`
	WantNoParent   bool                      `yaml:"wantNoParent"`
	SourceGone     bool                      `yaml:"sourceGone"`
	WantError      string                    `yaml:"wantError"`
	Reads          []string                  `yaml:"reads"`
	Stats          []string                  `yaml:"stats"`
	Readdirs       []string                  `yaml:"readdirs"`
}

type claudeLocatorChild struct {
	ID         string `yaml:"id"`
	Harness    string `yaml:"harness"`
	Path       string `yaml:"path"`
	Parent     string `yaml:"parent"`
	Unreadable string `yaml:"unreadable"`
}

type claudeLocatorUnreadable struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason"`
}

type claudeLocatorProbe struct {
	Kind string `yaml:"kind"`
	Path string `yaml:"path"`
}

func loadClaudeSessionLocatorFixture(t *testing.T) claudeLocatorFixture {
	t.Helper()
	var fixture claudeLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(claudeSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode claude session locator fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		if testCase.Lookup == nil && testCase.Located == nil {
			t.Fatalf("claude session locator case %q asserts nothing; add lookup or located", testCase.Name)
		}
	}
	return fixture
}

// TestClaudeSessionLocatorParity is the adapter-level guard: for one tree, a
// located source must reproduce exactly the sessions Discover records, a
// by-name lookup must report exactly the places it searched and the paths it
// hit, and neither step may walk a source root.
func TestClaudeSessionLocatorParity(t *testing.T) {
	fixture := loadClaudeSessionLocatorFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			rootText := claudeLocatorPath(t, fixture.Root, "")
			root, err := ingest.NewResolvedPath(rootText)
			if err != nil {
				t.Fatalf("resolve fixture root: %v", err)
			}
			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{root}, Enabled: true}
			if testCase.Lookup != nil {
				t.Run("lookup", func(t *testing.T) {
					runClaudeLookupParity(t, rootText, cfg, testCase)
				})
			}
			if testCase.Located != nil {
				t.Run("discover-at", func(t *testing.T) {
					runClaudeDiscoverAtParity(t, rootText, cfg, testCase)
				})
			}
		})
	}
}

func runClaudeLookupParity(t *testing.T, rootText string, cfg ingest.SourceConfig, testCase claudeLocatorCase) {
	t.Helper()
	lookupCase := testCase.Lookup
	counting := newClaudeLocatorCountingFS(t, rootText, testCase)
	adapter := ingest.NewClaudeAdapter(counting, testutil.NoGitResolver(), salt.Salt{})

	lookup, err := adapter.LookupSessionSources(context.Background(), cfg, ingest.SessionID(lookupCase.ID))
	if err != nil {
		t.Fatalf("LookupSessionSources: %v", err)
	}
	for _, searched := range lookup.Searched {
		if searched.Method == ingest.SearchFailed {
			t.Errorf("lookup recorded a SearchFailed entry itself: the core appends it from the returned error, so one search is reported once")
		}
	}

	gotSources := make([]string, 0, len(lookup.Sources))
	for _, source := range lookup.Sources {
		gotSources = append(gotSources, source.String())
	}
	wantSources := make([]string, 0, len(lookupCase.Sources))
	for _, source := range lookupCase.Sources {
		wantSources = append(wantSources, claudeLocatorPath(t, rootText, source))
	}
	sort.Strings(gotSources)
	sort.Strings(wantSources)
	if !reflect.DeepEqual(gotSources, wantSources) {
		t.Errorf("lookup sources = %v, want %v", gotSources, wantSources)
	}

	if lookupCase.Searched != nil {
		if len(lookup.Searched) != len(lookupCase.Searched) {
			t.Fatalf("lookup reported %d searched entries, want %d: %+v", len(lookup.Searched), len(lookupCase.Searched), lookup.Searched)
		}
		for index, want := range lookupCase.Searched {
			got := lookup.Searched[index]
			if want.Harness != "" && string(got.Harness) != want.Harness {
				t.Errorf("searched[%d] harness = %q, want %q", index, got.Harness, want.Harness)
			}
			method, ok := claudeSearchMethods[want.Method]
			if !ok {
				t.Fatalf("fixture method %q is outside the declared set", want.Method)
			}
			if got.Method != method {
				t.Errorf("searched[%d] method = %v, want %v", index, got.Method, method)
			}
			if got.Location != want.Location {
				t.Errorf("searched[%d] location = %q, want %q", index, got.Location, want.Location)
			}
		}
	}

	assertClaudeOpSet(t, counting, rootText, testutil.FSOpStat, "stat", lookupCase.Stats)
	assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", lookupCase.Readdirs)
	assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", lookupCase.Reads)
	if walks := counting.CountUnder(testutil.FSOpWalkDir, rootText); walks != 0 {
		t.Fatalf("LookupSessionSources walked the source root %d time(s); a lookup never walks", walks)
	}
}

func runClaudeDiscoverAtParity(t *testing.T, rootText string, cfg ingest.SourceConfig, testCase claudeLocatorCase) {
	t.Helper()
	locatedCase := testCase.Located
	counting := newClaudeLocatorCountingFS(t, rootText, testCase)
	cache := newClaudeLocatorEvidenceCache()
	derived := &claudeLocatorDerivedLookup{}
	if locatedCase.Derived != nil {
		derived.declarers = locatedCase.Derived.Declarers
		derived.spawners = locatedCase.Derived.Spawners
		for _, path := range locatedCase.Derived.SpawnerPaths {
			derived.spawnerPaths = append(derived.spawnerPaths, claudeLocatorPath(t, rootText, path))
		}
	}
	location := &claudeLocatorStoredLookup{stored: make(map[ingest.SessionID]struct{})}
	for _, id := range locatedCase.StoredSessions {
		location.stored[ingest.SessionID(id)] = struct{}{}
	}
	adapter := ingest.NewClaudeAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	ingest.AttachClaudeEvidenceCache(adapter, cache)
	ingest.AttachClaudeDerivedLookup(adapter, derived)
	ingest.AttachSessionLocationLookup(adapter, location)

	full, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if locatedCase.StaleOwner {
		path := claudeLocatorPath(t, rootText, locatedCase.ContextPaths[0])
		stale := cache.evidence[ingest.ResolvedPath(path)]
		stale.ModTimeUnixNano = -1
		stale.SizeBytes = -1
		cache.evidence[ingest.ResolvedPath(path)] = stale
	}

	counting.ResetCounts()
	cache.resetCalls()

	at := claudeLocatorLocatedSource(t, rootText, locatedCase)
	build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)
	if locatedCase.WantError != "" {
		if err == nil {
			t.Fatalf("DiscoverSessionAt accepted a source the case says must be refused")
		}
		if locatedCase.SourceGone && !errors.Is(err, ingest.ErrSourceGone) {
			t.Errorf("DiscoverSessionAt error = %v, want it to wrap ErrSourceGone", err)
		}
		if !strings.Contains(err.Error(), locatedCase.WantError) {
			t.Errorf("DiscoverSessionAt error %q does not name %q", err, locatedCase.WantError)
		}
		if len(build.Members) != 0 {
			t.Errorf("a refused located source produced %d member(s), want none", len(build.Members))
		}
		assertClaudeOpSet(t, counting, rootText, testutil.FSOpStat, "stat", locatedCase.Stats)
		assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", locatedCase.Readdirs)
		assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", locatedCase.Reads)
		return
	}
	if err != nil {
		t.Fatalf("DiscoverSessionAt: %v", err)
	}
	if build.Deferred == nil {
		t.Fatalf("DiscoverSessionAt returned no deferred writes; the caller applies them")
	}
	if cache.saves != 0 {
		t.Errorf("DiscoverSessionAt wrote the evidence cache %d time(s); a located build returns its writes as deferred", cache.saves)
	}
	if cache.wholeLoads != 0 {
		t.Errorf("DiscoverSessionAt called the whole-table evidence loader %d time(s); a scoped build reads only named paths", cache.wholeLoads)
	}

	membersByID := make(map[ingest.SessionID]ingest.LocatedMember, len(build.Members))
	for _, member := range build.Members {
		if _, duplicate := membersByID[member.ID]; duplicate {
			t.Fatalf("DiscoverSessionAt returned two members for %s", member.ID)
		}
		if member.Role != ingest.CohortRole(0) || member.Outcome != ingest.LocateOutcome(0) {
			t.Errorf("member %s carries role %v / outcome %v; the core sets both", member.ID, member.Role, member.Outcome)
		}
		membersByID[member.ID] = member
	}
	if len(build.Members) != len(locatedCase.WantMembers)+len(locatedCase.WantUnreadable) {
		t.Fatalf("DiscoverSessionAt returned %d members, want %d: %v", len(build.Members), len(locatedCase.WantMembers)+len(locatedCase.WantUnreadable), claudeLocatorMemberIDs(build.Members))
	}

	fullByID := make(map[ingest.SessionID]ingest.DiscoveredSession, len(full))
	for _, session := range full {
		fullByID[session.SessionID] = session
	}
	for _, wantID := range locatedCase.WantMembers {
		member, ok := membersByID[ingest.SessionID(wantID)]
		if !ok {
			t.Errorf("DiscoverSessionAt omitted member %s", wantID)
			continue
		}
		want, discovered := fullByID[ingest.SessionID(wantID)]
		if !discovered {
			t.Errorf("member %s is not in the full discovery", wantID)
			continue
		}
		if member.Source == nil {
			t.Errorf("member %s has no source, but the full discovery found it", wantID)
			continue
		}
		got := *member.Source
		if locatedCase.WantParent != "" || locatedCase.WantNoParent {
			// The parent is asserted from the fixture, not copied from the full
			// discovery, so a missing kept link cannot pass as a declared
			// difference.
			got.ParentUUID = want.ParentUUID
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("member %s differs from the full discovery field for field:\n got: %#v\nwant: %#v", wantID, got, want)
		}
		if ingest.SessionID(wantID) != at.ID {
			continue
		}
		switch {
		case locatedCase.WantParent != "":
			if got := member.Source.ParentUUID; got == nil || *got != ingest.SessionID(locatedCase.WantParent) {
				t.Errorf("located member %s parent = %v, want the kept link to %s", wantID, got, locatedCase.WantParent)
			}
		case locatedCase.WantNoParent:
			if got := member.Source.ParentUUID; got != nil {
				t.Errorf("located member %s parent = %v, want no link", wantID, *got)
			}
		}
	}
	for _, unreadable := range locatedCase.WantUnreadable {
		member, ok := membersByID[ingest.SessionID(unreadable.ID)]
		if !ok {
			t.Errorf("DiscoverSessionAt dropped stored child %s instead of returning it as unreadable", unreadable.ID)
			continue
		}
		if member.Source != nil {
			t.Errorf("stored child %s came back with a source, want nil", unreadable.ID)
		}
		wantReason, ok := claudeUnreadableReasons[unreadable.Reason]
		if !ok {
			t.Fatalf("fixture reason %q is outside the declared set", unreadable.Reason)
		}
		if member.Unreadable != wantReason {
			t.Errorf("stored child %s unreadable = %v, want %v", unreadable.ID, member.Unreadable, wantReason)
		}
	}

	assertClaudeParentProbe(t, rootText, at.ID, build.Members, locatedCase.WantProbe)
	assertClaudeOpSet(t, counting, rootText, testutil.FSOpStat, "stat", locatedCase.Stats)
	assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", locatedCase.Readdirs)
	assertClaudeOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", locatedCase.Reads)
	if walks := counting.CountUnder(testutil.FSOpWalkDir, rootText); walks != 0 {
		t.Fatalf("DiscoverSessionAt walked the source root %d time(s); a scoped build never walks", walks)
	}
	for _, contextPath := range locatedCase.ContextPaths {
		if !claudeLocatorContains(locatedCase.Stats, contextPath) && !claudeLocatorContains(locatedCase.Reads, contextPath) {
			t.Errorf("context path %q is not in the case's read or stat set; name it where the counter sees it", contextPath)
		}
	}

	if err := build.Deferred.Apply(context.Background()); err != nil {
		t.Fatalf("apply deferred discovery writes: %v", err)
	}
	if locatedCase.StaleOwner {
		ownerPath := ingest.ResolvedPath(claudeLocatorPath(t, rootText, locatedCase.ContextPaths[0]))
		if !cache.savedPath(ownerPath) {
			t.Errorf("the stale context owner's evidence was not refreshed by the deferred write")
		}
	}
}

func assertClaudeParentProbe(t *testing.T, rootText string, located ingest.SessionID, members []ingest.LocatedMember, want *claudeLocatorProbe) {
	t.Helper()
	for _, member := range members {
		switch {
		case member.ID != located:
			if member.ParentProbe != nil {
				t.Errorf("child %s carries an ancestor probe; only the located member's own parent is derived", member.ID)
			}
		case want == nil:
			if member.ParentProbe != nil {
				t.Errorf("located member %s carries a probe whose parent is stored or absent: %+v", located, member.ParentProbe)
			}
		default:
			if member.ParentProbe == nil {
				t.Fatalf("located member %s has no ancestor probe, want %s", located, want.Kind)
			}
			if want.Kind != "path" || member.ParentProbe.Kind != ingest.ProbePath {
				t.Errorf("probe kind = %v, want path", member.ParentProbe.Kind)
			}
			if member.ParentProbe.Harness != ingest.HarnessClaudeCode {
				t.Errorf("probe harness = %v, want claude-code", member.ParentProbe.Harness)
			}
			if got, wantPath := member.ParentProbe.Path.String(), claudeLocatorPath(t, rootText, want.Path); got != wantPath {
				t.Errorf("probe path = %q, want %q", got, wantPath)
			}
		}
	}
}

func newClaudeLocatorCountingFS(t *testing.T, rootText string, testCase claudeLocatorCase) *testutil.CountingFS {
	t.Helper()
	mfs := testutil.NewMemFS()
	for _, file := range testCase.Files {
		path := claudeLocatorPath(t, rootText, file.Path)
		if err := mfs.WriteFile(path, []byte(file.Body), 0o644); err != nil {
			t.Fatalf("write fixture file %s: %v", file.Path, err)
		}
	}
	return testutil.NewCountingFS(mfs)
}

func claudeLocatorLocatedSource(t *testing.T, rootText string, locatedCase *claudeLocatorLocated) ingest.LocatedSource {
	t.Helper()
	at := ingest.LocatedSource{
		ID:   ingest.SessionID(locatedCase.ID),
		Path: ingest.ResolvedPath(claudeLocatorPath(t, rootText, locatedCase.Path)),
	}
	if locatedCase.StoredParent != "" {
		parent := ingest.SessionID(locatedCase.StoredParent)
		at.Stored.Parent = &parent
	}
	for _, child := range locatedCase.StoredChildren {
		source := ingest.StoredSessionSource{
			ID:      ingest.SessionID(child.ID),
			Harness: ingest.Harness(child.Harness),
			Path:    ingest.ResolvedPath(claudeLocatorPath(t, rootText, child.Path)),
			Format:  ingest.SourceFormatJSONL,
		}
		if child.Parent != "" {
			parent := ingest.SessionID(child.Parent)
			source.Parent = &parent
		}
		at.Stored.Children = append(at.Stored.Children, source)
	}
	return at
}

func claudeLocatorPath(t *testing.T, rootText, relative string) string {
	t.Helper()
	if rootText == "" {
		return relative
	}
	return filepath.Join(rootText, relative)
}

func claudeLocatorMemberIDs(members []ingest.LocatedMember) []ingest.SessionID {
	ids := make([]ingest.SessionID, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids
}

// assertClaudeOpSet asserts the exact set of paths an operation ran on below
// root, and that each expected path ran exactly once. A path touched but not
// named makes the total exceed the expected sum and fails, so this is set
// equality rather than a subset check.
func assertClaudeOpSet(t *testing.T, counting *testutil.CountingFS, rootText string, op testutil.FSOp, name string, want []string) {
	t.Helper()
	for _, path := range want {
		expanded := filepath.Join(rootText, path)
		if got := counting.Count(op, expanded); got != 1 {
			t.Errorf("%s ran on %q %d time(s), want exactly 1", name, path, got)
		}
	}
	if got, wantCount := counting.CountUnder(op, rootText), len(want); got != wantCount {
		t.Errorf("%s ran on %d path(s) under the source root, want the %d named path(s)", name, got, wantCount)
	}
}

func claudeLocatorContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var claudeSearchMethods = map[string]ingest.SearchMethod{
	"stored-path":      ingest.SearchStoredPath,
	"name-pattern":     ingest.SearchNamePattern,
	"sqlite-row":       ingest.SearchSQLiteRow,
	"unsupported":      ingest.SearchUnsupported,
	"harness-disabled": ingest.SearchHarnessDisabled,
	"failed":           ingest.SearchFailed,
}

var claudeUnreadableReasons = map[string]ingest.UnreadableReason{
	"SourceGone":   ingest.UnreadableSourceGone,
	"SourceFailed": ingest.UnreadableSourceFailed,
}

// claudeLocatorEvidenceCache is the in-memory double for the evidence cache the
// store provides. It records every call so a case can assert that the located
// build read only named paths and wrote nothing before the caller applied the
// deferred writes.
type claudeLocatorEvidenceCache struct {
	evidence    map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence
	wholeLoads  int
	forLoads    int
	forPaths    [][]ingest.ResolvedPath
	saves       int
	lastUpserts []ingest.ClaudeTranscriptEvidence
	lastDeletes []ingest.ResolvedPath
}

func newClaudeLocatorEvidenceCache() *claudeLocatorEvidenceCache {
	return &claudeLocatorEvidenceCache{evidence: make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence)}
}

func (c *claudeLocatorEvidenceCache) resetCalls() {
	c.wholeLoads = 0
	c.forLoads = 0
	c.forPaths = nil
	c.saves = 0
	c.lastUpserts = nil
	c.lastDeletes = nil
}

func (c *claudeLocatorEvidenceCache) LoadClaudeEvidence(context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	c.wholeLoads++
	out := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, len(c.evidence))
	for path, record := range c.evidence {
		out[path] = record
	}
	return out, nil
}

func (c *claudeLocatorEvidenceCache) LoadClaudeEvidenceFor(_ context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	c.forLoads++
	c.forPaths = append(c.forPaths, append([]ingest.ResolvedPath(nil), paths...))
	out := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, len(paths))
	for _, path := range paths {
		if record, ok := c.evidence[path]; ok {
			out[path] = record
		}
	}
	return out, nil
}

func (c *claudeLocatorEvidenceCache) SaveClaudeEvidence(_ context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) error {
	c.saves++
	c.lastUpserts = append([]ingest.ClaudeTranscriptEvidence(nil), upserts...)
	c.lastDeletes = append([]ingest.ResolvedPath(nil), deletes...)
	for _, record := range upserts {
		c.evidence[record.SourcePath] = record
	}
	for _, path := range deletes {
		delete(c.evidence, path)
	}
	return nil
}

func (c *claudeLocatorEvidenceCache) savedPath(path ingest.ResolvedPath) bool {
	for _, record := range c.lastUpserts {
		if record.SourcePath == path {
			return true
		}
	}
	return false
}

// claudeLocatorDerivedLookup answers the two identity counts a scoped teammate
// link decision reads. The fixture states the counts a correct store would
// return for the tree.
type claudeLocatorDerivedLookup struct {
	declarers    int
	spawners     int
	spawnerPaths []string
}

func (d *claudeLocatorDerivedLookup) CountClaudeIdentityDeclarers(context.Context, ingest.ClaudeTeammateIdentity) (int, error) {
	return d.declarers, nil
}

func (d *claudeLocatorDerivedLookup) CountClaudeIdentitySpawners(context.Context, ingest.ClaudeTeammateIdentity) (int, error) {
	return d.spawners, nil
}

func (d *claudeLocatorDerivedLookup) ClaudeIdentitySpawnerPaths(context.Context, string, string) ([]string, error) {
	return append([]string(nil), d.spawnerPaths...), nil
}

func (d *claudeLocatorDerivedLookup) CountCodexRolloutsBySession(context.Context, ingest.SessionID) (int, error) {
	return 0, nil
}

func (d *claudeLocatorDerivedLookup) CodexRolloutPaths(context.Context, ingest.SessionID) ([]ingest.ResolvedPath, error) {
	return nil, nil
}

var _ ingest.DerivedLookupReader = (*claudeLocatorDerivedLookup)(nil)

// claudeLocatorStoredLookup answers whether a session id is already stored, the
// one question the teammate step asks of the store before pointing at a
// spawner that is not in the batch.
type claudeLocatorStoredLookup struct {
	stored map[ingest.SessionID]struct{}
}

func (l *claudeLocatorStoredLookup) LookupSessionLocation(_ context.Context, id ingest.SessionID) (string, string, error) {
	if l.stored == nil {
		return "", "", nil
	}
	if _, ok := l.stored[id]; ok {
		return "fake-host", "", nil
	}
	return "", "", nil
}
