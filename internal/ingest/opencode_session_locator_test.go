package ingest_test

// Parity between the full OpenCode walk and the two paths a scoped run uses:
// the by-name lookup and the located-source build. Every case builds its own
// real source tree — a genuine SQLite database or a legacy JSON session file —
// drives the production adapter over it, and asserts the exact set of
// filesystem operations the step performs. A lookup that lists a directory, a
// located build that walks the tree, or a database opened without answering for
// an id fails here rather than in review.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/opencode_session_locator.yaml
var openCodeSessionLocatorYAML []byte

// openCodeLocatorFixture is the typed case model. Every case declares the tree
// it builds and the exact operations each step may perform.
type openCodeLocatorFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []openCodeLocatorCase `yaml:"cases"`
}

type openCodeLocatorCase struct {
	Name        string                    `yaml:"name"`
	Roots       []string                  `yaml:"roots"`
	Env         map[string]string         `yaml:"env"`
	Files       []openCodeLocatorFile     `yaml:"files"`
	Directories []string                  `yaml:"directories"`
	Databases   []openCodeLocatorDatabase `yaml:"databases"`
	Lookup      *openCodeLocatorLookup    `yaml:"lookup"`
	Located     *openCodeLocatorLocated   `yaml:"located"`
}

// openCodeLocatorFile is one legacy JSON file the case's tree holds. A session
// file's body is generated from the declared fields; a stray file carries a
// literal body.
type openCodeLocatorFile struct {
	Path      string `yaml:"path"`
	Kind      string `yaml:"kind"` // session | stray
	ID        string `yaml:"id"`
	ParentID  string `yaml:"parentID"`
	Title     string `yaml:"title"`
	Slug      string `yaml:"slug"`
	Agent     string `yaml:"agent"`
	Version   string `yaml:"version"`
	Directory string `yaml:"directory"`
	CreatedMS int64  `yaml:"createdMS"`
	Body      string `yaml:"body"`
}

// openCodeLocatorDatabase is one real SQLite database materialized into the
// case's tree from a named synthetic source, then arranged exactly as the case
// declares. Parent links live on the session row, so the case sets them with
// one UPDATE on the same real file production reads.
type openCodeLocatorDatabase struct {
	Path        string                  `yaml:"path"`
	Fixture     string                  `yaml:"fixture"`
	ParentLinks []openCodeLocatorParent `yaml:"parentLinks"`
}

type openCodeLocatorParent struct {
	ID     string `yaml:"id"`
	Parent string `yaml:"parent"`
}

type openCodeLocatorLookup struct {
	ID            string                    `yaml:"id"`
	WantSources   []string                  `yaml:"wantSources"`
	WantSearched  []openCodeLocatorSearched `yaml:"wantSearched"`
	WantOpens     []string                  `yaml:"wantOpens"`
	WantQueries   []openCodeLocatorQueries  `yaml:"wantQueries"`
	WantStats     []string                  `yaml:"wantStats"`
	WantError     string                    `yaml:"wantError"`
	RendersAsMiss bool                      `yaml:"rendersAsMiss"`
}

type openCodeLocatorSearched struct {
	Harness  string `yaml:"harness"`
	Method   string `yaml:"method"`
	Location string `yaml:"location"`
	Reason   string `yaml:"reason"`
}

type openCodeLocatorQueries struct {
	Path string   `yaml:"path"`
	IDs  []string `yaml:"ids"`
}

type openCodeLocatorLocated struct {
	ID             string                      `yaml:"id"`
	Path           string                      `yaml:"path"`
	StoredParent   string                      `yaml:"storedParent"`
	StoredChildren []openCodeLocatorChild      `yaml:"storedChildren"`
	WantMembers    []string                    `yaml:"wantMembers"`
	WantProbe      string                      `yaml:"wantProbe"` // "" = no probe
	WantProbeID    string                      `yaml:"wantProbeID"`
	AllowedDiffs   []string                    `yaml:"allowedDiffs"`
	WantError      string                      `yaml:"wantError"`
	WantSourceGone bool                        `yaml:"wantSourceGone"`
	WantUnreadable []openCodeLocatorUnreadable `yaml:"wantUnreadable"`
	WantReads      []string                    `yaml:"wantReads"`
	WantStats      []string                    `yaml:"wantStats"`
	WantReaddirs   []string                    `yaml:"wantReaddirs"`
	WantOpens      []string                    `yaml:"wantOpens"`
	WantQueries    []openCodeLocatorQueries    `yaml:"wantQueries"`
}

