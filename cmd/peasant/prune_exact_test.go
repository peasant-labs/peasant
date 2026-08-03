package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/prune_exact.yaml
var pruneExactFixtureYAML []byte

type pruneExactFixtures struct {
	DeclaredRows      int               `yaml:"declared_rows"`
	Sessions          []pruneExactEntry `yaml:"sessions"`
	ConcurrentSession pruneExactEntry   `yaml:"concurrent_session"`
	ConsentWindow     consentWindowCase `yaml:"consent_window"`
}

// pruneExactRole is the closed set of parts a planned row plays in the
// exactness proof. Coverage over it is what makes each row undeletable: a floor
// is one constant in another file that the same edit can decrement.
type pruneExactRole string

const (
	pruneRoleFirstOfSlug  pruneExactRole = "first-of-slug"
	pruneRoleSecondOfSlug pruneExactRole = "second-of-slug"
)

// allPruneExactRoles: the proof needs at least two sessions sharing a host slug,
// so that removing both is what empties the parent and removing one is not.
var allPruneExactRoles = []pruneExactRole{pruneRoleFirstOfSlug, pruneRoleSecondOfSlug}

type pruneExactEntry struct {
	SessionID  string         `yaml:"session_id"`
	OutputPath string         `yaml:"output_path"`
	Role       pruneExactRole `yaml:"role"`
}

// consentWindowCase describes everything that happens while the consent prompt
// is open. All of it belongs to one case because the window is what makes any
// of it observable.
type consentWindowCase struct {
	LateDiskSession  pruneExactEntry `yaml:"late_disk_session"`
	LateStoreSession pruneExactEntry `yaml:"late_store_session"`
	OrphanSession    pruneExactEntry `yaml:"orphan_session"`
	Notice           residueNotice   `yaml:"notice"`
}

// residueNotice is the closed set of questions the leftover-transcript warning
// has to answer. safeguard is present because a remedy that names the wrong
// object is worse than no remedy: it reads as authoritative.
type residueNotice struct {
	Finding     string `yaml:"finding"`
	Consequence string `yaml:"consequence"`
	Remedy      string `yaml:"remedy"`
	Safeguard   string `yaml:"safeguard"`
}

func (n residueNotice) axes() []testutil.FixtureField {
	return []testutil.FixtureField{
		{Key: "notice.finding", Value: n.Finding},
		{Key: "notice.consequence", Value: n.Consequence},
		{Key: "notice.remedy", Value: n.Remedy},
		{Key: "notice.safeguard", Value: n.Safeguard},
	}
}

