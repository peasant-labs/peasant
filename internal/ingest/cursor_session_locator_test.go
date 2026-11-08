package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/cursor_session_locator.yaml
var cursorSessionLocatorYAML []byte

const cursorLocatorAgentTranscriptsDir = "agent-transcripts"

type cursorLocatorFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Root          string              `yaml:"root"`
	Cases         []cursorLocatorCase `yaml:"cases"`
}

type cursorLocatorCase struct {
	Name        string              `yaml:"name"`
	Tree        []cursorTreeEntry   `yaml:"tree"`
	Directories []string            `yaml:"directories"`
	DiscoverAt  *cursorDiscoverCase `yaml:"discoverAt"`
	Lookup      *cursorLookupCase   `yaml:"lookup"`
}

type cursorTreeEntry struct {
	Workspace string   `yaml:"workspace"`
	Session   string   `yaml:"session"`
	Subagents []string `yaml:"subagents"`
	Title     string   `yaml:"title"`
	// NoRoot writes only the subagent files, so a case can place a session's
	// child under a second workspace without duplicating the root session.
	NoRoot bool `yaml:"noRoot"`
}

type cursorDiscoverCase struct {
	ID             string                 `yaml:"id"`
	Workspace      string                 `yaml:"workspace"`
	Parent         string                 `yaml:"parent"`
	Path           string                 `yaml:"path"`
	WantError      string                 `yaml:"wantError"`
	Stored         cursorStoredCase       `yaml:"stored"`
	WantMembers    []string               `yaml:"wantMembers"`
	WantUnreadable []cursorUnreadableCase `yaml:"wantUnreadable"`
	WantProbe      *cursorProbeCase       `yaml:"wantProbe"`
	WantStats      []string               `yaml:"wantStats"`
	WantReaddirs   []string               `yaml:"wantReaddirs"`
	WantReads      []string               `yaml:"wantReads"`
	ContextPaths   []string               `yaml:"contextPaths"`
}

type cursorStoredCase struct {
	Parent   string            `yaml:"parent"`
	Children []cursorChildCase `yaml:"children"`
}

type cursorChildCase struct {
	Workspace    string `yaml:"workspace"`
	Session      string `yaml:"session"`
	Parent       string `yaml:"parent"`
	Harness      string `yaml:"harness"`
	HarnessUnset bool   `yaml:"harnessUnset"`
}

type cursorUnreadableCase struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason"`
	Detail string `yaml:"detail"`
}

type cursorProbeCase struct {
	Kind string `yaml:"kind"`
	Path string `yaml:"path"`
}

type cursorLookupCase struct {
	ID           string               `yaml:"id"`
	Roots        []string             `yaml:"roots"`
	Fault        *cursorFaultCase     `yaml:"fault"`
	WantSources  []string             `yaml:"wantSources"`
	WantSearched []cursorSearchedCase `yaml:"wantSearched"`
	WantStats    []string             `yaml:"wantStats"`
	WantReaddirs []string             `yaml:"wantReaddirs"`
	WantReads    []string             `yaml:"wantReads"`
}

type cursorSearchedCase struct {
	Harness  string `yaml:"harness"`
	Method   string `yaml:"method"`
	Location string `yaml:"location"`
	Detail   string `yaml:"detail"`
}

type cursorFaultCase struct {
	Op     string `yaml:"op"`
	Path   string `yaml:"path"`
	Detail string `yaml:"detail"`
}

func loadCursorSessionLocatorFixture(t *testing.T) cursorLocatorFixture {
	t.Helper()
	var fixture cursorLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(cursorSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode cursor session locator fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		if testCase.DiscoverAt == nil && testCase.Lookup == nil {
			t.Fatalf("cursor session locator case %q asserts nothing; add discoverAt or lookup", testCase.Name)
		}
	}
	return fixture
}