type openCodeLocatorChild struct {
	ID      string `yaml:"id"`
	Harness string `yaml:"harness"`
	Path    string `yaml:"path"`
	Parent  string `yaml:"parent"`
}

type openCodeLocatorUnreadable struct {
	ID             string `yaml:"id"`
	Reason         string `yaml:"reason"` // SourceGone | SourceFailed
	DetailContains string `yaml:"detailContains"`
}

// The closed set of session fields a located build may leave different from a
// full discovery, each because the full walk saw more of the tree than the one
// located source can.
const (
	// openCodeAllowedDiffParentUUID is a child of a parent that exists only
	// outside the searchable source: the full walk clears the link because it
	// did not discover the parent, while the located build keeps it and declares
	// the one probe that can find it.
	openCodeAllowedDiffParentUUID = "parentUUID"
)

func loadOpenCodeSessionLocatorFixture(t *testing.T) openCodeLocatorFixture {
	t.Helper()
	var fixture openCodeLocatorFixture
	if err := testutil.DecodeNamedFixtureYAML(openCodeSessionLocatorYAML, &fixture); err != nil {
		t.Fatalf("decode opencode session locator fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		if len(testCase.Roots) == 0 {
			t.Fatalf("case %q declares no source root", testCase.Name)
		}
		if testCase.Lookup == nil && testCase.Located == nil {
			t.Fatalf("case %q asserts nothing", testCase.Name)
		}
		for _, file := range testCase.Files {
			switch file.Kind {
			case "session", "stray":
			default:
				t.Fatalf("case %q file %q has an undeclared kind %q", testCase.Name, file.Path, file.Kind)
			}
			if file.Kind != "session" {
				continue
			}
			if _, err := ingest.NewSessionID(file.ID); err != nil {
				t.Fatalf("case %q file %q: %v", testCase.Name, file.Path, err)
			}
		}
		for _, database := range testCase.Databases {
			if database.Fixture == "" {
				t.Fatalf("case %q database %q names no synthetic fixture", testCase.Name, database.Path)
			}
			for _, link := range database.ParentLinks {
				if link.ID == "" || link.Parent == "" {
					t.Fatalf("case %q database %q declares an incomplete parent link", testCase.Name, database.Path)
				}
			}
		}
		for _, diff := range allowedDiffsOf(testCase) {
			if diff != openCodeAllowedDiffParentUUID {
				t.Fatalf("case %q allows an undeclared difference %q", testCase.Name, diff)
			}
		}
	}
	return fixture
}

func allowedDiffsOf(testCase openCodeLocatorCase) []string {
	if testCase.Located == nil {
		return nil
	}
	return testCase.Located.AllowedDiffs
}

// TestOpenCodeSessionLocatorParity drives both scoped paths per case.
func TestOpenCodeSessionLocatorParity(t *testing.T) {
	fixture := loadOpenCodeSessionLocatorFixture(t)

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			tree := t.TempDir()
			cfg := openCodeLocatorConfig(t, tree, testCase.Roots)
			writeOpenCodeLocatorTree(t, tree, testCase)

			if testCase.Lookup != nil {
				runOpenCodeLocatorLookup(t, tree, cfg, testCase)
			}
			if testCase.Located != nil {
				runOpenCodeLocatorLocated(t, tree, cfg, testCase)
			}
		})
	}
}

func openCodeLocatorConfig(t *testing.T, tree string, roots []string) ingest.SourceConfig {
	t.Helper()
	cfg := ingest.SourceConfig{Enabled: true}
	for _, root := range roots {
		cfg.Paths = append(cfg.Paths, ingest.ResolvedPath(openCodeLocatorPath(tree, root)))
	}
	return cfg
}

// openCodeLocatorPath resolves a fixture path against the case tree. A "~" path
// is absolute and used as written.
func openCodeLocatorPath(tree, declared string) string {
	if strings.HasPrefix(declared, "~") || filepath.IsAbs(declared) {
		return declared
	}
	return filepath.Join(tree, declared)
}

