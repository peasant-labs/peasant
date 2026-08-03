package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/migrations/v43_publications.yaml
var publicationFixture []byte

type publicationFixtureFile struct {
	Records    []publicationFixtureRecord    `yaml:"records"`
	Attempts   []publicationAttemptFixture   `yaml:"attempts"`
	Rejections []publicationRejectionFixture `yaml:"rejections"`
	Rollback   publicationRollbackFixture    `yaml:"rollback"`
}
type publicationRejectionFixture struct {
	Name                string                        `yaml:"name"`
	ErrorContains       string                        `yaml:"error_contains"`
	SQLiteErrorContains string                        `yaml:"sqlite_error_contains"`
	Stage               store.PublicationAttemptStage `yaml:"stage"`
}
type publicationRollbackFixture struct {
	Name             string `yaml:"name"`
	InitialLicense   string `yaml:"initial_license"`
	ExpectedLicense  string `yaml:"expected_license"`
	InitialPushedAt  int64  `yaml:"initial_pushed_at"`
	ExpectedPushedAt int64  `yaml:"expected_pushed_at"`
}
type publicationFixtureRecord struct {
	Name        string `yaml:"name"`
	Origin      string `yaml:"origin"`
	Owner       string `yaml:"owner"`
	SessionID   string `yaml:"session_id"`
	ProjectHash string `yaml:"project_hash"`
	ReceiptJSON string `yaml:"receipt_json"`
}
type publicationAttemptFixture struct {
	Name    string                        `yaml:"name"`
	Stage   store.PublicationAttemptStage `yaml:"stage"`
	Message string                        `yaml:"message"`
}

func loadPublicationFixture(t *testing.T) publicationFixtureFile {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(publicationFixture))
	decoder.KnownFields(true)
	var fixture publicationFixtureFile
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode publication fixture: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("publication fixture must contain exactly one document: %v", err)
	}
	if len(fixture.Records) != 2 || len(fixture.Attempts) != len(store.AllPublicationAttemptStages) || len(fixture.Rejections) != 1 {
		t.Fatalf("publication fixture rows = records:%d attempts:%d rejections:%d", len(fixture.Records), len(fixture.Attempts), len(fixture.Rejections))
	}
	names := map[string]bool{}
	projects := map[schema.ProjectHash]bool{}
	for index, record := range fixture.Records {
		projectHash, err := schema.NewProjectHash(record.ProjectHash)
		if record.Name == "" || record.Origin == "" || record.Owner == "" || record.SessionID == "" || record.ReceiptJSON == "" || err != nil || names[record.Name] || projects[projectHash] {
			t.Fatalf("invalid or duplicate publication record fixture: %#v, project error: %v", record, err)
		}
		if _, err := schema.DecodePublishResponse([]byte(record.ReceiptJSON)); err != nil {
			t.Fatalf("publication record %q has invalid receipt: %v", record.Name, err)
		}
		if index > 0 {
			first := fixture.Records[0]
			if record.Origin != first.Origin || record.Owner != first.Owner || record.SessionID != first.SessionID {
				t.Fatalf("wrong-project fixture must vary only project and receipt identity: first=%#v other=%#v", first, record)
			}
		}
		names[record.Name], projects[projectHash] = true, true
	}
	stages := map[store.PublicationAttemptStage]bool{}
	for _, attempt := range fixture.Attempts {
		if attempt.Name == "" || attempt.Stage == "" || attempt.Message == "" || names[attempt.Name] {
			t.Fatalf("invalid or duplicate publication attempt fixture: %#v", attempt)
		}
		names[attempt.Name], stages[attempt.Stage] = true, true
	}
	for _, stage := range store.AllPublicationAttemptStages {
		if !stages[stage] {
			t.Fatalf("publication fixture does not exercise stage %q", stage)
		}
	}
	if fixture.Rejections[0].Stage.IsValid() || fixture.Rejections[0].Name == "" || fixture.Rejections[0].ErrorContains == "" || fixture.Rejections[0].SQLiteErrorContains == "" || fixture.Rollback.Name == "" || fixture.Rollback.InitialPushedAt == 0 || fixture.Rollback.ExpectedPushedAt == 0 || fixture.Rollback.InitialLicense == "" || fixture.Rollback.ExpectedLicense == "" {
		t.Fatal("publication fixture does not activate unknown-stage and rollback arms")
	}
	return fixture
}