// TestCursorSessionLocatorParity is the adapter-level guard: for one tree, a
// located source must reproduce exactly the sessions Discover records, and a
// by-name lookup must report exactly the places it searched and the paths it
// hit. No case carries an allowed-difference field, because Cursor has no known
// scoped gap.
func TestCursorSessionLocatorParity(t *testing.T) {
	fixture := loadCursorSessionLocatorFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			rootText := cursorLocatorExpandedPath(t, fixture.Root)
			root, err := ingest.NewResolvedPath(rootText)
			if err != nil {
				t.Fatalf("resolve fixture root: %v", err)
			}
			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{root}, Enabled: true}
			if testCase.DiscoverAt != nil {
				t.Run("discover-at", func(t *testing.T) {
					runCursorDiscoverAtParity(t, cfg, rootText, testCase)
				})
			}
			if testCase.Lookup != nil {
				t.Run("lookup", func(t *testing.T) {
					runCursorLookupParity(t, fixture.Root, rootText, testCase)
				})
			}
		})
	}
}

func runCursorDiscoverAtParity(t *testing.T, cfg ingest.SourceConfig, rootText string, testCase cursorLocatorCase) {
	t.Helper()
	discoverCase := testCase.DiscoverAt
	counting := newCursorLocatorCountingFS(t, rootText, testCase)
	adapter := ingest.NewCursorAdapter(counting, testutil.NoGitResolver(), salt.Salt{})

	full, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	at := cursorLocatorLocatedSource(t, rootText, discoverCase)

	counting.ResetCounts()
	build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)
	if discoverCase.WantError != "" {
		if err == nil {
			t.Fatalf("DiscoverSessionAt accepted a source the case says must be refused")
		}
		if !strings.Contains(err.Error(), discoverCase.WantError) {
			t.Fatalf("DiscoverSessionAt error %q does not name %q", err, discoverCase.WantError)
		}
		assertCursorOpSet(t, counting, rootText, testutil.FSOpStat, "stat", discoverCase.WantStats)
		assertCursorOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", discoverCase.WantReaddirs)
		assertCursorOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", discoverCase.WantReads)
		if walks := counting.CountUnder(testutil.FSOpWalkDir, rootText); walks != 0 {
			t.Fatalf("a refused located source walked the root %d time(s)", walks)
		}
		return
	}
	if err != nil {
		t.Fatalf("DiscoverSessionAt: %v", err)
	}
	if _, ok := build.Deferred.(ingest.NoDeferredWrites); !ok {
		t.Fatalf("DiscoverSessionAt returned %T, want bare discovery writes (DeferredWrites applied by the caller only)", build.Deferred)
	}
	if err := build.Deferred.Apply(context.Background()); err != nil {
		t.Fatalf("apply bare discovery writes: %v", err)
	}

	if discoverCase.WantProbe != nil {
		if discoverCase.WantProbe.Kind != "path" {
			t.Fatalf("probe kind %q is outside the declared set this adapter uses", discoverCase.WantProbe.Kind)
		}
		if _, err := counting.Stat(cursorLocatorExpandedPath(t, discoverCase.WantProbe.Path)); err != nil {
			t.Fatalf("execute the declared ProbePath stat: %v", err)
		}
	}

	assertCursorOpSet(t, counting, rootText, testutil.FSOpStat, "stat", discoverCase.WantStats)
	assertCursorOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", discoverCase.WantReaddirs)
	assertCursorOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", discoverCase.WantReads)
	if walks := counting.CountUnder(testutil.FSOpWalkDir, rootText); walks != 0 {
		t.Fatalf("DiscoverSessionAt walked the source root %d time(s); a scoped build never walks", walks)
	}
	for _, contextPath := range discoverCase.ContextPaths {
		if !cursorContainsPath(discoverCase.WantReads, contextPath) && !cursorContainsPath(discoverCase.WantStats, contextPath) {
			t.Fatalf("context path %q is not in the case's read or stat set; name it where the recorder sees it", contextPath)
		}
	}

	fullByID := make(map[ingest.SessionID]ingest.DiscoveredSession, len(full))
	for _, session := range full {
		fullByID[session.SessionID] = session
	}
	membersByID := make(map[ingest.SessionID]ingest.LocatedMember, len(build.Members))
	for _, member := range build.Members {
		if _, duplicate := membersByID[member.ID]; duplicate {
			t.Fatalf("DiscoverSessionAt returned two members for %s", member.ID)
		}
		// The core owns the role and the outcome: only it knows whether the
		// member was requested, related-only, or an ancestor.
		if member.Role != ingest.CohortRole(0) || member.Outcome != ingest.LocateOutcome(0) {
			t.Errorf("member %s carries role %v / outcome %v; the core sets both", member.ID, member.Role, member.Outcome)
		}
		membersByID[member.ID] = member
	}
	if len(build.Members) != len(discoverCase.WantMembers) {
		t.Fatalf("DiscoverSessionAt returned %d members, want %d (%v)", len(build.Members), len(discoverCase.WantMembers), cursorLocatorMemberIDs(build.Members))
	}
	for _, wantID := range discoverCase.WantMembers {
		member, ok := membersByID[ingest.SessionID(wantID)]
		if !ok {
			t.Errorf("DiscoverSessionAt omitted member %s", wantID)
			continue
		}
		want, discovered := fullByID[ingest.SessionID(wantID)]
		if !discovered {
			continue // asserted through wantUnreadable
		}
		if member.Source == nil {
			t.Errorf("member %s has no source, but Discover found it", wantID)
			continue
		}
		if !reflect.DeepEqual(*member.Source, want) {
			t.Errorf("member %s differs from Discover field for field:\n got: %#v\nwant: %#v", wantID, *member.Source, want)
		}
	}

	for _, unreadable := range discoverCase.WantUnreadable {
		member, ok := membersByID[ingest.SessionID(unreadable.ID)]
		if !ok {
			t.Errorf("DiscoverSessionAt dropped stored child %s instead of returning it as unreadable", unreadable.ID)
			continue
		}
		if member.Source != nil {
			t.Errorf("stored child %s came back with a source, want nil", unreadable.ID)
		}
		wantReason, ok := cursorUnreadableReasons[unreadable.Reason]
		if !ok {
			t.Fatalf("fixture reason %q is outside the declared set", unreadable.Reason)
		}
		if member.Unreadable != wantReason {
			t.Errorf("stored child %s unreadable = %v, want %v", unreadable.ID, member.Unreadable, wantReason)
		}
		if wantReason == ingest.UnreadableSourceFailed && member.Detail == "" {
			t.Errorf("stored child %s is a read failure with no detail; name why it cannot be read", unreadable.ID)
		}
		if unreadable.Detail != "" && !strings.Contains(member.Detail, unreadable.Detail) {
			t.Errorf("stored child %s detail %q does not name %q", unreadable.ID, member.Detail, unreadable.Detail)
		}
	}

	assertCursorParentProbe(t, at.ID, build.Members, discoverCase.WantProbe)
}

