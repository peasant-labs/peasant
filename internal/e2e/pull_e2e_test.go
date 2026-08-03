//go:build e2e

// End-to-end pull round-trip + pollution gate. It REUSES the skip-gate harness's
// provisioning machinery (podman Postgres + MinIO + the real village ./cmd/server
// + the real peasant CLI in throwaway XDG sandboxes) and adds a SECOND village
// user (village_users.go) to drive the full pull surface end-to-end.
//
// Flow (the anchored validation set exercised below):
//
//	user1 (setup-demo) ingests the committed fixture → push → village holds 2
//	transcripts (root + subagent) owned by user1, each with a server content_hash.
//
//	LOGGED-OUT gates (user2 sandbox, no credentials): pull / remote list /
//	annotations sync exit non-zero with an actionable error naming
//	`peasant village login` and write NOTHING locally; `list --local` and
//	`context` WORK offline. Unauthenticated raw API ⇒ 401 on every pull route.
//
//	Group-share user1's ROOT transcript to user2 (BEFORE user2 annotates).
//	user2 pulls it by UUID → files (transcript + metadata + manifest) + V34 rows;
//	re-pull ⇒ up-to-date (no content rewrite); --force re-downloads; pull by
//	pasted URL succeeds. The NOT-shared subagent (set PUBLIC to prove public is
//	excluded) ⇒ not-found, no local writes. `context` renders a pulled transcript.
//
//	user2 annotates user1's shared transcript via the village API; user1 runs
//	`annotations sync` ⇒ user2's annotation surfaces foreign-marked WITH author
//	identity, user1's OWN are excluded.
//
//	POLLUTION GATE (table-level, user1's sandbox): sessions / session_entries /
//	session_metrics / annotations are row-identical before/after all pulls;
//	peasant-sync/ is byte-identical; `push --dry-run` candidate set is unchanged.
//	Local harness and XDG directories are never touched (sandbox isolation).
package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// foreignAnnotationType / foreignAnnotationValue is the manual annotation user2
// authors on user1's shared transcript via the village API. A distinct author
// (user2) makes its content hash differ from user1's own pushed annotations, so
// the refresh path keeps it (and excludes user1's own).
const (
	foreignAnnotationType = "quality.session_approval"
	foreignAnnotationVal  = "approve"
)

