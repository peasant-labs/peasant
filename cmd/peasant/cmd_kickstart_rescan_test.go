package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/kickstart_rescan.yaml
var kickstartRescanFixtureYAML []byte

// rescanSourceAge is the fixture spelling of where a session's source file sits
// relative to the ingest that recorded it. It is a closed set because the three
// positions produce three different diff verdicts, and a boolean column would
// collapse "still being written" into whichever neighbour it sat next to.
type rescanSourceAge string

const (
	// rescanSourceSettled is a source last written BEFORE its ingest and long
	// outside the staleness window.
	rescanSourceSettled rescanSourceAge = "settled"
	// rescanSourceChanged is a source written AFTER its ingest, but long enough
	// ago that it is no longer being written.
	rescanSourceChanged rescanSourceAge = "changed"
	// rescanSourceActive is a source written just now, inside the staleness
	// window, so this run will not re-ingest it.
	rescanSourceActive rescanSourceAge = "active"
)

var allRescanSourceAges = []rescanSourceAge{rescanSourceSettled, rescanSourceChanged, rescanSourceActive}

// rescanResolution is what a row asserts the scan DID: reuse the record, or walk
// git. It is the axis the whole slice exists for, so it is a named closed set
// rather than a bare bool.
type rescanResolution string

const (
	rescanReused   rescanResolution = "reused"
	rescanResolved rescanResolution = "resolved"
)

var allRescanResolutions = []rescanResolution{rescanReused, rescanResolved}

// rescanRecord is how much of a session the store holds. It is a closed set
// rather than a "recorded" boolean because a session row can outlive its
// metrics row: the ingest write path creates the pair together, but prune
// deletes the metrics row before the session row, so a read that treats metrics
// as mandatory drops a session the store still has. That third shape is a real
// state, so it is a named value here and not an untested branch.
type rescanRecord string

const (
	// rescanRecordComplete is a session with both its session row and its
	// computed metrics row.
	rescanRecordComplete rescanRecord = "complete"
	// rescanRecordWithoutMetrics is a session row whose metrics row is gone.
	rescanRecordWithoutMetrics rescanRecord = "without-metrics"
	// rescanRecordNone is a session the store never recorded.
	rescanRecordNone rescanRecord = "none"
)

var allRescanRecords = []rescanRecord{rescanRecordComplete, rescanRecordWithoutMetrics, rescanRecordNone}

// seeded reports whether the store holds a session row for this shape.
func (r rescanRecord) seeded() bool {
	return r == rescanRecordComplete || r == rescanRecordWithoutMetrics
}

// rescanSchemaAge is the metadata schema version a row's record was written
// under: the version this build writes, or one behind it.
type rescanSchemaAge string

const (
	rescanSchemaCurrent rescanSchemaAge = "current"
	rescanSchemaBehind  rescanSchemaAge = "behind"
)

// version returns the metadata schema version the record carries, failing the
// corpus on an unknown token rather than defaulting to one — a default would
// turn a typo into a weaker expectation that still passes.
func (a rescanSchemaAge) version(t *testing.T, caseName string) int {
	t.Helper()
	switch a {
	case rescanSchemaCurrent:
		return ingest.CurrentSchemaVersion
	case rescanSchemaBehind:
		return ingest.CurrentSchemaVersion - 1
	default:
		t.Fatalf("kickstart re-scan fixture case %q declares unknown recorded_schema_version %q; use %q or %q",
			caseName, string(a), rescanSchemaCurrent, rescanSchemaBehind)
		return 0
	}
}

type rescanFixtures struct {
	DeclaredRows   int          `yaml:"declared_rows"`
	ClaudeSlug     string       `yaml:"claude_slug"`
	ResolvedRemote string       `yaml:"resolved_remote"`
	ResolvedBranch string       `yaml:"resolved_branch"`
	Cases          []rescanCase `yaml:"cases"`
}

type rescanCase struct {
	Name                  string           `yaml:"name"`
	SessionID             string           `yaml:"session_id"`
	SessionCWD            string           `yaml:"session_cwd"`
	Record                rescanRecord     `yaml:"record"`
	RecordedRemote        string           `yaml:"recorded_remote"`
	RecordedBranch        string           `yaml:"recorded_branch"`
	RecordedTitle         string           `yaml:"recorded_title"`
	RecordedSchemaVersion rescanSchemaAge  `yaml:"recorded_schema_version"`
	SourceAge             rescanSourceAge  `yaml:"source_age"`
	WantResolution        rescanResolution `yaml:"want_resolution"`
	WantRemote            string           `yaml:"want_remote"`
	WantBranch            string           `yaml:"want_branch"`
	WantTitle             string           `yaml:"want_title"`
}