func runCursorLookupParity(t *testing.T, defaultRoot, rootText string, testCase cursorLocatorCase) {
	t.Helper()
	lookupCase := testCase.Lookup
	roots := lookupCase.Roots
	if len(roots) == 0 {
		roots = []string{defaultRoot}
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: make([]ingest.ResolvedPath, 0, len(roots))}
	for _, root := range roots {
		resolved, err := ingest.NewResolvedPath(cursorLocatorExpandedPath(t, root))
		if err != nil {
			t.Fatalf("resolve configured root %q: %v", root, err)
		}
		cfg.Paths = append(cfg.Paths, resolved)
	}
	counting := newCursorLocatorCountingFS(t, rootText, testCase)
	adapter := ingest.NewCursorAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	if lookupCase.Fault != nil {
		op, ok := testutil.NewFSOp(lookupCase.Fault.Op)
		if !ok {
			t.Fatalf("fixture fault op %q is outside the counted set", lookupCase.Fault.Op)
		}
		counting.Fail(op, cursorLocatorExpandedPath(t, lookupCase.Fault.Path), errors.New(lookupCase.Fault.Detail))
	}

	lookup, err := adapter.LookupSessionSources(context.Background(), cfg, ingest.SessionID(lookupCase.ID))

	// The convention every adapter's lookup obeys: a reported failure is
	// recorded once, by the core, from the returned error. No entry may carry
	// the failure method.
	for _, searched := range lookup.Searched {
		if searched.Method == ingest.SearchFailed {
			t.Errorf("lookup recorded a SearchFailed entry itself: the core appends it from the returned error, so one search is reported once")
		}
	}

	if lookupCase.Fault != nil {
		if err == nil {
			t.Fatalf("LookupSessionSources returned no error after a failed %s; the failure must not vanish", lookupCase.Fault.Op)
		}
	} else if err != nil {
		t.Fatalf("LookupSessionSources: %v", err)
	}

	gotSources := make([]string, 0, len(lookup.Sources))
	for _, source := range lookup.Sources {
		gotSources = append(gotSources, source.String())
	}
	wantSources := make([]string, 0, len(lookupCase.WantSources))
	for _, source := range lookupCase.WantSources {
		wantSources = append(wantSources, cursorLocatorExpandedPath(t, source))
	}
	sort.Strings(gotSources)
	sort.Strings(wantSources)
	if !reflect.DeepEqual(gotSources, wantSources) {
		t.Errorf("lookup sources = %v, want %v", gotSources, wantSources)
	}

	if len(lookup.Searched) != len(lookupCase.WantSearched) {
		t.Fatalf("lookup reported %d searched sources, want %d: %+v", len(lookup.Searched), len(lookupCase.WantSearched), lookup.Searched)
	}
	for i, want := range lookupCase.WantSearched {
		got := lookup.Searched[i]
		if want.Harness != "" && string(got.Harness) != want.Harness {
			t.Errorf("searched[%d] harness = %q, want %q", i, got.Harness, want.Harness)
		}
		method, ok := cursorSearchMethods[want.Method]
		if !ok {
			t.Fatalf("fixture method %q is outside the declared set", want.Method)
		}
		if got.Method != method {
			t.Errorf("searched[%d] method = %v, want %v", i, got.Method, method)
		}
		if got.Location != want.Location {
			t.Errorf("searched[%d] location = %q, want %q", i, got.Location, want.Location)
		}
		if got.Detail != want.Detail {
			t.Errorf("searched[%d] detail = %q, want %q", i, got.Detail, want.Detail)
		}
	}

	assertCursorOpSet(t, counting, rootText, testutil.FSOpStat, "stat", lookupCase.WantStats)
	assertCursorOpSet(t, counting, rootText, testutil.FSOpReadDir, "readdir", lookupCase.WantReaddirs)
	assertCursorOpSet(t, counting, rootText, testutil.FSOpReadFile, "read", lookupCase.WantReads)
	if walks := counting.CountUnder(testutil.FSOpWalkDir, rootText); walks != 0 {
		t.Fatalf("LookupSessionSources walked the source root %d time(s); a lookup never walks a workspace", walks)
	}
}

