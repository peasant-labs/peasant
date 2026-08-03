//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type convergenceRemoteState struct {
	ID, OwnerID, LocalID, Visibility, ContentHash, Fingerprint, BlobKey, Title string
	License                                                                    sql.NullString
	PublishedAt, UpdatedAt, AuditCount, AuditSequence                          int64
	Associations                                                               []schema.PublishedAssociation
}

func TestPublicationConvergenceE2E(t *testing.T) {
	document, err := loadPublicationConvergenceFixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := document.Cases[0]
	bins := resolveVillageBinaries(t)
	peasantBin := buildPeasant(t)
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Skip("publication convergence evidence requires harness-owned Village, Postgres, and MinIO")
	}
	sandbox := newDisposableSandbox(t, peasantBin)
	controlRepo := sandbox.initRepository(t, "publication-control")
	targetRepo := sandbox.initRepository(t, "publication-target")
	controlSource := reseedClaudeFixture(t, claudeReseed{Destination: filepath.Join(sandbox.root, "fixtures", "control"), RecordedWorkingDirectory: controlRepo, RootSessionID: want.ControlRootSessionID, SubagentSessionID: want.ControlSubagentSessionID})
	targetSource := reseedClaudeFixture(t, claudeReseed{Destination: filepath.Join(sandbox.root, "fixtures", "target"), RecordedWorkingDirectory: targetRepo})
	writeConvergenceConfig(t, sandbox, controlSource, targetSource, want.InitialLicense, want.Visibility)
	_ = mintDemoCredentials(t, bins.setupDemo, stack.dsn, stack.villageURL, sandbox.configHome)
	runPeasantInSandbox(t, peasantBin, sandbox, "ingest", "--include-active")
	dbPath := filepath.Join(sandbox.dataHome, string(defaults.AppName), "peasant.db")
	seedConvergenceAssociations(t, dbPath, want.TargetSessionID, want.ExpectedAssociations)
	for _, repository := range []string{controlRepo, targetRepo} {
		runConvergencePush(t, peasantBin, sandbox, true, repository)
	}
	ownerID := readDemoUserID(t, sandbox.configHome)
	initialRows := listVillageTranscripts(t, stack.db, ownerID)
	if len(initialRows) != want.ExpectedProjects*want.ExpectedSessionsPerProject {
		t.Fatalf("initial Village rows = %d, want %d project-scoped sessions", len(initialRows), want.ExpectedProjects*want.ExpectedSessionsPerProject)
	}
	controlBefore := readConvergenceRemote(t, stack.db, ownerID, want.ControlRootSessionID)
	targetBefore := readConvergenceRemote(t, stack.db, ownerID, want.TargetSessionID)
	assertExactAssociations(t, "initial Village read-back", targetBefore.Associations, want.ExpectedAssociations)
	targetProject := readLocalProjectHash(t, dbPath, want.TargetSessionID)
	targetProjectHash, err := schema.NewProjectHash(targetProject)
	if err != nil {
		t.Fatalf("validate target project hash %q: %v", targetProject, err)
	}
	deleteLocalReceipt(t, dbPath, stack.villageURL, ownerID, want.TargetSessionID)
	mutateConvergenceSource(t, targetSource, want)
	writeConvergenceConfig(t, sandbox, controlSource, targetSource, want.ChangedLicense, want.Visibility)
	runPeasantInSandbox(t, peasantBin, sandbox, "ingest", "--include-active", "--force")
	pushedBeforeFailure := readLocalPushedAt(t, dbPath, want.TargetSessionID)
	installReceiptFailureTrigger(t, dbPath, targetProject)
	failure := runConvergencePush(t, peasantBin, sandbox, false, targetRepo)
	for _, needle := range want.FailureOutputContains {
		if !strings.Contains(failure, needle) {
			t.Errorf("partial failure output does not contain %q:\n%s", needle, failure)
		}
	}
	targetAfterFailure := readConvergenceRemote(t, stack.db, ownerID, want.TargetSessionID)
	if targetAfterFailure.ID != targetBefore.ID || targetAfterFailure.ContentHash == targetBefore.ContentHash || targetAfterFailure.Fingerprint == targetBefore.Fingerprint || targetAfterFailure.UpdatedAt <= targetBefore.UpdatedAt {
		t.Fatalf("Village did not durably converge before local receipt failure: before=%+v after=%+v", targetBefore, targetAfterFailure)
	}
	local := openLocalPublicationStore(t, dbPath)
	if receipt, readErr := local.Publication(context.Background(), stack.villageURL, ownerID, targetProjectHash, want.TargetSessionID); readErr != nil || receipt != nil {
		t.Fatalf("terminal receipt was fabricated after persistence failure: receipt=%+v err=%v", receipt, readErr)
	}
	diagnostic, err := local.LatestPublicationAttempt(context.Background(), stack.villageURL, ownerID, targetProjectHash, want.TargetSessionID)
	if err != nil || diagnostic == nil {
		t.Fatalf("read durable persistence diagnostic: %+v, %v", diagnostic, err)
	}
	if diagnostic.VillageOrigin != stack.villageURL || diagnostic.OwnerUserID != ownerID || diagnostic.ProjectHash.String() != targetProject || diagnostic.SessionID != want.TargetSessionID || diagnostic.Stage != want.ExpectedFailureStage {
		t.Errorf("persistence diagnostic identity/stage mismatch: %+v", diagnostic)
	}
	for _, needle := range want.DiagnosticContains {
		if !strings.Contains(diagnostic.Message, needle) {
			t.Errorf("diagnostic message does not contain %q: %s", needle, diagnostic.Message)
		}
	}
	local.Close()
	if got := readLocalPushedAt(t, dbPath, want.TargetSessionID); got != pushedBeforeFailure {
		t.Fatalf("failed receipt persistence advanced the local publication cursor: before=%d after=%d", pushedBeforeFailure, got)
	}
	dropReceiptFailureTrigger(t, dbPath)
	runConvergencePush(t, peasantBin, sandbox, true, targetRepo)
	controlAfter := readConvergenceRemote(t, stack.db, ownerID, want.ControlRootSessionID)
	if !reflect.DeepEqual(controlAfter, controlBefore) {
		t.Fatalf("already-current control project was replayed: before=%+v after=%+v", controlBefore, controlAfter)
	}
	targetAfterRetry := readConvergenceRemote(t, stack.db, ownerID, want.TargetSessionID)
	assertExactAssociations(t, "retry Village read-back", targetAfterRetry.Associations, want.ExpectedAssociations)
	assertConvergenceRemote(t, targetAfterFailure, targetAfterRetry, want)
	content := getVillageTranscriptContent(t, stack.villageURL, readAPIKey(t, sandbox.configHome), targetAfterRetry.ID)
	if !strings.Contains(content, want.ChangedContent) || strings.Contains(content, want.OriginalContent) {
		t.Errorf("authoritative Village body did not carry only changed content marker")
	}
	assertLocalPublicationReceiptsMatchVillage(t, sandbox, stack.villageURL, ownerID, []villageTranscript{{ID: targetAfterRetry.ID, OwnerID: ownerID, LocalID: targetAfterRetry.LocalID, Visibility: targetAfterRetry.Visibility, ContentHash: targetAfterRetry.ContentHash, OperationFingerprint: targetAfterRetry.Fingerprint, License: targetAfterRetry.License, PublishedAtMillis: targetAfterRetry.PublishedAt, UpdatedAtMillis: targetAfterRetry.UpdatedAt}})
	local = openLocalPublicationStore(t, dbPath)
	finalRecord, readErr := local.Publication(context.Background(), stack.villageURL, ownerID, targetProjectHash, want.TargetSessionID)
	if readErr != nil || finalRecord == nil {
		t.Fatalf("final V43 receipt: %+v %v", finalRecord, readErr)
	}
	assertExactAssociations(t, "local V43 receipt", finalRecord.Receipt.Applied.Associations, want.ExpectedAssociations)
	local.Close()
	other := mintVillageUser(t, stack.db, stack.villageURL, 181181, "convergence-other-owner", "convergence-proof")
	local = openLocalPublicationStore(t, dbPath)
	defer local.Close()
	if record, readErr := local.Publication(context.Background(), stack.villageURL, other.UserID, targetProjectHash, want.TargetSessionID); readErr != nil || record != nil {
		t.Fatalf("different owner satisfied target receipt lookup: record=%+v err=%v", record, readErr)
	}
	controlProject := readLocalProjectHash(t, dbPath, want.ControlRootSessionID)
	controlProjectHash, hashErr := schema.NewProjectHash(controlProject)
	if hashErr != nil {
		t.Fatalf("validate control project hash %q: %v", controlProject, hashErr)
	}
	if record, readErr := local.Publication(context.Background(), stack.villageURL, ownerID, controlProjectHash, want.TargetSessionID); readErr != nil || record != nil {
		t.Fatalf("different project %s satisfied target receipt lookup: record=%+v err=%v", controlProject, record, readErr)
	}
	if attempt, readErr := local.LatestPublicationAttempt(context.Background(), stack.villageURL, ownerID, controlProjectHash, want.TargetSessionID); readErr != nil || attempt != nil {
		t.Fatalf("different project %s satisfied target diagnostic lookup: diagnostic=%+v err=%v", controlProject, attempt, readErr)
	}
	t.Logf("publication convergence: control=%s target=%s project=%s fingerprint=%s content=%s audit=%d/%d associations=%d", controlAfter.ID, targetAfterRetry.ID, targetProject, targetAfterRetry.Fingerprint, targetAfterRetry.ContentHash, targetAfterRetry.AuditSequence, targetAfterRetry.AuditCount, len(targetAfterRetry.Associations))
}

