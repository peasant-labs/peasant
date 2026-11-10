package ingest_test

// Parity between a full Pi discovery and the two paths a scoped run uses: the
// by-name lookup and the located-source build. Every case builds its own source
// tree, drives the production adapter over it, and asserts the exact filesystem
// operations each step performs -- so a lookup that reads a header, a build that
// lists a directory, or a located source that accepts another session's
// recording fails here rather than in review.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/pi_session_locator.yaml
var piSessionLocatorYAML []byte

type piLocatorFixture struct {
	RequiredNames []string        `yaml:"requiredNames"`
	AncestorProbe string          `yaml:"ancestorProbe"`
	Miss          piLocatorMiss   `yaml:"miss"`
	KnownGaps     []piLocatorGap  `yaml:"knownGaps"`
	Cases         []piLocatorCase `yaml:"cases"`
}

// piLocatorMiss is the exact wording a Pi miss renders: one searched row and the
// one-clause summary an `open` line carries.
type piLocatorMiss struct {
	Line   string `yaml:"line"`
	Clause string `yaml:"clause"`
}

// piLocatorGap declares one difference a located build can still have from a
// full walk, with the exact DiscoveredSession fields it may change. A case
// names the gap it exercises, and the runner refuses an allowance the gap does
// not declare.
type piLocatorGap struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Fields      []string `yaml:"fields"`
}

type piLocatorCase struct {
	Name    string            `yaml:"name"`
	Roots   []string          `yaml:"roots"`
	Files   []piLocatorFile   `yaml:"files"`
	Lookup  *piLocatorLookup  `yaml:"lookup"`
	Located *piLocatorLocated `yaml:"located"`
}

// piLocatorFile is one recording in the case's source tree. The file name is
// deliberately not the session id: Pi's id lives only in the header, and the
// fixture must never make a by-name lookup look plausible.
type piLocatorFile struct {
	Path      string `yaml:"path"`
	SessionID string `yaml:"sessionId"`
	Title     string `yaml:"title"`
	// ExtraTurn makes one copy larger than another with the same header id, so
	// the full walk's newest/largest winner is deterministic.
	ExtraTurn bool `yaml:"extraTurn"`
}

type piLocatorLookup struct {
	ID    string   `yaml:"id"`
	Roots []string `yaml:"roots"`
}

type piLocatorLocated struct {
	ID           string        `yaml:"id"`
	Path         string        `yaml:"path"`
	WantMembers  []string      `yaml:"wantMembers"`
	Ops          []piLocatorOp `yaml:"ops"`
	WantGone     bool          `yaml:"wantGone"`
	WantError    string        `yaml:"wantError"`
	KnownGap     string        `yaml:"knownGap"`
	AllowedDiffs []string      `yaml:"allowedDiffs"`
}

// piLocatorOp is one declared filesystem operation and its exact count.
type piLocatorOp struct {
	Op    string `yaml:"op"`
	Path  string `yaml:"path"`
	Count int    `yaml:"count"`
}

// piLocatorComparableFields is the closed set of DiscoveredSession fields a
// declared known gap may allow to differ. A field outside this set cannot be
// excused, so the fixture cannot silence a difference nobody reviewed.
var piLocatorComparableFields = map[string]struct{}{
	"sourcePath": {},
	"modTime":    {},
}

// piLocatorWriteOps is every operation that would mutate the source tree.
var piLocatorWriteOps = []testutil.FSOp{
	testutil.FSOpWriteFile, testutil.FSOpMkdirAll, testutil.FSOpRename,
	testutil.FSOpRemove, testutil.FSOpRemoveAll, testutil.FSOpCopyFile,
}