// assertCursorOpSet asserts the exact set of paths an operation ran on below
// root, and that each expected path ran exactly once. A path touched but not
// named makes the total exceed the expected sum and fails, so this is set
// equality rather than a subset check.
func assertCursorOpSet(t *testing.T, counting *testutil.CountingFS, rootText string, op testutil.FSOp, name string, want []string) {
	t.Helper()
	for _, path := range want {
		expanded := cursorLocatorExpandedPath(t, path)
		if got := counting.Count(op, expanded); got != 1 {
			t.Errorf("%s ran on %q %d time(s), want exactly 1", name, path, got)
		}
	}
	if got, wantCount := counting.CountUnder(op, rootText), len(want); got != wantCount {
		t.Errorf("%s ran on %d path(s) under the source root, want the %d named path(s)", name, got, wantCount)
	}
}

func assertCursorParentProbe(t *testing.T, located ingest.SessionID, members []ingest.LocatedMember, want *cursorProbeCase) {
	t.Helper()
	for _, member := range members {
		switch {
		case member.ID != located:
			if member.ParentProbe != nil {
				t.Errorf("child %s carries an ancestor probe; only the located subagent's own parent is derived", member.ID)
			}
		case want == nil:
			if member.ParentProbe != nil {
				t.Errorf("located member %s carries a probe whose parent is stored or absent: %+v", located, member.ParentProbe)
			}
		default:
			if member.ParentProbe == nil {
				t.Fatalf("located subagent %s has no ancestor probe, want %s", located, want.Kind)
			}
			if want.Kind != "path" || member.ParentProbe.Kind != ingest.ProbePath {
				t.Errorf("probe kind = %v, want path", member.ParentProbe.Kind)
			}
			if member.ParentProbe.Harness != ingest.HarnessCursor {
				t.Errorf("probe harness = %v, want cursor", member.ParentProbe.Harness)
			}
			if got, wantPath := member.ParentProbe.Path.String(), cursorLocatorExpandedPath(t, want.Path); got != wantPath {
				t.Errorf("probe path = %q, want %q", got, wantPath)
			}
		}
	}
}