// loadRescanFixtures decodes the corpus and holds it to its own declared shape:
// the row count is pinned, every field an assertion depends on is non-blank, and
// both closed sets are covered. A corpus that silently loses a row still reports
// the coverage it no longer has.
func loadRescanFixtures(t *testing.T) rescanFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartRescanFixtureYAML))
	decoder.KnownFields(true)
	var fixtures rescanFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode kickstart re-scan fixtures: %v", err)
	}
	if fixtures.DeclaredRows != len(fixtures.Cases) {
		t.Fatalf("kickstart re-scan fixture declares %d rows but carries %d; a dropped row silently narrows the corpus",
			fixtures.DeclaredRows, len(fixtures.Cases))
	}
	testutil.RequireFixtureFields(t, "kickstart re-scan", "corpus", []testutil.FixtureField{
		{Key: "claude_slug", Value: fixtures.ClaudeSlug},
		{Key: "resolved_remote", Value: fixtures.ResolvedRemote},
		{Key: "resolved_branch", Value: fixtures.ResolvedBranch},
	})
	var ages []rescanSourceAge
	var resolutions []rescanResolution
	var records []rescanRecord
	for _, c := range fixtures.Cases {
		testutil.RequireFixtureFields(t, "kickstart re-scan", c.Name, []testutil.FixtureField{
			{Key: "name", Value: c.Name},
			{Key: "session_id", Value: c.SessionID},
			{Key: "session_cwd", Value: c.SessionCWD},
			{Key: "want_remote", Value: c.WantRemote},
			{Key: "want_branch", Value: c.WantBranch},
		})
		// A row with no metrics row has nowhere to keep a title, so declaring one
		// would describe a database that cannot exist.
		if c.Record == rescanRecordWithoutMetrics && c.RecordedTitle != "" {
			t.Fatalf("kickstart re-scan fixture case %q has record %q but declares recorded_title %q; the title lives in the metrics row this case removes",
				c.Name, string(c.Record), c.RecordedTitle)
		}
		ages = append(ages, c.SourceAge)
		resolutions = append(resolutions, c.WantResolution)
		records = append(records, c.Record)
	}
	testutil.RequireClosedSetCoverage(t, "kickstart re-scan", "source_age", allRescanSourceAges, ages)
	testutil.RequireClosedSetCoverage(t, "kickstart re-scan", "want_resolution", allRescanResolutions, resolutions)
	testutil.RequireClosedSetCoverage(t, "kickstart re-scan", "record", allRescanRecords, records)
	return fixtures
}

// dirCountingGitResolver records which directories a scan asked git about. Counting
// is the only way to prove a lookup did NOT happen: a scan that resolves a
// session it should have reused can still produce the right-looking listing, and
// only the call count shows the seconds it spent doing so.
type dirCountingGitResolver struct {
	*testutil.StubGitResolver
	remoteCalls map[string]int
	branchCalls map[string]int
}

var _ ingest.GitResolver = (*dirCountingGitResolver)(nil)

func newDirCountingGitResolver(remote, branch string) *dirCountingGitResolver {
	return &dirCountingGitResolver{
		StubGitResolver: &testutil.StubGitResolver{Remote: remote, BranchName: branch},
		remoteCalls:     map[string]int{},
		branchCalls:     map[string]int{},
	}
}

func (c *dirCountingGitResolver) RemoteURL(ctx context.Context, dir string) (string, error) {
	c.remoteCalls[dir]++
	return c.StubGitResolver.RemoteURL(ctx, dir)
}

func (c *dirCountingGitResolver) Branch(ctx context.Context, dir string) (string, error) {
	c.branchCalls[dir]++
	return c.StubGitResolver.Branch(ctx, dir)
}

func (c *dirCountingGitResolver) calls(dir string) int {
	return c.remoteCalls[dir] + c.branchCalls[dir]
}

// rescanTranscriptRoot is the in-memory Claude transcript root the fixture
// sessions are written under.
const rescanTranscriptRoot = "/rescan/claude/projects"

// rescanIngestedAt is how long before now the seeded records claim to have been
// ingested. It is far outside the staleness window, so a source written at that
// moment plus or minus half an hour is unambiguously settled or changed.
const rescanIngestedAgo = time.Hour

