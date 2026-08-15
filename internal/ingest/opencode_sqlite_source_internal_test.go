package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const expectedDeniedCatalogStatementCount = 20

//go:embed testdata/opencode_sqlite_source.yaml
var sourceSafetyYAML []byte

type sourceSafetyCorpus struct {
	DeclaredDeniedStatements int               `yaml:"declared_denied_statements"`
	DeniedStatements         []deniedStatement `yaml:"denied_statements"`
	ForbiddenSourceTokens    []string          `yaml:"forbidden_source_tokens"`
}

type deniedStatement struct {
	Name          string `yaml:"name"`
	Statement     string `yaml:"statement"`
	ErrorContains string `yaml:"error_contains"`
}

func TestPrivateCatalogExecutorDeniesEveryForbiddenOperationClass(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	for _, denied := range fixture.DeniedStatements {
		denied := denied
		t.Run(denied.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, "current-session-message")
			before := testfixture.SnapshotSource(t, materialized)
			source := openConcreteSyntheticSource(t, materialized)

			err := executePrivateCatalogStatement(t, source, denied.Statement)
			if err == nil {
				t.Fatalf("private catalog executor accepted forbidden statement %q", denied.Statement)
			}
			if !strings.Contains(err.Error(), denied.ErrorContains) || !strings.Contains(err.Error(), "query_only remains enabled") {
				t.Errorf("forbidden statement error = %q, want %q and query_only enforcement", err, denied.ErrorContains)
			}
			if err := source.Close(t.Context()); err != nil {
				t.Fatalf("close source after denied statement: %v", err)
			}
			testfixture.AssertUnchanged(t, materialized, before)
		})
	}
}

func TestDeniedStatementFixtureIsNonVacuous(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	insert := fixture.DeniedStatements[0]
	if insert.Name != "insert-row" {
		t.Fatalf("first denial fixture = %q, want insert-row control", insert.Name)
	}
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open writable synthetic control: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, insert.Statement, nil); err != nil {
		_ = writer.Close()
		t.Fatalf("execute denial fixture against writable synthetic control: %v", err)
	}
	var sessions int64
	readErr := sqlitex.ExecuteTransient(writer, "SELECT count(*) FROM session", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		sessions = stmt.ColumnInt64(0)
		return nil
	}})
	closeErr := writer.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close writable synthetic control: %v", errors.Join(readErr, closeErr))
	}
	if sessions != 2 {
		t.Fatalf("writable synthetic control session count = %d, want 2 proving the denied statement is effective", sessions)
	}
}

func TestSQLiteSourceFilesContainNoDirectMutationEscape(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source guard location")
	}
	directory := filepath.Dir(currentFile)
	production := []string{
		filepath.Join(directory, "opencode_sqlite_source.go"),
		filepath.Join(directory, "opencode_sqlite_source_zombiezen.go"),
	}
	for _, filename := range production {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read source-boundary production file %q: %v", filename, err)
		}
		for _, token := range fixture.ForbiddenSourceTokens {
			if bytes.Contains(data, []byte(token)) {
				t.Errorf("source-boundary production file %q contains forbidden mutation escape token %q", filepath.Base(filename), token)
			}
		}
	}
}

func TestCloseCancelsActiveLockedCatalogWithinInjectedBound(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-message-part")
	path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic locked source path: %v", err)
	}
	options, err := NewOpenCodeSQLiteSourceOptions(500*time.Millisecond, time.Second, systemOpenCodeSQLiteDeadlineClock{})
	if err != nil {
		t.Fatalf("create bounded locked-source options: %v", err)
	}
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
	if err != nil {
		t.Fatalf("open synthetic locked source: %v", err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)

	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic exclusive writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "BEGIN EXCLUSIVE", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("begin synthetic exclusive transaction: %v", err)
	}
	catalogDone := make(chan error, 1)
	go func() {
		_, catalogErr := source.Catalog(context.Background())
		catalogDone <- catalogErr
	}()
	waitForActiveCatalog(t, source)

	started := time.Now()
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close source with active locked catalog: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= options.queryTimeout {
		t.Errorf("active catalog cleanup elapsed %s, want less than injected %s bound", elapsed, options.queryTimeout)
	}
	if err := <-catalogDone; !errors.Is(err, context.Canceled) {
		t.Errorf("canceled active catalog error = %v, want context.Canceled", err)
	}
	rollbackErr := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil)
	closeErr := writer.Close()
	if rollbackErr != nil || closeErr != nil {
		t.Fatalf("release synthetic exclusive writer: %v", errors.Join(rollbackErr, closeErr))
	}
}