func writeOpenCodeLocatorTree(t *testing.T, tree string, testCase openCodeLocatorCase) {
	t.Helper()
	for _, file := range testCase.Files {
		var body string
		switch file.Kind {
		case "session":
			body = openCodeLocatorSessionBody(t, file)
		case "stray":
			body = file.Body
		}
		path := openCodeLocatorPath(tree, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture directory for %q: %v", file.Path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture file %q: %v", file.Path, err)
		}
	}
	for _, directory := range testCase.Directories {
		if err := os.MkdirAll(openCodeLocatorPath(tree, directory), 0o700); err != nil {
			t.Fatalf("create fixture directory %q: %v", directory, err)
		}
	}
	for _, database := range testCase.Databases {
		materialized := testfixture.MaterializeByName(t, database.Fixture)
		data, err := os.ReadFile(materialized.Path)
		if err != nil {
			t.Fatalf("read materialized database for %q: %v", database.Path, err)
		}
		path := openCodeLocatorPath(tree, database.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture directory for database %q: %v", database.Path, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write fixture database %q: %v", database.Path, err)
		}
		applyOpenCodeLocatorParentLinks(t, path, database.Path, database.ParentLinks)
	}
}

// applyOpenCodeLocatorParentLinks sets the declared parent links on the real
// session table, so discovery and the located build read the same row.
func applyOpenCodeLocatorParentLinks(t *testing.T, path, declaredPath string, links []openCodeLocatorParent) {
	t.Helper()
	if len(links) == 0 {
		return
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open fixture database %q: %v", declaredPath, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close fixture database %q: %v", declaredPath, closeErr)
		}
	}()
	for _, link := range links {
		seen := 0
		if err := sqlitex.Execute(conn, `SELECT id FROM session WHERE id = ?1;`, &sqlitex.ExecOptions{Args: []any{link.ID}, ResultFunc: func(*sqlite.Stmt) error {
			seen++
			return nil
		}}); err != nil {
			t.Fatalf("read fixture session %q in database %q: %v", link.ID, declaredPath, err)
		}
		if seen != 1 {
			t.Fatalf("fixture database %q holds %d session row(s) for %q, want one to arrange", declaredPath, seen, link.ID)
		}
		if err := sqlitex.Execute(conn, `UPDATE session SET parent_id = ?1 WHERE id = ?2;`, &sqlitex.ExecOptions{Args: []any{link.Parent, link.ID}}); err != nil {
			t.Fatalf("set parent link on fixture database %q: %v", declaredPath, err)
		}
	}
}

// openCodeLocatorSessionBody emits the legacy JSON document the adapter reads.
func openCodeLocatorSessionBody(t *testing.T, file openCodeLocatorFile) string {
	t.Helper()
	document := map[string]any{"id": file.ID}
	if file.Slug != "" {
		document["slug"] = file.Slug
	}
	if file.Title != "" {
		document["title"] = file.Title
	}
	if file.Agent != "" {
		document["agent"] = file.Agent
	}
	if file.Version != "" {
		document["version"] = file.Version
	}
	if file.Directory != "" {
		document["directory"] = file.Directory
	}
	if file.ParentID != "" {
		document["parentID"] = file.ParentID
	}
	if file.CreatedMS > 0 {
		document["time"] = map[string]any{"created": file.CreatedMS}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode fixture session %q: %v", file.ID, err)
	}
	return string(encoded)
}

// openCodeLocatorRecorder records the filesystem operations one step performs,
// so an unneeded listing, walk or read fails the case instead of passing
// silently.
type openCodeLocatorRecorder struct {
	*ingest.OSFileSystem
	mu     sync.Mutex
	counts map[string]map[string]int
}

func newOpenCodeLocatorRecorder() *openCodeLocatorRecorder {
	return &openCodeLocatorRecorder{OSFileSystem: &ingest.OSFileSystem{}, counts: make(map[string]map[string]int)}
}

func (r *openCodeLocatorRecorder) record(op, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[op] == nil {
		r.counts[op] = make(map[string]int)
	}
	r.counts[op][path]++
}

func (r *openCodeLocatorRecorder) paths(op string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]int, len(r.counts[op]))
	for path, count := range r.counts[op] {
		copied[path] = count
	}
	return copied
}

func (r *openCodeLocatorRecorder) ReadFile(path string) ([]byte, error) {
	r.record("read", path)
	return r.OSFileSystem.ReadFile(path)
}

func (r *openCodeLocatorRecorder) ReadDir(path string) ([]os.DirEntry, error) {
	r.record("readdir", path)
	return r.OSFileSystem.ReadDir(path)
}

