package ingest_test

// Parity between the full Strike walk and the two paths a scoped run uses: the
// by-name lookup and the located-source build. Every case builds its own source
// tree, drives the production adapter over it, and asserts the exact set of
// filesystem operations each step performs — so a lookup that searches a second
// directory, a build that lists a folder, or a second file-name parser fails
// here rather than in review.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/strike_session_locator.yaml
var strikeSessionLocatorYAML []byte

type strikeLocatorFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	KnownGaps     []strikeKnownGap    `yaml:"knownGaps"`
	Cases         []strikeLocatorCase `yaml:"cases"`
}

// strikeKnownGap declares one difference a located build can still have from a
// full walk, with the exact DiscoveredSession fields it may change. A case
// names the gap it exercises, and the runner refuses an allowance the gap does
// not declare.
type strikeKnownGap struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Fields      []string `yaml:"fields"`
}

type strikeLocatorCase struct {
	Name    string                `yaml:"name"`
	Roots   []string              `yaml:"roots"`
	Files   []strikeLocatorFile   `yaml:"files"`
	Lookup  *strikeLocatorLookup  `yaml:"lookup"`
	Located *strikeLocatorLocated `yaml:"located"`
}

// strikeLocatorFile is one node of the case's source tree.
type strikeLocatorFile struct {
	Path            string   `yaml:"path"`
	Kind            string   `yaml:"kind"` // transcript | sidecar | directory
	Children        []string `yaml:"children"`
	Title           string   `yaml:"title"`
	Text            string   `yaml:"text"`
	SessionID       string   `yaml:"sessionId"`
	ParentSessionID string   `yaml:"parentSessionId"`
	ForkedFrom      string   `yaml:"forkedFrom"`
}

type strikeLocatorLookup struct {
	ID            string   `yaml:"id"`
	Roots         []string `yaml:"roots"`
	Hits          []string `yaml:"hits"`
	FaultPath     string   `yaml:"faultPath"`
	ErrorContains string   `yaml:"errorContains"`
	SearchedRoots []string `yaml:"searchedRoots"`
}

type strikeLocatorLocated struct {
	ID                    string                    `yaml:"id"`
	Path                  string                    `yaml:"path"`
	StoredParent          string                    `yaml:"storedParent"`
	StoredChildren        []strikeLocatorChild      `yaml:"storedChildren"`
	WantMembers           []string                  `yaml:"wantMembers"`
	WantUnreadable        []strikeLocatorUnreadable `yaml:"wantUnreadable"`
	WantProbe             *strikeLocatorProbe       `yaml:"wantProbe"`
	Stats                 []string                  `yaml:"stats"`
	Reads                 []string                  `yaml:"reads"`
	WantGone              bool                      `yaml:"wantGone"`
	WantError             string                    `yaml:"wantError"`
	KnownGap      string   `yaml:"knownGap"`
	AllowedDiffs  []string `yaml:"allowedDiffs"`
	AbsentMembers []string `yaml:"absentMembers"`
}

type strikeLocatorChild struct {
	ID             string `yaml:"id"`
	Harness        string `yaml:"harness"`
	HarnessUnset   bool   `yaml:"harnessUnset"`
	Path           string `yaml:"path"`
	Unreadable     string `yaml:"unreadable"`
	DetailContains string `yaml:"detailContains"`
}

type strikeLocatorUnreadable struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason"`
	Detail string `yaml:"detail"`
}

type strikeLocatorProbe struct {
	Path string `yaml:"path"`
	Miss bool   `yaml:"miss"`
}

var strikeLocatorHarnesses = map[string]ingest.Harness{
	string(ingest.HarnessStrike):     ingest.HarnessStrike,
	string(ingest.HarnessClaudeCode): ingest.HarnessClaudeCode,
}

var strikeLocatorUnreadableReasons = map[string]ingest.UnreadableReason{
	"SourceGone":   ingest.UnreadableSourceGone,
	"SourceFailed": ingest.UnreadableSourceFailed,
}