// TestPullRoundTripE2E is the asserted pull round-trip + pollution gate.
func TestPullRoundTripE2E(t *testing.T) {
	requirePodman(t)
	reapStaleE2EInfra(t)
	bins := resolveVillageBinaries(t)
	peasantBin := buildPeasant(t)

	// Sandbox isolation: never touch the real data dir. We snapshot the real DATA
	// dir before and assert it unchanged after (mirrors the skip-gate harness).
	realStateDir := string(defaults.ResolveStateDirPath())
	realDataDir := string(defaults.ResolveDataDirPath())
	if err := pruneStaleSandboxes(realStateDir); err != nil {
		t.Fatalf("prune stale sandboxes: %v", err)
	}
	realBefore := snapshotRealDataDir(realDataDir)

	// TWO independent sandboxes (distinct nanosecond tokens): user1 publishes +
	// owns, user2 pulls. Each has its own XDG_{DATA,CONFIG,STATE}_HOME so they
	// never share local state.
	sandboxToken := time.Now().UnixNano()
	user1XDG, user1Dirs := makeSandbox(t, realStateDir, sandboxToken)
	user2XDG, user2Dirs := makeSandbox(t, realStateDir, sandboxToken+1)

	// Ephemeral infra — Postgres (with an open database/sql handle for in-DB SQL),
	// MinIO, the real village server, user1 via setup-demo, user2 via the sibling
	// mint. startEphemeralPostgres owns container cleanup; the parameterized seed
	// helpers take the returned *sql.DB.
	bucket := uniqueName("transcripts")
	dsn, db := startEphemeralPostgres(t)
	minioEndpoint := startEphemeralMinIO(t, bucket)
	villageURL := startVillageServer(t, bins.server, dsn, minioEndpoint, bucket)

	// user1 owns the published transcripts; capture its API key to drive the
	// owner-authenticated seed steps (group-share + set-public) through the real
	// village API. The pull harness also drives user1's peasant subprocesses via the
	// sandbox credentials.json mintDemoCredentials writes, and reads user1's village
	// id from it.
	user1APIKey := mintDemoCredentials(t, bins.setupDemo, dsn, villageURL, user1Dirs.config)
	user1ID := readDemoUserID(t, user1Dirs.config)
	user1Username := readDemoUsername(t, user1Dirs.config)
	user2 := mintVillageUser(t, db, villageURL, secondUserGitHubID, secondUsername, secondKeyLabel)
	writeCredentialsFor(t, user2Dirs.config, user2)

	// user1: scope ingest to the committed fixture, write under the sandbox, push.
	writeSandboxConfig(t, user1Dirs.config, user1Dirs.data)
	runPeasant(t, peasantBin, user1XDG, "ingest", "--include-active")

	user1DB := filepath.Join(user1Dirs.data, string(defaults.AppName), "peasant.db")
	assertDBUnderSandbox(t, harnessOptions{assert: true}, user1Dirs.root, user1DB)
	associationFixture := loadAssociationRoundTripFixture(t)
	ingestedDB := user1DB + ".ingested-fixture"
	if err := os.Rename(user1DB, ingestedDB); err != nil {
		t.Fatalf("move ingested fixture database aside before constructing V39 bridge: %v", err)
	}
	buildV39LegacyFixture(t, user1DB, ingestedDB, associationFixture)
	if err := os.Remove(ingestedDB); err != nil {
		t.Fatalf("remove temporary current-schema ingest fixture database: %v", err)
	}
	assertV39AssociationFixture(t, user1DB, associationFixture)

	sessionIDs := readSessionIDs(t, user1DB)
	if len(sessionIDs) != ExpectedTranscriptCount {
		t.Fatalf("ingested %d sessions, want %d (see Expected*Transcripts in fixture.go)", len(sessionIDs), ExpectedTranscriptCount)
	}
	for _, sid := range sessionIDs {
		runPeasant(t, peasantBin, user1XDG, "annotate", "create", sid, systemAnnotationType, systemAnnotationVal)
	}
	// The first annotation command above is the current CLI's first open of this
	// database. Assert its ordinary V40/V41 upgrade created the anchor and made
	// the already-pushed session eligible before adding the target annotation.
	associationID := assertMigratedAssociationReplay(t, user1DB, associationFixture)
	createAssociationRoundTripAnnotation(t, user1DB, associationID, associationFixture)

	// This is an ordinary, non-force CLI push. The local target annotation exists
	// before the call, so Village can accept it only after the replayed transcript
	// stage has published the owner-scoped association anchor.
	p1 := runPeasantAssociationPush(t, peasantBin, user1XDG)
	if p1.New != ExpectedClaudeTranscripts || p1.Updated != 0 || p1.Skipped != 0 || p1.Errors != 0 || p1.Held != 0 {
		t.Fatalf("historical association push transcript result = %+v, want %d new and no other outcomes", p1, ExpectedClaudeTranscripts)
	}
	if p1.Annotations.Total == 0 || p1.Annotations.Created != p1.Annotations.Total || p1.Annotations.Updated != 0 || p1.Annotations.Skipped != 0 || p1.Annotations.Retracted != 0 || p1.Annotations.Errors != 0 || p1.Annotations.Error != "" {
		t.Fatalf("historical association push annotation result = %+v, want all first-push annotations created", p1.Annotations)
	}

	// Discover the village UUIDs user1's push produced.
	vts := listVillageTranscripts(t, db, user1ID)
	if len(vts) != ExpectedClaudeTranscripts {
		t.Fatalf("village holds %d transcripts for user1, want %d", len(vts), ExpectedClaudeTranscripts)
	}
	rootTranscript := villageTranscriptByLocalID(t, vts, FixtureRootSessionID)
	subagentTranscript := villageTranscriptByLocalID(t, vts, FixtureClaudeSubagentSessionID)
	associationContentHash := assertVillageAssociationRoundTrip(t, db, user1ID, rootTranscript.ID, associationID, associationFixture)
	assertQuietAssociationPush(t, runPeasantAssociationPush(t, peasantBin, user1XDG), p1.Annotations.Total)

	// ============================ POLLUTION GATE: BEFORE ============================
	// Snapshot every user1-owned local artifact the pull MUST NOT touch. Captured
	// here, AFTER push (the steady state), and re-checked at the end.
	beforeTables := snapshotPollutionTables(t, user1DB)
	beforeSyncTree := snapshotDirTree(t, filepath.Join(user1Dirs.data, string(defaults.AppName), "peasant-sync"))
	beforeDryRun := runPeasantPushDryRun(t, peasantBin, user1XDG)

	// ============================ LOGGED-OUT GATES (user2 has NO creds yet) ============================
	// user2's sandbox so far has credentials.json written; remove it to test the
	// genuinely-logged-out path, then restore it for the pull cases below.
	loggedOutXDG, loggedOut := makeSandbox(t, realStateDir, sandboxToken+2)
	assertLoggedOutGates(t, peasantBin, loggedOutXDG, loggedOut, rootTranscript.ID, villageURL)

	// ============================ UNAUTHENTICATED API ⇒ 401 ============================
	assertUnauthenticatedAPI401(t, villageURL, rootTranscript.ID)

	// ============================ GROUP-SHARE (before user2 annotates) ============================
	groupShareTranscript(t, villageURL, user1APIKey, user2.Username, rootTranscript.ID, "pull-e2e-group")
	assertLatestVisibilityAudit(t, db, rootTranscript.ID, "shared", user1ID)
	assertRemoteListAssociationCount(t, peasantBin, user2XDG, rootTranscript.ID, associationFixture.ExpectedAnnotationCount)

	// ============================ user2 PULLS the shared transcript ============================
	pullsRoot := filepath.Join(user2Dirs.data, string(defaults.AppName), defaults.VillagePullsSubdir)
	host := villageHostOf(t, villageURL)
	pullDir := filepath.Join(pullsRoot, host, rootTranscript.ID)

	// Pull by bare UUID.
	pr := runPeasantPullJSON(t, peasantBin, user2XDG, rootTranscript.ID)
	if pr.Status != pull.PullStatusPulled.String() {
		t.Fatalf("pull by UUID status = %q, want %q\n%+v", pr.Status, pull.PullStatusPulled, pr)
	}
	if pr.AnnotationCount < associationFixture.ExpectedAnnotationCount {
		t.Fatalf("pull by UUID annotation count = %d, want at least %d to include the association annotation", pr.AnnotationCount, associationFixture.ExpectedAnnotationCount)
	}
	assertPullArtifacts(t, pullDir)
	user2DB := filepath.Join(user2Dirs.data, string(defaults.AppName), "peasant.db")
	assertPulledRows(t, user2DB, rootTranscript.ID, host)
	assertPulledAssociationAnnotation(t, user2DB, rootTranscript.ID, host, associationID, associationContentHash, associationFixture, user1ID, user1Username)

	// Re-pull (unchanged) ⇒ up-to-date, NO content rewrite (manifest mtime stable).
	manifestPath := filepath.Join(pullDir, "pull-manifest.json")
	beforeManifest := readFileBytes(t, manifestPath)
	rePull := runPeasantPullJSON(t, peasantBin, user2XDG, rootTranscript.ID)
	if rePull.Status != pull.PullStatusUpToDate.String() {
		t.Fatalf("re-pull status = %q, want %q (no re-download)", rePull.Status, pull.PullStatusUpToDate)
	}
	if afterManifest := readFileBytes(t, manifestPath); !bytesEqual(beforeManifest, afterManifest) {
		t.Fatalf("re-pull rewrote pull-manifest.json — expected no content re-download")
	}

	// --force re-downloads (status pulled again, even though unchanged). Symmetric
	// with the re-pull byte-stability check above: capture the manifest before and
	// assert the bytes CHANGED after — a genuine re-download rewrites the manifest
	// with a fresh pulledAt, so stable bytes would mean --force reported "pulled"
	// but skipped the actual rewrite.
	beforeForce := readFileBytes(t, manifestPath)
	forced := runPeasantPullJSON(t, peasantBin, user2XDG, rootTranscript.ID, "--force")
	if forced.Status != pull.PullStatusPulled.String() {
		t.Fatalf("--force pull status = %q, want %q (re-download)", forced.Status, pull.PullStatusPulled)
	}
	if afterForce := readFileBytes(t, manifestPath); bytesEqual(beforeForce, afterForce) {
		t.Fatalf("--force left pull-manifest.json byte-identical — expected a re-download to rewrite it (fresh pulledAt)")
	}

	// Pull by pasted village web URL (same transcript, different sandbox-free ref).
	url := fmt.Sprintf("%s/transcripts/%s", villageURL, rootTranscript.ID)
	byURL := runPeasantPullJSON(t, peasantBin, user2XDG, url)
	if !byURL.FromURL {
		t.Fatalf("pull by URL: fromUrl = false, want true (URL ref not recognized)")
	}
	if byURL.Status != pull.PullStatusPulled.String() && byURL.Status != pull.PullStatusUpToDate.String() {
		t.Fatalf("pull by URL status = %q, want pulled or up-to-date", byURL.Status)
	}

	// ============================ NOT-SHARED (PUBLIC-but-unshared) ⇒ 404-style, no local writes ============================
	// Set the subagent PUBLIC to prove the MVP pull policy EXCLUDES public.
	setTranscriptPublic(t, villageURL, user1APIKey, subagentTranscript.ID)
	assertLatestVisibilityAudit(t, db, subagentTranscript.ID, "public", user1ID)
	subagentPullDir := filepath.Join(pullsRoot, host, subagentTranscript.ID)
	notShared := runPeasantPullExpectFail(t, peasantBin, user2XDG, subagentTranscript.ID)
	if notShared.Status != pull.PullStatusNotFound.String() {
		t.Fatalf("public-but-unshared pull status = %q, want %q (404-style)\nerror: %s",
			notShared.Status, pull.PullStatusNotFound, notShared.Error)
	}
	if _, err := os.Stat(subagentPullDir); err == nil {
		t.Fatalf("not-shared pull wrote a local dir %s — expected no local writes", subagentPullDir)
	}
	if rowExists(t, user2DB, "pulled_transcripts", "transcript_id", subagentTranscript.ID) {
		t.Fatalf("not-shared pull inserted a pulled_transcripts row for %s — expected none", subagentTranscript.ID)
	}

	// ============================ context renders a pulled transcript (sanity) ============================
	ctxOut := runPeasant(t, peasantBin, user2XDG, "village", "transcripts", "context", rootTranscript.ID)
	if !strings.Contains(ctxOut, rootTranscript.ID) {
		t.Fatalf("context output does not mention the transcript id %s:\n%s", rootTranscript.ID, ctxOut)
	}

	// list --local shows the pulled transcript (offline).
	localList := runPeasant(t, peasantBin, user2XDG, "village", "transcripts", "list", "--local")
	if !strings.Contains(localList, rootTranscript.ID) {
		t.Fatalf("list --local does not show the pulled transcript %s:\n%s", rootTranscript.ID, localList)
	}

	// ============================ FOREIGN ANNOTATION REFRESH ============================
	// user2 annotates user1's SHARED transcript via the village API (gate passes
	// now that it is shared). Then user1 runs `annotations sync` and must surface
	// user2's annotation foreign-marked WITH author identity, EXCLUDING its own.
	status, body := createVillageAnnotation(t, villageURL, user2.APIKey, rootTranscript.ID,
		foreignAnnotationType, foreignAnnotationVal, 0)
	if status != http.StatusCreated {
		t.Fatalf("user2 create annotation on shared transcript: status = %d, want 201\nbody: %s", status, body)
	}

	sync := runPeasantSyncJSON(t, peasantBin, user1XDG)
	if sync.Created < 1 {
		t.Fatalf("annotations sync created = %d, want >=1 (user2's foreign annotation)\n%+v", sync.Created, sync)
	}
	if sync.Excluded < 1 {
		t.Fatalf("annotations sync excluded = %d, want >=1 (user1's own annotations)\n%+v", sync.Excluded, sync)
	}
	// The persisted foreign row carries user2's author identity, never user1's.
	assertForeignAnnotationAuthor(t, user1DB, rootTranscript.ID, user2.UserID, secondUsername, user1ID)

	// ============================ POLLUTION GATE: AFTER ============================
	afterTables := snapshotPollutionTables(t, user1DB)
	if diff := diffTableSnapshots(beforeTables, afterTables); diff != "" {
		t.Fatalf("POLLUTION GATE FAILED — user1 analytics tables changed after pulls:\n%s", diff)
	}
	afterSyncTree := snapshotDirTree(t, filepath.Join(user1Dirs.data, string(defaults.AppName), "peasant-sync"))
	if diff := diffTrees(beforeSyncTree, afterSyncTree); diff != "" {
		t.Fatalf("POLLUTION GATE FAILED — user1 peasant-sync/ tree changed after pulls:\n%s", diff)
	}
	afterDryRun := runPeasantPushDryRun(t, peasantBin, user1XDG)
	if beforeDryRun != afterDryRun {
		t.Fatalf("POLLUTION GATE FAILED — push --dry-run candidate set changed after pulls\nbefore: %s\nafter:  %s",
			beforeDryRun, afterDryRun)
	}
	assertVillageHealth(t, harnessOptions{assert: true}, villageURL, "after full pull round-trip flow")

	// ============================ SANDBOX ISOLATION ============================
	assertRealDataDirUnchanged(t, harnessOptions{assert: true}, realDataDir, realBefore)
}