func (r *openCodeLocatorRecorder) Stat(path string) (os.FileInfo, error) {
	r.record("stat", path)
	return r.OSFileSystem.Stat(path)
}

func (r *openCodeLocatorRecorder) WalkDir(root string, fn fs.WalkDirFunc) error {
	r.record("walk", root)
	return r.OSFileSystem.WalkDir(root, fn)
}

func (r *openCodeLocatorRecorder) Open(path string) (io.ReadCloser, error) {
	r.record("open", path)
	return r.OSFileSystem.Open(path)
}

// openCodeLocatorOpens records every attempt to open a candidate database, by
// path, and the session ids each opened database was asked about.
type openCodeLocatorOpens struct {
	mu     sync.Mutex
	opened []string
	ids    map[string][]string
}

func newOpenCodeLocatorOpens() *openCodeLocatorOpens {
	return &openCodeLocatorOpens{ids: make(map[string][]string)}
}

func (o *openCodeLocatorOpens) opener() ingest.OpenCodeSQLiteSourceOpener {
	return func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		o.mu.Lock()
		o.opened = append(o.opened, path.String())
		o.mu.Unlock()
		source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if err != nil {
			return nil, err
		}
		return openCodeLocatorQueryRecorder{OpenCodeSQLiteSource: source, opens: o, path: path.String()}, nil
	}
}

func (o *openCodeLocatorOpens) recordQuery(path, id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids[path] = append(o.ids[path], id)
}

func (o *openCodeLocatorOpens) openedPaths() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	unique := make(map[string]struct{}, len(o.opened))
	for _, path := range o.opened {
		unique[path] = struct{}{}
	}
	if len(unique) == 0 {
		return nil
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (o *openCodeLocatorOpens) queried() map[string][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	copied := make(map[string][]string, len(o.ids))
	for path, ids := range o.ids {
		copied[path] = append([]string(nil), ids...)
	}
	return copied
}

// openCodeLocatorQueryRecorder records the session ids one database was asked
// about. A database that never opened has no entry, which is exactly how "a
// failed open yields no id query" becomes assertable.
type openCodeLocatorQueryRecorder struct {
	ingest.OpenCodeSQLiteSource
	opens *openCodeLocatorOpens
	path  string
}

func (r openCodeLocatorQueryRecorder) SessionRecords(ctx context.Context, request ingest.OpenCodeSessionRecordPageRequest) (ingest.OpenCodeSessionRecordPage, error) {
	if request.SessionID != nil {
		r.opens.recordQuery(r.path, request.SessionID.String())
	}
	return r.OpenCodeSQLiteSource.SessionRecords(ctx, request)
}

func openCodeLocatorAdapter(t *testing.T, fs *openCodeLocatorRecorder, opens *openCodeLocatorOpens, environment ingest.OpenCodeEnvironmentLookup) *ingest.OpenCodeAdapter {
	t.Helper()
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(fs, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, fs, opens.opener(), ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct candidate-capable adapter: %v", err)
	}
	return adapter
}

func runOpenCodeLocatorLookup(t *testing.T, tree string, cfg ingest.SourceConfig, testCase openCodeLocatorCase) {
	t.Helper()
	lookup := testCase.Lookup
	id, err := ingest.NewSessionID(lookup.ID)
	if err != nil {
		t.Fatalf("lookup id %q: %v", lookup.ID, err)
	}

	fs := newOpenCodeLocatorRecorder()
	opens := newOpenCodeLocatorOpens()
	adapter := openCodeLocatorAdapter(t, fs, opens, fixedCandidateEnvironment(testCase.Env))

	sources, lookupErr := adapter.LookupSessionSources(context.Background(), cfg, id)

	// The convention every adapter's lookup obeys: a reported failure is
	// recorded once, by the core, from the returned error. No entry may carry
	// the failure method.
	for _, searched := range sources.Searched {
		if searched.Method == ingest.SearchFailed {
			t.Errorf("lookup recorded a SearchFailed entry itself: the core appends it from the returned error, so one search is reported once")
		}
	}

	if lookup.WantError != "" {
		if lookupErr == nil {
			t.Fatalf("LookupSessionSources accepted the case: %#v", sources)
		}
		if !strings.Contains(lookupErr.Error(), lookup.WantError) {
			t.Errorf("lookup error %q does not name %q", lookupErr, lookup.WantError)
		}
		if len(sources.Sources) != 0 {
			t.Errorf("failed lookup returned %d hit(s), want none", len(sources.Sources))
		}
		if lookup.RendersAsMiss {
			assertOpenCodeLookupRendersAsMiss(t, tree, id, lookup, sources, lookupErr)
		}
	} else {
		if lookupErr != nil {
			t.Fatalf("LookupSessionSources(%s): %v", id, lookupErr)
		}
		if got, want := normalizePaths(sources.Sources), openCodeLocatorSet(tree, lookup.WantSources); !reflect.DeepEqual(got, want) {
			t.Errorf("lookup hits = %v, want %v", got, want)
		}
	}

	assertOpenCodeLocatorSearched(t, tree, sources.Searched, lookup.WantSearched)
	assertOpenCodeLocatorOpens(t, tree, opens, lookup.WantOpens)
	assertOpenCodeLocatorQueries(t, tree, opens, lookup.WantQueries, true)
	// A by-name lookup reads no transcript and lists no directory: it only
	// checks that each candidate database exists before asking it for its row.
	assertOpenCodeLocatorFSOps(t, tree, fs, nil, lookup.WantStats, nil)
}

// assertOpenCodeLocatorSearched pins the searched block: one entry per source
// format, each carrying the method the lookup actually used and, for a
// database, the location it read.
func assertOpenCodeLocatorSearched(t *testing.T, tree string, got []ingest.SearchedSource, want []openCodeLocatorSearched) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lookup recorded %d searched source(s), want %d: %#v", len(got), len(want), got)
	}
	for index, expected := range want {
		entry := got[index]
		if entry.Harness != ingest.HarnessOpenCode {
			t.Errorf("searched[%d].harness = %q, want opencode", index, entry.Harness)
		}
		if wantMethod := openCodeSearchMethod(t, expected.Method); entry.Method != wantMethod {
			t.Errorf("searched[%d].method = %v, want %v", index, entry.Method, wantMethod)
		}
		if wantReason := openCodeUnsupportedReason(t, expected.Reason); entry.Reason != wantReason {
			t.Errorf("searched[%d].reason = %v, want %v", index, entry.Reason, wantReason)
		}
		if expected.Location != "" {
			if wantLocation := openCodeLocatorPath(tree, expected.Location); entry.Location != wantLocation {
				t.Errorf("searched[%d].location = %q, want %q", index, entry.Location, wantLocation)
			}
		}
		if entry.Detail != "" {
			t.Errorf("searched[%d] carries a detail %q; an adapter never reports its own failure", index, entry.Detail)
		}
	}
}