func runConvergencePush(t *testing.T, binary string, sandbox disposableSandbox, success bool, repository string) string {
	t.Helper()
	command := exec.Command(binary, "village", "push", "--non-interactive", "--yes", "--repository", repository)
	command.Env = sandbox.environment
	output, err := command.CombinedOutput()
	if success && err != nil {
		t.Fatalf("publication push for %s failed: %v\n%s", repository, err, output)
	}
	// Batch publication reports per-session partial failures in its summary while
	// preserving the command's non-blocking exit contract. The caller asserts the
	// actionable failure text and durable diagnostic rather than an exit code.
	return string(output)
}

func writeConvergenceConfig(t *testing.T, sandbox disposableSandbox, controlSource, targetSource string, license schema.License, visibility schema.Visibility) {
	writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf("version: 1\nredaction:\n  level: standard\nsources:\n  claude-code:\n    enabled: true\n    paths: [%q, %q]\n  opencode: {enabled: false}\n  codex: {enabled: false}\n  cursor: {enabled: false}\n  strike: {enabled: false}\noutput:\n  basePath: %q\npush:\n  visibility: %s\n  license: %s\n", controlSource, targetSource, sandbox.transcriptOutputPath(), visibility, license))
}

func mutateConvergenceSource(t *testing.T, root string, want publicationConvergenceCase) {
	t.Helper()
	for _, replacement := range [][2]string{{want.OriginalContent, want.ChangedContent}, {want.OriginalMetadata, want.ChangedMetadata}} {
		changed := 0
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return err
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(raw), replacement[0]) {
				raw = []byte(strings.Replace(string(raw), replacement[0], replacement[1], 1))
				changed++
				return os.WriteFile(path, raw, 0o600)
			}
			return nil
		})
		if err != nil || changed != 1 {
			t.Fatalf("mutate target source marker %q exactly once: changed=%d err=%v", replacement[0], changed, err)
		}
	}
}

