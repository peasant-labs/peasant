package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sessions_search.yaml
var sessionsSearchYAML []byte

//go:embed testdata/sessions_search.manifest.yaml
var sessionsSearchManifestYAML []byte

type searchFixtureDataDir string
type searchFixtureStore string
type searchFixtureListWrite string

const (
	searchDirectory    searchFixtureDataDir   = "directory"
	searchRegularFile  searchFixtureDataDir   = "regularFile"
	searchSeeded       searchFixtureStore     = "seeded"
	searchEmpty        searchFixtureStore     = "empty"
	searchNone         searchFixtureStore     = "none"
	searchNeedsRebuild searchFixtureStore     = "needsRebuild"
	searchNoIndex      searchFixtureStore     = "noIndex"
	searchListIDs      searchFixtureListWrite = "ids"
	searchListEmpty    searchFixtureListWrite = "empty"
	searchListMissing  searchFixtureListWrite = "none"
)

type searchFixtureSeed struct {
	ID      string           `yaml:"sessionID"`
	Hash    string           `yaml:"projectHash"`
	Path    string           `yaml:"projectPath"`
	Remote  *string          `yaml:"gitRemote"`
	Harness defaults.Harness `yaml:"harness"`
	Start   int64            `yaml:"startMs"`
	Tags    []string         `yaml:"tags"`
	Entries []string         `yaml:"entries"`
}

type searchFixtureSetup struct {
	DataDir      searchFixtureDataDir `yaml:"dataDir"`
	Store        searchFixtureStore   `yaml:"store"`
	SessionsFrom *struct {
		Path  string                 `yaml:"path"`
		Write searchFixtureListWrite `yaml:"write"`
		IDs   []string               `yaml:"ids"`
	} `yaml:"sessionsFrom"`
}

type searchFixtureCase struct {
	Name                  string                  `yaml:"name"`
	Setup                 searchFixtureSetup      `yaml:"setup"`
	Seeds                 []searchFixtureSeed     `yaml:"seeds"`
	Args                  []string                `yaml:"args"`
	WantExit              defaults.ExitCode       `yaml:"wantExit"`
	WantNoDBFile          bool                    `yaml:"wantNoDBFile"`
	WantEmptyCode         clicontract.EmptyCode   `yaml:"wantEmptyCode"`
	WantReasonContains    string                  `yaml:"wantReasonContains"`
	WantStdoutContains    []string                `yaml:"wantStdoutContains"`
	WantStdoutNotContains []string                `yaml:"wantStdoutNotContains"`
	WantStdoutEmpty       bool                    `yaml:"wantStdoutEmpty"`
	WantStderrContains    []string                `yaml:"wantStderrContains"`
	WantStderrNotContains []string                `yaml:"wantStderrNotContains"`
	WantStderrEmpty       bool                    `yaml:"wantStderrEmpty"`
	WantColumns           []string                `yaml:"wantColumns"`
	WantResults           []string                `yaml:"wantResults"`
	WantWarningCode       clicontract.WarningCode `yaml:"wantWarningCode"`
	WantSnippetSuffix     string                  `yaml:"wantSnippetSuffix"`
	WantSnippetRunes      int                     `yaml:"wantSnippetRunes"`
}

type searchParityCase struct {
	Query string `yaml:"query"`
	Limit string `yaml:"limit"`
}

type searchFixtureFamily struct {
	Seeds  []searchFixtureSeed `yaml:"seeds"`
	Cases  []searchFixtureCase `yaml:"cases"`
	Parity []searchParityCase  `yaml:"parity"`
}

