package ingest_test

// Parity between the full Codex walk and the two paths a scoped run uses: the
// by-name lookup and the located-source build. Every case builds its own source
// tree, drives the production adapter over it, and asserts the exact set of
// filesystem operations the step performs — so a lookup that reads a third date
// folder, a build that walks the tree, or a second file-name parser fails here
// rather than in review.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/codex_session_locator.yaml
var codexSessionLocatorYAML []byte

// codexLocatorFixture is the typed case model. `now` anchors the two-folder
// lookup window, so every case names the folders it expects instead of relying
// on the wall clock.
type codexLocatorFixture struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Now           string             `yaml:"now"`
	Cases         []codexLocatorCase `yaml:"cases"`
}

type codexLocatorCase struct {
	Name    string               `yaml:"name"`
	Roots   []string             `yaml:"roots"`
	Files   []codexLocatorFile   `yaml:"files"`
	Lookup  *codexLocatorLookup  `yaml:"lookup"`
	Located *codexLocatorLocated `yaml:"located"`
}

// codexLocatorFile is one file the case's source tree holds. A rollout's body is
// generated from its declared session_meta fields; an index or a stray file
// carries a literal body.
type codexLocatorFile struct {
	Path         string `yaml:"path"`
	Kind         string `yaml:"kind"` // rollout | index | stray
	SessionID    string `yaml:"sessionID"`
	ParentID     string `yaml:"parentID"`
	NestedParent bool   `yaml:"nestedParent"`
	CWD          string `yaml:"cwd"`
	Body         string `yaml:"body"`
}

type codexLocatorLookup struct {
	ID      string   `yaml:"id"`
	Hits    []string `yaml:"hits"`
	Folders []string `yaml:"folders"`
}

type codexLocatorLocated struct {
	ID        string              `yaml:"id"`
	Path      string              `yaml:"path"`
	Parent    string              `yaml:"storedParent"`
	Children  []codexLocatorChild `yaml:"storedChildren"`
	FaultRead bool                `yaml:"faultRead"`
	Reads     []string            `yaml:"reads"`
	Stats     []string            `yaml:"stats"`
	Expect    codexLocatorExpect  `yaml:"expect"`
}

type codexLocatorChild struct {
	ID             string `yaml:"id"`
	Harness        string `yaml:"harness"`
	Path           string `yaml:"path"`
	Parent         string `yaml:"parent"`
	FaultRead      bool   `yaml:"faultRead"`
	Unreadable     string `yaml:"unreadable"` // "" = readable; SourceGone | SourceFailed
	DetailContains string `yaml:"detailContains"`
}

type codexLocatorExpect struct {
	SourceGone    bool   `yaml:"sourceGone"`
	ErrorContains string `yaml:"errorContains"`
	ProbeKind     string `yaml:"probeKind"` // "" = no probe
	ProbeID       string `yaml:"probeID"`
}

func loadCodexSessionLocatorFixture(t *testing.T) (codexLocatorFixture, time.Time) {
	t.Helper()
	var fixture codexLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(codexSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode codex session locator fixture: %v", err)
	}
	anchor, err := time.Parse(time.RFC3339, fixture.Now)
	if err != nil {
		t.Fatalf("codex session locator fixture now %q: %v", fixture.Now, err)
	}
	for _, testCase := range fixture.Cases {
		if len(testCase.Roots) == 0 {
			t.Fatalf("case %q declares no source root", testCase.Name)
		}
		for _, file := range testCase.Files {
			switch file.Kind {
			case "rollout", "index", "stray":
			default:
				t.Fatalf("case %q file %q has an undeclared kind %q", testCase.Name, file.Path, file.Kind)
			}
			if file.Kind != "rollout" {
				continue
			}
			if _, err := ingest.NewSessionID(file.SessionID); err != nil {
				t.Fatalf("case %q file %q: %v", testCase.Name, file.Path, err)
			}
		}
		if testCase.Lookup == nil && testCase.Located == nil {
			t.Fatalf("case %q asserts nothing", testCase.Name)
		}
		for _, child := range storedChildrenOf(testCase) {
			if _, ok := codexLocatorHarnesses[child.Harness]; !ok {
				t.Fatalf("case %q child %q names an undeclared harness %q", testCase.Name, child.ID, child.Harness)
			}
			switch child.Unreadable {
			case "", "SourceGone", "SourceFailed":
			default:
				t.Fatalf("case %q child %q names an undeclared unreadable reason %q", testCase.Name, child.ID, child.Unreadable)
			}
		}
	}
	return fixture, anchor
}