func openCodeSearchMethod(t *testing.T, name string) ingest.SearchMethod {
	t.Helper()
	switch name {
	case "sqlite-row":
		return ingest.SearchSQLiteRow
	case "unsupported":
		return ingest.SearchUnsupported
	default:
		t.Fatalf("fixture names an undeclared search method %q", name)
		return 0
	}
}

func openCodeUnsupportedReason(t *testing.T, name string) ingest.UnsupportedReason {
	t.Helper()
	switch name {
	case "":
		return ingest.UnsupportedReason(0)
	case "IDOnlyInFileBody":
		return ingest.IDOnlyInFileBody
	default:
		t.Fatalf("fixture names an undeclared unsupported reason %q", name)
		return 0
	}
}

// assertOpenCodeLocatorOpens asserts the set of database paths the step opened.
// A database the candidate list does not name must never appear.
func assertOpenCodeLocatorOpens(t *testing.T, tree string, opens *openCodeLocatorOpens, want []string) {
	t.Helper()
	wantPaths := openCodeLocatorSet(tree, want)
	if got := opens.openedPaths(); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("database opens = %v, want %v (set equality, not a subset)", got, wantPaths)
	}
}

// assertOpenCodeLocatorQueries asserts which session ids each database was
// asked for. A lookup is exact with multiplicity, so a second row read for one
// candidate database is visible; a located build asserts the id set, because
// its bounded reads (presence, records) are implementation detail.
func assertOpenCodeLocatorQueries(t *testing.T, tree string, opens *openCodeLocatorOpens, want []openCodeLocatorQueries, exact bool) {
	t.Helper()
	declared := make(map[string][]string, len(want))
	for _, queries := range want {
		path := openCodeLocatorPath(tree, queries.Path)
		if _, duplicate := declared[path]; duplicate {
			t.Fatalf("fixture declares queries for %q twice", queries.Path)
		}
		declared[path] = queries.IDs
	}
	queried := opens.queried()
	for path, wanted := range declared {
		got := queried[path]
		if exact {
			if !reflect.DeepEqual(got, wanted) {
				t.Errorf("database %q was asked for %v, want %v (one row read per candidate database)", path, got, wanted)
			}
			continue
		}
		if gotSet, wantSet := distinctStrings(got), distinctStrings(wanted); !reflect.DeepEqual(gotSet, wantSet) {
			t.Errorf("database %q was asked for the id set %v, want %v", path, gotSet, wantSet)
		}
	}
	for path, ids := range queried {
		if _, ok := declared[path]; ok {
			continue
		}
		t.Errorf("database %q was asked for %v but the case declares no queries for it", path, ids)
	}
}

func distinctStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func openCodeLocatorPaths(tree string, declared []string) []string {
	if len(declared) == 0 {
		return nil
	}
	paths := make([]string, 0, len(declared))
	for _, path := range declared {
		paths = append(paths, openCodeLocatorPath(tree, path))
	}
	return paths
}

func normalizePaths(paths []ingest.ResolvedPath) []string {
	if len(paths) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		normalized = append(normalized, string(path))
	}
	return normalized
}

// assertOpenCodeLocatorFSOps asserts the filesystem operation sets exactly: the
// declared reads, stats and directory listings are the only ones that ran, and
// no source tree was walked. A wider window or an unneeded listing fails here.
func assertOpenCodeLocatorFSOps(t *testing.T, tree string, fs *openCodeLocatorRecorder, reads, stats, readdirs []string) {
	t.Helper()
	for _, set := range []struct {
		name string
		op   string
		want []string
	}{
		{"read", "read", reads},
		{"stat", "stat", stats},
		{"list", "readdir", readdirs},
	} {
		want := openCodeLocatorSet(tree, set.want)
		if got := pathKeys(fs.paths(set.op)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s paths = %v, want %v (exact set, not a subset)", set.name, got, want)
		}
	}
	if walked := pathKeys(fs.paths("walk")); len(walked) != 0 {
		t.Errorf("walked %v; a by-name lookup and a located build never walk", walked)
	}
}

func openCodeLocatorSet(tree string, declared []string) []string {
	paths := openCodeLocatorPaths(tree, declared)
	sort.Strings(paths)
	return paths
}