func publicationRecordFromFixture(t *testing.T, row publicationFixtureRecord) store.PublicationRecord {
	t.Helper()
	projectHash, err := schema.NewProjectHash(row.ProjectHash)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := schema.DecodePublishResponse([]byte(row.ReceiptJSON))
	if err != nil {
		t.Fatal(err)
	}
	return store.PublicationRecord{VillageOrigin: row.Origin, OwnerUserID: row.Owner, SessionID: row.SessionID, ProjectHash: projectHash, Receipt: receipt}
}

func TestMigrationV43SQLiteCheckRejectsUnknownDiagnosticStage(t *testing.T) {
	fixture := loadPublicationFixture(t)
	row, rejected := fixture.Records[0], fixture.Rejections[0]
	dbPath := filepath.Join(t.TempDir(), "stage-check.db")
	s, err := store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	projectHash, _ := schema.NewProjectHash(row.ProjectHash)
	storetest.SeedSessionInProject(t, s, row.SessionID, projectHash)
	s.Close()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = sqlitex.Execute(conn, `INSERT INTO publication_attempt_diagnostics(village_origin,owner_user_id,session_id,project_hash,attempted_at,stage,message) VALUES(?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{row.Origin, row.Owner, row.SessionID, row.ProjectHash, 1, rejected.Stage, "direct SQLite boundary"}})
	if err == nil || !strings.Contains(err.Error(), rejected.SQLiteErrorContains) {
		t.Fatalf("direct unknown-stage SQLite error=%v want %q", err, rejected.SQLiteErrorContains)
	}
	var count int64
	if err = sqlitex.Execute(conn, `SELECT COUNT(*) FROM publication_attempt_diagnostics`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { count = stmt.ColumnInt64(0); return nil }}); err != nil || count != 0 {
		t.Fatalf("unknown stage inserted count=%d err=%v", count, err)
	}
}

func TestMigrationV43RejectsUnknownDiagnosticStage(t *testing.T) {
	fixture := loadPublicationFixture(t)
	row, rejected := fixture.Records[0], fixture.Rejections[0]
	s := openTestStore(t)
	defer s.Close()
	projectHash, _ := schema.NewProjectHash(row.ProjectHash)
	storetest.SeedSessionInProject(t, s, row.SessionID, projectHash)
	err := s.RecordPublicationAttempt(context.Background(), store.PublicationAttemptDiagnostic{VillageOrigin: row.Origin, OwnerUserID: row.Owner, SessionID: row.SessionID, ProjectHash: projectHash, Stage: rejected.Stage, Message: "must reject"})
	if err == nil || !strings.Contains(err.Error(), rejected.ErrorContains) {
		t.Fatalf("unknown diagnostic stage error = %v, want %q", err, rejected.ErrorContains)
	}
}

func TestSavePublicationRollsBackReceiptWhenCursorUpdateFails(t *testing.T) {
	fixture := loadPublicationFixture(t)
	row, rollback := fixture.Records[0], fixture.Rollback
	dbPath := filepath.Join(t.TempDir(), "publication.db")
	s, err := store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	record := publicationRecordFromFixture(t, row)
	storetest.SeedSessionInProject(t, s, row.SessionID, record.ProjectHash)
	s.Close()
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = sqlitex.Execute(conn, `UPDATE sessions SET pushed_at=?,license_id=? WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{rollback.InitialPushedAt, rollback.InitialLicense, row.SessionID}}); err != nil {
		t.Fatal(err)
	}
	if err = sqlitex.Execute(conn, `CREATE TRIGGER fail_publication_cursor BEFORE UPDATE OF pushed_at,license_id ON sessions WHEN NEW.session_id='99d59925-36bc-424c-a789-8be54d9702ba' BEGIN SELECT RAISE(ABORT,'cursor update fault'); END`, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	s, err = store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePublication(context.Background(), record); err == nil {
		t.Fatal("cursor fault unexpectedly saved publication")
	}
	s.Close()
	conn, _ = sqlite.OpenConn(dbPath)
	var receiptCount, pushedAt int64
	var license string
	_ = sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_publications`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { receiptCount = stmt.ColumnInt64(0); return nil }})
	_ = sqlitex.Execute(conn, `SELECT pushed_at,license_id FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{row.SessionID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		pushedAt = stmt.ColumnInt64(0)
		license = stmt.ColumnText(1)
		return nil
	}})
	if receiptCount != 0 || pushedAt != rollback.ExpectedPushedAt || license != rollback.ExpectedLicense {
		t.Fatalf("rollback state receipt=%d pushed=%d license=%q", receiptCount, pushedAt, license)
	}
	if err = sqlitex.Execute(conn, `DROP TRIGGER fail_publication_cursor`, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	s, err = store.Open(dbPath, store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SavePublication(context.Background(), record); err != nil {
		t.Fatalf("save after dropping trigger: %v", err)
	}
	if got, readErr := s.Publication(context.Background(), row.Origin, row.Owner, record.ProjectHash, row.SessionID); readErr != nil || got == nil {
		t.Fatalf("atomic retry receipt=%+v err=%v", got, readErr)
	}
}

func TestMigrationV43PersistsOnlyCompleteAuthoritativeReceipts(t *testing.T) {
	fixture := loadPublicationFixture(t)
	s := openTestStore(t)
	defer s.Close()
	row := fixture.Records[0]
	record := publicationRecordFromFixture(t, row)
	receipt, projectHash := record.Receipt, record.ProjectHash
	storetest.SeedSessionInProject(t, s, row.SessionID, projectHash)
	if err := s.SavePublication(context.Background(), record); err != nil {
		t.Fatalf("save receipt: %v", err)
	}
	got, err := s.Publication(context.Background(), row.Origin, row.Owner, projectHash, row.SessionID)
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if got == nil || got.Receipt.RequestOperationFingerprint != receipt.RequestOperationFingerprint {
		t.Fatalf("receipt round trip mismatch: %#v", got)
	}
	bad := record
	bad.Receipt.TranscriptURL = "http://invalid.example/transcripts/" + receipt.TranscriptID.String()
	if err = s.SavePublication(context.Background(), bad); err == nil {
		t.Fatal("invalid receipt unexpectedly replaced terminal state")
	}
	again, err := s.Publication(context.Background(), row.Origin, row.Owner, projectHash, row.SessionID)
	if err != nil || again.Receipt.TranscriptURL != receipt.TranscriptURL {
		t.Fatalf("prior terminal receipt changed after rejected write: %#v, %v", again, err)
	}
	for index, attempt := range fixture.Attempts {
		if err := s.RecordPublicationAttempt(context.Background(), store.PublicationAttemptDiagnostic{VillageOrigin: row.Origin, OwnerUserID: row.Owner, SessionID: row.SessionID, ProjectHash: projectHash, AttemptedAt: int64(index + 2), Stage: attempt.Stage, Message: attempt.Message}); err != nil {
			t.Fatalf("record attempt %q: %v", attempt.Name, err)
		}
		diagnostic, readErr := s.LatestPublicationAttempt(context.Background(), row.Origin, row.Owner, projectHash, row.SessionID)
		if readErr != nil || diagnostic == nil || diagnostic.Stage != attempt.Stage || diagnostic.ProjectHash != projectHash || diagnostic.Message != attempt.Message {
			t.Fatalf("diagnostic round trip for %q: %#v, %v", attempt.Name, diagnostic, readErr)
		}
	}
	afterDiagnostic, err := s.Publication(context.Background(), row.Origin, row.Owner, projectHash, row.SessionID)
	if err != nil || afterDiagnostic.Receipt.RequestOperationFingerprint != receipt.RequestOperationFingerprint {
		t.Fatalf("diagnostic changed terminal receipt: %#v, %v", afterDiagnostic, err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.SavePublication(context.Background(), record) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent identical receipt: %v", err)
		}
	}
	converged, err := s.Publication(context.Background(), row.Origin, row.Owner, projectHash, row.SessionID)
	if err != nil || converged.Receipt.RequestOperationFingerprint != receipt.RequestOperationFingerprint {
		t.Fatalf("concurrent duplicate did not converge: %#v, %v", converged, err)
	}
}

func TestMigrationV43ProjectIdentityCannotReadOrOverwritePublicationState(t *testing.T) {
	fixture := loadPublicationFixture(t)
	primary := publicationRecordFromFixture(t, fixture.Records[0])
	wrongProject := publicationRecordFromFixture(t, fixture.Records[1])
	attempt := fixture.Attempts[0]
	s := openTestStore(t)
	defer s.Close()
	storetest.SeedSessionInProject(t, s, primary.SessionID, primary.ProjectHash)

	if err := s.SavePublication(context.Background(), primary); err != nil {
		t.Fatalf("save primary publication identity: %v", err)
	}
	if got, err := s.Publication(context.Background(), primary.VillageOrigin, primary.OwnerUserID, wrongProject.ProjectHash, primary.SessionID); err != nil || got != nil {
		t.Fatalf("wrong project read primary receipt: record=%+v err=%v", got, err)
	}
	if err := s.SavePublication(context.Background(), wrongProject); err == nil {
		t.Fatal("wrong project overwrote or created publication state")
	}
	got, err := s.Publication(context.Background(), primary.VillageOrigin, primary.OwnerUserID, primary.ProjectHash, primary.SessionID)
	if err != nil || got == nil || got.Receipt.TranscriptID != primary.Receipt.TranscriptID {
		t.Fatalf("wrong-project write changed primary receipt: record=%+v err=%v", got, err)
	}

	diagnostic := store.PublicationAttemptDiagnostic{VillageOrigin: primary.VillageOrigin, OwnerUserID: primary.OwnerUserID, SessionID: primary.SessionID, ProjectHash: primary.ProjectHash, AttemptedAt: 1, Stage: attempt.Stage, Message: attempt.Message}
	if err := s.RecordPublicationAttempt(context.Background(), diagnostic); err != nil {
		t.Fatalf("record primary diagnostic: %v", err)
	}
	if got, err := s.LatestPublicationAttempt(context.Background(), primary.VillageOrigin, primary.OwnerUserID, wrongProject.ProjectHash, primary.SessionID); err != nil || got != nil {
		t.Fatalf("wrong project read primary diagnostic: diagnostic=%+v err=%v", got, err)
	}
	diagnostic.ProjectHash = wrongProject.ProjectHash
	diagnostic.AttemptedAt++
	if err := s.RecordPublicationAttempt(context.Background(), diagnostic); err == nil {
		t.Fatal("wrong project persisted publication diagnostic")
	}
	latest, err := s.LatestPublicationAttempt(context.Background(), primary.VillageOrigin, primary.OwnerUserID, primary.ProjectHash, primary.SessionID)
	if err != nil || latest == nil || latest.Message != attempt.Message || latest.ProjectHash != primary.ProjectHash {
		t.Fatalf("wrong-project diagnostic changed primary diagnostic: diagnostic=%+v err=%v", latest, err)
	}
}