// strikeLocatorComparableFields is the closed set of DiscoveredSession fields a
// declared known gap may allow to differ. A field outside this set cannot be
// excused, so the fixture cannot silence a difference nobody reviewed.
var strikeLocatorComparableFields = map[string]struct{}{
	"parentUUID":    {},
	"subagentPaths": {},
	"modTime":       {},
}

// strikeLocatorWriteOps is every operation that would mutate the source tree.
var strikeLocatorWriteOps = []testutil.FSOp{
	testutil.FSOpWriteFile, testutil.FSOpMkdirAll, testutil.FSOpRename,
	testutil.FSOpRemove, testutil.FSOpRemoveAll, testutil.FSOpCopyFile,
}

func loadStrikeLocatorFixture(t *testing.T) strikeLocatorFixture {
	t.Helper()
	var fixture strikeLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(strikeSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode strike session locator fixture: %v", err)
	}
	if len(fixture.KnownGaps) == 0 {
		t.Fatal("strike session locator fixture declares no knownGaps; every accepted scoped difference must be stated by name")
	}

	gaps := make(map[string]strikeKnownGap, len(fixture.KnownGaps))
	for _, gap := range fixture.KnownGaps {
		if gap.ID == "" || gap.Description == "" {
			t.Fatalf("known gap %q states no id or description; both are required so a difference reads by substance", gap.ID)
		}
		if _, duplicate := gaps[gap.ID]; duplicate {
			t.Fatalf("known gap %q is declared twice", gap.ID)
		}
		for _, field := range gap.Fields {
			if _, declared := strikeLocatorComparableFields[field]; !declared {
				t.Fatalf("known gap %q allows field %q, which is outside the comparable set", gap.ID, field)
			}
		}
		gaps[gap.ID] = gap
	}

	exercised := make(map[string]bool, len(gaps))
	for _, testCase := range fixture.Cases {
		if testCase.Lookup == nil && testCase.Located == nil {
			t.Fatalf("strike session locator case %q asserts nothing; add lookup or located", testCase.Name)
		}
		for _, file := range testCase.Files {
			switch file.Kind {
			case "transcript", "sidecar", "directory":
			default:
				t.Fatalf("case %q file %q has an undeclared kind %q", testCase.Name, file.Path, file.Kind)
			}
		}
		if testCase.Located == nil {
			continue
		}
		located := testCase.Located
		if located.KnownGap == "" {
			if len(located.AllowedDiffs) > 0 || len(located.AbsentMembers) > 0 {
				t.Fatalf("case %q declares an allowance but no known gap; a scoped difference needs a declared gap", testCase.Name)
			}
			continue
		}
		gap, declared := gaps[located.KnownGap]
		if !declared {
			t.Fatalf("case %q names undeclared known gap %q", testCase.Name, located.KnownGap)
		}
		exercised[located.KnownGap] = true
		for _, field := range located.AllowedDiffs {
			if !slices.Contains(gap.Fields, field) {
				t.Fatalf("case %q allows field %q, which known gap %q does not declare", testCase.Name, field, gap.ID)
			}
		}
		for _, id := range located.AbsentMembers {
			if _, err := ingest.NewSessionID(id); err != nil {
				t.Fatalf("case %q allows missing member %q: %v", testCase.Name, id, err)
			}
		}
	}
	for id := range gaps {
		if !exercised[id] {
			t.Fatalf("known gap %q has no case exercising it; a declared difference must be proven by a tree", id)
		}
	}
	return fixture
}

// TestStrikeSessionLocatorParity drives both scoped paths per case and asserts
// the exact operations each performed.
func TestStrikeSessionLocatorParity(t *testing.T) {
	fixture := loadStrikeLocatorFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			mfs := testutil.NewMemFS()
			writeStrikeLocatorTree(t, mfs, tree, testCase.Files)
			cfg := strikeLocatorConfig(t, tree, testCase.Roots)
			if testCase.Lookup != nil {
				runStrikeLocatorLookup(t, mfs, tree, testCase)
			}
			if testCase.Located != nil {
				runStrikeLocatorLocated(t, mfs, tree, cfg, testCase)
			}
		})
	}
}