func loadSessionsSearchFixtures(t *testing.T) searchFixtureFamily {
	t.Helper()
	var family searchFixtureFamily
	dec := yaml.NewDecoder(bytes.NewReader(sessionsSearchYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&family); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("expected one fixture document: %v", err)
	}
	var manifest testutil.RequiredNamesManifest
	if err := yaml.Unmarshal(sessionsSearchManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range family.Cases {
		names = append(names, c.Name)
		if len(c.WantStdoutContains)+len(c.WantStdoutNotContains)+len(c.WantStderrContains)+len(c.WantStderrNotContains)+len(c.WantColumns)+len(c.WantResults) == 0 && !c.WantStdoutEmpty && !c.WantStderrEmpty {
			t.Fatalf("%s declares no stream assertion", c.Name)
		}
		switch c.Setup.DataDir {
		case "", searchDirectory, searchRegularFile:
		default:
			t.Fatalf("%s: unknown dataDir %q", c.Name, c.Setup.DataDir)
		}
		switch c.Setup.Store {
		case searchSeeded, searchEmpty, searchNone, searchNeedsRebuild, searchNoIndex:
		default:
			t.Fatalf("%s: unknown store %q", c.Name, c.Setup.Store)
		}
		if c.Setup.SessionsFrom != nil {
			switch c.Setup.SessionsFrom.Write {
			case searchListIDs, searchListEmpty, searchListMissing:
			default:
				t.Fatalf("%s: unknown list write mode", c.Name)
			}
		}
		for _, seed := range append(append([]searchFixtureSeed{}, family.Seeds...), c.Seeds...) {
			if _, err := schema.NewProjectHash(seed.Hash); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			if _, err := schema.NewSessionID(seed.ID); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "sessions search"); err != nil {
		t.Fatal(err)
	}
	return family
}

func seedSearchFixtures(t *testing.T, db *store.Store, seeds []searchFixtureSeed) {
	t.Helper()
	for _, seed := range seeds {
		seedOne(t, t.Context(), db, "search", seedOpts{SessionID: seed.ID, ProjectHash: seed.Hash, ProjectPath: seed.Path, GitRemote: seed.Remote, Harness: seed.Harness, StartMs: seed.Start})
		if len(seed.Tags) > 0 {
			if err := db.SetTags(t.Context(), ingest.SessionID(seed.ID), seed.Tags); err != nil {
				t.Fatal(err)
			}
		}
		var entries []schema.SessionEntry
		for i, text := range seed.Entries {
			entries = append(entries, schema.SessionEntry{SessionID: schema.SessionID(seed.ID), EntryIndex: i, Harness: seed.Harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text})
		}
		if err := db.IndexSessionEntries(t.Context(), schema.SessionID(seed.ID), entries); err != nil {
			t.Fatal(err)
		}
	}
}

func setupSearchFixture(t *testing.T, family searchFixtureFamily, c searchFixtureCase, dir string) []string {
	t.Helper()
	if c.Setup.Store != searchNone {
		db := openSeedStore(t, dir, c.Name)
		// V76 drops the retired index at open, so rebuild the
		// pre-consolidation dual-index state the transition fixtures model
		// before any writes: mirror rows land in the retired index again
		// and the merged reads keep their exact pre-V76 distribution.
		storetest.SeedLegacySearchIndex(t, db)
		if c.Setup.Store != searchEmpty {
			seeds := c.Seeds
			if seeds == nil {
				seeds = family.Seeds
			}
			seedSearchFixtures(t, db, seeds)
		}
		conn, err := db.Pool().Take(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !store.LegacySearchIndexExists(conn) || !store.UnifiedSearchIndexExists(conn) {
			t.Fatal("fixtures require both transition indexes before mutation")
		}
		switch c.Setup.Store {
		case searchNeedsRebuild:
			err = store.SearchStateSetNeedsRebuild(t.Context(), conn)
		case searchNoIndex:
			err = sqlitex.ExecuteScript(conn, "DROP TABLE session_search_fts; DROP TABLE session_entries_fts;", nil)
		}
		db.Pool().Put(conn)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	args := make([]string, len(c.Args))
	for i, arg := range c.Args {
		args[i] = strings.ReplaceAll(arg, "{{dir}}", dir)
	}
	if list := c.Setup.SessionsFrom; list != nil && list.Write != searchListMissing {
		text := ""
		if list.Write == searchListIDs {
			text = strings.Join(list.IDs, "\n") + "\n"
		}
		if err := os.WriteFile(strings.ReplaceAll(list.Path, "{{dir}}", dir), []byte(text), defaults.PrivateFilePerm); err != nil {
			t.Fatal(err)
		}
	}
	if c.Setup.DataDir == searchRegularFile {
		path := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(path, nil, defaults.PrivateFilePerm); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--data-dir", path)
	}
	return append([]string{"search"}, args...)
}

func TestSessionsSearchFixtures(t *testing.T) {
	t.Parallel()
	family := loadSessionsSearchFixtures(t)
	for _, c := range family.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			args := setupSearchFixture(t, family, c, dir)
			stdout, stderr, err := executeWithDataDirStreams(t, BuildSessionsCommand(), dir, args)
			gotExit := defaults.ExitOK
			if err != nil {
				gotExit = exitCodeFor(err)
			}
			if got := gotExit; got != c.WantExit {
				t.Fatalf("exit %d, want %d: %v\nstdout=%s\nstderr=%s", got, c.WantExit, err, stdout, stderr)
			}
			if c.WantStdoutEmpty && stdout != "" {
				t.Errorf("stdout not empty: %q", stdout)
			}
			if c.WantStderrEmpty && stderr != "" {
				t.Errorf("stderr not empty: %q", stderr)
			}
			for _, s := range c.WantStdoutContains {
				if !strings.Contains(stdout, s) {
					t.Errorf("stdout missing %q: %s", s, stdout)
				}
			}
			for _, s := range c.WantStderrContains {
				if !strings.Contains(stderr, s) {
					t.Errorf("stderr missing %q: %s", s, stderr)
				}
			}
			for _, s := range c.WantStdoutNotContains {
				if strings.Contains(stdout, s) {
					t.Errorf("stdout contains forbidden %q: %s", s, stdout)
				}
			}
			for _, s := range c.WantStderrNotContains {
				if strings.Contains(stderr, s) {
					t.Errorf("stderr contains forbidden %q: %s", s, stderr)
				}
			}
			if c.WantNoDBFile {
				if _, err := os.Stat(string(defaults.ResolveDBFilePathWith(dir))); !os.IsNotExist(err) {
					t.Errorf("database created before refusal: %v", err)
				}
			}
			if containsString(c.Args, "--json") && c.WantExit == 0 {
				var doc sessionsSearchOutput
				dec := json.NewDecoder(strings.NewReader(stdout))
				if err := dec.Decode(&doc); err != nil {
					t.Fatal(err)
				}
				if err := dec.Decode(new(any)); err != io.EOF {
					t.Fatalf("stdout must be one JSON document: %v", err)
				}
				if doc.Warnings == nil || doc.Results == nil {
					t.Fatal("warnings/results must be arrays, not null")
				}
				if doc.Query != c.Args[0] {
					t.Errorf("query %q, want original %q", doc.Query, c.Args[0])
				}
				if doc.EmptyCode != c.WantEmptyCode || !strings.Contains(doc.EmptyReason, c.WantReasonContains) {
					t.Errorf("meta: %+v", doc.Meta)
				}
				if c.WantResults != nil {
					coords := make([]string, 0, len(doc.Results))
					for _, r := range doc.Results {
						coords = append(coords, fmt.Sprintf("%s:%d", r.SessionID, r.EntryIndex))
					}
					if !reflect.DeepEqual(coords, c.WantResults) {
						t.Errorf("results %v, want %v", coords, c.WantResults)
					}
				}
				if c.WantWarningCode != "" {
					if len(doc.Warnings) != 1 || doc.Warnings[0].Code != c.WantWarningCode || doc.Warnings[0].SessionID == "" {
						t.Errorf("warnings: %+v", doc.Warnings)
					}
				} else if len(doc.Warnings) != 0 {
					t.Errorf("unexpected warnings: %+v", doc.Warnings)
				}
			}
			if len(c.WantColumns) > 0 {
				lines := strings.Split(strings.TrimSpace(stdout), "\n")
				if !reflect.DeepEqual(strings.Fields(lines[0]), c.WantColumns) {
					t.Fatalf("columns: %s", lines[0])
				}
				if len(lines) < 3 {
					t.Fatalf("missing table rows: %s", stdout)
				}
				cells := searchTableCells(lines[2])
				if len(cells) != len(c.WantColumns) {
					t.Fatalf("row cells: %v", cells)
				}
				snippet := cells[len(cells)-1]
				if !strings.HasSuffix(snippet, c.WantSnippetSuffix) {
					t.Errorf("snippet %q lacks suffix %q", snippet, c.WantSnippetSuffix)
				}
				if c.WantSnippetRunes > 0 && len([]rune(snippet)) != c.WantSnippetRunes {
					t.Errorf("snippet rune count %d, want %d: %q", len([]rune(snippet)), c.WantSnippetRunes, snippet)
				}
			}
		})
	}
}

func searchTableCells(line string) []string {
	var cells []string
	for _, part := range strings.Split(line, "  ") {
		if s := strings.TrimSpace(part); s != "" {
			cells = append(cells, s)
		}
	}
	return cells
}

// TestNextPageHintStopsAtTheOffsetBound keeps the continuation hint from
// suggesting a command the reader refuses. A full page that ends at the bound
// has no next page to offer, only a narrower query.
func TestNextPageHintStopsAtTheOffsetBound(t *testing.T) {
	t.Parallel()
	atBound := strconv.Itoa(codemap.SearchMaxOffset)
	if got := nextPageHint(codemap.SearchMaxOffset-5, 5); !strings.Contains(got, "--offset "+atBound) {
		t.Fatalf("hint whose next page is exactly the bound = %q, want --offset %s", got, atBound)
	}
	if got := nextPageHint(codemap.SearchMaxOffset-4, 5); strings.Contains(got, "--offset") {
		t.Fatalf("hint whose next page is past the bound = %q, want no offset a reader would refuse", got)
	}
	if got := nextPageHint(codemap.SearchMaxOffset-4, 5); !strings.Contains(got, "narrow the query") {
		t.Fatalf("hint past the bound = %q, want the narrowing guidance", got)
	}
}

// TestSearchWindowClampsToTheDocumentedCap observes the value the CLI actually
// sends to the reader. A clamp that stops being applied, or an off-by-one that
// lets the cap through, fails here rather than in a 500-row fixture.
func TestSearchWindowClampsToTheDocumentedCap(t *testing.T) {
	t.Parallel()
	atCap, err := searchWindow(strconv.Itoa(codemap.SearchMaxLimit))
	if err != nil || atCap != codemap.SearchMaxLimit {
		t.Fatalf("searchWindow(%d) = %d, %v; want the cap unchanged", codemap.SearchMaxLimit, atCap, err)
	}
	aboveCap, err := searchWindow(strconv.Itoa(codemap.SearchMaxLimit + 1))
	if err != nil || aboveCap != codemap.SearchMaxLimit {
		t.Fatalf("searchWindow(%d) = %d, %v; want the documented cap", codemap.SearchMaxLimit+1, aboveCap, err)
	}
	if _, err := searchWindow("0"); err == nil {
		t.Fatal("searchWindow(0) was accepted; sessions search refuses a zero page")
	}
}

func TestSessionsSearchHarnessHelpUsesKnownHarnesses(t *testing.T) {
	cmd := buildSessionsSearchCommand()
	flag := cmd.Flags().Lookup("harness")
	if flag == nil {
		t.Fatal("missing harness flag")
	}
	if want := joinHarnesses(defaults.Harnesses()); !strings.Contains(flag.Usage, want) {
		t.Errorf("harness help %q does not contain %q", flag.Usage, want)
	}
}