func loadPiLocatorFixture(t *testing.T) piLocatorFixture {
	t.Helper()
	var fixture piLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(piSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode Pi session locator fixture: %v", err)
	}
	if fixture.AncestorProbe != "absent" {
		t.Fatalf("ancestorProbe = %q; Pi records no parent it can derive a location for, so the one declared probe is absent", fixture.AncestorProbe)
	}
	if fixture.Miss.Line == "" || fixture.Miss.Clause == "" {
		t.Fatal("the fixture states no miss line or clause; a Pi miss must be pinned by its exact words")
	}
	if len(fixture.KnownGaps) == 0 {
		t.Fatal("Pi session locator fixture declares no knownGaps; every accepted scoped difference must be stated by name")
	}

	gaps := make(map[string]piLocatorGap, len(fixture.KnownGaps))
	for _, gap := range fixture.KnownGaps {
		if gap.ID == "" || gap.Description == "" {
			t.Fatalf("known gap %q states no id or description; both are required so a difference reads by substance", gap.ID)
		}
		if _, duplicate := gaps[gap.ID]; duplicate {
			t.Fatalf("known gap %q is declared twice", gap.ID)
		}
		for _, field := range gap.Fields {
			if _, declared := piLocatorComparableFields[field]; !declared {
				t.Fatalf("known gap %q allows field %q, which is outside the comparable set", gap.ID, field)
			}
		}
		gaps[gap.ID] = gap
	}

	exercised := make(map[string]bool, len(gaps))
	for _, testCase := range fixture.Cases {
		if testCase.Lookup == nil && testCase.Located == nil {
			t.Fatalf("Pi session locator case %q asserts nothing; add lookup or located", testCase.Name)
		}
		for _, file := range testCase.Files {
			if file.Path == "" {
				t.Fatalf("case %q has a recording with no path", testCase.Name)
			}
			if _, err := ingest.NewSessionID(file.SessionID); err != nil {
				t.Fatalf("case %q recording %q carries an invalid session id %q: %v", testCase.Name, file.Path, file.SessionID, err)
			}
		}
		if testCase.Located == nil {
			continue
		}
		located := testCase.Located
		if located.KnownGap == "" {
			if len(located.AllowedDiffs) > 0 {
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
	}
	for id := range gaps {
		if !exercised[id] {
			t.Fatalf("known gap %q has no case exercising it; a declared difference must be proven by a tree", id)
		}
	}
	return fixture
}

// TestPiSessionLocatorParity drives both scoped paths per case and asserts the
// exact operations each performed.
func TestPiSessionLocatorParity(t *testing.T) {
	fixture := loadPiLocatorFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			mfs := testutil.NewMemFS()
			writePiLocatorTree(t, mfs, tree, testCase.Files)
			if testCase.Lookup != nil {
				runPiLocatorLookup(t, mfs, tree, fixture.Miss, testCase)
			}
			if testCase.Located != nil {
				runPiLocatorLocated(t, mfs, tree, testCase)
			}
		})
	}
}

func writePiLocatorTree(t *testing.T, mfs *testutil.MemFS, tree string, files []piLocatorFile) {
	t.Helper()
	for _, file := range files {
		path := piLocatorPath(tree, file.Path)
		if err := mfs.WriteFile(path, []byte(piLocatorRecordingBody(file)), 0o600); err != nil {
			t.Fatalf("write recording %q: %v", file.Path, err)
		}
	}
}

// piLocatorRecordingBody emits a v3 recording: the header that carries the only
// copy of the session id, one user turn, an optional title, and an optional
// second turn that makes one copy of a shared id larger.
func piLocatorRecordingBody(file piLocatorFile) string {
	lines := []string{
		fmt.Sprintf(`{"type":"session","version":3,"id":%s,"cwd":"/workspace","timestamp":"2026-11-08T09:00:00Z"}`, piLocatorJSONString(file.SessionID)),
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-11-08T09:00:01Z","message":{"role":"user","content":"fixture turn","timestamp":1762506001000}}`,
	}
	if file.Title != "" {
		lines = append(lines, fmt.Sprintf(`{"type":"session_info","id":"n1","parentId":"u1","name":%s}`, piLocatorJSONString(file.Title)))
	}
	if file.ExtraTurn {
		lines = append(lines, `{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-11-08T09:00:02Z","message":{"role":"user","content":"a larger second copy","timestamp":1762506002000}}`)
	}
	return strings.Join(lines, "\n") + "\n"
}

func piLocatorJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func runPiLocatorLookup(t *testing.T, mfs *testutil.MemFS, tree string, miss piLocatorMiss, testCase piLocatorCase) {
	t.Helper()
	lookupCase := testCase.Lookup
	id, err := ingest.NewSessionID(lookupCase.ID)
	if err != nil {
		t.Fatalf("lookup id %q: %v", lookupCase.ID, err)
	}
	cfg := piLocatorConfig(t, tree, lookupCase.Roots)

	counting := testutil.NewCountingFS(mfs)
	adapter := ingest.NewPiAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	lookup, err := adapter.LookupSessionSources(context.Background(), cfg, id)
	if err != nil {
		t.Fatalf("LookupSessionSources(%s): %v", id, err)
	}

	// A Pi lookup can never hit: it has no by-name evidence to find a recording
	// with, and it must not manufacture a hit by reading headers.
	if len(lookup.Sources) != 0 {
		t.Errorf("lookup returned %d source(s); Pi has no by-name lookup, so it never claims a hit", len(lookup.Sources))
	}
	if len(lookup.Searched) != 1 {
		t.Fatalf("lookup recorded %d searched source(s), want exactly 1: %+v", len(lookup.Searched), lookup.Searched)
	}
	searched := lookup.Searched[0]
	if searched.Harness != ingest.HarnessPi {
		t.Errorf("searched harness = %q, want pi", searched.Harness)
	}
	if searched.Method != ingest.SearchUnsupported {
		t.Errorf("searched method = %v, want unsupported; a plain miss would claim the id does not exist", searched.Method)
	}
	if searched.Reason != ingest.IDOnlyInFileHeader {
		t.Errorf("searched reason = %v, want the id only in the file header", searched.Reason)
	}
	if searched.Location != "" || searched.Detail != "" {
		t.Errorf("searched entry names location %q or detail %q; it read nothing to name", searched.Location, searched.Detail)
	}

	// The whole point of the unsupported answer: it costs zero I/O. Not one
	// stat, listing, read, or header scan.
	for _, op := range testutil.AllFSOps {
		if got := counting.Total(op); got != 0 {
			t.Errorf("lookup performed %d %s operation(s); a Pi lookup reads nothing and never scans headers", got, op)
		}
	}

	// The exact words the user sees: one searched row naming pi and the
	// limitation, and the one-clause summary enough for a one-line renderer.
	scopeErr := &ingest.SessionScopeError{
		Misses:  []ingest.ScopeMiss{{ID: id, Reason: ingest.MissNotFound, Searched: lookup.Searched}},
		Command: "harvest",
		Store:   ingest.StoreAttached,
	}
	block := scopeErr.Error()
	if got := strings.Count(block, miss.Line); got != 1 {
		t.Errorf("miss block carries the sentence %q %d time(s), want exactly 1:\n%s", miss.Line, got, block)
	}
	if got, want := piLocatorSearchedRow(block, string(ingest.HarnessPi)), normalizedWords(string(ingest.HarnessPi)+" "+miss.Line); got != want {
		t.Errorf("miss block searched row = %q, want %q", got, want)
	}
	if reason := scopeErr.Reason(); strings.Count(reason, miss.Clause) != 1 {
		t.Errorf("miss reason %q does not carry the clause %q exactly once", reason, miss.Clause)
	}
}

// piLocatorSearchedRow returns the searched row naming one harness, with runs of
// spaces collapsed, so the assertion pins the words rather than a column width.
func piLocatorSearchedRow(block, harness string) string {
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, harness+" ") {
			return normalizedWords(trimmed)
		}
	}
	return ""
}