// --- sandbox helpers ---

type sandboxDirs struct {
	root, data, config, state string
}

// makeSandbox creates a fresh timestamped sandbox (data/config/state) and returns
// the XDG env slice + the resolved dirs. Each user gets its own so they never
// share local state. t.Cleanup removes it.
func makeSandbox(t *testing.T, realStateDir string, unixTS int64) ([]string, sandboxDirs) {
	t.Helper()
	root := computeSandbox(realStateDir, unixTS)
	data, config, state := sandboxXDG(root)
	for _, d := range []string{data, config, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir sandbox dir %s: %v", d, err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return xdgEnvAssignments(data, config, state), sandboxDirs{root: root, data: data, config: config, state: state}
}

// readDemoUserID reads the user_id from the demo credentials.json setup-demo wrote.
func readDemoUserID(t *testing.T, configHome string) string {
	t.Helper()
	data := readFileBytes(t, filepath.Join(configHome, string(defaults.AppName), "credentials.json"))
	var creds struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(data, &creds); err != nil || creds.UserID == "" {
		t.Fatalf("read demo user_id from credentials.json: %v\n%s", err, data)
	}
	return creds.UserID
}

func readDemoUsername(t *testing.T, configHome string) string {
	t.Helper()
	data := readFileBytes(t, filepath.Join(configHome, string(defaults.AppName), "credentials.json"))
	var creds struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &creds); err != nil || creds.Username == "" {
		t.Fatalf("read demo username from credentials.json: %v\n%s", err, data)
	}
	return creds.Username
}

func loadAssociationRoundTripFixture(t *testing.T) fixtureAssociationRoundTrip {
	t.Helper()
	m, err := LoadFixtureIndex(filepath.Join(FixtureSourcePath(), fixtureIndexFile))
	if err != nil {
		t.Fatalf("load Claude association_roundtrip fixture: %v", err)
	}
	if m.AssociationRoundTrip == nil {
		t.Fatal("Claude fixture index is missing association_roundtrip")
	}
	return *m.AssociationRoundTrip
}

func assertV39AssociationFixture(t *testing.T, dbPath string, fixture fixtureAssociationRoundTrip) {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()

	var version int
	if err := sqlitex.Execute(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read V39 fixture user_version: %v", err)
	}
	if version != 39 {
		t.Fatalf("legacy fixture user_version = %d, want 39 before the current CLI opens it", version)
	}

	var ledgerTables int
	if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_commit_associations'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ledgerTables = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count V40 ledger tables in legacy fixture: %v", err)
	}
	if ledgerTables != 0 {
		t.Fatalf("legacy fixture contains %d V40 association ledger tables, want none", ledgerTables)
	}

	var (
		pushedRows int
		pushedAt   int64
		license    string
	)
	if err := sqlitex.Execute(conn, `SELECT pushed_at, COALESCE(license_id, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			pushedRows++
			if stmt.ColumnType(0) == sqlite.TypeNull {
				return fmt.Errorf("target session has NULL pushed_at")
			}
			pushedAt = stmt.ColumnInt64(0)
			license = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("read V39 already-pushed session state: %v", err)
	}
	if pushedRows != 1 || pushedAt != fixture.PushedAt || license != string(schema.LicenseCCBY) {
		t.Fatalf("V39 session state = rows=%d pushed_at=%d license=%q, want rows=1 pushed_at=%d license=%q", pushedRows, pushedAt, license, fixture.PushedAt, schema.LicenseCCBY)
	}

	var sourceRows int
	if err := sqlitex.Execute(conn, `SELECT session_id, commit_hash, message, author_time
FROM session_commits
WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sourceRows++
			if stmt.ColumnType(3) == sqlite.TypeNull {
				return fmt.Errorf("observed commit row has NULL author_time")
			}
			if stmt.ColumnText(0) != fixture.SessionID || stmt.ColumnText(1) != fixture.ObservedCommitHash || stmt.ColumnText(2) != fixture.Subject || stmt.ColumnInt64(3) != fixture.AuthorTime {
				return fmt.Errorf("observed commit row = session=%q hash=%q subject=%q author_time=%d, want session=%q hash=%q subject=%q author_time=%d", stmt.ColumnText(0), stmt.ColumnText(1), stmt.ColumnText(2), stmt.ColumnInt64(3), fixture.SessionID, fixture.ObservedCommitHash, fixture.Subject, fixture.AuthorTime)
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read V39 observed commit source row: %v", err)
	}
	if sourceRows != 1 {
		t.Fatalf("V39 observed commit source rows = %d, want exactly 1", sourceRows)
	}
}