// writeRescanSources writes one transcript per fixture case and stamps each with
// the modification time its source_age describes. Each line carries only a
// working directory: with no branch and no user message in the transcript, a
// scan that does not reuse a record MUST ask git for both the remote and the
// branch, which is what makes the two paths distinguishable.
func writeRescanSources(t *testing.T, fs *testutil.MemFS, fixtures rescanFixtures, ingestedAt time.Time) {
	t.Helper()
	for _, c := range fixtures.Cases {
		path := filepath.Join(rescanTranscriptRoot, fixtures.ClaudeSlug, c.SessionID+defaults.ExtJSONL.String())
		line := fmt.Sprintf("{\"type\":\"assistant\",\"cwd\":%q}\n", c.SessionCWD)
		if err := fs.WriteFile(path, []byte(line), defaults.PublicFilePerm); err != nil {
			t.Fatalf("write transcript for %q: %v", c.Name, err)
		}
		switch c.SourceAge {
		case rescanSourceSettled:
			fs.ModTimes[path] = ingestedAt.Add(-30 * time.Minute)
		case rescanSourceChanged:
			fs.ModTimes[path] = ingestedAt.Add(30 * time.Minute)
		case rescanSourceActive:
			fs.ModTimes[path] = time.Now()
		default:
			t.Fatalf("kickstart re-scan fixture case %q declares unknown source_age %q; use one of %v",
				c.Name, string(c.SourceAge), allRescanSourceAges)
		}
	}
}

// seedRescanStore writes the recorded cases into a REAL store at dbPath through
// the production ingest write path. The database it leaves behind is the one a
// returning user would already have.
func seedRescanStore(t *testing.T, dbPath string, fixtures rescanFixtures, ingestedAt time.Time) {
	t.Helper()
	var withoutMetrics []string
	func() {
		db, err := store.Open(dbPath, store.WithPoolSize(1))
		if err != nil {
			t.Fatalf("open seed store: %v", err)
		}
		defer db.Close()

		ctx := t.Context()
		for i, c := range fixtures.Cases {
			if !c.Record.seeded() {
				continue
			}
			entry := rescanStoreEntry(t, fixtures, c, i, ingestedAt)
			if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
				t.Fatalf("seed session for %q: %v", c.Name, err)
			}
			if c.Record == rescanRecordWithoutMetrics {
				withoutMetrics = append(withoutMetrics, c.SessionID)
				continue
			}
			if c.RecordedTitle == "" {
				continue
			}
			title := c.RecordedTitle
			if err := db.SaveMetrics(ctx, &ingest.SessionMetrics{
				SessionID:      ingest.SessionID(c.SessionID),
				QualityMetrics: schema.QualityMetrics{TitleGenerated: &title},
			}); err != nil {
				t.Fatalf("seed metrics for %q: %v", c.Name, err)
			}
		}
	}()
	for _, sessionID := range withoutMetrics {
		dropSessionMetrics(t, dbPath, sessionID)
	}
}

// dropSessionMetrics removes one session's metrics row while leaving the session
// itself in place. The ingest write path creates the pair together, so the only
// way to reach the state a read must survive - a session the store still holds
// whose metrics are gone, which is what prune leaves behind between its two
// deletes - is to take the row out directly.
func dropSessionMetrics(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open %s to drop metrics: %v", dbPath, err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, "DELETE FROM session_metrics WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{sessionID},
	}); err != nil {
		t.Fatalf("drop metrics for %s: %v", sessionID, err)
	}
	if changed := conn.Changes(); changed != 1 {
		t.Fatalf("dropping metrics for %s removed %d rows, want 1; the case no longer describes a session that HAD metrics", sessionID, changed)
	}
}