func loadPruneExactFixtures(t *testing.T) pruneExactFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(pruneExactFixtureYAML))
	decoder.KnownFields(true)
	var fixtures pruneExactFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode exact prune fixture with strict fields: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("exact prune fixture must contain one YAML document: %v", err)
	}
	// Layer one: floor equals the row count, so deleting a row and decrementing
	// the declaration still trips it.
	if fixtures.DeclaredRows != len(fixtures.Sessions) || fixtures.DeclaredRows < 2 {
		t.Fatalf("exact prune fixture row guard failed: declared=%d actual=%d minimum=2", fixtures.DeclaredRows, len(fixtures.Sessions))
	}
	observedRoles := make([]pruneExactRole, 0, len(fixtures.Sessions))
	for i, session := range fixtures.Sessions {
		testutil.RequireFixtureFields(t, "exact prune", fmt.Sprintf("sessions[%d]", i), []testutil.FixtureField{
			{Key: "session_id", Value: session.SessionID},
			{Key: "output_path", Value: session.OutputPath},
			{Key: "role", Value: string(session.Role)},
		})
		if !slices.Contains(allPruneExactRoles, session.Role) {
			t.Fatalf("exact prune fixture sessions[%d] declares unknown role %q; use one of %v", i, session.Role, allPruneExactRoles)
		}
		observedRoles = append(observedRoles, session.Role)
	}
	// Layer two: derived coverage, for a row swapped at the same count.
	testutil.RequireClosedSetCoverage(t, "exact prune", "role", allPruneExactRoles, observedRoles)

	testutil.RequireFixtureFields(t, "exact prune", "concurrent_session", []testutil.FixtureField{
		{Key: "concurrent_session.session_id", Value: fixtures.ConcurrentSession.SessionID},
		{Key: "concurrent_session.output_path", Value: fixtures.ConcurrentSession.OutputPath},
	})

	window := fixtures.ConsentWindow
	for key, entry := range map[string]pruneExactEntry{
		"late_disk_session":  window.LateDiskSession,
		"late_store_session": window.LateStoreSession,
		"orphan_session":     window.OrphanSession,
	} {
		testutil.RequireFixtureFields(t, "exact prune", "consent_window."+key, []testutil.FixtureField{
			{Key: "consent_window." + key + ".session_id", Value: entry.SessionID},
			{Key: "consent_window." + key + ".output_path", Value: entry.OutputPath},
		})
	}
	if window.OrphanSession.OutputPath == window.LateDiskSession.OutputPath {
		t.Fatalf("consent_window.orphan_session must live under a DIFFERENT host slug than the late arrival (%q); sharing one slug makes 'named the orphan' and 'named the slug that happens to survive' the same assertion", window.OrphanSession.OutputPath)
	}
	testutil.RequireFixtureFields(t, "exact prune", "consent_window.notice", window.Notice.axes())
	seenFragment := map[string]string{}
	for _, axis := range window.Notice.axes() {
		if previous, duplicated := seenFragment[axis.Value]; duplicated {
			t.Fatalf("consent_window.notice reuses %q for both %s and %s; each axis must pin its own wording or one of them asserts nothing", axis.Value, previous, axis.Key)
		}
		seenFragment[axis.Value] = axis.Key
	}
	return fixtures
}

// openTestTerminal returns a pseudo-terminal pair. The prune command refuses to
// prompt for consent unless its input is a real terminal, so the mounted consent
// tests supply one rather than relaxing the production gate: a test seam that
// weakened the gate would prove the gate rather than the prompt.
//
// It is the sole gateway for every mounted guard on the destructive-consent
// path, so it SKIPS only where a pseudo-terminal genuinely cannot exist — a
// non-Linux builder — and FAILS everywhere else. A Linux box that cannot open
// /dev/ptmx is a broken test environment, not an unsupported one, and silently
// dropping the consent guards there reports a clean pass over an unobserved
// irreversible delete.
func openTestTerminal(t *testing.T) (master, terminal *os.File) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the pseudo-terminal allocation below is Linux-specific (/dev/ptmx + TIOCSPTLCK/TIOCGPTN); this is %s", runtime.GOOS)
	}
	const (
		ioctlUnlockPT = 0x40045431 // TIOCSPTLCK
		ioctlGetPTN   = 0x80045430 // TIOCGPTN
		brokenEnv     = "cannot allocate a pseudo-terminal on Linux, so the mounted consent guards cannot run: %v. This is a broken test environment rather than an unsupported platform — check /dev/ptmx and /dev/pts are present and the container is not restricting them."
	)
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf(brokenEnv, err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlUnlockPT, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatalf(brokenEnv, errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlGetPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatalf(brokenEnv, errno)
	}
	terminal, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf(brokenEnv, err)
	}
	t.Cleanup(func() { _ = terminal.Close() })
	if !term.IsTerminal(int(terminal.Fd())) {
		t.Fatalf(brokenEnv, errors.New("the allocated pseudo-terminal is not reported as a terminal"))
	}
	return master, terminal
}

