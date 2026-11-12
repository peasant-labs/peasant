package store

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/turn_count_recount.yaml
var turnCountRecountYAML []byte

type turnCountRecountWant struct {
	WantTurnCount             int `yaml:"wantTurnCount"`
	WantConversationTurnCount int `yaml:"wantConversationTurnCount"`
}

type turnCountRecountSession struct {
	Name                        string `yaml:"name"`
	SessionID                   string `yaml:"sessionID"`
	ActiveGeneration            bool   `yaml:"activeGeneration"`
	StoredTurnCount             int    `yaml:"storedTurnCount"`
	StoredConversationTurnCount int    `yaml:"storedConversationTurnCount"`
	MissingBodyIndex            *int   `yaml:"missingBodyIndex"`
	Entries                     []struct {
		Index     int    `yaml:"index"`
		Role      string `yaml:"role"`
		EntryType string `yaml:"entryType"`
		Depth     int    `yaml:"depth"`
		Content   string `yaml:"content"`
	} `yaml:"entries"`
	Report                    string                `yaml:"report"`
	WantTurnCount             int                   `yaml:"wantTurnCount"`
	WantConversationTurnCount int                   `yaml:"wantConversationTurnCount"`
	FromV73                   *turnCountRecountWant `yaml:"fromV73"`
}

type turnCountRecountFixture struct {
	RequiredSessions     []string                  `yaml:"requiredSessions"`
	RequiredRows         []string                  `yaml:"requiredRows"`
	Sessions             []turnCountRecountSession `yaml:"sessions"`
	InterruptedFirstOpen struct {
		FailAfterSessions             int `yaml:"failAfterSessions"`
		SentinelTurnCount             int `yaml:"sentinelTurnCount"`
		SentinelConversationTurnCount int `yaml:"sentinelConversationTurnCount"`
	} `yaml:"interruptedFirstOpen"`
}

const (
	turnCountRecountReportRecounted  = "recounted"
	turnCountRecountReportEntryLess  = "entry-less"
	turnCountRecountReportUnreadable = "unreadable"
)

// turnCountRecountRows binds each fixture row name to the test that runs it,
// so a row cannot be listed without a test or tested without being listed.
var turnCountRecountRows = map[string]func(*testing.T, turnCountRecountFixture){
	"from-v75":               runTurnCountRecountFromV75,
	"from-v73":               runTurnCountRecountFromV73,
	"interrupted-first-open": runTurnCountRecountInterruptedFirstOpen,
	"new-store":              runTurnCountRecountNewStore,
}