func TestLegacyMessageCancellationWhileLockedReturnsNoPartialPage(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	source := openConcreteSyntheticSource(t, materialized)
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic exclusive legacy writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "BEGIN EXCLUSIVE", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("begin synthetic exclusive legacy transaction: %v", err)
	}
	sessionID, err := NewOpenCodeLegacySessionID("ses_reader_a")
	if err != nil {
		t.Fatalf("construct locked legacy session identifier: %v", err)
	}
	pageSize, err := NewOpenCodeLegacyPageSize(2)
	if err != nil {
		t.Fatalf("construct locked legacy page size: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan struct {
		page OpenCodeLegacyMessagePage
		err  error
	}, 1)
	go func() {
		page, readErr := source.LegacyMessages(ctx, OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize})
		readDone <- struct {
			page OpenCodeLegacyMessagePage
			err  error
		}{page: page, err: readErr}
	}()
	waitForActiveCatalog(t, source)
	cancel()
	result := <-readDone
	if !errors.Is(result.err, context.Canceled) || len(result.page.Messages) != 0 || result.page.Next != nil {
		t.Errorf("locked canceled legacy page = %+v error=%v, want zero page and cancellation cause", result.page, result.err)
	}
	rollbackErr := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil)
	closeWriterErr := writer.Close()
	if rollbackErr != nil || closeWriterErr != nil {
		t.Fatalf("release synthetic exclusive legacy writer: %v", errors.Join(rollbackErr, closeWriterErr))
	}
	if err := source.Close(t.Context()); err != nil {
		t.Fatalf("close source after locked cancellation: %v", err)
	}
}

func TestCloseCancelsActiveLockedLegacyMessageReadWithinInjectedBound(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic locked legacy source path: %v", err)
	}
	options, err := NewOpenCodeSQLiteSourceOptions(500*time.Millisecond, time.Second, systemOpenCodeSQLiteDeadlineClock{})
	if err != nil {
		t.Fatalf("create bounded locked legacy options: %v", err)
	}
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
	if err != nil {
		t.Fatalf("open synthetic locked legacy source: %v", err)
	}
	source := opened.(*zombiezenOpenCodeSQLiteSource)
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic exclusive legacy writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "BEGIN EXCLUSIVE", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("begin synthetic exclusive legacy transaction: %v", err)
	}
	sessionID, _ := NewOpenCodeLegacySessionID("ses_reader_a")
	pageSize, _ := NewOpenCodeLegacyPageSize(2)
	readDone := make(chan error, 1)
	go func() {
		page, readErr := source.LegacyMessages(context.Background(), OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize})
		if len(page.Messages) != 0 || page.Next != nil {
			readDone <- fmt.Errorf("close-canceled legacy read returned partial page: %+v", page)
			return
		}
		readDone <- readErr
	}()
	waitForActiveCatalog(t, source)
	started := time.Now()
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close source with active locked legacy read: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= options.queryTimeout {
		t.Errorf("active legacy cleanup elapsed %s, want less than injected %s bound", elapsed, options.queryTimeout)
	}
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Errorf("close-canceled active legacy read error = %v, want context.Canceled", err)
	}
	rollbackErr := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil)
	closeWriterErr := writer.Close()
	if rollbackErr != nil || closeWriterErr != nil {
		t.Fatalf("release synthetic exclusive legacy writer: %v", errors.Join(rollbackErr, closeWriterErr))
	}
}

func waitForActiveCatalog(t *testing.T, source *zombiezenOpenCodeSQLiteSource) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		source.stateMu.Lock()
		active := source.activeCancel != nil
		source.stateMu.Unlock()
		if active {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("catalog operation did not become active within the test bound")
}

func openConcreteSyntheticSource(t *testing.T, materialized testfixture.MaterializedSource) *zombiezenOpenCodeSQLiteSource {
	t.Helper()
	path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open concrete synthetic source: %v", err)
	}
	return source.(*zombiezenOpenCodeSQLiteSource)
}

func executePrivateCatalogStatement(t *testing.T, source *zombiezenOpenCodeSQLiteSource, statement string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-source.permit:
	}
	defer func() { source.permit <- struct{}{} }()
	oldInterrupt := source.conn.SetInterrupt(ctx.Done())
	defer source.conn.SetInterrupt(oldInterrupt)
	return source.executeRowsLocked(ctx, statement, nil, nil)
}

func loadSourceSafetyFixture(t *testing.T) sourceSafetyCorpus {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourceSafetyYAML))
	decoder.KnownFields(true)
	var fixture sourceSafetyCorpus
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode source safety fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode source safety fixtures: expected exactly one YAML document: %v", err)
	}
	if fixture.DeclaredDeniedStatements != expectedDeniedCatalogStatementCount || len(fixture.DeniedStatements) != expectedDeniedCatalogStatementCount {
		t.Fatalf("source safety fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredDeniedStatements, len(fixture.DeniedStatements), expectedDeniedCatalogStatementCount)
	}
	if len(fixture.ForbiddenSourceTokens) == 0 {
		t.Fatal("source safety fixture must declare structural mutation escape tokens")
	}
	names := make(map[string]struct{}, len(fixture.DeniedStatements))
	for _, denied := range fixture.DeniedStatements {
		if strings.TrimSpace(denied.Name) == "" || strings.TrimSpace(denied.Statement) == "" || strings.TrimSpace(denied.ErrorContains) == "" {
			t.Fatalf("source safety fixture has missing required fields: %+v", denied)
		}
		if _, duplicate := names[denied.Name]; duplicate {
			t.Fatalf("source safety fixture has duplicate name %q", denied.Name)
		}
		names[denied.Name] = struct{}{}
	}
	tokens := make(map[string]struct{}, len(fixture.ForbiddenSourceTokens))
	for _, token := range fixture.ForbiddenSourceTokens {
		if strings.TrimSpace(token) == "" {
			t.Fatal("source safety fixture has empty structural token")
		}
		if _, duplicate := tokens[token]; duplicate {
			t.Fatalf("source safety fixture has duplicate structural token %q", token)
		}
		tokens[token] = struct{}{}
	}
	return fixture
}