// TestPruneCmd_ConsentCountEqualsDeletedCount drives the REAL prune command
// through the interactive consent path and asserts that the number the user
// approves is the number the command deletes. That equality is the whole reason
// the plan is frozen before the prompt, and it is claimed to the user on the one
// line that precedes an irreversible delete — so it has to be observed on the
// command, not on a plan a test builds for itself.
func TestPruneCmd_ConsentCountEqualsDeletedCount(t *testing.T) {
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store to count prunable sessions: %v", err)
	}
	seeded, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("count prunable sessions: %v", err)
	}
	db.Close()
	if len(seeded) == 0 {
		t.Fatal("the fixture seeded no prunable sessions, so a consent count of zero would pass vacuously")
	}

	master, terminal := openTestTerminal(t)
	if _, err := master.WriteString("y\n"); err != nil {
		t.Fatalf("write consent to the pseudo-terminal: %v", err)
	}

	cmd := BuildPruneCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(terminal)
	cmd.SetArgs([]string{"--all", "--data-dir", dir, "--config-dir", dir})
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("config-dir", "", "")
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prune --all through the interactive consent path: %v\n%s", err, out.String())
	}

	output := out.String()
	consented := fmt.Sprintf("Delete %d session(s)?", len(seeded))
	if !strings.Contains(output, consented) {
		t.Fatalf("the consent prompt does not ask about the %d session(s) the plan holds (looked for %q); a user approving a different number than the command deletes is the failure this test exists for:\n%s", len(seeded), consented, output)
	}
	deleted := fmt.Sprintf("deleted %d session(s)", len(seeded))
	if !strings.Contains(output, deleted) {
		t.Fatalf("the command did not report deleting the %d session(s) it asked about (looked for %q):\n%s", len(seeded), deleted, output)
	}

	verify, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("reopen store after prune: %v", err)
	}
	defer verify.Close()
	remaining, err := verify.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("count sessions after prune: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("the user consented to deleting %d session(s) but %d remain; the consented set and the deleted set must be the same set", len(seeded), len(remaining))
	}
}

// signalWriter reports the first time a fragment appears in what a command has
// written, so a test can act at a known point in the command's execution
// instead of sleeping and hoping.
type signalWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	needle string
	seen   chan struct{}
	fired  bool
}