var codexLocatorHarnesses = map[string]ingest.Harness{
	"codex":       ingest.HarnessCodex,
	"claude-code": ingest.HarnessClaudeCode,
}

func storedChildrenOf(testCase codexLocatorCase) []codexLocatorChild {
	if testCase.Located == nil {
		return nil
	}
	return testCase.Located.Children
}

// TestCodexSessionLocatorParity drives both scoped paths per case and asserts
// the exact operations each performed.
func TestCodexSessionLocatorParity(t *testing.T) {
	fixture, anchor := loadCodexSessionLocatorFixture(t)

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			tree := t.TempDir()
			mfs := testutil.NewMemFS()
			var cfg ingest.SourceConfig
			for _, root := range testCase.Roots {
				cfg.Paths = append(cfg.Paths, ingest.ResolvedPath(filepath.Join(tree, root)))
			}
			cfg.Enabled = true
			writeCodexLocatorTree(t, mfs, tree, testCase.Files)

			if testCase.Lookup != nil {
				runCodexLocatorLookup(t, mfs, tree, anchor, cfg, testCase)
			}
			if testCase.Located != nil {
				runCodexLocatorLocated(t, mfs, tree, anchor, cfg, testCase)
			}
		})
	}
}

func writeCodexLocatorTree(t *testing.T, mfs *testutil.MemFS, tree string, files []codexLocatorFile) {
	t.Helper()
	for _, file := range files {
		var body string
		switch file.Kind {
		case "rollout":
			body = codexLocatorRolloutBody(file)
		case "index", "stray":
			body = file.Body
		}
		if err := mfs.WriteFile(filepath.Join(tree, file.Path), []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture file %q: %v", file.Path, err)
		}
	}
}

// codexLocatorRolloutBody emits the two lines the adapter reads: a session_meta
// envelope carrying the declared identity, cwd and parent, then one user turn so
// the file is a plausible rollout.
func codexLocatorRolloutBody(file codexLocatorFile) string {
	meta := map[string]any{
		"id":          file.SessionID,
		"timestamp":   "2026-11-08T09:00:00Z",
		"cwd":         file.CWD,
		"cli_version": "0.133.0",
		"originator":  "codex_cli",
	}
	if file.ParentID != "" {
		if file.NestedParent {
			meta["source"] = map[string]any{
				"subagent": map[string]any{
					"thread_spawn": map[string]any{"parent_thread_id": file.ParentID},
				},
			}
		} else {
			meta["parent_thread_id"] = file.ParentID
		}
	}
	envelope, err := json.Marshal(map[string]any{
		"timestamp": "2026-11-08T09:00:00Z",
		"type":      "session_meta",
		"payload":   meta,
	})
	if err != nil {
		panic(err)
	}
	return string(envelope) + "\n" +
		`{"timestamp":"2026-11-08T09:00:01Z","type":"response_item","payload":{"type":"message","role":"user"}}` + "\n"
}