func newCursorLocatorCountingFS(t *testing.T, rootText string, testCase cursorLocatorCase) *testutil.CountingFS {
	t.Helper()
	mfs := testutil.NewMemFS()
	for _, directory := range testCase.Directories {
		if err := mfs.MkdirAll(cursorLocatorExpandedPath(t, directory), 0755); err != nil {
			t.Fatalf("create directory %s: %v", directory, err)
		}
	}
	for _, entry := range testCase.Tree {
		data := cursorLocatorTranscript(entry.Title)
		if !entry.NoRoot {
			rootPath := filepath.Join(rootText, entry.Workspace, cursorLocatorAgentTranscriptsDir, entry.Session, entry.Session+".jsonl")
			if err := mfs.WriteFile(rootPath, data, 0644); err != nil {
				t.Fatalf("write root session %s: %v", entry.Session, err)
			}
		}
		for _, subagent := range entry.Subagents {
			subPath := filepath.Join(rootText, entry.Workspace, cursorLocatorAgentTranscriptsDir, entry.Session, "subagents", subagent+".jsonl")
			if err := mfs.WriteFile(subPath, data, 0644); err != nil {
				t.Fatalf("write subagent session %s: %v", subagent, err)
			}
		}
	}
	return testutil.NewCountingFS(mfs)
}

func cursorLocatorTranscript(title string) []byte {
	if title == "" {
		title = "Hello"
	}
	return []byte(`{"role":"user","timestamp":"2026-06-04T10:00:00Z","message":{"content":` + `"` + title + `"` + `}}` + "\n")
}

func cursorLocatorLocatedSource(t *testing.T, rootText string, testCase *cursorDiscoverCase) ingest.LocatedSource {
	t.Helper()
	path := cursorLocatorSessionPath(rootText, testCase.Workspace, testCase.Parent, testCase.ID)
	if testCase.Path != "" {
		path = cursorLocatorExpandedPath(t, testCase.Path)
	}
	at := ingest.LocatedSource{
		ID:   ingest.SessionID(testCase.ID),
		Path: ingest.ResolvedPath(path),
	}
	if testCase.Stored.Parent != "" {
		parent := ingest.SessionID(testCase.Stored.Parent)
		at.Stored.Parent = &parent
	}
	for _, child := range testCase.Stored.Children {
		harness := ingest.HarnessCursor
		if child.HarnessUnset {
			harness = ""
		} else if child.Harness != "" {
			harness = ingest.Harness(child.Harness)
		}
		at.Stored.Children = append(at.Stored.Children, ingest.StoredSessionSource{
			ID:      ingest.SessionID(child.Session),
			Harness: harness,
			Path:    ingest.ResolvedPath(cursorLocatorSessionPath(rootText, child.Workspace, child.Parent, child.Session)),
			Format:  ingest.SourceFormatJSONL,
		})
	}
	return at
}