func newSignalWriter(needle string) *signalWriter {
	return &signalWriter{needle: needle, seen: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if !w.fired && strings.Contains(w.buf.String(), w.needle) {
		w.fired = true
		close(w.seen)
	}
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestPruneCmd_PlanIsFrozenAcrossTheConsentWindow proves on the mounted command
// what the frozen plan exists to guarantee: a session that appears WHILE the
// user is deciding is not swept into a delete they never saw. The consent prompt
// is exactly where a real user leaves that window open, so that is where the
// test opens it.
func TestPruneCmd_PlanIsFrozenAcrossTheConsentWindow(t *testing.T) {
	window := loadPruneExactFixtures(t).ConsentWindow
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	syncDir := filepath.Join(string(defaults.ResolveDataDirPathWith(dir)), "peasant-sync")

	// On disk before the run, with no database row: genuinely leftover, and the
	// only thing the post-run notice may point a user at.
	orphanDir := filepath.Join(syncDir, window.OrphanSession.OutputPath, window.OrphanSession.SessionID)
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatalf("create an orphaned transcript directory: %v", err)
	}

	master, terminal := openTestTerminal(t)
	out := newSignalWriter("[y/N]")
	cmd := BuildPruneCommand()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(terminal)
	cmd.SetArgs([]string{"--all", "--data-dir", dir, "--config-dir", dir})
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("config-dir", "", "")

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	select {
	case <-out.seen:
	case err := <-done:
		t.Fatalf("prune finished without ever prompting for consent: %v\n%s", err, out.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("prune never reached the consent prompt:\n%s", out.String())
	}

	// The window is open: the user is looking at a preview of two sessions.
	// Two more arrive now — one on disk only, one in the STORE. The plan has to
	// hold against BOTH, because the delete has two halves and freezing only
	// the filesystem still lets the database sweep a row nobody previewed.
	lateDir := filepath.Join(syncDir, window.LateDiskSession.OutputPath, window.LateDiskSession.SessionID)
	if err := os.MkdirAll(lateDir, 0o700); err != nil {
		t.Fatalf("create a session that arrives on disk during the consent window: %v", err)
	}
	insertPruneSession(t, dir, window.LateStoreSession)

	if _, err := master.WriteString("y\n"); err != nil {
		t.Fatalf("give consent: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("prune --all after consent: %v\n%s", err, out.String())
	}
	output := out.String()

	if !strings.Contains(output, "Delete 2 session(s)?") {
		t.Fatalf("the user was asked about a set other than the two previewed sessions:\n%s", output)
	}
	if _, err := os.Stat(lateDir); err != nil {
		t.Fatalf("a session that appeared on disk after the preview was deleted anyway; only the confirmed plan may be removed: %v\n%s", err, output)
	}

	// The database half. Re-deriving the delete set from the store at execute
	// time is a plausible refactor and the exact time-of-check/time-of-use bug
	// the frozen plan exists to prevent.
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store after prune: %v", err)
	}
	defer db.Close()
	remaining, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("query sessions after prune: %v", err)
	}
	survived := slices.ContainsFunc(remaining, func(row ingest.PruneSessionRow) bool {
		return string(row.SessionID) == window.LateStoreSession.SessionID
	})
	if !survived {
		t.Fatalf("a session that entered the store after the preview was deleted from the database; the user consented to 2 rows and the delete must be those 2 rows, not whatever the store held at execute time (remaining=%d)\n%s", len(remaining), output)
	}

	// The residue notice, read with a live arrival present — the only condition
	// under which its wording can be wrong in the dangerous direction.
	orphanRelative := filepath.Join(window.OrphanSession.OutputPath, window.OrphanSession.SessionID)
	for _, axis := range window.Notice.axes() {
		if !strings.Contains(output, axis.Value) {
			t.Errorf("the leftover-transcript warning does not answer %s (%q):\n%s", axis.Key, axis.Value, output)
		}
	}
	if !strings.Contains(output, orphanRelative) {
		t.Errorf("the warning does not name the orphaned transcript directory %q, which is the only thing it may tell a user to delete:\n%s", orphanRelative, output)
	}
	if strings.Contains(output, window.LateDiskSession.SessionID) || strings.Contains(output, window.LateStoreSession.SessionID) {
		t.Errorf("the warning names a session that arrived during the consent window; those are live sessions the command deliberately protected, and listing them as leftover invites the user to delete exactly what it saved:\n%s", output)
	}
	// The remedy must not take the transcript tree as its object. Removing the
	// tree destroys every survivor, so the tree may appear ONLY as something the
	// notice tells the user to keep.
	for _, dangerous := range []string{
		"remove " + strconv.Quote(syncDir) + " manually",
		"and remove " + strconv.Quote(syncDir),
	} {
		if strings.Contains(output, dangerous) {
			t.Errorf("the warning instructs the user to remove the transcript tree itself (%q); followed literally that deletes the session this run just protected:\n%s", dangerous, output)
		}
	}
	if !strings.Contains(output, window.Notice.Safeguard+" "+strconv.Quote(syncDir)) {
		t.Errorf("the warning must say %q for the transcript tree by name, so a user acting on it cannot take the tree as the object:\n%s", window.Notice.Safeguard, output)
	}
}

// insertPruneSession adds one session row to the store the command is running
// against, so a test can make a session appear mid-run.
func insertPruneSession(t *testing.T, dir string, entry pruneExactEntry) {
	t.Helper()
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store to insert a mid-run session: %v", err)
	}
	defer db.Close()
	start := int64(1700200000000)
	ingested := start + 60000
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{{
		Metadata: &schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     schema.SessionID(entry.SessionID),
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("claude-opus-4-6"),
			HostSlug:      schema.HostSlug(entry.OutputPath),
			Project: schema.ProjectContext{
				Hash:     schema.ProjectHash(pruneTestProject),
				Name:     "prune-test",
				FilePath: "/test/prune",
			},
			Timestamp: schema.TimestampInfo{Start: start, End: start + 60000, Ingested: &ingested},
			Source:    schema.SourceInfo{FilePath: "/test/late.jsonl", Format: schema.SourceFormatJSONL},
		},
	}}); err != nil {
		t.Fatalf("insert a session during the consent window: %v", err)
	}
}