func runCodexLocatorLookup(t *testing.T, mfs *testutil.MemFS, tree string, anchor time.Time, cfg ingest.SourceConfig, testCase codexLocatorCase) {
	t.Helper()
	lookup := testCase.Lookup
	id, err := ingest.NewSessionID(lookup.ID)
	if err != nil {
		t.Fatalf("lookup id %q: %v", lookup.ID, err)
	}

	counting := testutil.NewCountingFS(mfs)
	adapter := ingest.NewCodexAdapter(counting, testutil.DefaultGitResolver(), salt.Salt{},
		ingest.WithCodexWindowClock(func() time.Time { return anchor }))

	sources, err := adapter.LookupSessionSources(context.Background(), cfg, id)
	if err != nil {
		t.Fatalf("LookupSessionSources(%s): %v", id, err)
	}

	var want []ingest.ResolvedPath
	for _, hit := range lookup.Hits {
		want = append(want, ingest.ResolvedPath(filepath.Join(tree, hit)))
	}
	if len(want) == 0 {
		want = nil
	}
	if got := sources.Sources; len(got) == 0 && len(want) == 0 {
		// Equal: no hit and no expected hit.
	} else if !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup hits = %v, want %v", got, want)
	}

	assertCodexExactOps(t, counting, tree, nil, nil, lookup.Folders)
	for _, write := range codexWriteOps {
		if got := counting.Total(write); got != 0 {
			t.Errorf("lookup performed %d %s operation(s); a lookup reads no tree it does not list and writes nothing", got, write)
		}
	}

	if len(sources.Searched) != len(cfg.Paths) {
		t.Fatalf("lookup recorded %d searched source(s), want one per configured root (%d)", len(sources.Searched), len(cfg.Paths))
	}
	for _, searched := range sources.Searched {
		if searched.Harness != ingest.HarnessCodex {
			t.Errorf("searched harness = %q, want codex", searched.Harness)
		}
		if searched.Method != ingest.SearchNamePattern {
			t.Errorf("searched method = %v, want the by-name pattern", searched.Method)
		}
		if searched.Reason != ingest.UnsupportedReason(0) || searched.Detail != "" {
			t.Errorf("searched entry carries a reason %v or detail %q; the lookup neither refused nor failed", searched.Reason, searched.Detail)
		}
		for _, folder := range lookup.Folders {
			if !strings.Contains(searched.Location, filepath.Join(tree, folder)) {
				t.Errorf("searched location %q does not name the listed folder %q", searched.Location, folder)
			}
		}
		if !strings.Contains(searched.Location, string(id)) || !strings.Contains(searched.Location, "rollout-") {
			t.Errorf("searched location %q does not name the pattern it matched", searched.Location)
		}
	}
}

func runCodexLocatorLocated(t *testing.T, mfs *testutil.MemFS, tree string, anchor time.Time, cfg ingest.SourceConfig, testCase codexLocatorCase) {
	t.Helper()
	located := testCase.Located
	id, err := ingest.NewSessionID(located.ID)
	if err != nil {
		t.Fatalf("located id %q: %v", located.ID, err)
	}

	counting := testutil.NewCountingFS(mfs)
	if located.FaultRead {
		counting.Fail(testutil.FSOpReadFile, filepath.Join(tree, located.Path), errors.New("the located source could not be read"))
	}
	at := ingest.LocatedSource{
		ID:   id,
		Path: ingest.ResolvedPath(filepath.Join(tree, located.Path)),
	}
	if located.Parent != "" {
		parent := ingest.SessionID(located.Parent)
		at.Stored.Parent = &parent
	}
	for _, child := range located.Children {
		childID, err := ingest.NewSessionID(child.ID)
		if err != nil {
			t.Fatalf("child id %q: %v", child.ID, err)
		}
		source := ingest.StoredSessionSource{
			ID:      childID,
			Harness: codexLocatorHarnesses[child.Harness],
			Path:    ingest.ResolvedPath(filepath.Join(tree, child.Path)),
			Format:  ingest.SourceFormatJSONL,
		}
		if child.Parent != "" {
			parent := ingest.SessionID(child.Parent)
			source.Parent = &parent
		}
		if child.FaultRead {
			counting.Fail(testutil.FSOpReadFile, string(source.Path), errors.New("the child source could not be read"))
		}
		at.Stored.Children = append(at.Stored.Children, source)
	}

	adapter := ingest.NewCodexAdapter(counting, testutil.DefaultGitResolver(), salt.Salt{},
		ingest.WithCodexWindowClock(func() time.Time { return anchor }))
	build, err := adapter.DiscoverSessionAt(context.Background(), cfg, at)

	switch {
	case located.Expect.SourceGone || located.Expect.ErrorContains != "":
		if err == nil {
			t.Fatalf("DiscoverSessionAt accepted the case: %#v", build)
		}
		if len(build.Members) != 0 || build.Deferred != nil {
			t.Fatalf("refused located source returned a build: %#v", build)
		}
		if located.Expect.SourceGone && !errors.Is(err, ingest.ErrSourceGone) {
			t.Errorf("error does not wrap ErrSourceGone: %v", err)
		}
		if !strings.Contains(err.Error(), located.Expect.ErrorContains) {
			t.Errorf("error %q does not name %q", err, located.Expect.ErrorContains)
		}
	default:
		if err != nil {
			t.Fatalf("DiscoverSessionAt: %v", err)
		}
		assertCodexLocatedMembers(t, mfs, anchor, cfg, testCase, &at, build)
		if _, ok := build.Deferred.(ingest.NoDeferredWrites); !ok {
			t.Errorf("deferred writes = %T, want none for a Codex source", build.Deferred)
		}
	}

	assertCodexExactOps(t, counting, tree, located.Reads, located.Stats, nil)
	for _, write := range codexWriteOps {
		if got := counting.Total(write); got != 0 {
			t.Errorf("located build performed %d %s operation(s); a scoped build writes nothing", got, write)
		}
	}
}