func strikeLocatorConfig(t *testing.T, tree string, roots []string) ingest.SourceConfig {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{"."}
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: make([]ingest.ResolvedPath, 0, len(roots))}
	for _, root := range roots {
		resolved, err := ingest.NewResolvedPath(strikeLocatorPath(tree, root))
		if err != nil {
			t.Fatalf("resolve configured root %q: %v", root, err)
		}
		cfg.Paths = append(cfg.Paths, resolved)
	}
	return cfg
}

func writeStrikeLocatorTree(t *testing.T, mfs *testutil.MemFS, tree string, files []strikeLocatorFile) {
	t.Helper()
	for _, file := range files {
		path := strikeLocatorPath(tree, file.Path)
		switch file.Kind {
		case "transcript":
			if err := mfs.WriteFile(path, []byte(strikeLocatorTranscriptBody(file)), 0o600); err != nil {
				t.Fatalf("write transcript %q: %v", file.Path, err)
			}
		case "sidecar":
			if err := mfs.WriteFile(path, []byte(strikeLocatorSidecarBody(file)), 0o600); err != nil {
				t.Fatalf("write sidecar %q: %v", file.Path, err)
			}
		case "directory":
			if err := mfs.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("create directory %q: %v", file.Path, err)
			}
		}
	}
}

// strikeLocatorTranscriptBody emits the records the adapter reads from a
// transcript: one turn, an optional title, and one child.started record per
// named child, carrying the session the event belongs to.
func strikeLocatorTranscriptBody(file strikeLocatorFile) string {
	self := strings.TrimSuffix(filepath.Base(file.Path), ".jsonl")
	text := file.Text
	if text == "" {
		text = "fixture turn"
	}
	lines := []string{
		`{"type":"session.header","time":"2026-11-08T09:00:00Z"}`,
		`{"type":"user.message","time":"2026-11-08T09:00:01Z","data":{"sessionId":` + strikeLocatorJSONString(self) + `,"text":` + strikeLocatorJSONString(text) + `}}`,
	}
	if file.Title != "" {
		lines = append(lines, `{"type":"session.titled","time":"2026-11-08T09:00:02Z","data":{"sessionId":`+strikeLocatorJSONString(self)+`,"title":`+strikeLocatorJSONString(file.Title)+`}}`)
	}
	for index, child := range file.Children {
		lines = append(lines, fmt.Sprintf(
			`{"type":"child.started","time":"2026-11-08T09:00:%02dZ","data":{"sessionId":%s,"parentSessionId":%s}}`,
			3+index, strikeLocatorJSONString(child), strikeLocatorJSONString(self)))
	}
	return strings.Join(lines, "\n") + "\n"
}

