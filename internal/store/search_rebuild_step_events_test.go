package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_rebuild.yaml
var searchRebuildYAML []byte

//go:embed testdata/search_rebuild.manifest.yaml
var searchRebuildManifestYAML []byte

type searchRebuildCorpus struct {
	Sessions          int    `yaml:"sessions"`
	EntriesPerSession int    `yaml:"entriesPerSession"`
	TermPrefix        string `yaml:"termPrefix"`
	PreviewPadding    string `yaml:"previewPadding"`
}

type searchRebuildCase struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type searchRebuildFixtures struct {
	Corpus searchRebuildCorpus `yaml:"corpus"`
	Cases  []searchRebuildCase `yaml:"cases"`
}

func loadSearchRebuildFixtures(t *testing.T) searchRebuildFixtures {
	t.Helper()
	var fixtures searchRebuildFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(searchRebuildYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode testdata/search_rebuild.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(searchRebuildManifestYAML)
	if err != nil {
		t.Fatalf("decode the search rebuild manifest: %v", err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "search rebuild"); err != nil {
		t.Fatal(err)
	}
	if fixtures.Corpus.Sessions < 2 || fixtures.Corpus.EntriesPerSession < 1 ||
		strings.TrimSpace(fixtures.Corpus.TermPrefix) == "" ||
		strings.TrimSpace(fixtures.Corpus.PreviewPadding) == "" {
		t.Fatalf("search rebuild corpus is incomplete: %#v", fixtures.Corpus)
	}
	return fixtures
}

// seedSearchRebuildCorpus writes the synthetic corpus through the production
// entry writer. Every preview is one unique FTS term plus shared padding.
func seedSearchRebuildCorpus(t *testing.T, s *Store, corpus searchRebuildCorpus) (terms []string, textBytes int64) {
	t.Helper()
	ctx := context.Background()
	for sessionIndex := 0; sessionIndex < corpus.Sessions; sessionIndex++ {
		sessionID := fmt.Sprintf("%08d-0000-4000-8000-000000000000", sessionIndex+1)
		seedSearchRebuildSession(t, s, sessionID)
		entries := make([]schema.SessionEntry, 0, corpus.EntriesPerSession)
		for entryIndex := 0; entryIndex < corpus.EntriesPerSession; entryIndex++ {
			term := fmt.Sprintf("%s%de%d", corpus.TermPrefix, sessionIndex, entryIndex)
			preview := term + corpus.PreviewPadding
			terms = append(terms, term)
			textBytes += int64(len(preview))
			entries = append(entries, schema.SessionEntry{
				SessionID:      schema.SessionID(sessionID),
				Harness:        defaults.HarnessClaudeCode,
				EntryIndex:     entryIndex,
				EntryType:      schema.EntryTypeText,
				Role:           schema.RoleUser,
				ContentPreview: &preview,
			})
		}
		if err := s.IndexSessionEntries(ctx, schema.SessionID(sessionID), entries); err != nil {
			t.Fatalf("index the synthetic session %s: %v", sessionID, err)
		}
	}
	return terms, textBytes
}

func seedSearchRebuildSession(t *testing.T, s *Store, sessionID string) {
	t.Helper()
	startMs := int64(1700000000000)
	ingested := startMs + 60000
	entry := ingest.StoreEntry{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     schema.SessionID(sessionID),
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("claude-opus-4-6"),
			HostSlug:      schema.HostSlug("github.com--search-rebuild--test"),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
				Name:     "search-rebuild-test",
				FilePath: "/search/rebuild/test",
			},
			Timestamp: schema.TimestampInfo{Start: startMs, End: startMs + 30000, Ingested: &ingested},
			Source:    schema.SourceInfo{FilePath: "/search/rebuild/session.jsonl", Format: schema.SourceFormatJSONL},
		},
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seed session %s: %v", sessionID, err)
	}
}

func openSyntheticStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("open the synthetic store %s: %v", path, err)
	}
	return s
}

func closeSyntheticStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close the synthetic store: %v", err)
	}
}

func setSearchNeedsRebuild(t *testing.T, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := SearchStateSetNeedsRebuild(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

func searchNeedsRebuild(t *testing.T, s *Store) bool {
	t.Helper()
	state, err := s.SearchState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state.NeedsRebuild
}

func searchTermRows(t *testing.T, s *Store, term string) []string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var rows []string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT session_id || ':' || entry_index FROM session_search_fts WHERE session_search_fts MATCH ? ORDER BY session_id, entry_index`,
		&sqlitex.ExecOptions{Args: []any{term}, ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, stmt.ColumnText(0))
			return nil
		}}); err != nil {
		t.Fatalf("search %q: %v", term, err)
	}
	return rows
}

func TestSearchRebuild(t *testing.T) {
	fixtures := loadSearchRebuildFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Name {
			case "harvest-start-rebuild-emits-uncounted-step-events":
				testHarvestStartRebuildEmitsUncountedStepEvents(t, fixtures.Corpus)
			case "current-store-emits-no-step-events":
				testCurrentStoreEmitsNoStepEvents(t)
			case "cancelled-rebuild-before-it-starts-changes-nothing":
				testCancelledRebuildBeforeItStartsChangesNothing(t, fixtures.Corpus)
			case "deferred-commit-failure-is-returned":
				testDeferredCommitFailureIsReturned(t)
			default:
				t.Fatalf("search rebuild fixture names case %q with no runner", c.Name)
			}
		})
	}
}

func testHarvestStartRebuildEmitsUncountedStepEvents(t *testing.T, corpus searchRebuildCorpus) {
	t.Helper()
	source := openSyntheticStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer closeSyntheticStore(t, source)
	seedSearchRebuildCorpus(t, source, corpus)
	setSearchNeedsRebuild(t, source)

	var events []UpgradeEvent
	rebuilt, err := source.EnsureSearchIndexHealthyWithProgress(context.Background(), func(event UpgradeEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("EnsureSearchIndexHealthyWithProgress: %v", err)
	}
	if !rebuilt {
		t.Fatal("a flagged store reported that it did not rebuild")
	}
	if len(events) != 3 {
		t.Fatalf("rebuild emitted %d events, want started, progress and finished", len(events))
	}
	wantKinds := []UpgradeEventKind{UpgradeEventStepStarted, UpgradeEventStepProgress, UpgradeEventStepFinished}
	for i, event := range events {
		if event.Kind != wantKinds[i] {
			t.Fatalf("event %d kind = %q, want %q", i, event.Kind, wantKinds[i])
		}
		if event.Step.Kind != UpgradeStepSearch {
			t.Fatalf("event %d step kind = %q, want the search step", i, event.Step.Kind)
		}
		if event.Step.Counts != UpgradeUnitNone {
			t.Fatalf("event %d unit = %q, want the uncounted unit: the one-statement rebuild cannot report a fraction", i, event.Step.Counts)
		}
		if event.Done != 0 || event.Of != 0 {
			t.Fatalf("event %d count = %d of %d, want no count for an uncounted step", i, event.Done, event.Of)
		}
		if event.Index != 1 || event.Total != 1 {
			t.Fatalf("event %d position = %d of %d, want 1 of 1", i, event.Index, event.Total)
		}
	}
	if events[0].Elapsed > events[len(events)-1].Elapsed {
		t.Fatalf("finished event elapsed %s is before the started event's %s", events[len(events)-1].Elapsed, events[0].Elapsed)
	}
	if events[len(events)-1].WALBytes < 0 {
		t.Fatalf("finished event reports negative write-ahead log growth %d", events[len(events)-1].WALBytes)
	}
	if searchNeedsRebuild(t, source) {
		t.Fatal("the rebuild flag is still set after the harvest-start rebuild")
	}
}

func testCurrentStoreEmitsNoStepEvents(t *testing.T) {
	t.Helper()
	source := openSyntheticStore(t, filepath.Join(t.TempDir(), "current.db"))
	defer closeSyntheticStore(t, source)
	called := false
	rebuilt, err := source.EnsureSearchIndexHealthyWithProgress(context.Background(), func(UpgradeEvent) {
		called = true
	})
	if err != nil {
		t.Fatalf("EnsureSearchIndexHealthyWithProgress: %v", err)
	}
	if rebuilt || called {
		t.Fatalf("a healthy store rebuilt = %v and called progress = %v, want neither", rebuilt, called)
	}
}

func testCancelledRebuildBeforeItStartsChangesNothing(t *testing.T, corpus searchRebuildCorpus) {
	t.Helper()
	source := openSyntheticStore(t, filepath.Join(t.TempDir(), "cancelled.db"))
	defer closeSyntheticStore(t, source)
	terms, _ := seedSearchRebuildCorpus(t, source, corpus)
	setSearchNeedsRebuild(t, source)
	wantRows := searchTermRows(t, source, terms[0])

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := source.RebuildSearchIndex(ctx, nil)
	if err == nil {
		t.Fatal("a cancelled rebuild reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rebuild error = %v, want it to wrap context.Canceled", err)
	}
	if !searchNeedsRebuild(t, source) {
		t.Fatal("a cancelled rebuild cleared the search rebuild flag")
	}
	if got := searchTermRows(t, source, terms[0]); !equalStrings(wantRows, got) {
		t.Fatalf("after a cancelled rebuild term %q returns %v, want the old index rows %v", terms[0], got, wantRows)
	}
}

// testDeferredCommitFailureIsReturned proves the rebuild returns a real
// COMMIT failure instead of dropping it. The seam runs inside the rebuild's
// own transaction and defers a foreign-key violation to COMMIT, which is the
// same point SQLite reports a failed commit through the deferred end
// function. A caller that returned a separate nil, as the old pattern did,
// would report a rolled-back rebuild as success.
func testDeferredCommitFailureIsReturned(t *testing.T) {
	t.Helper()
	s := openFreshStore(t, filepath.Join(t.TempDir(), "commit.db"))
	setSearchNeedsRebuild(t, s)
	s.searchRebuildSeam = func(conn *sqlite.Conn) error {
		if err := sqlitex.ExecuteTransient(conn, `CREATE TABLE zz_search_rebuild_commit_probe (
			id INTEGER PRIMARY KEY,
			parent_id TEXT REFERENCES sessions(session_id))`, nil); err != nil {
			return err
		}
		if err := sqlitex.ExecuteTransient(conn, `PRAGMA defer_foreign_keys = ON`, nil); err != nil {
			return err
		}
		return sqlitex.Execute(conn, `INSERT INTO zz_search_rebuild_commit_probe(parent_id) VALUES ('missing-session')`, nil)
	}
	started := time.Now()
	err := s.RebuildSearchIndex(context.Background(), nil)
	if err == nil {
		t.Fatal("a deferred COMMIT failure was reported as success")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("commit failure = %v, want the foreign-key COMMIT error", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("the commit failure took %s; the probe is not the deterministic commit path", time.Since(started))
	}
	if !searchNeedsRebuild(t, s) {
		t.Fatal("a failed COMMIT cleared the rebuild flag, so the transaction was not rolled back")
	}
	conn, takeErr := s.pool.Take(context.Background())
	if takeErr != nil {
		t.Fatal(takeErr)
	}
	defer s.pool.Put(conn)
	var probeTables int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'zz_search_rebuild_commit_probe'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { probeTables = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if probeTables != 0 {
		t.Fatalf("a failed COMMIT left %d probe tables, want the transaction rolled back", probeTables)
	}
}

func equalStrings(a, b []string) bool {
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