// rescanStoreEntry builds the ingest metadata for one recorded case. The remote
// and branch are the values a previous run resolved from git, which is exactly
// what a re-scan must be able to reuse without asking again.
func rescanStoreEntry(t *testing.T, fixtures rescanFixtures, c rescanCase, index int, ingestedAt time.Time) ingest.StoreEntry {
	t.Helper()
	sessionID, err := ingest.NewSessionID(c.SessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", c.SessionID, err)
	}
	projectHash, err := ingest.NewProjectHash(strings.Repeat(fmt.Sprintf("%d", index+1), 64))
	if err != nil {
		t.Fatalf("NewProjectHash for %q: %v", c.Name, err)
	}
	hostSlug, err := ingest.NewHostSlug(fmt.Sprintf("github.com--acme--rescan-%d", index+1))
	if err != nil {
		t.Fatalf("NewHostSlug for %q: %v", c.Name, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	sourcePath, err := ingest.NewResolvedPath(filepath.Join(rescanTranscriptRoot, fixtures.ClaudeSlug, c.SessionID+defaults.ExtJSONL.String()))
	if err != nil {
		t.Fatalf("NewResolvedPath for %q: %v", c.Name, err)
	}
	ingestedMs := ingestedAt.UnixMilli()
	remote := c.RecordedRemote
	branch := c.RecordedBranch
	worktree := c.SessionCWD
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: c.RecordedSchemaVersion.version(t, c.Name),
			SessionID:     sessionID,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hostSlug,
			Timestamp:     ingest.TimestampInfo{Start: ingestedMs - 60000, End: ingestedMs, Ingested: &ingestedMs},
			Source:        ingest.SourceInfo{FilePath: string(sourcePath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: projectHash, Name: "rescan-project", FilePath: c.SessionCWD},
			Git:           ingest.GitContext{Remote: &remote, Branch: &branch, Worktree: &worktree},
			Stats:         ingest.StatsInfo{TurnCount: 1, ToolCallCount: 0, DurationMs: 60000},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sessionID,
			Harness:      defaults.HarnessClaudeCode,
			SourcePath:   sourcePath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

// rescanConfig points discovery at the in-memory transcript root and at nothing
// else, so the only sessions a run can find are the fixture's own.
func rescanConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.BaseConfig()
	for harness := range ingest.DefaultAdapterRegistry {
		src, ok := cfg.Sources.Provider(harness)
		if !ok {
			t.Fatalf("registered harness %q has no source configuration", harness)
		}
		if harness == defaults.HarnessClaudeCode {
			src.Enabled = true
			src.Paths = []string{rescanTranscriptRoot}
			continue
		}
		src.Paths = nil
	}
	return cfg
}

func listingByID(sessions []ftue.SessionListing) map[string]ftue.SessionListing {
	byID := make(map[string]ftue.SessionListing, len(sessions))
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	return byID
}

// TestKickstartRescan_ReusesRecordedSessions drives the real discovery core over
// a real store: a session the store already holds, whose source has not moved
// since, must cost ZERO git lookups and keep the project and branch that store
// recorded, while a new session, a changed one, and one recorded under an older
// metadata version are each resolved.
func TestKickstartRescan_ReusesRecordedSessions(t *testing.T) {
	t.Parallel()
	fixtures := loadRescanFixtures(t)
	ingestedAt := time.Now().Add(-rescanIngestedAgo)

	fs := testutil.NewMemFS()
	writeRescanSources(t, fs, fixtures, ingestedAt)

	dbPath := filepath.Join(t.TempDir(), "peasant.db")
	seedRescanStore(t, dbPath, fixtures, ingestedAt)

	known := loadKnownSessions(t.Context(), dbPath)
	if len(known) == 0 {
		t.Fatal("a database this build just wrote must be reusable, but no recorded session was loaded from it")
	}
	for _, fixture := range fixtures.Cases {
		if !fixture.Record.seeded() {
			continue
		}
		record, ok := known[fixture.SessionID]
		if !ok {
			t.Fatalf("recorded session %s is missing from the reusable kickstart index", fixture.SessionID)
		}
		if record.GitWorktree != fixture.SessionCWD {
			t.Errorf("session %s reusable worktree = %q, want %q", fixture.SessionID, record.GitWorktree, fixture.SessionCWD)
		}
		if record.CanonicalCwd != fixture.SessionCWD {
			t.Errorf("session %s reusable canonical cwd = %q, want %q", fixture.SessionID, record.CanonicalCwd, fixture.SessionCWD)
		}
	}

	git := newDirCountingGitResolver(fixtures.ResolvedRemote, fixtures.ResolvedBranch)
	inventory, sessions := ftueDiscoverWith(t.Context(), rescanConfig(t), fs, git, known, nil)

	if got := inventory[defaults.HarnessClaudeCode].SessionCount; got != len(fixtures.Cases) {
		t.Errorf("Claude inventory count = %d, want %d discovered sessions", got, len(fixtures.Cases))
	}
	byID := listingByID(sessions)
	if len(byID) != len(fixtures.Cases) {
		t.Fatalf("discovery listed %d sessions, want %d", len(byID), len(fixtures.Cases))
	}

	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			listing, ok := byID[c.SessionID]
			if !ok {
				t.Fatalf("session %s is missing from the listing", c.SessionID)
			}
			if listing.GitRemote != c.WantRemote {
				t.Errorf("git remote = %q, want %q", listing.GitRemote, c.WantRemote)
			}
			if listing.Branch != c.WantBranch {
				t.Errorf("branch = %q, want %q", listing.Branch, c.WantBranch)
			}
			if listing.Title != c.WantTitle {
				t.Errorf("title = %q, want %q", listing.Title, c.WantTitle)
			}
			switch c.WantResolution {
			case rescanReused:
				if got := git.calls(c.SessionCWD); got != 0 {
					t.Errorf("scan made %d git lookups for an already recorded, unchanged session; a re-scan must reuse the record instead", got)
				}
			case rescanResolved:
				if got := git.remoteCalls[c.SessionCWD]; got != 1 {
					t.Errorf("remote lookups = %d, want 1 for a session the store cannot answer for", got)
				}
				if got := git.branchCalls[c.SessionCWD]; got != 1 {
					t.Errorf("branch lookups = %d, want 1 for a session the store cannot answer for", got)
				}
			default:
				t.Fatalf("case declares unknown want_resolution %q; use one of %v", string(c.WantResolution), allRescanResolutions)
			}
		})
	}
}