func assertCodexLocatedMembers(t *testing.T, mfs *testutil.MemFS, anchor time.Time, cfg ingest.SourceConfig, testCase codexLocatorCase, at *ingest.LocatedSource, build ingest.LocatedBuild) {
	t.Helper()
	located := testCase.Located
	if len(build.Members) == 0 {
		t.Fatal("located build returned no member")
	}

	member := build.Members[0]
	if member.ID != at.ID {
		t.Fatalf("first member id = %q, want the located id %q", member.ID, at.ID)
	}
	if member.Harness != ingest.HarnessCodex {
		t.Errorf("member harness = %q, want codex", member.Harness)
	}
	// The core owns the role and the outcome; the adapter must not decide them.
	if member.Role != ingest.CohortRole(0) || member.Outcome != ingest.LocateOutcome(0) {
		t.Errorf("member carries role %v / outcome %v; the core sets both", member.Role, member.Outcome)
	}
	if member.Source == nil {
		t.Fatalf("located member has no source: %#v", member)
	}
	assertCodexMemberMatchesDiscovery(t, mfs, anchor, cfg, member)

	switch located.Expect.ProbeKind {
	case "":
		if member.ParentProbe != nil {
			t.Fatalf("parent probe = %#v, want none", member.ParentProbe)
		}
	default:
		if member.ParentProbe == nil {
			t.Fatal("parent probe is missing")
		}
		if got, want := member.ParentProbe.Kind, codexProbeKind(t, located.Expect.ProbeKind); got != want {
			t.Errorf("parent probe kind = %v, want %v", got, want)
		}
		if member.ParentProbe.Harness != ingest.HarnessCodex {
			t.Errorf("parent probe harness = %q, want codex", member.ParentProbe.Harness)
		}
		if got, want := string(member.ParentProbe.ID), located.Expect.ProbeID; got != want {
			t.Errorf("parent probe id = %q, want %q", got, want)
		}
		if member.ParentProbe.Path != "" {
			t.Errorf("parent probe names a path %q; the codex probe names no path", member.ParentProbe.Path)
		}
	}

	if len(build.Members) != 1+len(located.Children) {
		t.Fatalf("build returned %d members, want the located source plus %d stored child(ren)", len(build.Members), len(located.Children))
	}
	for index, child := range located.Children {
		got := build.Members[index+1]
		childID := ingest.SessionID(child.ID)
		if got.ID != childID {
			t.Fatalf("child member %d id = %q, want %q", index, got.ID, childID)
		}
		if got.Harness != codexLocatorHarnesses[child.Harness] {
			t.Errorf("child %q harness = %q, want %q", child.ID, got.Harness, child.Harness)
		}
		switch child.Unreadable {
		case "":
			if got.Source == nil {
				t.Fatalf("child %q came back with no source: %#v", child.ID, got)
			}
			assertCodexMemberMatchesDiscovery(t, mfs, anchor, cfg, got)
		case "SourceGone":
			if got.Source != nil {
				t.Fatalf("child %q kept a source %#v; the file is gone", child.ID, got.Source)
			}
			if got.Unreadable != ingest.UnreadableSourceGone {
				t.Errorf("child %q reason = %v, want source gone", child.ID, got.Unreadable)
			}
		case "SourceFailed":
			if got.Source != nil {
				t.Fatalf("child %q kept a source %#v; the file is unreadable", child.ID, got.Source)
			}
			if got.Unreadable != ingest.UnreadableSourceFailed {
				t.Errorf("child %q reason = %v, want source failed", child.ID, got.Unreadable)
			}
			if !strings.Contains(got.Detail, child.DetailContains) {
				t.Errorf("child %q detail %q does not name %q", child.ID, got.Detail, child.DetailContains)
			}
		}
	}
}