func normalizedWords(text string) string { return strings.Join(strings.Fields(text), " ") }

func runPiLocatorLocated(t *testing.T, mfs *testutil.MemFS, tree string, testCase piLocatorCase) {
	t.Helper()
	located := testCase.Located
	id, err := ingest.NewSessionID(located.ID)
	if err != nil {
		t.Fatalf("located id %q: %v", located.ID, err)
	}
	cfg := piLocatorConfig(t, tree, testCase.Roots)
	at := ingest.LocatedSource{ID: id, Path: ingest.ResolvedPath(piLocatorPath(tree, located.Path))}

	counting := testutil.NewCountingFS(mfs)
	adapter := ingest.NewPiAdapter(counting, testutil.NoGitResolver(), salt.Salt{})
	build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)

	if located.WantGone || located.WantError != "" {
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
	} else {
		if err != nil {
			t.Fatalf("DiscoverSessionAt: %v", err)
		}
		if _, ok := build.Deferred.(ingest.NoDeferredWrites); !ok {
			t.Errorf("deferred writes = %T, want none for a Pi source", build.Deferred)
		}
		assertPiLocatedBuild(t, mfs, cfg, testCase, &at, build)
	}

	assertPiLocatorOps(t, counting, tree, located.Ops)
	assertPiNoListingOrWrites(t, counting, tree)
}

func assertPiLocatedBuild(t *testing.T, mfs *testutil.MemFS, cfg ingest.SourceConfig, testCase piLocatorCase, at *ingest.LocatedSource, build ingest.LocatedBuild) {
	t.Helper()
	located := testCase.Located
	full := piLocatorDiscover(t, mfs, cfg)
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
		// Pi records no parent, so it derives no ancestor probe and spends no
		// I/O on one.
		if member.ParentProbe != nil {
			t.Errorf("member %s carries an ancestor probe %+v; the Pi adapter declares no probe kind", member.ID, member.ParentProbe)
		}
		membersByID[member.ID] = member
	}
	if len(build.Members) != len(located.WantMembers) {
		t.Fatalf("DiscoverSessionAt returned %d members, want %d (%v)", len(build.Members), len(located.WantMembers), piLocatorMemberIDs(build.Members))
	}
	for _, wantID := range located.WantMembers {
		member, ok := membersByID[ingest.SessionID(wantID)]
		if !ok {
			t.Errorf("DiscoverSessionAt omitted member %s", wantID)
			continue
		}
		want, discovered := fullByID[ingest.SessionID(wantID)]
		if !discovered {
			t.Errorf("a full discovery of the same tree does not find %s", wantID)
			continue
		}
		if member.Source == nil {
			t.Errorf("member %s has no source, but a full discovery finds it", wantID)
			continue
		}
		got := piLocatorSessionExcluding(*member.Source, located.AllowedDiffs)
		expected := piLocatorSessionExcluding(want, located.AllowedDiffs)
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("member %s differs from a full discovery beyond the declared gap:\n got: %#v\nwant: %#v", wantID, got, expected)
		}
	}

	assertPiKnownGapExercised(t, located, membersByID, fullByID)
}

// piLocatorDiscover runs the full walk the located build is compared against. It
// reads through the case's tree directly: the operation counts of the step under
// test must not see this comparison.
func piLocatorDiscover(t *testing.T, mfs *testutil.MemFS, cfg ingest.SourceConfig) []ingest.DiscoveredSession {
	t.Helper()
	adapter := ingest.NewPiAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return discovered
}

// assertPiKnownGapExercised makes a declared difference prove itself: at least
// one allowed field really differs, so a case cannot quietly stop covering its
// gap.
func assertPiKnownGapExercised(t *testing.T, located *piLocatorLocated, members map[ingest.SessionID]ingest.LocatedMember, full map[ingest.SessionID]ingest.DiscoveredSession) {
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
			if !reflect.DeepEqual(piLocatorFieldValue(*member.Source, field), piLocatorFieldValue(want, field)) {
				differs = true
			}
		}
	}
	if !differs {
		t.Errorf("known gap %s is declared but no allowed field differs; the case no longer exercises it", located.KnownGap)
	}
}