func pathKeys(counts map[string]int) []string {
	if len(counts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(counts))
	for path := range counts {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	return keys
}

func runOpenCodeLocatorLocated(t *testing.T, tree string, cfg ingest.SourceConfig, testCase openCodeLocatorCase) {
	t.Helper()
	located := testCase.Located
	id, err := ingest.NewSessionID(located.ID)
	if err != nil {
		t.Fatalf("located id %q: %v", located.ID, err)
	}

	at := ingest.LocatedSource{ID: id, Path: ingest.ResolvedPath(openCodeLocatorPath(tree, located.Path))}
	if located.StoredParent != "" {
		parent := ingest.SessionID(located.StoredParent)
		at.Stored.Parent = &parent
	}
	for _, child := range located.StoredChildren {
		childID, err := ingest.NewSessionID(child.ID)
		if err != nil {
			t.Fatalf("child id %q: %v", child.ID, err)
		}
		source := ingest.StoredSessionSource{
			ID:      childID,
			Harness: openCodeLocatorHarness(t, child.Harness),
			Path:    ingest.ResolvedPath(openCodeLocatorPath(tree, child.Path)),
			Format:  ingest.SourceFormatJSON,
		}
		if child.Parent != "" {
			parent := ingest.SessionID(child.Parent)
			source.Parent = &parent
		}
		at.Stored.Children = append(at.Stored.Children, source)
	}

	fs := newOpenCodeLocatorRecorder()
	opens := newOpenCodeLocatorOpens()
	adapter := openCodeLocatorAdapter(t, fs, opens, fixedCandidateEnvironment(testCase.Env))
	build, buildErr := adapter.DiscoverSessionAt(context.Background(), cfg, at)

	switch {
	case located.WantSourceGone || located.WantError != "":
		if buildErr == nil {
			t.Fatalf("DiscoverSessionAt accepted the case: %#v", build)
		}
		if len(build.Members) != 0 || build.Deferred != nil {
			t.Fatalf("refused located source returned a build: %#v", build)
		}
		if located.WantSourceGone && !errors.Is(buildErr, ingest.ErrSourceGone) {
			t.Errorf("error does not wrap ErrSourceGone: %v", buildErr)
		}
		if located.WantError != "" && !strings.Contains(buildErr.Error(), located.WantError) {
			t.Errorf("error %q does not name %q", buildErr, located.WantError)
		}
	default:
		if buildErr != nil {
			t.Fatalf("DiscoverSessionAt: %v", buildErr)
		}
		assertOpenCodeLocatedMembers(t, cfg, testCase, &at, build)
		if _, ok := build.Deferred.(ingest.NoDeferredWrites); !ok {
			t.Errorf("deferred writes = %T, want none for an OpenCode source", build.Deferred)
		}
	}

	assertOpenCodeLocatorOpens(t, tree, opens, located.WantOpens)
	assertOpenCodeLocatorQueries(t, tree, opens, located.WantQueries, false)
	assertOpenCodeLocatorFSOps(t, tree, fs, located.WantReads, located.WantStats, located.WantReaddirs)
}

func assertOpenCodeLocatedMembers(t *testing.T, cfg ingest.SourceConfig, testCase openCodeLocatorCase, at *ingest.LocatedSource, build ingest.LocatedBuild) {
	t.Helper()
	located := testCase.Located
	wantMembers := located.WantMembers
	if wantMembers == nil {
		wantMembers = []string{located.ID}
	}
	if len(build.Members) != len(wantMembers) {
		t.Fatalf("build returned %d members, want %d", len(build.Members), len(wantMembers))
	}
	unreadable := make(map[string]openCodeLocatorUnreadable, len(located.WantUnreadable))
	for _, entry := range located.WantUnreadable {
		unreadable[entry.ID] = entry
	}

	discovery := openCodeLocatorAdapter(t, newOpenCodeLocatorRecorder(), newOpenCodeLocatorOpens(), fixedCandidateEnvironment(testCase.Env))
	discovered, err := discovery.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	walked := make(map[string]ingest.DiscoveredSession, len(discovered))
	for _, session := range discovered {
		walked[string(session.SessionID)] = session
	}

	for index, member := range build.Members {
		wantID := wantMembers[index]
		if string(member.ID) != wantID {
			t.Fatalf("member %d id = %q, want %q", index, member.ID, wantID)
		}
		if member.Harness != ingest.HarnessOpenCode {
			t.Errorf("member %q harness = %q, want opencode", member.ID, member.Harness)
		}
		// The core owns the role and the outcome; the adapter decides neither.
		if member.Role != ingest.CohortRole(0) || member.Outcome != ingest.LocateOutcome(0) {
			t.Errorf("member %q carries role %v / outcome %v; the core sets both", member.ID, member.Role, member.Outcome)
		}
		if entry, ok := unreadable[wantID]; ok {
			if member.Source != nil {
				t.Fatalf("member %q kept a source %#v; the source is unreadable", wantID, member.Source)
			}
			wantReason := openCodeUnreadableReason(t, entry.Reason)
			if member.Unreadable != wantReason {
				t.Errorf("member %q reason = %v, want %v", wantID, member.Unreadable, wantReason)
			}
			if entry.DetailContains != "" && !strings.Contains(member.Detail, entry.DetailContains) {
				t.Errorf("member %q detail %q does not name %q", wantID, member.Detail, entry.DetailContains)
			}
			continue
		}
		if member.Source == nil {
			t.Fatalf("member %q has no source", wantID)
		}
		if member.Unreadable != ingest.UnreadableReason(0) {
			t.Errorf("member %q carries an unreadable reason %v with a source", wantID, member.Unreadable)
		}
		assertOpenCodeMemberMatchesDiscovery(t, member, walked, located.AllowedDiffs)
	}

	locatedMember := build.Members[0]
	switch located.WantProbe {
	case "":
		if locatedMember.ParentProbe != nil {
			t.Fatalf("parent probe = %#v, want none", locatedMember.ParentProbe)
		}
	default:
		if locatedMember.ParentProbe == nil {
			t.Fatal("parent probe is missing")
		}
		if got, want := locatedMember.ParentProbe.Kind, openCodeProbeKind(t, located.WantProbe); got != want {
			t.Errorf("parent probe kind = %v, want %v", got, want)
		}
		if locatedMember.ParentProbe.Harness != ingest.HarnessOpenCode {
			t.Errorf("parent probe harness = %q, want opencode", locatedMember.ParentProbe.Harness)
		}
		if got, want := string(locatedMember.ParentProbe.ID), located.WantProbeID; got != want {
			t.Errorf("parent probe id = %q, want %q", got, want)
		}
		if locatedMember.ParentProbe.Path != "" {
			t.Errorf("parent probe names a path %q; the opencode probe names no path", locatedMember.ParentProbe.Path)
		}
	}
}

// assertOpenCodeMemberMatchesDiscovery is the parity assertion: the member a
// located build returns equals, field for field, the session a full discovery
// of the same tree returns for that id. A case that documents a gap zeroes
// exactly the fields it names.
func assertOpenCodeMemberMatchesDiscovery(t *testing.T, member ingest.LocatedMember, walked map[string]ingest.DiscoveredSession, allowed []string) {
	t.Helper()
	found, ok := walked[string(member.ID)]
	if !ok {
		t.Fatalf("full discovery found no session for %s", member.ID)
	}
	got := *member.Source
	for _, diff := range allowed {
		switch diff {
		case openCodeAllowedDiffParentUUID:
			got.ParentUUID = nil
			found.ParentUUID = nil
		default:
			t.Fatalf("undeclared allowed difference %q", diff)
		}
	}
	if !reflect.DeepEqual(got, found) {
		t.Errorf("located member for %s differs from full discovery:\n located: %#v\n  walked: %#v", member.ID, got, found)
	}
}

func openCodeLocatorHarness(t *testing.T, name string) ingest.Harness {
	t.Helper()
	switch name {
	case string(ingest.HarnessOpenCode):
		return ingest.HarnessOpenCode
	case string(ingest.HarnessClaudeCode):
		return ingest.HarnessClaudeCode
	case "":
		return ""
	default:
		t.Fatalf("fixture names an undeclared harness %q", name)
		return ""
	}
}

func openCodeUnreadableReason(t *testing.T, name string) ingest.UnreadableReason {
	t.Helper()
	switch name {
	case "SourceGone":
		return ingest.UnreadableSourceGone
	case "SourceFailed":
		return ingest.UnreadableSourceFailed
	default:
		t.Fatalf("fixture names an undeclared unreadable reason %q", name)
		return 0
	}
}

func openCodeProbeKind(t *testing.T, name string) ingest.ProbeKind {
	t.Helper()
	switch name {
	case "ProbeRow":
		return ingest.ProbeRow
	default:
		t.Fatalf("fixture names an undeclared probe kind %q", name)
		return 0
	}
}

// assertOpenCodeLookupRendersAsMiss feeds the adapter's failure into the scope
// error the way the core does — one SearchFailed entry built from the returned
// error's first line — and asserts the rendered miss. A database that did not
// open must read as "could not be looked up in all sources" and name both the
// database and its first error line, never as a plain "was not found".
func assertOpenCodeLookupRendersAsMiss(t *testing.T, tree string, id ingest.SessionID, lookup *openCodeLocatorLookup, sources ingest.SourceLookup, lookupErr error) {
	t.Helper()
	detail := strings.SplitN(strings.TrimSpace(lookupErr.Error()), "\n", 2)[0]
	miss := ingest.ScopeMiss{ID: id, Reason: ingest.MissNotFound, Searched: sources.Searched}
	miss.Searched = append(miss.Searched, ingest.SearchedSource{
		Harness: ingest.HarnessOpenCode,
		Method:  ingest.SearchFailed,
		Detail:  detail,
	})
	scopeErr := &ingest.SessionScopeError{
		Misses:  []ingest.ScopeMiss{miss},
		Command: "harvest",
		Store:   ingest.StoreAttached,
	}
	reason := scopeErr.Reason()
	for _, want := range []string{"could not be looked up in all sources", "cannot search by id: opencode (legacy JSON)", detail} {
		if !strings.Contains(reason, want) {
			t.Errorf("rendered miss %q does not name %q", reason, want)
		}
	}
	for _, opened := range lookup.WantOpens {
		if location := openCodeLocatorPath(tree, opened); !strings.Contains(reason, location) && !strings.Contains(detail, location) {
			t.Errorf("rendered miss %q names no database path", reason)
		}
	}
}