func strikeLocatorSidecarBody(file strikeLocatorFile) string {
	id := file.SessionID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(file.Path), ".meta.json")
	}
	fields := []string{`"sessionId":` + strikeLocatorJSONString(id)}
	if file.ParentSessionID != "" {
		fields = append(fields, `"parentSessionId":`+strikeLocatorJSONString(file.ParentSessionID))
	}
	if file.ForkedFrom != "" {
		fields = append(fields, `"forkedFrom":`+strikeLocatorJSONString(file.ForkedFrom))
	}
	if file.Title != "" {
		fields = append(fields, `"title":`+strikeLocatorJSONString(file.Title))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

func strikeLocatorJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func runStrikeLocatorLookup(t *testing.T, mfs *testutil.MemFS, tree string, testCase strikeLocatorCase) {
	t.Helper()
	lookupCase := testCase.Lookup
	id, err := ingest.NewSessionID(lookupCase.ID)
	if err != nil {
		t.Fatalf("lookup id %q: %v", lookupCase.ID, err)
	}
	// A lookup case names its own roots: the case's tree root is not implied,
	// because a case can search a root that holds no session at all.
	cfg := strikeLocatorConfig(t, tree, lookupCase.Roots)
	roots := strikeLocatorRootPaths(tree, strikeLocatorRootsOrDefault(lookupCase.Roots))

	counting := testutil.NewCountingFS(mfs)
	if lookupCase.FaultPath != "" {
		counting.Fail(testutil.FSOpStat, strikeLocatorPath(tree, lookupCase.FaultPath), errors.New("the source path could not be observed"))
	}
	adapter := ingest.NewStrikeAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	lookup, err := adapter.LookupSessionSources(context.Background(), cfg, id)

	// The convention every adapter's lookup obeys: a reported failure is
	// recorded once, by the core, from the returned error. No entry may carry
	// the failure method.
	for _, searched := range lookup.Searched {
		if searched.Method == ingest.SearchFailed {
			t.Errorf("lookup recorded a SearchFailed entry itself: the core appends it from the returned error, so one search is reported once")
		}
	}

	if lookupCase.ErrorContains != "" {
		if err == nil {
			t.Fatalf("LookupSessionSources accepted the faulted stat: %#v", lookup)
		}
		if !strings.Contains(err.Error(), lookupCase.ErrorContains) {
			t.Errorf("lookup error %q does not name %q", err, lookupCase.ErrorContains)
		}
	} else if err != nil {
		t.Fatalf("LookupSessionSources(%s): %v", id, err)
	}

	// A hit from a root the lookup completed survives a later root's failure, so
	// the case's declared hits are compared in both outcomes.
	gotSources := make([]string, 0, len(lookup.Sources))
	for _, source := range lookup.Sources {
		gotSources = append(gotSources, source.String())
	}
	wantSources := make([]string, 0, len(lookupCase.Hits))
	for _, hit := range lookupCase.Hits {
		wantSources = append(wantSources, strikeLocatorPath(tree, hit))
	}
	sort.Strings(gotSources)
	sort.Strings(wantSources)
	if !reflect.DeepEqual(gotSources, wantSources) {
		t.Errorf("lookup sources = %v, want %v", gotSources, wantSources)
	}

	// One searched entry per root the lookup reached, each carrying the pattern
	// the adapter searched: the exact file the writer lays a session down as.
	// Never a resolved folder, and never a file that happens to exist.
	searchedRoots := roots
	if lookupCase.ErrorContains != "" {
		searchedRoots = strikeLocatorRootPaths(tree, lookupCase.SearchedRoots)
	}
	if len(lookup.Searched) != len(searchedRoots) {
		t.Fatalf("lookup recorded %d searched source(s), want %d: %+v", len(lookup.Searched), len(searchedRoots), lookup.Searched)
	}
	for index, root := range searchedRoots {
		searched := lookup.Searched[index]
		if searched.Harness != ingest.HarnessStrike {
			t.Errorf("searched[%d] harness = %q, want strike", index, searched.Harness)
		}
		if searched.Method != ingest.SearchNamePattern {
			t.Errorf("searched[%d] method = %v, want the by-name pattern", index, searched.Method)
		}
		if wantLocation := filepath.Join(root, string(id)+".jsonl"); searched.Location != wantLocation {
			t.Errorf("searched[%d].location = %q, want the pattern %q", index, searched.Location, wantLocation)
		}
		if searched.Reason != ingest.UnsupportedReason(0) || searched.Detail != "" {
			t.Errorf("searched[%d] carries a reason %v or detail %q; the lookup neither refused nor failed", index, searched.Reason, searched.Detail)
		}
	}

	stats := make([]string, 0, len(roots))
	for _, root := range roots {
		stats = append(stats, filepath.Join(root, string(id)+".jsonl"))
	}
	assertStrikeExactOps(t, counting, tree, nil, stats)
	assertStrikeNoListingOrWrites(t, counting, tree)
}

func runStrikeLocatorLocated(t *testing.T, mfs *testutil.MemFS, tree string, cfg ingest.SourceConfig, testCase strikeLocatorCase) {
	t.Helper()
	located := testCase.Located
	id, err := ingest.NewSessionID(located.ID)
	if err != nil {
		t.Fatalf("located id %q: %v", located.ID, err)
	}
	at := ingest.LocatedSource{
		ID:   id,
		Path: ingest.ResolvedPath(strikeLocatorPath(tree, located.Path)),
	}
	if located.StoredParent != "" {
		parent, err := ingest.NewSessionID(located.StoredParent)
		if err != nil {
			t.Fatalf("stored parent %q: %v", located.StoredParent, err)
		}
		at.Stored.Parent = &parent
	}
	for _, child := range located.StoredChildren {
		childID, err := ingest.NewSessionID(child.ID)
		if err != nil {
			t.Fatalf("stored child id %q: %v", child.ID, err)
		}
		harness := ingest.HarnessStrike
		if child.HarnessUnset {
			harness = ""
		} else if child.Harness != "" {
			declared, ok := strikeLocatorHarnesses[child.Harness]
			if !ok {
				t.Fatalf("stored child %q names an undeclared harness %q", child.ID, child.Harness)
			}
			harness = declared
		}
		at.Stored.Children = append(at.Stored.Children, ingest.StoredSessionSource{
			ID:      childID,
			Harness: harness,
			Path:    ingest.ResolvedPath(strikeLocatorPath(tree, child.Path)),
			Format:  ingest.SourceFormatJSONL,
		})
	}

	counting := testutil.NewCountingFS(mfs)
	adapter := ingest.NewStrikeAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)

	switch {
	case located.WantGone || located.WantError != "":
		if err == nil {
			t.Fatalf("DiscoverSessionAt accepted the case: %#v", build)
		}
		if len(build.Members) != 0 || build.Deferred != nil {
			t.Fatalf("refused located source returned a build: %#v", build)
		}
		if located.WantGone && !errors.Is(err, ingest.ErrSourceGone) {
			t.Errorf("error does not wrap ErrSourceGone: %v", err)
		}
		if !strings.Contains(err.Error(), located.WantError) {
			t.Errorf("error %q does not name %q", err, located.WantError)
		}
	default:
		if err != nil {
			t.Fatalf("DiscoverSessionAt: %v", err)
		}
		if _, ok := build.Deferred.(ingest.NoDeferredWrites); !ok {
			t.Errorf("deferred writes = %T, want none for a Strike source", build.Deferred)
		}
		assertStrikeLocatedBuild(t, mfs, tree, cfg, testCase, &at, build, counting)
	}

	assertStrikeExactOps(t, counting, tree, located.Reads, located.Stats)
	assertStrikeNoListingOrWrites(t, counting, tree)
}