func assertMigratedAssociationReplay(t *testing.T, dbPath string, fixture fixtureAssociationRoundTrip) schema.AssociationID {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()

	var version int
	if err := sqlitex.Execute(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read upgraded fixture user_version: %v", err)
	}
	if version < 41 {
		t.Fatalf("current CLI upgraded fixture to user_version=%d, want V40/V41 or later", version)
	}

	var associationCount int
	if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_commit_associations`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			associationCount = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count migrated association ledger rows: %v", err)
	}
	if associationCount != 1 {
		t.Fatalf("migrated association ledger rows = %d, want exactly 1 backfilled source fact", associationCount)
	}

	var (
		associationID   schema.AssociationID
		associationRows int
	)
	if err := sqlitex.Execute(conn, `SELECT association_id, session_id, observed_commit_hash, subject, author_time, created_at
FROM session_commit_associations
WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			associationRows++
			id, err := schema.NewAssociationID(stmt.ColumnText(0))
			if err != nil {
				return fmt.Errorf("backfilled association ID %q is invalid: %w", stmt.ColumnText(0), err)
			}
			if stmt.ColumnType(4) == sqlite.TypeNull {
				return fmt.Errorf("backfilled association %q has NULL author_time", id)
			}
			if stmt.ColumnText(1) != fixture.SessionID || stmt.ColumnText(2) != fixture.ObservedCommitHash || stmt.ColumnText(3) != fixture.Subject || stmt.ColumnInt64(4) != fixture.AuthorTime || stmt.ColumnInt64(5) <= 0 {
				return fmt.Errorf("backfilled association = session=%q hash=%q subject=%q author_time=%d created_at=%d, want session=%q hash=%q subject=%q author_time=%d and a created_at timestamp", stmt.ColumnText(1), stmt.ColumnText(2), stmt.ColumnText(3), stmt.ColumnInt64(4), stmt.ColumnInt64(5), fixture.SessionID, fixture.ObservedCommitHash, fixture.Subject, fixture.AuthorTime)
			}
			associationID = id
			return nil
		},
	}); err != nil {
		t.Fatalf("read migrated association ledger row: %v", err)
	}
	if associationRows != 1 {
		t.Fatalf("migrated target association rows = %d, want exactly 1", associationRows)
	}

	var sourceRows int
	if err := sqlitex.Execute(conn, `SELECT session_id, commit_hash, message, author_time
FROM session_commits
WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sourceRows++
			if stmt.ColumnType(3) == sqlite.TypeNull || stmt.ColumnText(0) != fixture.SessionID || stmt.ColumnText(1) != fixture.ObservedCommitHash || stmt.ColumnText(2) != fixture.Subject || stmt.ColumnInt64(3) != fixture.AuthorTime {
				return fmt.Errorf("migration changed the observed commit source row")
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read observed commit source after V40: %v", err)
	}
	if sourceRows != 1 {
		t.Fatalf("observed commit source rows after V40 = %d, want exactly 1", sourceRows)
	}

	var replayRows int
	if err := sqlitex.Execute(conn, `SELECT pushed_at FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{fixture.SessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			replayRows++
			if stmt.ColumnType(0) != sqlite.TypeNull {
				return fmt.Errorf("backfilled already-pushed session retained pushed_at=%d", stmt.ColumnInt64(0))
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read V40 replay eligibility: %v", err)
	}
	if replayRows != 1 {
		t.Fatalf("V40 replay eligibility rows = %d, want exactly 1", replayRows)
	}

	var targetAnnotations int
	if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM annotation_target_associations`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			targetAnnotations = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count association-target annotations before target creation: %v", err)
	}
	if targetAnnotations != 0 {
		t.Fatalf("association-target annotations before explicit creation = %d, want 0", targetAnnotations)
	}
	return associationID
}

func createAssociationRoundTripAnnotation(t *testing.T, dbPath string, associationID schema.AssociationID, fixture fixtureAssociationRoundTrip) {
	t.Helper()
	sessionID, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatalf("association_roundtrip session_id %q is not a valid session ID: %v", fixture.SessionID, err)
	}
	localStore, err := store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatalf("open production store for association_roundtrip seed at %s: %v", dbPath, err)
	}
	defer func() {
		if closeErr := localStore.Close(); closeErr != nil {
			t.Errorf("close production store after association_roundtrip seed: %v", closeErr)
		}
	}()

	ctx := context.Background()
	associations, err := localStore.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListCurrentSessionCommitAssociations(%s): %v", sessionID, err)
	}
	if len(associations) != 1 {
		t.Fatalf("ListCurrentSessionCommitAssociations(%s) returned %d rows, want exactly 1", sessionID, len(associations))
	}
	association := associations[0]
	if association.ID != associationID || association.ObservedCommitHash != fixture.ObservedCommitHash {
		t.Fatalf("durable association = id=%q observed_commit_hash=%q, want id=%q observed_commit_hash=%q", association.ID, association.ObservedCommitHash, associationID, fixture.ObservedCommitHash)
	}

	annotationTypeID, err := localStore.GetAnnotationTypeID(ctx, fixture.Annotation.TypeID)
	if err != nil {
		t.Fatalf("get annotation type %q for association_roundtrip seed: %v", fixture.Annotation.TypeID, err)
	}
	if annotationTypeID == "" {
		t.Fatalf("annotation type %q is missing from the production store registry", fixture.Annotation.TypeID)
	}
	annotatorID, err := localStore.GetAnnotatorIDByName(ctx, fixture.Annotation.Annotator)
	if err != nil {
		t.Fatalf("get annotator %q for association_roundtrip seed: %v", fixture.Annotation.Annotator, err)
	}
	if annotatorID == "" {
		t.Fatalf("annotator %q is missing from the production store registry", fixture.Annotation.Annotator)
	}
	if _, err := localStore.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AssociationID:    &associationID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: annotationTypeID,
		Value:            fixture.Annotation.Value,
		IsPrimary:        fixture.Annotation.Primary,
	}); err != nil {
		t.Fatalf("CreateAnnotation(association target): %v", err)
	}
}

func runPeasantAssociationPush(t *testing.T, peasantBin string, xdg []string) pushJSON {
	t.Helper()
	out, err := runPeasantRaw(peasantBin, xdg, "village", "push", "--source-provider", defaults.HarnessClaudeCode.String(), "--json", "--non-interactive")
	if err != nil {
		t.Fatalf("ordinary association push failed: %v\n%s", err, out)
	}
	var result pushJSON
	if err := json.Unmarshal([]byte(stdoutJSON(out)), &result); err != nil {
		t.Fatalf("parse ordinary association push JSON: %v\n%s", err, out)
	}
	if result.Errors != 0 || result.Annotations.Errors != 0 || result.Annotations.Error != "" {
		t.Fatalf("ordinary association push contains errors: transcripts=%d annotations=%+v", result.Errors, result.Annotations)
	}
	return result
}

func assertQuietAssociationPush(t *testing.T, result pushJSON, expectedAnnotationSkips int) {
	t.Helper()
	if expectedAnnotationSkips <= 0 {
		t.Fatal("first association push produced no annotations to exercise the ordinary skip gate")
	}
	if result.New != 0 || result.Updated != 0 || result.Skipped != 0 || result.Errors != 0 || result.Held != 0 || len(result.Sessions) != 0 || strings.TrimSpace(result.EmptyReason) == "" {
		t.Fatalf("second ordinary association push transcript result = %+v, want no transcript outcomes and an empty-candidate reason", result)
	}
	if result.Annotations.Total != expectedAnnotationSkips || result.Annotations.Created != 0 || result.Annotations.Updated != 0 || result.Annotations.Skipped != expectedAnnotationSkips || result.Annotations.Retracted != 0 || result.Annotations.Errors != 0 || result.Annotations.Error != "" || result.Annotations.SkipReason != "" {
		t.Fatalf("second ordinary association push annotation result = %+v, want only %d skipped annotations", result.Annotations, expectedAnnotationSkips)
	}
}

func assertVillageAssociationRoundTrip(t *testing.T, db *sql.DB, ownerID, transcriptID string, associationID schema.AssociationID, fixture fixtureAssociationRoundTrip) string {
	t.Helper()
	var associationCount int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM transcript_associations
		WHERE owner_id = $1 AND transcript_id = $2;`, ownerID, transcriptID).Scan(&associationCount); err != nil {
		fatalActionable(t, actionableFailure{
			title: "query Village association count failed",
			what:  fmt.Sprintf("the owner-scoped association count for transcript %s could not be read", transcriptID),
			why:   err.Error(),
			where: "internal/e2e/pull_e2e_test.go assertVillageAssociationRoundTrip",
			when:  "after the real Peasant push and before the pull round-trip",
			means: "the E2E cannot prove how many durable commit associations Village persisted",
			fix:   "confirm the harness Postgres is reachable and the pinned Village association migration is applied",
		})
	}
	if associationCount != 1 {
		t.Fatalf("Village owner %s has %d transcript association rows for %s, want exactly 1", ownerID, associationCount, transcriptID)
	}

	var gotOwner, gotID, gotTranscript, gotObservedHash string
	err := db.QueryRow(`
		SELECT owner_id::text, association_id, transcript_id::text, observed_commit_sha
		FROM transcript_associations
		WHERE owner_id = $1 AND association_id = $2;`, ownerID, associationID.String()).Scan(
		&gotOwner, &gotID, &gotTranscript, &gotObservedHash)
	if err != nil {
		why := err.Error()
		if errors.Is(err, sql.ErrNoRows) {
			why = "the exact owner-scoped association row was not found"
		}
		fatalActionable(t, actionableFailure{
			title: "query Village association row failed",
			what:  fmt.Sprintf("association %s for transcript %s could not be read for owner %s", associationID, transcriptID, ownerID),
			why:   why,
			where: "internal/e2e/pull_e2e_test.go assertVillageAssociationRoundTrip",
			when:  "checking durable association persistence after the real push",
			means: "the producer-owned association identity or its transcript binding is missing",
			fix:   "inspect the publish association payload and the Village transcript_associations owner/transcript foreign key",
		})
	}
	if gotOwner != ownerID || gotID != associationID.String() || gotTranscript != transcriptID || gotObservedHash != fixture.ObservedCommitHash {
		t.Fatalf("Village association row = owner=%q id=%q transcript=%q observed=%q, want owner=%q id=%q transcript=%q observed=%q",
			gotOwner, gotID, gotTranscript, gotObservedHash, ownerID, associationID, transcriptID, fixture.ObservedCommitHash)
	}

	var annotationCount int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM annotations
		WHERE owner_id = $1 AND target_association_id = $2;`, ownerID, associationID.String()).Scan(&annotationCount); err != nil {
		fatalActionable(t, actionableFailure{
			title: "query Village association annotation count failed",
			what:  fmt.Sprintf("the association-target annotation count for association %s could not be read", associationID),
			why:   err.Error(),
			where: "internal/e2e/pull_e2e_test.go assertVillageAssociationRoundTrip",
			when:  "checking annotation persistence after the real push",
			means: "the E2E cannot prove the annotation reached the owner-scoped Village row",
			fix:   "confirm the annotation push response succeeded and the association target was resolved for the authenticated owner",
		})
	}
	if annotationCount != 1 {
		t.Fatalf("Village owner %s has %d annotations for association %s, want exactly 1", ownerID, annotationCount, associationID)
	}

	var (
		annotationOwner, targetKind, targetAssociationID                         sql.NullString
		sessionID, entrySessionID, annotationID, projectHash, targetTranscriptID sql.NullString
		typeID, value, annotatorName, contentHash                                sql.NullString
		entryIndex, entryEndIndex                                                sql.NullInt64
		isPrimary                                                                bool
	)
	err = db.QueryRow(`
		SELECT owner_id::text, target_kind, target_association_id,
		       session_id, entry_session_id, entry_index, entry_end_index,
		       annotation_id, project_hash, target_transcript_id,
		       type_id, value, annotator_name, content_hash, is_primary
		FROM annotations
		WHERE owner_id = $1 AND target_association_id = $2;`, ownerID, associationID.String()).Scan(
		&annotationOwner, &targetKind, &targetAssociationID,
		&sessionID, &entrySessionID, &entryIndex, &entryEndIndex,
		&annotationID, &projectHash, &targetTranscriptID,
		&typeID, &value, &annotatorName, &contentHash, &isPrimary)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "query Village association annotation failed",
			what:  fmt.Sprintf("the exact association-target annotation row for %s could not be read", associationID),
			why:   err.Error(),
			where: "internal/e2e/pull_e2e_test.go assertVillageAssociationRoundTrip",
			when:  "checking persisted target arms and annotation content after the real push",
			means: "the database assertion cannot distinguish a correct association annotation from a malformed target row",
			fix:   "inspect the Village annotations schema and rerun against a freshly migrated harness database",
		})
	}
	if !annotationOwner.Valid || annotationOwner.String != ownerID || !targetKind.Valid || targetKind.String != schema.TargetAssociation.String() ||
		!targetAssociationID.Valid || targetAssociationID.String != associationID.String() ||
		sessionID.Valid || entrySessionID.Valid || entryIndex.Valid || entryEndIndex.Valid || annotationID.Valid || projectHash.Valid || targetTranscriptID.Valid {
		t.Fatalf("Village association annotation target arms are not exclusive: owner=%+v kind=%+v association=%+v session=%+v entrySession=%+v entryIndex=%+v entryEnd=%+v annotation=%+v project=%+v transcript=%+v",
			annotationOwner, targetKind, targetAssociationID, sessionID, entrySessionID, entryIndex, entryEndIndex, annotationID, projectHash, targetTranscriptID)
	}
	if !typeID.Valid || typeID.String != fixture.Annotation.TypeID || !value.Valid || value.String != fixture.Annotation.Value ||
		!annotatorName.Valid || annotatorName.String != fixture.Annotation.Annotator || !isPrimary {
		t.Fatalf("Village association annotation values = type=%q value=%q annotator=%q primary=%v, want type=%q value=%q annotator=%q primary=%v",
			typeID.String, value.String, annotatorName.String, isPrimary, fixture.Annotation.TypeID, fixture.Annotation.Value, fixture.Annotation.Annotator, fixture.Annotation.Primary)
	}
	if !contentHash.Valid || contentHash.String == "" {
		t.Fatalf("Village association annotation content_hash = %q, want a non-empty post-push content hash", contentHash.String)
	}
	return contentHash.String
}

func assertRemoteListAssociationCount(t *testing.T, peasantBin string, xdg []string, transcriptID string, minimumCount int) {
	t.Helper()
	out, err := runPeasantRaw(peasantBin, xdg, "village", "transcripts", "list", "--json")
	if err != nil {
		t.Fatalf("production remote transcript list failed: %v\n%s", err, out)
	}
	var response schema.PullListResponse
	if err := json.Unmarshal([]byte(stdoutJSON(out)), &response); err != nil {
		t.Fatalf("parse production remote transcript list JSON: %v\n%s", err, out)
	}
	for i := range response.Transcripts {
		transcript := &response.Transcripts[i]
		if transcript.TranscriptID.String() != transcriptID {
			continue
		}
		if transcript.AnnotationCount < minimumCount {
			t.Fatalf("production remote list annotation count for shared root %s = %d, want at least %d to include the association annotation", transcriptID, transcript.AnnotationCount, minimumCount)
		}
		return
	}
	t.Fatalf("production remote list did not contain shared root transcript %s; returned %d transcript rows", transcriptID, len(response.Transcripts))
}

func assertPulledAssociationAnnotation(t *testing.T, dbPath, transcriptID, host string, associationID schema.AssociationID, contentHash string, fixture fixtureAssociationRoundTrip, wantAuthorID, wantAuthorUsername string) {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	associationMatches := 0
	err := sqlitex.Execute(conn, `
		SELECT content_hash, author_user_id, author_username, payload
		FROM pulled_annotations
		WHERE village_host = ? AND transcript_id = ?`, &sqlitex.ExecOptions{
		Args: []any{host, transcriptID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var annotation schema.PullAnnotation
			if err := json.Unmarshal([]byte(stmt.ColumnText(3)), &annotation); err != nil {
				return fmt.Errorf("decode pulled association annotation payload: %w", err)
			}
			if annotation.TargetKind != schema.TargetAssociation {
				return nil
			}
			associationMatches++
			if annotation.TargetAssociationID == nil || *annotation.TargetAssociationID != associationID ||
				annotation.TargetSessionID != nil || annotation.TargetEntryIndex != nil || annotation.TargetEntryEndIndex != nil ||
				annotation.TargetAnnotID != nil || annotation.TargetProjectHash != nil || annotation.TargetFilePath != nil || annotation.TargetContentHash != nil {
				t.Fatalf("pulled association annotation has incorrect or non-exclusive target arms: %+v", annotation.AnnotationSummary)
			}
			if annotation.ContentHash == nil || *annotation.ContentHash != contentHash || stmt.ColumnText(0) != contentHash {
				t.Fatalf("pulled association annotation content hash = payload %v, row %q, want %q", annotation.ContentHash, stmt.ColumnText(0), contentHash)
			}
			if annotation.TypeID != fixture.Annotation.TypeID || annotation.Value != fixture.Annotation.Value || annotation.AnnotatorName != fixture.Annotation.Annotator || !annotation.IsPrimary {
				t.Fatalf("pulled association annotation values = type=%q value=%q annotator=%q primary=%v, want type=%q value=%q annotator=%q primary=%v",
					annotation.TypeID, annotation.Value, annotation.AnnotatorName, annotation.IsPrimary, fixture.Annotation.TypeID, fixture.Annotation.Value, fixture.Annotation.Annotator, fixture.Annotation.Primary)
			}
			if annotation.AuthorUserID != wantAuthorID || annotation.AuthorUsername != wantAuthorUsername || stmt.ColumnText(1) != wantAuthorID || stmt.ColumnText(2) != wantAuthorUsername {
				t.Fatalf("pulled association annotation author = payload %s/@%s, row %s/@%s, want %s/@%s",
					annotation.AuthorUserID, annotation.AuthorUsername, stmt.ColumnText(1), stmt.ColumnText(2), wantAuthorID, wantAuthorUsername)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("scan pulled association annotation payload: %v", err)
	}
	if associationMatches != 1 {
		t.Fatalf("pulled annotation rows for transcript %s include %d association-target payloads, want exactly 1", transcriptID, associationMatches)
	}
}

// villageHostOf returns the on-disk namespace key (URL host) for a village URL —
// the same value the pull pipeline derives for village-pulls/{host}/.
func villageHostOf(t *testing.T, villageURL string) string {
	t.Helper()
	u := strings.TrimPrefix(strings.TrimPrefix(villageURL, "http://"), "https://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	if u == "" {
		t.Fatalf("villageHostOf: empty host from %q", villageURL)
	}
	return u
}

// --- logged-out + unauthenticated gates ---

// assertLoggedOutGates runs each village-CONTACTING command (pull, remote list,
// annotations sync) in a sandbox with NO credentials and asserts: exit non-zero,
// an actionable error naming `peasant village login`, and NOTHING written under
// village-pulls/. Then it asserts the OFFLINE commands (list --local, context)
// SUCCEED without credentials because only Village-contacting commands require
// login.
func assertLoggedOutGates(t *testing.T, peasantBin string, xdg []string, dirs sandboxDirs, transcriptID, villageURL string) {
	t.Helper()
	pullsRoot := filepath.Join(dirs.data, string(defaults.AppName), defaults.VillagePullsSubdir)

	type contactCase struct {
		name string
		args []string
	}
	cases := []contactCase{
		{"pull", []string{"village", "transcripts", "pull", transcriptID}},
		{"list-remote", []string{"village", "transcripts", "list"}},
		{"annotations-sync", []string{"village", "annotations", "sync"}},
	}
	for _, c := range cases {
		out, err := runPeasantExpectErr(t, peasantBin, xdg, c.args...)
		if err == nil {
			t.Fatalf("logged-out %s: expected non-zero exit, got success\n%s", c.name, out)
		}
		if !strings.Contains(out, "peasant village login") {
			t.Fatalf("logged-out %s: error does not name `peasant village login`:\n%s", c.name, out)
		}
	}
	// NOTHING written under village-pulls/ by any of the failed contact commands.
	if entries, err := os.ReadDir(pullsRoot); err == nil && len(entries) > 0 {
		t.Fatalf("logged-out commands wrote %d entries under %s — expected nothing", len(entries), pullsRoot)
	}

	// OFFLINE commands work logged-out: list --local (empty inventory) succeeds;
	// context on a not-pulled transcript fails with a LOCAL not-pulled error (NOT a
	// login error) — proving it never contacted the village.
	if _, err := runPeasantExpectOK(t, peasantBin, xdg, "village", "transcripts", "list", "--local"); err != nil {
		t.Fatalf("logged-out `list --local` should succeed offline: %v", err)
	}
	ctxOut, ctxErr := runPeasantExpectErr(t, peasantBin, xdg, "village", "transcripts", "context", transcriptID)
	if ctxErr == nil {
		t.Fatalf("logged-out `context` on a not-pulled transcript should fail (nothing to render):\n%s", ctxOut)
	}
	if strings.Contains(ctxOut, "peasant village login") {
		t.Fatalf("logged-out `context` failed with a LOGIN error — it must be offline-only:\n%s", ctxOut)
	}
}

// assertUnauthenticatedAPI401 hits each /api/v1/pull route with NO Authorization
// header and asserts 401 (the AuthRequired server-side gate).
func assertUnauthenticatedAPI401(t *testing.T, villageURL, transcriptID string) {
	t.Helper()
	routes := []string{
		"/api/v1/pull/transcripts",
		"/api/v1/pull/transcripts/" + transcriptID,
		"/api/v1/pull/transcripts/" + transcriptID + "/content",
		"/api/v1/pull/transcripts/" + transcriptID + "/annotations",
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	for _, route := range routes {
		resp, err := hc.Get(villageURL + route)
		if err != nil {
			t.Fatalf("unauth GET %s: %v", route, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauth GET %s: status = %d, want 401", route, resp.StatusCode)
		}
	}
}

// --- pull artifacts + DB-row assertions ---

// assertPullArtifacts asserts the three on-disk pull artifacts exist in the pull
// dir: the transcript blob, the metadata snapshot, and the provenance manifest.
func assertPullArtifacts(t *testing.T, pullDir string) {
	t.Helper()
	for _, name := range []string{"transcript.jsonl", "metadata.json", "pull-manifest.json"} {
		p := filepath.Join(pullDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected pull artifact %s after pull: %v", p, err)
		}
	}
}

// assertPulledRows asserts a pulled_transcripts row exists for (host, id) and at
// least one pulled_annotations row was committed with it.
func assertPulledRows(t *testing.T, dbPath, transcriptID, host string) {
	t.Helper()
	if !rowExistsHost(t, dbPath, "pulled_transcripts", host, transcriptID) {
		t.Fatalf("no pulled_transcripts row for (host=%s, id=%s) after pull", host, transcriptID)
	}
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var n int
	if err := sqlitex.Execute(conn,
		`SELECT COUNT(*) FROM pulled_annotations WHERE village_host = ? AND transcript_id = ?`,
		&sqlitex.ExecOptions{
			Args:       []any{host, transcriptID},
			ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt(0); return nil },
		}); err != nil {
		t.Fatalf("count pulled_annotations: %v", err)
	}
	if n < 1 {
		t.Fatalf("pulled_annotations rows = %d for transcript %s, want >=1", n, transcriptID)
	}
}

// assertForeignAnnotationAuthor asserts at least one pulled_annotations row on the
// transcript carries user2's author identity, and NONE carries user1's (own
// exclusion).
func assertForeignAnnotationAuthor(t *testing.T, dbPath, transcriptID, wantAuthorID, wantAuthorName, ownAuthorID string) {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var foreign, own int
	if err := sqlitex.Execute(conn,
		`SELECT author_user_id, author_username FROM pulled_annotations WHERE transcript_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{transcriptID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				id := stmt.ColumnText(0)
				name := stmt.ColumnText(1)
				switch id {
				case wantAuthorID:
					if name != wantAuthorName {
						t.Fatalf("foreign annotation author_username = %q, want %q", name, wantAuthorName)
					}
					foreign++
				case ownAuthorID:
					own++
				}
				return nil
			},
		}); err != nil {
		t.Fatalf("scan pulled_annotations authors: %v", err)
	}
	if foreign < 1 {
		t.Fatalf("no foreign-marked pulled_annotations row with author %s (@%s) on transcript %s",
			wantAuthorID, wantAuthorName, transcriptID)
	}
	if own > 0 {
		t.Fatalf("%d pulled_annotations rows carry user1's OWN author id %s — own annotations must be excluded",
			own, ownAuthorID)
	}
}