// TestKickstartRescan_FallsBackWithoutCompatibleDatabase proves the gate: a
// missing database, one stamped with an older schema, and one stamped with a
// newer schema each fall back to resolving every session, and the fallback
// listing carries the resolved values rather than the recorded ones.
func TestKickstartRescan_FallsBackWithoutCompatibleDatabase(t *testing.T) {
	t.Parallel()
	fixtures := loadRescanFixtures(t)
	ingestedAt := time.Now().Add(-rescanIngestedAgo)

	dir := t.TempDir()
	missingPath := filepath.Join(dir, "absent.db")
	if known := loadKnownSessions(t.Context(), missingPath); known != nil {
		t.Errorf("a missing database loaded %d records; a first run has nothing to reuse", len(known))
	}

	for _, delta := range []int{-1, 1} {
		version := store.CurrentSchemaVersion() + delta
		path := filepath.Join(dir, fmt.Sprintf("stamped-%d.db", version))
		seedRescanStore(t, path, fixtures, ingestedAt)
		stampSchemaVersion(t, path, version)
		if known := loadKnownSessions(t.Context(), path); known != nil {
			t.Errorf("a database stamped at schema version %d (this build writes %d) loaded %d records; it must be re-scanned the full way instead",
				version, store.CurrentSchemaVersion(), len(known))
		}
	}

	// What that fallback then does: every session, recorded or not, is resolved.
	fs := testutil.NewMemFS()
	writeRescanSources(t, fs, fixtures, ingestedAt)
	git := newDirCountingGitResolver(fixtures.ResolvedRemote, fixtures.ResolvedBranch)
	_, sessions := ftueDiscoverWith(t.Context(), rescanConfig(t), fs, git, nil, nil)

	byID := listingByID(sessions)
	for _, c := range fixtures.Cases {
		listing, ok := byID[c.SessionID]
		if !ok {
			t.Fatalf("session %s is missing from the fallback listing", c.SessionID)
		}
		if listing.GitRemote != fixtures.ResolvedRemote {
			t.Errorf("%s: fallback git remote = %q, want the resolved %q", c.Name, listing.GitRemote, fixtures.ResolvedRemote)
		}
		if listing.Branch != fixtures.ResolvedBranch {
			t.Errorf("%s: fallback branch = %q, want the resolved %q", c.Name, listing.Branch, fixtures.ResolvedBranch)
		}
		if got := git.calls(c.SessionCWD); got == 0 {
			t.Errorf("%s: fallback made no git lookup; without a reusable database every session is resolved", c.Name)
		}
	}
}

// stampSchemaVersion rewrites the PRAGMA user_version of an existing database so
// it looks like the file another build left behind, with its data intact. The
// data staying readable is the point: the gate must refuse on the version alone.
func stampSchemaVersion(t *testing.T, dbPath string, version int) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open %s to stamp schema version: %v", dbPath, err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d", version), nil); err != nil {
		t.Fatalf("stamp schema version %d: %v", version, err)
	}
}