func withLocalSQLite(t *testing.T, dbPath string, fn func(*sqlite.Conn)) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.Execute(conn, "PRAGMA busy_timeout=5000", nil); err != nil {
		t.Fatal(err)
	}
	fn(conn)
}
func deleteLocalReceipt(t *testing.T, path, origin, owner, session string) {
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		if err := sqlitex.Execute(c, `DELETE FROM session_publications WHERE village_origin=? AND owner_user_id=? AND session_id=?`, &sqlitex.ExecOptions{Args: []any{origin, owner, session}}); err != nil || c.Changes() != 1 {
			t.Fatalf("delete target receipt exactly once: changes=%d err=%v", c.Changes(), err)
		}
	})
}
func installReceiptFailureTrigger(t *testing.T, path, project string) {
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		q := fmt.Sprintf(`CREATE TRIGGER fail_target_receipt BEFORE INSERT ON session_publications WHEN NEW.project_hash='%s' BEGIN SELECT RAISE(ABORT,'disposable target receipt failure'); END`, strings.ReplaceAll(project, "'", "''"))
		if err := sqlitex.Execute(c, q, nil); err != nil {
			t.Fatal(err)
		}
	})
}
func dropReceiptFailureTrigger(t *testing.T, path string) {
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		if err := sqlitex.Execute(c, `DROP TRIGGER fail_target_receipt`, nil); err != nil {
			t.Fatal(err)
		}
	})
}
func readLocalProjectHash(t *testing.T, path, session string) (out string) {
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		if err := sqlitex.Execute(c, `SELECT project_hash FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{session}, ResultFunc: func(s *sqlite.Stmt) error { out = s.ColumnText(0); return nil }}); err != nil || out == "" {
			t.Fatalf("read project hash: %q %v", out, err)
		}
	})
	return
}
func readLocalPushedAt(t *testing.T, path, session string) (out int64) {
	out = -1
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		if err := sqlitex.Execute(c, `SELECT pushed_at FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{session}, ResultFunc: func(s *sqlite.Stmt) error {
			if s.ColumnType(0) != sqlite.TypeNull {
				out = s.ColumnInt64(0)
			}
			return nil
		}}); err != nil {
			t.Fatalf("read pushed_at: %d %v", out, err)
		}
	})
	return
}
func seedConvergenceAssociations(t *testing.T, path, session string, associations []schema.PublishedAssociation) {
	withLocalSQLite(t, path, func(c *sqlite.Conn) {
		for _, association := range associations {
			if err := sqlitex.Execute(c, `INSERT INTO session_commits(session_id,commit_hash,message,commit_time,author_time) VALUES(?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{session, association.ObservedCommitHash, "convergence fixture commit", 1, 1}}); err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.Execute(c, `INSERT INTO session_commit_associations(association_id,session_id,observed_commit_hash,subject,author_time) VALUES(?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{association.ID.String(), session, association.ObservedCommitHash, "convergence fixture commit", 1}}); err != nil {
				t.Fatal(err)
			}
		}
	})
}
func assertExactAssociations(t *testing.T, surface string, got, want []schema.PublishedAssociation) {
	t.Helper()
	if len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s associations=%+v, want exact nonempty ordered %+v", surface, got, want)
	}
}
func openLocalPublicationStore(t *testing.T, path string) *store.Store {
	t.Helper()
	local, err := store.Open(path, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func readConvergenceRemote(t *testing.T, db *sql.DB, owner, localID string) convergenceRemoteState {
	t.Helper()
	var r convergenceRemoteState
	err := db.QueryRow(`SELECT t.id::text,t.owner_id::text,t.local_id,t.visibility,t.content_hash,t.accepted_request_operation_fingerprint,t.blob_key,COALESCE(t.title,''),t.license_id,FLOOR(EXTRACT(EPOCH FROM t.published_at)*1000)::bigint,FLOOR(EXTRACT(EPOCH FROM t.updated_at)*1000)::bigint,COALESCE((SELECT COUNT(*) FROM transcript_governance_events_audit g WHERE g.transcript_id=t.id),0),COALESCE((SELECT MAX(seq) FROM transcript_governance_events_audit g WHERE g.transcript_id=t.id),0) FROM transcripts t WHERE t.owner_id=$1 AND t.local_id=$2`, owner, localID).Scan(&r.ID, &r.OwnerID, &r.LocalID, &r.Visibility, &r.ContentHash, &r.Fingerprint, &r.BlobKey, &r.Title, &r.License, &r.PublishedAt, &r.UpdatedAt, &r.AuditCount, &r.AuditSequence)
	if err != nil {
		t.Fatalf("read authoritative Village state for %s: %v", localID, err)
	}
	rows, err := db.Query(`SELECT association_id,observed_commit_sha FROM transcript_associations WHERE transcript_id=$1 ORDER BY association_id,observed_commit_sha`, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a schema.PublishedAssociation
		if err := rows.Scan(&a.ID, &a.ObservedCommitHash); err != nil {
			t.Fatal(err)
		}
		r.Associations = append(r.Associations, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return r
}

func assertConvergenceRemote(t *testing.T, accepted, retried convergenceRemoteState, want publicationConvergenceCase) {
	t.Helper()
	if retried.ID != accepted.ID || retried.PublishedAt != accepted.PublishedAt || retried.Visibility != want.Visibility.String() || !retried.License.Valid || retried.License.String != want.ChangedLicense.String() || retried.ContentHash != accepted.ContentHash || retried.Fingerprint != accepted.Fingerprint || retried.BlobKey != accepted.BlobKey || retried.UpdatedAt < accepted.UpdatedAt || retried.AuditCount < accepted.AuditCount || retried.AuditSequence < accepted.AuditSequence || !reflect.DeepEqual(retried.Associations, accepted.Associations) {
		t.Fatalf("restart retry did not preserve authoritative accepted state: accepted=%+v retried=%+v", accepted, retried)
	}
}