func cursorLocatorSessionPath(rootText, workspace, parent, id string) string {
	if parent != "" {
		return filepath.Join(rootText, workspace, cursorLocatorAgentTranscriptsDir, parent, "subagents", id+".jsonl")
	}
	return filepath.Join(rootText, workspace, cursorLocatorAgentTranscriptsDir, id, id+".jsonl")
}

func cursorLocatorExpandedPath(t testing.TB, path string) string {
	t.Helper()
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home for %q: %v", path, err)
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func cursorContainsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func cursorLocatorMemberIDs(members []ingest.LocatedMember) []ingest.SessionID {
	ids := make([]ingest.SessionID, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids
}

var cursorSearchMethods = map[string]ingest.SearchMethod{
	"stored-path":      ingest.SearchStoredPath,
	"name-pattern":     ingest.SearchNamePattern,
	"sqlite-row":       ingest.SearchSQLiteRow,
	"unsupported":      ingest.SearchUnsupported,
	"harness-disabled": ingest.SearchHarnessDisabled,
	"failed":           ingest.SearchFailed,
}

var cursorUnreadableReasons = map[string]ingest.UnreadableReason{
	"SourceGone":   ingest.UnreadableSourceGone,
	"SourceFailed": ingest.UnreadableSourceFailed,
}

// cursorCancelingFS cancels a lookup's context during the first root's listing,
// so a later iteration meets the cancellation after one root has completed.
type cursorCancelingFS struct {
	*testutil.MemFS
	root   string
	cancel context.CancelFunc
	fired  bool
}

func (f *cursorCancelingFS) ReadDir(path string) ([]os.DirEntry, error) {
	if !f.fired && path == f.root {
		f.fired = true
		f.cancel()
	}
	return f.MemFS.ReadDir(path)
}

// TestCursorLookupKeepsCompletedRootsOnCancellation pins the promise the method
// makes: a completed root's searched entry and hit survive a later
// cancellation, so the miss still names where the id was looked for.
func TestCursorLookupKeepsCompletedRootsOnCancellation(t *testing.T) {
	t.Parallel()
	rootText := cursorLocatorExpandedPath(t, "~/.cursor/projects")
	id := ingest.SessionID("11111111-1111-4111-8111-111111111111")
	mfs := testutil.NewMemFS()
	source := filepath.Join(rootText, "test-workspace-a", cursorLocatorAgentTranscriptsDir, string(id), string(id)+".jsonl")
	if err := mfs.WriteFile(source, cursorLocatorTranscript("Hello"), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceling := &cursorCancelingFS{MemFS: mfs, root: rootText, cancel: cancel}
	adapter := ingest.NewCursorAdapter(canceling, testutil.NoGitResolver(), salt.Salt{})

	first, err := ingest.NewResolvedPath(rootText)
	if err != nil {
		t.Fatalf("resolve first root: %v", err)
	}
	second, err := ingest.NewResolvedPath(filepath.Join(rootText, "..", "other-projects"))
	if err != nil {
		t.Fatalf("resolve second root: %v", err)
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{first, second}}

	lookup, err := adapter.LookupSessionSources(ctx, cfg, id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LookupSessionSources error = %v, want context.Canceled", err)
	}
	if len(lookup.Searched) != 1 {
		t.Fatalf("cancelled lookup reported %d searched entries, want the one completed root's entry: %+v", len(lookup.Searched), lookup.Searched)
	}
	if len(lookup.Sources) != 1 || lookup.Sources[0] != ingest.ResolvedPath(source) {
		t.Fatalf("cancelled lookup sources = %v, want the completed root's hit %q", lookup.Sources, source)
	}
}