func assertStrikeLocatedBuild(t *testing.T, mfs *testutil.MemFS, tree string, cfg ingest.SourceConfig, testCase strikeLocatorCase, at *ingest.LocatedSource, build ingest.LocatedBuild, counting *testutil.CountingFS) {
	t.Helper()
	located := testCase.Located
	full := strikeLocatorDiscover(t, mfs, cfg)
	fullByID := make(map[ingest.SessionID]ingest.DiscoveredSession, len(full))
	for _, session := range full {
		if _, duplicate := fullByID[session.SessionID]; duplicate {
			t.Fatalf("a full discovery of the case tree finds two sessions for %s; the tree must be unambiguous", session.SessionID)
		}
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
	if len(build.Members) != len(located.WantMembers) {
		t.Fatalf("DiscoverSessionAt returned %d members, want %d (%v)", len(build.Members), len(located.WantMembers), strikeLocatorMemberIDs(build.Members))
	}
	for _, wantID := range located.WantMembers {
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
			t.Errorf("member %s has no source, but a full discovery finds it", wantID)
			continue
		}
		got := strikeLocatorSessionExcluding(*member.Source, located.AllowedDiffs)
		expected := strikeLocatorSessionExcluding(want, located.AllowedDiffs)
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("member %s differs from a full discovery beyond the declared gap:\n got: %#v\nwant: %#v", wantID, got, expected)
		}
	}

	for _, unreadable := range located.WantUnreadable {
		member, ok := membersByID[ingest.SessionID(unreadable.ID)]
		if !ok {
			t.Errorf("DiscoverSessionAt dropped stored child %s instead of returning it as unreadable", unreadable.ID)
			continue
		}
		if member.Source != nil {
			t.Errorf("stored child %s came back with a source, want nil", unreadable.ID)
		}
		wantReason, ok := strikeLocatorUnreadableReasons[unreadable.Reason]
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

	assertStrikeAncestorProbe(t, at.ID, build.Members, located.WantProbe, tree, counting)
	assertStrikeKnownGapExercised(t, located, membersByID, fullByID)
}

