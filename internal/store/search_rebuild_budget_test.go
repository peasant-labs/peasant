package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_rebuild_budget.yaml
var searchRebuildBudgetYAML []byte

// searchRebuildEntry names the production path a budget case rebuilds
// through. The set is closed.
type searchRebuildEntry string

const (
	searchRebuildEntryRepairStep           searchRebuildEntry = "repair-step"
	searchRebuildEntrySearchIndex          searchRebuildEntry = "search-index-rebuild"
	searchRebuildEntryPruneHealthGate      searchRebuildEntry = "prune-health-gate"
	searchRebuildEntryPruneUntrustedDelete searchRebuildEntry = "prune-untrusted-delete"
	searchRebuildEntrySweepFlagged         searchRebuildEntry = "sweep-flagged"
)

var allSearchRebuildEntries = []searchRebuildEntry{
	searchRebuildEntryRepairStep, searchRebuildEntrySearchIndex, searchRebuildEntryPruneHealthGate,
	searchRebuildEntryPruneUntrustedDelete, searchRebuildEntrySweepFlagged,
}

// searchRebuildOutcome names how a budget case ends. The set is closed.
type searchRebuildOutcome string

const (
	searchRebuildOutcomeCommit searchRebuildOutcome = "commit"
	searchRebuildOutcomeFail   searchRebuildOutcome = "fail"
	searchRebuildOutcomeCancel searchRebuildOutcome = "cancel"
)

var allSearchRebuildOutcomes = []searchRebuildOutcome{searchRebuildOutcomeCommit, searchRebuildOutcomeFail, searchRebuildOutcomeCancel}

// searchRebuildFillerSession owns the filler bodies, so pruning or sweeping
// another session leaves the filler indexed.
const searchRebuildFillerSession = "ffffffff-6464-4464-8464-ffffffffffff"

// searchRebuildSweptSession is the managed session the sweep case sweeps.
const searchRebuildSweptSession = "eeeeeeee-6464-4464-8464-eeeeeeeeeeee"

type searchRebuildBudgetFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Filler        struct {
		Rows       int `yaml:"rows"`
		RowBytes   int `yaml:"rowBytes"`
		Vocabulary int `yaml:"vocabulary"`
	} `yaml:"filler"`
	Queries []string `yaml:"queries"`
	Cases   []struct {
		Name    string               `yaml:"name"`
		Entry   searchRebuildEntry   `yaml:"entry"`
		Outcome searchRebuildOutcome `yaml:"outcome"`
	} `yaml:"cases"`
}

func loadSearchRebuildBudgetFixture(t *testing.T) searchRebuildBudgetFixture {
	t.Helper()
	var fixture searchRebuildBudgetFixture
	if err := yaml.Unmarshal(searchRebuildBudgetYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
		if !containsValue(allSearchRebuildEntries, c.Entry) {
			t.Fatalf("case %s names entry %q, outside %q", c.Name, c.Entry, allSearchRebuildEntries)
		}
		if !containsValue(allSearchRebuildOutcomes, c.Outcome) {
			t.Fatalf("case %s names outcome %q, outside %q", c.Name, c.Outcome, allSearchRebuildOutcomes)
		}
		if c.Entry != searchRebuildEntryRepairStep && c.Outcome != searchRebuildOutcomeCommit {
			t.Fatalf("case %s: only the repair step has a failure seam inside its transaction; give it outcome %s", c.Name, searchRebuildOutcomeCommit)
		}
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "search rebuild budget"); err != nil {
		t.Fatal(err)
	}
	fillerSized := fixture.Filler.Rows > 0 && fixture.Filler.RowBytes > 0 && fixture.Filler.Vocabulary > 0
	if !fillerSized || len(fixture.Queries) == 0 {
		t.Fatal("search rebuild budget fixture has no filler or no queries")
	}
	return fixture
}

func containsValue[T comparable](set []T, value T) bool {
	for _, member := range set {
		if member == value {
			return true
		}
	}
	return false
}