// TestPruneCmd_DeclinedConsentDeletesNothing is the other half: the same mounted
// path, an answer that is not "y", and nothing removed.
func TestPruneCmd_DeclinedConsentDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)

	master, terminal := openTestTerminal(t)
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatalf("write refusal to the pseudo-terminal: %v", err)
	}

	cmd := BuildPruneCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(terminal)
	cmd.SetArgs([]string{"--all", "--data-dir", dir, "--config-dir", dir})
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().String("config-dir", "", "")
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prune --all with consent refused: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Fatalf("refusing consent must abort visibly:\n%s", out.String())
	}
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatalf("open store after refused consent: %v", err)
	}
	defer db.Close()
	remaining, err := db.QueryPrunableSessions(t.Context(), ingest.PruneFilter{All: true})
	if err != nil {
		t.Fatalf("count sessions after refused consent: %v", err)
	}
	if len(remaining) == 0 {
		t.Fatal("consent was refused and every session was deleted anyway")
	}
}

func TestPrunePlan_IsImmutableAndFilesystemExact(t *testing.T) {
	t.Parallel()
	fixtures := loadPruneExactFixtures(t)
	rows := make([]ingest.PruneSessionRow, len(fixtures.Sessions))
	for i, fixture := range fixtures.Sessions {
		sessionID, err := ingest.NewSessionID(fixture.SessionID)
		if err != nil {
			t.Fatalf("fixture session ID: %v", err)
		}
		rows[i] = ingest.PruneSessionRow{SessionID: sessionID, OutputPath: fixture.OutputPath}
	}
	plan := ingest.NewPrunePlan(rows)
	rows[0].OutputPath = "mutated-after-preview"
	returned := plan.Sessions()
	returned[0].OutputPath = "mutated-through-accessor"
	planned := plan.Sessions()
	plannedIDs := plan.SessionIDs()
	for i, fixture := range fixtures.Sessions {
		if planned[i].OutputPath != fixture.OutputPath || string(planned[i].SessionID) != fixture.SessionID || string(plannedIDs[i]) != fixture.SessionID {
			t.Fatalf("immutable plan row %d changed: session=%+v id=%q fixture=%+v", i, planned[i], plannedIDs[i], fixture)
		}
	}

	dataHome := t.TempDir()
	syncDir := filepath.Join(dataHome, "peasant", "peasant-sync")
	for _, session := range fixtures.Sessions {
		if err := os.MkdirAll(filepath.Join(syncDir, session.OutputPath, session.SessionID), 0o700); err != nil {
			t.Fatalf("create planned session directory: %v", err)
		}
	}
	concurrentID, err := ingest.NewSessionID(fixtures.ConcurrentSession.SessionID)
	if err != nil {
		t.Fatalf("fixture concurrent session ID: %v", err)
	}
	concurrentDir := filepath.Join(syncDir, fixtures.ConcurrentSession.OutputPath, string(concurrentID))
	if err := os.MkdirAll(concurrentDir, 0o700); err != nil {
		t.Fatalf("create concurrent session directory: %v", err)
	}

	if errs := pruneFilesystem(dataDirCmd(t, dataHome), planned); len(errs) != 0 {
		t.Fatalf("prune exact filesystem plan: %v", errs)
	}
	for _, session := range fixtures.Sessions {
		if _, err := os.Stat(filepath.Join(syncDir, session.OutputPath, session.SessionID)); !os.IsNotExist(err) {
			t.Fatalf("planned session %q still exists: %v", session.SessionID, err)
		}
	}
	if _, err := os.Stat(concurrentDir); err != nil {
		t.Fatalf("concurrent unplanned session was removed: %v", err)
	}
}