func loadTurnCountRecountFixture(t *testing.T) turnCountRecountFixture {
	t.Helper()
	var fixture turnCountRecountFixture
	decoder := yaml.NewDecoder(bytes.NewReader(turnCountRecountYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("turn count recount fixture must contain exactly one document: %v", err)
	}
	required := make(map[string]bool, len(fixture.RequiredSessions))
	for _, name := range fixture.RequiredSessions {
		if strings.TrimSpace(name) == "" || required[name] {
			t.Fatalf("turn count recount fixture has a blank or repeated required session %q", name)
		}
		required[name] = true
	}
	seen := make(map[string]bool, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		if !required[session.Name] || seen[session.Name] {
			t.Fatalf("turn count recount fixture has unlisted or repeated session %q", session.Name)
		}
		seen[session.Name] = true
		switch session.Report {
		case turnCountRecountReportRecounted:
			if session.WantTurnCount == session.WantConversationTurnCount {
				t.Fatalf("session %q expects equal counts; the two counts must differ so a swapped column fails", session.Name)
			}
			if session.WantTurnCount == session.StoredTurnCount || session.WantConversationTurnCount == session.StoredConversationTurnCount {
				t.Fatalf("session %q stores a count the fold also produces; a skipped recount would pass", session.Name)
			}
		case turnCountRecountReportEntryLess, turnCountRecountReportUnreadable:
			if session.WantTurnCount != session.StoredTurnCount || session.WantConversationTurnCount != session.StoredConversationTurnCount {
				t.Fatalf("session %q is reported %s and must keep its stored counts", session.Name, session.Report)
			}
		default:
			t.Fatalf("session %q has unknown report %q", session.Name, session.Report)
		}
	}
	for name := range required {
		if !seen[name] {
			t.Fatalf("turn count recount fixture is missing required session %q", name)
		}
	}
	rows := make(map[string]bool, len(fixture.RequiredRows))
	for _, name := range fixture.RequiredRows {
		if _, ok := turnCountRecountRows[name]; !ok || rows[name] {
			t.Fatalf("turn count recount fixture names row %q with no test bound to it, or repeats it", name)
		}
		rows[name] = true
	}
	for name := range turnCountRecountRows {
		if !rows[name] {
			t.Fatalf("turn count recount test row %q is not listed in requiredRows", name)
		}
	}
	return fixture
}

// seedTurnCountRecountStore builds a database at the given frozen schema
// version and seeds the fixture sessions with their stored counts. A V73 store
// has no conversation column, so only the legacy turn_count is seeded there and
// a session with no fromV73 expectation is left out.
func seedTurnCountRecountStore(t *testing.T, fixture turnCountRecountFixture, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(version)); err != nil {
		t.Fatal(err)
	}
	legacy := conversationTurnCountMigrationFixture{}
	var seeded []turnCountRecountSession
	for _, session := range fixture.Sessions {
		if version < 74 && session.FromV73 == nil {
			continue
		}
		stored := session.StoredTurnCount
		legacy.Sessions = append(legacy.Sessions, conversationTurnCountMigrationSession{
			Name: session.Name, SessionID: session.SessionID, ActiveGeneration: session.ActiveGeneration,
			LegacyTurnCount: &stored, Entries: session.Entries,
		})
		seeded = append(seeded, session)
	}
	seedConversationTurnCountSessions(t, conn, legacy)
	for _, session := range seeded {
		if version >= 74 {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET conversation_turn_count = ? WHERE session_id = ?`,
				&sqlitex.ExecOptions{Args: []any{session.StoredConversationTurnCount, session.SessionID}}); err != nil {
				t.Fatal(err)
			}
		}
		if session.MissingBodyIndex != nil {
			if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE session_id = ? AND entry_index = ?`,
				&sqlitex.ExecOptions{Args: []any{session.SessionID, *session.MissingBodyIndex}}); err != nil {
				t.Fatal(err)
			}
			if conn.Changes() != 1 {
				t.Fatalf("session %q: deleting body %d removed %d rows, want 1", session.Name, *session.MissingBodyIndex, conn.Changes())
			}
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type storedTurnCounts struct{ turnCount, conversationTurnCount int }

// readTurnCountRecountState reads the stored counts, the marker and the schema
// version from the raw file, without Open, so reading never runs the recount.
func readTurnCountRecountState(t *testing.T, path string) (counts map[string]storedTurnCounts, markerPending bool, version int64) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	counts = make(map[string]storedTurnCounts)
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id, COALESCE(turn_count, -1), conversation_turn_count FROM session_captured_stats`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			counts[stmt.ColumnText(0)] = storedTurnCounts{stmt.ColumnInt(1), stmt.ColumnInt(2)}
			return nil
		}}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM pending_recounts WHERE name = 'turn_counts'`,
		&sqlitex.ExecOptions{ResultFunc: func(*sqlite.Stmt) error { markerPending = true; return nil }}); err != nil {
		t.Fatal(err)
	}
	version, err = readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		t.Fatal(err)
	}
	return counts, markerPending, version
}

func openAndCloseForRecount(t *testing.T, path string, options ...OpenOption) error {
	t.Helper()
	s, err := Open(path, append([]OpenOption{WithPoolSize(1)}, options...)...)
	if err != nil {
		return err
	}
	return s.Close()
}

// TestMigrationV77RecountsTurnCounts runs every fixture row through the real
// Open on a database file at an older schema version.
func TestMigrationV77RecountsTurnCounts(t *testing.T) {
	t.Parallel()
	fixture := loadTurnCountRecountFixture(t)
	for _, name := range fixture.RequiredRows {
		run := turnCountRecountRows[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, fixture)
		})
	}
}