// TestSearchRebuildBudget proves that every whole-index rebuild path uses the
// widened postings budget and leaves no trace of it. Each case runs a real
// entry point on a store whose postings need several flushes at FTS5's
// default budget, then rebuilds a copy of the result at the default budget:
// the result must return the same rows and ranks for every query, hold one
// segment where the default writes several, and keep the index
// configuration it had before. A repair step that fails or is cancelled
// inside its transaction must roll back with no stored budget, and a clean
// upgrade afterwards must give the same results.
func TestSearchRebuildBudget(t *testing.T) {
	t.Parallel()
	fixture := loadSearchRebuildBudgetFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "budget.db")
			conn := openMigrationV76Store(t, path, "delete", false)
			removeBodySentinel(t, conn)
			seedSearchRebuildFillerSession(t, conn)
			seedSearchRebuildFiller(t, conn, searchRebuildFillerSession, fixture.Filler.Rows, fixture.Filler.RowBytes, fixture.Filler.Vocabulary)
			configBefore := searchConfigRows(t, conn)
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}

			switch c.Entry {
			case searchRebuildEntryRepairStep:
				runSearchRebuildRepairCase(t, path, c.Outcome, configBefore)
			default:
				if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
					t.Fatalf("upgrade the store before the %s: %v", c.Entry, err)
				}
				runSearchRebuildHeadEntry(t, dir, path, c.Entry)
			}
			assertSearchRebuiltAtWideBudget(t, dir, path, fixture.Queries, configBefore)
		})
	}
}

// runSearchRebuildRepairCase runs the upgrade's search trigger repair to the
// case's outcome. A stopped repair is asserted rolled back with no stored
// budget, then the store is upgraded cleanly.
func runSearchRebuildRepairCase(t *testing.T, path string, outcome searchRebuildOutcome, configBefore []string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failure := errors.New("synthetic failure after the repair's rebuild")
	opts := UpgradeOptions{}
	switch outcome {
	case searchRebuildOutcomeFail:
		opts.afterDataFill = func(version int) error {
			if version == searchRepairSchemaVersion {
				return failure
			}
			return nil
		}
	case searchRebuildOutcomeCancel:
		opts.afterDataFill = func(version int) error {
			if version == searchRepairSchemaVersion {
				cancel()
			}
			return nil
		}
	}
	report, err := Upgrade(ctx, path, opts)
	if outcome == searchRebuildOutcomeCommit {
		if err != nil {
			t.Fatal(err)
		}
		repaired := false
		for _, step := range report.Steps {
			repaired = repaired || step.Step.rebuildsSearchIndex()
		}
		if !repaired {
			t.Fatal("the upgrade did not plan the long search trigger repair; the store carries no residue")
		}
		return
	}
	if err == nil {
		t.Fatal("the upgrade succeeded; the case's failure did not reach the repair step")
	}
	if outcome == searchRebuildOutcomeFail && !errors.Is(err, failure) {
		t.Fatalf("upgrade error = %v, want the synthetic failure", err)
	}
	stopped, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	version, err := readPragmaInt(stopped, "PRAGMA user_version")
	if err != nil {
		t.Fatal(err)
	}
	if int(version) != searchRepairSchemaVersion-1 {
		t.Errorf("the store is at schema %d after the stopped repair, want %d: the step must roll back", version, searchRepairSchemaVersion-1)
	}
	if err := ftsContentIntegrityCheck(stopped); err == nil {
		t.Error("the stale results passed the integrity check after the stopped repair; the repair did not roll back")
	}
	if storedSearchHashBudget(t, stopped) {
		t.Errorf("the stopped repair left the %s option stored: the widen must roll back with the step", searchHashConfigKey)
	}
	if got := searchConfigRows(t, stopped); !equalStrings(got, configBefore) {
		t.Errorf("search index configuration after the stopped repair = %q, want %q as before", got, configBefore)
	}
	if err := stopped.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("the upgrade after the stopped repair: %v", err)
	}
}