// strikeLocatorDiscover runs the full walk the located build is compared
// against. It reads through the case's tree directly: the operation counts of
// the step under test must not see this comparison.
func strikeLocatorDiscover(t *testing.T, mfs *testutil.MemFS, cfg ingest.SourceConfig) []ingest.DiscoveredSession {
	t.Helper()
	adapter := ingest.NewStrikeAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return discovered
}

// assertStrikeAncestorProbe pins the one probe a located child can carry: a
// ProbePath naming the parent's exact file beside it, which the runner executes
// here so the probe's cost is part of the case's operation set.
func assertStrikeAncestorProbe(t *testing.T, located ingest.SessionID, members []ingest.LocatedMember, want *strikeLocatorProbe, tree string, counting *testutil.CountingFS) {
	t.Helper()
	for _, member := range members {
		switch {
		case member.ID != located:
			if member.ParentProbe != nil {
				t.Errorf("member %s carries an ancestor probe; only the located session's own parent is derived", member.ID)
			}
		case want == nil:
			if member.ParentProbe != nil {
				t.Errorf("located member %s carries a probe whose parent is stored or absent: %+v", located, member.ParentProbe)
			}
		default:
			if member.ParentProbe == nil {
				t.Fatalf("located member %s has no ancestor probe, want a path probe", located)
			}
			if member.ParentProbe.Kind != ingest.ProbePath {
				t.Errorf("probe kind = %v, want path", member.ParentProbe.Kind)
			}
			if member.ParentProbe.Harness != ingest.HarnessStrike {
				t.Errorf("probe harness = %v, want strike", member.ParentProbe.Harness)
			}
			if member.ParentProbe.ID != "" {
				t.Errorf("probe names an id %q; a path probe names the parent's file", member.ParentProbe.ID)
			}
			wantPath := strikeLocatorPath(tree, want.Path)
			if got := member.ParentProbe.Path.String(); got != wantPath {
				t.Errorf("probe path = %q, want %q", got, wantPath)
			}
			_, err := counting.Stat(wantPath)
			if want.Miss {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a probe the case says misses resolved (stat error %v)", err)
				}
				continue
			}
			if err != nil {
				t.Errorf("the declared probe's stat failed: %v", err)
			}
		}
	}
}

// assertStrikeKnownGapExercised makes a declared difference prove itself: every
// member a gap allows to be missing really is missing from the located build and
// present in the full walk, and at least one allowed field really differs, so a
// case cannot quietly stop covering its gap.
func assertStrikeKnownGapExercised(t *testing.T, located *strikeLocatorLocated, members map[ingest.SessionID]ingest.LocatedMember, full map[ingest.SessionID]ingest.DiscoveredSession) {
	t.Helper()
	if located.KnownGap == "" {
		return
	}
	differs := false
	for _, field := range located.AllowedDiffs {
		for id, member := range members {
			if member.Source == nil {
				continue
			}
			want, discovered := full[id]
			if !discovered {
				t.Errorf("known gap %s allows field %q to differ for %s, but a full discovery does not find it", located.KnownGap, field, id)
				continue
			}
			if !reflect.DeepEqual(strikeLocatorFieldValue(*member.Source, field), strikeLocatorFieldValue(want, field)) {
				differs = true
			}
		}
	}
	for _, id := range located.AbsentMembers {
		if _, present := members[ingest.SessionID(id)]; present {
			t.Errorf("known gap %s says %s cannot be reached, but the located build returned it", located.KnownGap, id)
		}
		if _, discovered := full[ingest.SessionID(id)]; !discovered {
			t.Errorf("known gap %s allows %s to be missing, but a full discovery does not find it either; the case proves nothing", located.KnownGap, id)
		}
	}
	if len(located.AllowedDiffs) > 0 && !differs {
		t.Errorf("known gap %s is declared but no allowed field differs; the case no longer exercises it", located.KnownGap)
	}
}