func piLocatorSessionExcluding(session ingest.DiscoveredSession, fields []string) ingest.DiscoveredSession {
	for _, field := range fields {
		switch field {
		case "sourcePath":
			session.SourcePath = ""
		case "modTime":
			session.ModTime = time.Time{}
		}
	}
	return session
}

func piLocatorFieldValue(session ingest.DiscoveredSession, field string) any {
	switch field {
	case "sourcePath":
		return session.SourcePath.String()
	case "modTime":
		return session.ModTime
	}
	return nil
}

// assertPiLocatorOps asserts the operation sets exactly for the two operations a
// Pi located build performs -- every operation it performed is declared with its
// count, and every declared count ran. A second file, an extra read, or a probe
// stat fails here.
func assertPiLocatorOps(t *testing.T, counting *testutil.CountingFS, tree string, ops []piLocatorOp) {
	t.Helper()
	want := map[testutil.FSOp]int{}
	for _, expected := range ops {
		op, ok := testutil.NewFSOp(expected.Op)
		if !ok {
			t.Fatalf("declared operation %q is outside the counted set", expected.Op)
		}
		if op != testutil.FSOpReadFile && op != testutil.FSOpStat {
			t.Fatalf("declared operation %q is not one a Pi located build performs", expected.Op)
		}
		want[op] += expected.Count
		if got := counting.Count(op, piLocatorPath(tree, expected.Path)); got != expected.Count {
			t.Errorf("%s of %q ran %d time(s), want %d", expected.Op, expected.Path, got, expected.Count)
		}
	}
	for _, op := range []testutil.FSOp{testutil.FSOpReadFile, testutil.FSOpStat} {
		if got := counting.CountUnder(op, tree); got != want[op] {
			t.Errorf("%s operations under the source root = %d, want the declared %d (exact set, not a subset)", op, got, want[op])
		}
	}
}

// assertPiNoListingOrWrites pins the two promises a lookup and a located build
// make: neither lists a directory or walks the tree, and neither writes.
func assertPiNoListingOrWrites(t *testing.T, counting *testutil.CountingFS, tree string) {
	t.Helper()
	if got := counting.CountUnder(testutil.FSOpReadDir, tree); got != 0 {
		t.Errorf("listed the source root %d time(s); a located build reads the named path and nothing else", got)
	}
	if got := counting.CountUnder(testutil.FSOpWalkDir, tree); got != 0 {
		t.Errorf("walked the source root %d time(s); a located build never walks", got)
	}
	for _, op := range piLocatorWriteOps {
		if got := counting.Total(op); got != 0 {
			t.Errorf("performed %d write operation(s); a located build writes nothing", got)
		}
	}
	if got := counting.Total(testutil.FSOpLstat); got != 0 {
		t.Errorf("performed %d lstat operation(s); a located build stats the paths it names", got)
	}
}

func piLocatorMemberIDs(members []ingest.LocatedMember) []ingest.SessionID {
	ids := make([]ingest.SessionID, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids
}

func piLocatorConfig(t *testing.T, tree string, roots []string) ingest.SourceConfig {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{"."}
	}
	cfg := ingest.SourceConfig{Enabled: true, Paths: make([]ingest.ResolvedPath, 0, len(roots))}
	for _, root := range roots {
		resolved, err := ingest.NewResolvedPath(piLocatorPath(tree, root))
		if err != nil {
			t.Fatalf("resolve configured root %q: %v", root, err)
		}
		cfg.Paths = append(cfg.Paths, resolved)
	}
	return cfg
}

// piLocatorPath resolves a fixture path against the case's tree. An absolute
// path is used as written, which is how a case presents a source outside every
// configured root.
func piLocatorPath(tree, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(tree, path)
}