// --- village visibility mutation the harness drives via the real API ---

// setTranscriptPublic flips a transcript to visibility='public' through the REAL
// authenticated village API as the transcript OWNER (ownerAPIKey), so the handler
// carries the owner's UUID through inTxAs and the governance trigger records
// visibility_changed. It proves the MVP pull policy EXCLUDES public —
// public-but-unshared must still 404 to a non-owner.
func setTranscriptPublic(t *testing.T, villageURL, ownerAPIKey, transcriptID string) {
	t.Helper()
	status, resp := villageAPIRequest(t, http.MethodPatch, villageURL,
		fmt.Sprintf("/api/v1/transcripts/%s", transcriptID), ownerAPIKey,
		map[string]any{"visibility": "public"})
	if status != http.StatusOK {
		t.Fatalf("e2e: setTranscriptPublic: PATCH /api/v1/transcripts/%s status = %d, want 200\nbody: %s",
			transcriptID, status, resp)
	}
}

// --- pollution-gate snapshots ---

// pollutionTables are the user1-owned local tables a pull MUST NOT touch.
var pollutionTables = []string{"sessions", "session_entries", "session_metrics", "annotations"}

// snapshotPollutionTables captures a deterministic, order-independent fingerprint
// of each pollution table: the row count + a sorted SHA-256 over every row. Any
// inserted/updated/deleted row changes the fingerprint.
func snapshotPollutionTables(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	out := map[string]string{}
	for _, table := range pollutionTables {
		var rows []string
		// SELECT * as a quoted, column-ordered text projection; ROWID-independent.
		if err := sqlitex.Execute(conn, fmt.Sprintf(`SELECT * FROM %s`, table), &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				var b strings.Builder
				for i := 0; i < stmt.ColumnCount(); i++ {
					b.WriteString(stmt.ColumnText(i))
					b.WriteByte('\x1f') // unit separator
				}
				rows = append(rows, b.String())
				return nil
			},
		}); err != nil {
			t.Fatalf("snapshot table %s: %v", table, err)
		}
		sort.Strings(rows)
		h := sha256.Sum256([]byte(strings.Join(rows, "\x1e")))
		out[table] = fmt.Sprintf("rows=%d sha=%s", len(rows), hex.EncodeToString(h[:]))
	}
	return out
}