func runTurnCountRecountFromV75(t *testing.T, fixture turnCountRecountFixture) {
	path := seedTurnCountRecountStore(t, fixture, 75)
	if err := openAndCloseForRecount(t, path); err != nil {
		t.Fatalf("open the V75 store: %v", err)
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != 77 {
		t.Fatalf("schema version = %d, want 77", version)
	}
	if pending {
		t.Fatal("the recount marker is still set after a completed open")
	}
	for _, session := range fixture.Sessions {
		want := storedTurnCounts{session.WantTurnCount, session.WantConversationTurnCount}
		if got := counts[session.SessionID]; got != want {
			t.Errorf("%s: stored counts = %+v, want %+v", session.Name, got, want)
		}
	}

	// The report is read from the recount itself, on a second store brought to
	// head by the migration chain alone so the marker is still pending.
	reportPath := seedTurnCountRecountStore(t, fixture, 75)
	conn, err := sqlite.OpenConn(reportPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := sqlitex.NewPool(reportPath, sqlitex.PoolOptions{PoolSize: 1, PrepareConn: preparePragmas})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	report, ran, err := recountTurnCounts(pool, nil, logger)
	if err != nil || !ran {
		t.Fatalf("recount ran = %t, err = %v; want a completed run", ran, err)
	}
	var wantEntryLess, wantUnreadable []ingest.SessionID
	wantRecounted := 0
	for _, session := range fixture.Sessions {
		switch session.Report {
		case turnCountRecountReportRecounted:
			wantRecounted++
		case turnCountRecountReportEntryLess:
			wantEntryLess = append(wantEntryLess, ingest.SessionID(session.SessionID))
		case turnCountRecountReportUnreadable:
			wantUnreadable = append(wantUnreadable, ingest.SessionID(session.SessionID))
		}
	}
	if report.SessionsUpdated != wantRecounted {
		t.Errorf("report recounted %d sessions, want %d", report.SessionsUpdated, wantRecounted)
	}
	if !equalSessionIDs(report.EntryLessSessions, wantEntryLess) {
		t.Errorf("report entry-less = %v, want %v", report.EntryLessSessions, wantEntryLess)
	}
	if !equalSessionIDs(report.UnreadableSessions, wantUnreadable) {
		t.Errorf("report unreadable = %v, want %v", report.UnreadableSessions, wantUnreadable)
	}
	logTurnCountRecount(logger, report)
	logged := logs.String()
	for _, fragment := range []string{
		"recounting turn counts for every stored session",
		"sessions=6",
		"could not be read during the turn count recount",
		"session_id=" + string(wantUnreadable[0]),
		"turn count recount complete",
		"sessions_recounted=3",
		"entry_less=2",
		"unreadable=1",
	} {
		if !strings.Contains(logged, fragment) {
			t.Errorf("recount log lacks %q:\n%s", fragment, logged)
		}
	}
	if _, ranAgain, err := recountTurnCounts(pool, nil, logger); err != nil || ranAgain {
		t.Fatalf("second recount ran = %t, err = %v; want no run once the marker is cleared", ranAgain, err)
	}
}

func equalSessionIDs(got, want []ingest.SessionID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func runTurnCountRecountFromV73(t *testing.T, fixture turnCountRecountFixture) {
	path := seedTurnCountRecountStore(t, fixture, 73)
	if err := openAndCloseForRecount(t, path); err != nil {
		t.Fatalf("open the V73 store: %v", err)
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != 77 {
		t.Fatalf("schema version = %d, want 77", version)
	}
	if pending {
		t.Fatal("the recount marker is still set after a completed open")
	}
	seeded := 0
	for _, session := range fixture.Sessions {
		if session.FromV73 == nil {
			continue
		}
		seeded++
		want := storedTurnCounts{session.FromV73.WantTurnCount, session.FromV73.WantConversationTurnCount}
		if got := counts[session.SessionID]; got != want {
			t.Errorf("%s: stored counts = %+v, want %+v", session.Name, got, want)
		}
	}
	if seeded == 0 || len(counts) != seeded {
		t.Fatalf("store holds %d stats rows for %d seeded sessions", len(counts), seeded)
	}
	// Neither upgrade fold runs again: a value written after the upgrade
	// survives the next open.
	sentinel := storedTurnCounts{fixture.InterruptedFirstOpen.SentinelTurnCount, fixture.InterruptedFirstOpen.SentinelConversationTurnCount}
	writeTurnCountSentinel(t, path, fixture.Sessions[0].SessionID, sentinel)
	if err := openAndCloseForRecount(t, path); err != nil {
		t.Fatalf("reopen the upgraded store: %v", err)
	}
	counts, _, _ = readTurnCountRecountState(t, path)
	if got := counts[fixture.Sessions[0].SessionID]; got != sentinel {
		t.Fatalf("a reopen folded again: stored counts = %+v, want the written %+v", got, sentinel)
	}
}

func writeTurnCountSentinel(t *testing.T, path, sessionID string, sentinel storedTurnCounts) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET turn_count = ?, conversation_turn_count = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{sentinel.turnCount, sentinel.conversationTurnCount, sessionID}}); err != nil {
		t.Fatal(err)
	}
	if conn.Changes() != 1 {
		t.Fatalf("sentinel write changed %d rows, want 1", conn.Changes())
	}
}

func runTurnCountRecountInterruptedFirstOpen(t *testing.T, fixture turnCountRecountFixture) {
	path := seedTurnCountRecountStore(t, fixture, 75)
	failAfter := fixture.InterruptedFirstOpen.FailAfterSessions
	interrupted := errors.New("fixture interruption")
	handled := 0
	err := openAndCloseForRecount(t, path, func(o *openOptions) {
		o.turnCountRecountHook = func(done int, _ ingest.SessionID) error {
			handled = done
			if done == failAfter {
				return interrupted
			}
			return nil
		}
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("interrupted open returned %v, want the hook's error", err)
	}
	if handled != failAfter {
		t.Fatalf("the recount handled %d sessions before stopping, want %d", handled, failAfter)
	}
	for _, fragment := range []string{"turn count recount stopped after 2 of 6 sessions", "the recount stays pending", "reopen the store"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("interrupted open error lacks %q: %v", fragment, err)
		}
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != 77 {
		t.Fatalf("schema version after the failed open = %d, want 77: the version cannot say the recount is owed", version)
	}
	if !pending {
		t.Fatal("the failed open cleared the recount marker; the next open would not recount")
	}
	for _, session := range fixture.Sessions {
		want := storedTurnCounts{session.StoredTurnCount, session.StoredConversationTurnCount}
		if got := counts[session.SessionID]; got != want {
			t.Errorf("%s: the failed open wrote counts %+v, want the stored %+v", session.Name, got, want)
		}
	}

	if err := openAndCloseForRecount(t, path); err != nil {
		t.Fatalf("reopen after the interrupted open: %v", err)
	}
	counts, pending, _ = readTurnCountRecountState(t, path)
	if pending {
		t.Fatal("the reopen left the recount marker set")
	}
	for _, session := range fixture.Sessions {
		want := storedTurnCounts{session.WantTurnCount, session.WantConversationTurnCount}
		if got := counts[session.SessionID]; got != want {
			t.Errorf("%s: counts after the reopen = %+v, want %+v", session.Name, got, want)
		}
	}

	sentinel := storedTurnCounts{fixture.InterruptedFirstOpen.SentinelTurnCount, fixture.InterruptedFirstOpen.SentinelConversationTurnCount}
	writeTurnCountSentinel(t, path, fixture.Sessions[0].SessionID, sentinel)
	hookCalls := 0
	if err := openAndCloseForRecount(t, path, func(o *openOptions) {
		o.turnCountRecountHook = func(int, ingest.SessionID) error { hookCalls++; return nil }
	}); err != nil {
		t.Fatalf("third open: %v", err)
	}
	if hookCalls != 0 {
		t.Fatalf("the third open folded %d sessions, want none", hookCalls)
	}
	counts, _, _ = readTurnCountRecountState(t, path)
	if got := counts[fixture.Sessions[0].SessionID]; got != sentinel {
		t.Fatalf("the third open changed stored counts to %+v, want the written %+v", got, sentinel)
	}
}

func runTurnCountRecountNewStore(t *testing.T, _ turnCountRecountFixture) {
	path := filepath.Join(t.TempDir(), "new.db")
	hookCalls := 0
	if err := openAndCloseForRecount(t, path, func(o *openOptions) {
		o.turnCountRecountHook = func(int, ingest.SessionID) error { hookCalls++; return nil }
	}); err != nil {
		t.Fatalf("open a new store: %v", err)
	}
	counts, pending, version := readTurnCountRecountState(t, path)
	if version != 77 {
		t.Fatalf("new store schema version = %d, want 77", version)
	}
	if pending {
		t.Fatal("a new store holds a pending recount after its first open")
	}
	if hookCalls != 0 || len(counts) != 0 {
		t.Fatalf("a new store folded %d sessions over %d stats rows, want none", hookCalls, len(counts))
	}
	// A store with no sessions that reaches V77 through the migration chain
	// owes no recount either.
	chainPath := filepath.Join(t.TempDir(), "chain.db")
	conn, err := sqlite.OpenConn(chainPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(context.Background(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := readTurnCountRecountState(t, chainPath); pending {
		t.Fatal("an empty store migrated through the chain holds a pending recount")
	}
}