// runSearchRebuildHeadEntry runs one rebuild entry point on an upgraded
// store, through the store's public API.
func runSearchRebuildHeadEntry(t *testing.T, dir, path string, entry searchRebuildEntry) {
	t.Helper()
	ctx := t.Context()
	switch entry {
	case searchRebuildEntrySearchIndex:
		s, err := Open(path, WithPoolSize(1))
		if err != nil {
			t.Fatal(err)
		}
		defer closeSearchRebuildStore(t, s)
		if err := s.RebuildSearchIndex(ctx, nil); err != nil {
			t.Fatal(err)
		}
	case searchRebuildEntryPruneHealthGate:
		s, err := Open(path, WithPoolSize(1))
		if err != nil {
			t.Fatal(err)
		}
		defer closeSearchRebuildStore(t, s)
		flagSearchIndexForRebuild(t, s)
		// No stored session matches, so the gate's rebuild is the prune's
		// only search write.
		result, err := s.PruneSessions(ctx, []ingest.SessionID{"00000000-6464-4464-8464-000000000000"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Deleted != 0 {
			t.Fatalf("the gate case pruned %d sessions, want none", result.Deleted)
		}
		assertSearchFlagCleared(t, s)
	case searchRebuildEntryPruneUntrustedDelete:
		s, err := Open(path, WithPoolSize(1))
		if err != nil {
			t.Fatal(err)
		}
		defer closeSearchRebuildStore(t, s)
		// The bodies session's stored body carries a digest that does not
		// match its text, so the delete cannot be trusted and the prune
		// rebuilds after its deletes.
		result, err := s.PruneSessions(ctx, []ingest.SessionID{v76BodiesSession})
		if err != nil {
			t.Fatal(err)
		}
		if result.Deleted != 1 {
			t.Fatalf("the untrusted-delete case pruned %d sessions, want 1", result.Deleted)
		}
		assertSearchFlagCleared(t, s)
	case searchRebuildEntrySweepFlagged:
		s := openGenerationStoreOnPath(t, dir, path)
		defer closeSearchRebuildStore(t, s)
		sid := gcSession(t, s, searchRebuildSweptSession)
		gcActivate(t, s, sid, "gen_budget_sweep", []string{"sweepbudgeta", "sweepbudgetb", "sweepbudgetc"}, nil)
		flagSearchIndexForRebuild(t, s)
		result, err := s.SweepSession(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Rebuilt {
			t.Fatal("the flagged sweep did not rebuild the search index")
		}
		assertSearchFlagCleared(t, s)
	default:
		t.Fatalf("entry %q has no head-store runner", entry)
	}
}

// assertSearchRebuiltAtWideBudget compares the store's index with a rebuild
// of a copy at FTS5's default budget.
func assertSearchRebuiltAtWideBudget(t *testing.T, dir, path string, queries, configBefore []string) {
	t.Helper()
	reference := filepath.Join(dir, "reference.db")
	copySearchRebuildStore(t, path, reference)
	refConn, err := sqlite.OpenConn(reference, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer refConn.Close()
	if err := sqlitex.ExecuteTransient(refConn, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, nil); err != nil {
		t.Fatal(err)
	}
	if segments := searchIndexSegments(t, refConn); segments < 2 {
		t.Fatalf("a default-budget rebuild wrote %d segment(s); the filler must need several flushes for the case to prove the wider budget", segments)
	}
	wantDigest := searchRebuildDigest(t, refConn, queries)

	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if storedSearchHashBudget(t, conn) {
		t.Errorf("the rebuild left the %s option stored", searchHashConfigKey)
	}
	if got := searchConfigRows(t, conn); !equalStrings(got, configBefore) {
		t.Errorf("search index configuration after the rebuild = %q, want %q as before", got, configBefore)
	}
	if err := ftsContentIntegrityCheck(conn); err != nil {
		t.Errorf("integrity-check after the rebuild: %v", err)
	}
	if got := searchRebuildDigest(t, conn, queries); got != wantDigest {
		t.Error("the rebuild returned different rows or ranks than a rebuild at FTS5's default budget")
	}
	if segments := searchIndexSegments(t, conn); segments != 1 {
		t.Errorf("the rebuild wrote %d segments, want 1: the widened budget holds every posting of this store", segments)
	}
}

// openGenerationStoreOnPath opens the store at path with managed generation
// support, as openGenerationStoreAt does for its own path.
func openGenerationStoreOnPath(t *testing.T, dir, path string) *Store {
	t.Helper()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, WithPoolSize(2), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeSearchRebuildStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("close the store: %v", err)
	}
}

func flagSearchIndexForRebuild(t *testing.T, s *Store) {
	t.Helper()
	execGenerationSQL(t, s, `UPDATE session_search_state SET needs_rebuild = 1 WHERE id = 1;`)
}

func assertSearchFlagCleared(t *testing.T, s *Store) {
	t.Helper()
	state, err := s.SearchState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.NeedsRebuild {
		t.Fatal("the search index still needs a rebuild after the entry point ran")
	}
}

// seedSearchRebuildFillerSession adds the session that owns the filler.
func seedSearchRebuildFillerSession(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code', 'fixture-model', 'v76-host', 'v76-project', 1, 2, 3, '/synthetic/filler.jsonl', 'jsonl')`, &sqlitex.ExecOptions{
		Args: []any{searchRebuildFillerSession},
	}); err != nil {
		t.Fatalf("seed the filler session: %v", err)
	}
}

// seedSearchRebuildFiller adds body rows of filler text to a session; the
// body trigger indexes them into the union index.
func seedSearchRebuildFiller(t *testing.T, conn *sqlite.Conn, sessionID string, rows, rowBytes, vocabulary int) {
	t.Helper()
	words := rand.New(rand.NewPCG(76, 64))
	end := sqlitex.Transaction(conn)
	var err error
	defer end(&err)
	for row := 0; row < rows && err == nil; row++ {
		var text strings.Builder
		for text.Len() < rowBytes {
			fmt.Fprintf(&text, "w%d ", words.IntN(vocabulary))
		}
		err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, content_preview, has_tool_use, has_thinking, is_error, depth)
VALUES((SELECT coalesce(max(body_id), ?) + 1 FROM session_entry_bodies), ?, printf('%064x', ?), ?, 'claude-code', 'text', 'assistant', ?, 0, 0, 0, 0)`,
			&sqlitex.ExecOptions{Args: []any{BodyRowIDBase - 1, sessionID, row + 1000, row, text.String()}})
	}
	if err != nil {
		t.Fatalf("seed the filler bodies: %v", err)
	}
}

// searchRebuildDigest hashes every query's rows and bm25 ranks in rank order.
// A query that matches nothing fails the test: it would prove nothing.
func searchRebuildDigest(t *testing.T, conn *sqlite.Conn, queries []string) string {
	t.Helper()
	hash := sha256.New()
	for _, query := range queries {
		rows := 0
		if err := sqlitex.ExecuteTransient(conn, `SELECT rowid, rank FROM session_search_fts WHERE session_search_fts MATCH ? ORDER BY rank, rowid`, &sqlitex.ExecOptions{
			Args: []any{query},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows++
				_, _ = fmt.Fprintf(hash, "%s %d %.12g\n", query, stmt.ColumnInt64(0), stmt.ColumnFloat(1))
				return nil
			},
		}); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if rows == 0 {
			t.Fatalf("query %q matched nothing; it proves nothing about the rebuild", query)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// searchIndexSegments counts the union index's segments.
func searchIndexSegments(t *testing.T, conn *sqlite.Conn) int {
	t.Helper()
	segments := 0
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(DISTINCT segid) FROM session_search_fts_idx`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { segments = stmt.ColumnInt(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return segments
}

// storedSearchHashBudget reports whether the union index stores a postings
// budget.
func storedSearchHashBudget(t *testing.T, conn *sqlite.Conn) bool {
	t.Helper()
	stored := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_search_fts_config WHERE k = ?`, &sqlitex.ExecOptions{
		Args:       []any{searchHashConfigKey},
		ResultFunc: func(*sqlite.Stmt) error { stored = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return stored
}

// searchConfigRows lists the union index's stored configuration as k=v.
func searchConfigRows(t *testing.T, conn *sqlite.Conn) []string {
	t.Helper()
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT k, v FROM session_search_fts_config`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, stmt.ColumnText(0)+"="+stmt.ColumnText(1))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	return rows
}

// copySearchRebuildStore copies a closed store, and its log if one remains.
func copySearchRebuildStore(t *testing.T, from, to string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(from + suffix)
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(to + suffix)
		if err != nil {
			_ = in.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(out, in)
		_ = in.Close()
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if copyErr != nil {
			t.Fatal(copyErr)
		}
	}
}