// diffTableSnapshots returns a human-readable diff of two pollution-table
// snapshots, or "" when identical.
func diffTableSnapshots(before, after map[string]string) string {
	var b strings.Builder
	for _, table := range pollutionTables {
		if before[table] != after[table] {
			fmt.Fprintf(&b, "  %s: before[%s] != after[%s]\n", table, before[table], after[table])
		}
	}
	return b.String()
}

// snapshotDirTree maps every file under root to its SHA-256, for a byte-identical
// tree comparison. A missing root yields an empty map (a valid "no tree" state).
func snapshotDirTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		h := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot dir tree %s: %v", root, err)
	}
	return out
}

// diffTrees returns a human-readable diff of two file-tree snapshots, or "" when
// byte-identical.
func diffTrees(before, after map[string]string) string {
	var b strings.Builder
	for rel, h := range before {
		if ah, ok := after[rel]; !ok {
			fmt.Fprintf(&b, "  removed: %s\n", rel)
		} else if ah != h {
			fmt.Fprintf(&b, "  changed: %s\n", rel)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			fmt.Fprintf(&b, "  added: %s\n", rel)
		}
	}
	return b.String()
}

// --- peasant subprocess wrappers (success / failure / JSON) ---

// runPeasantExpectOK runs peasant and requires success, returning combined output.
func runPeasantExpectOK(t *testing.T, bin string, xdg []string, args ...string) (string, error) {
	t.Helper()
	out, err := runPeasantRaw(bin, xdg, args...)
	if err != nil {
		t.Logf("peasant %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out, err
}

// runPeasantExpectErr runs peasant and returns its output + error WITHOUT failing
// the test (the caller asserts the non-zero exit + message).
func runPeasantExpectErr(t *testing.T, bin string, xdg []string, args ...string) (string, error) {
	t.Helper()
	return runPeasantRaw(bin, xdg, args...)
}

// runPeasantPullJSON pulls a transcript with --json and requires success.
func runPeasantPullJSON(t *testing.T, bin string, xdg []string, ref string, extra ...string) jsonPullResult {
	t.Helper()
	args := append([]string{"village", "transcripts", "pull", ref, "--json"}, extra...)
	out, err := runPeasantRaw(bin, xdg, args...)
	if err != nil {
		t.Fatalf("peasant pull --json (%s): %v\n%s", ref, err, out)
	}
	return parsePullJSON(t, out)
}

// runPeasantPullExpectFail pulls a transcript with --json expecting a NON-zero
// exit (the not-found / not-shared case) and returns the parsed result (which
// carries the typed status + error).
func runPeasantPullExpectFail(t *testing.T, bin string, xdg []string, ref string) jsonPullResult {
	t.Helper()
	out, err := runPeasantRaw(bin, xdg, "village", "transcripts", "pull", ref, "--json")
	if err == nil {
		t.Fatalf("peasant pull --json (%s): expected non-zero exit, got success\n%s", ref, out)
	}
	return parsePullJSON(t, out)
}

// runPeasantSyncJSON runs `village annotations sync --json` and requires success.
func runPeasantSyncJSON(t *testing.T, bin string, xdg []string) jsonSyncResultE2E {
	t.Helper()
	out, err := runPeasantRaw(bin, xdg, "village", "annotations", "sync", "--json")
	if err != nil {
		t.Fatalf("peasant annotations sync --json: %v\n%s", err, out)
	}
	var r jsonSyncResultE2E
	if uerr := json.Unmarshal([]byte(stdoutJSON(out)), &r); uerr != nil {
		t.Fatalf("parse sync JSON: %v\n%s", uerr, out)
	}
	return r
}

// runPeasantPushDryRun runs `village push --dry-run --json --non-interactive` and
// returns the normalized candidate-set fingerprint (new+updated+annotation
// counts), the pollution-gate signal that pulls never grow the push candidate set.
func runPeasantPushDryRun(t *testing.T, bin string, xdg []string) string {
	t.Helper()
	out, err := runPeasantRaw(bin, xdg, "village", "push", "--dry-run", "--json", "--non-interactive")
	if err != nil {
		t.Fatalf("peasant push --dry-run --json: %v\n%s", err, out)
	}
	var p pushJSON
	if uerr := json.Unmarshal([]byte(stdoutJSON(out)), &p); uerr != nil {
		t.Fatalf("parse push dry-run JSON: %v\n%s", uerr, out)
	}
	return fmt.Sprintf("new=%d updated=%d annCreated=%d annUpdated=%d annRetracted=%d",
		p.New, p.Updated, p.Annotations.Created, p.Annotations.Updated, p.Annotations.Retracted)
}

// jsonPullResult mirrors cmd/peasant's jsonPullResult for parsing `pull --json`
// (cmd/peasant is package main and not importable, so the wire shape is mirrored
// here exactly — a drift breaks parsing, which the e2e run surfaces).
type jsonPullResult struct {
	TranscriptID    string `json:"transcriptId"`
	FromURL         bool   `json:"fromUrl"`
	Status          string `json:"status"`
	VillageHost     string `json:"villageHost,omitempty"`
	PullDir         string `json:"pullDir,omitempty"`
	ServedBlobHash  string `json:"servedBlobHash,omitempty"`
	AnnotationCount int    `json:"annotationCount"`
	Error           string `json:"error,omitempty"`
}

// jsonSyncResultE2E mirrors cmd/peasant's jsonSyncResult for parsing sync --json.
type jsonSyncResultE2E struct {
	Status             string `json:"status"`
	TranscriptsScanned int    `json:"transcriptsScanned"`
	Created            int    `json:"created"`
	Updated            int    `json:"updated"`
	Skipped            int    `json:"skipped"`
	Excluded           int    `json:"excluded"`
	Error              string `json:"error,omitempty"`
}

// parsePullJSON parses a `pull --json` payload (mirrors cmd/peasant's
// jsonPullResult). It tolerates leading non-JSON lines (dry-run banners go to
// stderr, but be defensive) by isolating the JSON object.
func parsePullJSON(t *testing.T, out string) jsonPullResult {
	t.Helper()
	var r jsonPullResult
	if err := json.Unmarshal([]byte(stdoutJSON(out)), &r); err != nil {
		t.Fatalf("parse pull JSON: %v\n%s", err, out)
	}
	return r
}

// stdoutJSON returns the substring of out from the first '{' to the last '}', so a
// stray banner line never breaks json.Unmarshal. The --json commands write the
// object to stdout and banners to stderr; combined output may interleave.
func stdoutJSON(out string) string {
	i := strings.IndexByte(out, '{')
	j := strings.LastIndexByte(out, '}')
	if i < 0 || j < 0 || j < i {
		return out
	}
	return out[i : j+1]
}

// --- low-level subprocess + DB helpers ---

// runPeasantRaw runs peasant with the sandbox XDG env and returns combined output
// + error (no t.Fatal — the caller decides whether the error is expected). Mirrors
// the skip-gate harness's runPeasant env wiring.
func runPeasantRaw(bin string, xdg []string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), xdg...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// rowExists reports whether a row exists in table where the given (text) column
// equals val.
func rowExists(t *testing.T, dbPath, table, col, val string) bool {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var n int
	if err := sqlitex.Execute(conn, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, table, col),
		&sqlitex.ExecOptions{
			Args:       []any{val},
			ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt(0); return nil },
		}); err != nil {
		t.Fatalf("rowExists %s.%s: %v", table, col, err)
	}
	return n > 0
}

// rowExistsHost reports whether a row exists in table keyed by (village_host,
// transcript_id).
func rowExistsHost(t *testing.T, dbPath, table, host, transcriptID string) bool {
	t.Helper()
	conn := openSandboxDB(t, dbPath)
	defer conn.Close()
	var n int
	if err := sqlitex.Execute(conn,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE village_host = ? AND transcript_id = ?`, table),
		&sqlitex.ExecOptions{
			Args:       []any{host, transcriptID},
			ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt(0); return nil },
		}); err != nil {
		t.Fatalf("rowExistsHost %s: %v", table, err)
	}
	return n > 0
}

// readFileBytes reads a file, failing the test on error.
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// bytesEqual reports byte equality.
func bytesEqual(a, b []byte) bool {
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