func strikeLocatorSessionExcluding(session ingest.DiscoveredSession, fields []string) ingest.DiscoveredSession {
	for _, field := range fields {
		switch field {
		case "parentUUID":
			session.ParentUUID = nil
		case "subagentPaths":
			session.SubagentPaths = nil
		case "modTime":
			session.ModTime = time.Time{}
		}
	}
	return session
}

func strikeLocatorFieldValue(session ingest.DiscoveredSession, field string) any {
	switch field {
	case "parentUUID":
		if session.ParentUUID == nil {
			return ""
		}
		return session.ParentUUID.String()
	case "subagentPaths":
		paths := make([]string, 0, len(session.SubagentPaths))
		for _, path := range session.SubagentPaths {
			paths = append(paths, path.String())
		}
		return paths
	case "modTime":
		return session.ModTime
	}
	return nil
}

// assertStrikeExactOps asserts the operation sets exactly — every operation the
// step performed is declared, and every declared operation ran exactly once. A
// second directory, an extra read, or a tree walk fails here.
func assertStrikeExactOps(t *testing.T, counting *testutil.CountingFS, tree string, reads, stats []string) {
	t.Helper()
	for _, set := range []struct {
		name string
		op   testutil.FSOp
		want []string
	}{
		{"read", testutil.FSOpReadFile, reads},
		{"stat", testutil.FSOpStat, stats},
	} {
		if got := counting.CountUnder(set.op, tree); got != len(set.want) {
			t.Errorf("%s operations under the source root = %d, want the %d named path(s) (exact set, not a subset)", set.name, got, len(set.want))
		}
		for _, path := range set.want {
			abs := strikeLocatorPath(tree, path)
			if got := counting.Count(set.op, abs); got != 1 {
				t.Errorf("%s of %q ran %d time(s), want exactly 1", set.name, path, got)
			}
		}
	}
}

// assertStrikeNoListingOrWrites pins the two promises a lookup and a located
// build make: neither lists a directory or walks the tree, and neither writes.
func assertStrikeNoListingOrWrites(t *testing.T, counting *testutil.CountingFS, tree string) {
	t.Helper()
	if got := counting.CountUnder(testutil.FSOpReadDir, tree); got != 0 {
		t.Errorf("listed the source root %d time(s); a by-name lookup and a located build never list a directory", got)
	}
	if got := counting.CountUnder(testutil.FSOpWalkDir, tree); got != 0 {
		t.Errorf("walked the source root %d time(s); a by-name lookup and a located build never walk", got)
	}
	for _, op := range strikeLocatorWriteOps {
		if got := counting.Total(op); got != 0 {
			t.Errorf("performed %d write operation(s); a lookup and a located build write nothing", got)
		}
	}
	if got := counting.Total(testutil.FSOpLstat); got != 0 {
		t.Errorf("performed %d lstat operation(s); a located build stats the paths it names", got)
	}
}

func strikeLocatorMemberIDs(members []ingest.LocatedMember) []ingest.SessionID {
	ids := make([]ingest.SessionID, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids
}

func strikeLocatorRootsOrDefault(roots []string) []string {
	if len(roots) == 0 {
		return []string{"."}
	}
	return roots
}

// strikeLocatorRootPaths resolves the configured roots in configuration order
// and collapses a root named twice, which is what the lookup searches.
func strikeLocatorRootPaths(tree string, roots []string) []string {
	seen := make(map[string]struct{}, len(roots))
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		path := strikeLocatorPath(tree, root)
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

// strikeLocatorPath resolves a fixture path against the case's tree. An absolute
// path is used as written, which is how a case presents a source outside every
// configured root.
func strikeLocatorPath(tree, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(tree, path)
}
