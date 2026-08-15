package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
			materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "current-session-message"))
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
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "current-session-message"))
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

func TestPublicSQLiteSourceSurfaceIsCatalogOnly(t *testing.T) {
	interfaceType := reflect.TypeOf((*OpenCodeSQLiteSource)(nil)).Elem()
	if interfaceType.NumMethod() != 2 {
		t.Fatalf("public SQLite source method count = %d, want only Catalog and Close", interfaceType.NumMethod())
	}
	catalog, ok := interfaceType.MethodByName("Catalog")
	if !ok || catalog.Type.NumIn() != 1 || catalog.Type.NumOut() != 2 || catalog.Type.Out(0) != reflect.TypeOf(OpenCodeSchemaEvidence{}) {
		t.Fatalf("public Catalog signature = %v, want context-only input and detached schema evidence", catalog.Type)
	}
	closeMethod, ok := interfaceType.MethodByName("Close")
	if !ok || closeMethod.Type.NumIn() != 1 || closeMethod.Type.NumOut() != 1 {
		t.Fatalf("public Close signature = %v, want context-only bounded cleanup", closeMethod.Type)
	}
	for index := 0; index < interfaceType.NumMethod(); index++ {
		methodText := interfaceType.Method(index).Type.String()
		if strings.Contains(methodText, "sqlite.Conn") || strings.Contains(methodText, "[]interface {}") || strings.Contains(methodText, "func(") && strings.Count(methodText, "func(") > 1 {
			t.Errorf("public SQLite source method exposes raw connection, arbitrary arguments, or callback: %s", methodText)
		}
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
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "legacy-message-part"))
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