// assertCodexMemberMatchesDiscovery is the parity assertion: the member a
// located build returns equals, field for field, the member a full discovery of
// the same tree returns for that id.
func assertCodexMemberMatchesDiscovery(t *testing.T, mfs *testutil.MemFS, anchor time.Time, cfg ingest.SourceConfig, member ingest.LocatedMember) {
	t.Helper()
	counting := testutil.NewCountingFS(mfs)
	adapter := ingest.NewCodexAdapter(counting, testutil.DefaultGitResolver(), salt.Salt{},
		ingest.WithCodexWindowClock(func() time.Time { return anchor }))
	discovered, err := adapter.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	var matches []ingest.DiscoveredSession
	for _, session := range discovered {
		if session.SessionID == member.ID {
			matches = append(matches, session)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("full discovery found %d sessions for %s, want exactly one", len(matches), member.ID)
	}
	if !reflect.DeepEqual(*member.Source, matches[0]) {
		t.Errorf("located member for %s differs from full discovery:\n located: %#v\n  walked: %#v", member.ID, *member.Source, matches[0])
	}
}

// codexWriteOps is every operation that mutates the source tree.
var codexWriteOps = []testutil.FSOp{
	testutil.FSOpWriteFile, testutil.FSOpMkdirAll, testutil.FSOpRename,
	testutil.FSOpRemove, testutil.FSOpRemoveAll, testutil.FSOpCopyFile,
}

// assertCodexExactOps asserts the operation sets exactly — every operation the
// step performed is declared, and every declared operation ran exactly once. A
// wider window, an extra read, or a tree walk fails here.
func assertCodexExactOps(t *testing.T, counting *testutil.CountingFS, tree string, reads, stats, readDirs []string) {
	t.Helper()
	for _, set := range []struct {
		name string
		op   testutil.FSOp
		want []string
	}{
		{"read", testutil.FSOpReadFile, reads},
		{"stat", testutil.FSOpStat, stats},
		{"list", testutil.FSOpReadDir, readDirs},
	} {
		if got := counting.Total(set.op); got != len(set.want) {
			t.Errorf("%s operations = %d, want %d (exact set, not a subset)", set.name, got, len(set.want))
		}
		for _, path := range set.want {
			abs := filepath.Join(tree, path)
			if got := counting.Count(set.op, abs); got != 1 {
				t.Errorf("%s of %q ran %d time(s), want exactly 1", set.name, path, got)
			}
		}
	}
	if got := counting.Total(testutil.FSOpWalkDir); got != 0 {
		t.Errorf("walked the source tree %d time(s); a scoped build and a by-name lookup never walk", got)
	}
}

func codexProbeKind(t *testing.T, name string) ingest.ProbeKind {
	t.Helper()
	switch name {
	case "ProbePath":
		return ingest.ProbePath
	case "ProbeRow":
		return ingest.ProbeRow
	case "ProbeCatalogWindow":
		return ingest.ProbeCatalogWindow
	default:
		t.Fatalf("fixture names an undeclared probe kind %q", name)
		return 0
	}
}
