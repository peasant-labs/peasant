package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedDeniedCatalogStatementCount = 20
	expectedDenialMutationProofs        = 1
	expectedSourceCancellationCases     = 1
	expectedSourceLifecycleCases        = 1
)

//go:embed testdata/opencode_sqlite_source.yaml
var sourceSafetyYAML []byte

type sourceSafetyCorpus struct {
	DeclaredDeniedStatements      int                      `yaml:"declared_denied_statements"`
	DeniedStatements              []deniedStatement        `yaml:"denied_statements"`
	DeclaredDenialMutationProofs  int                      `yaml:"declared_denial_mutation_proofs"`
	DeclaredCancellationCases     int                      `yaml:"declared_cancellation_cases"`
	CancellationCases             []sourceCancellationCase `yaml:"cancellation_cases"`
	DeclaredSourceLifecycleCases  int                      `yaml:"declared_source_lifecycle_cases"`
	DeniedFixture                 string                   `yaml:"denied_fixture"`
	SourceLifecycleCases          []sourceLifecycleCase    `yaml:"source_lifecycle_cases"`
	ForbiddenSourceBoundaryTokens []string                 `yaml:"forbidden_source_boundary_tokens"`
	ForbiddenIngestSQLiteTokens   []string                 `yaml:"forbidden_ingest_sqlite_tokens"`
}

type sourceCancellationCase struct {
	Name                    string                      `yaml:"name"`
	Fixture                 string                      `yaml:"fixture"`
	SessionID               string                      `yaml:"session_id"`
	PageSize                int                         `yaml:"page_size"`
	PendingRowsBeforeCancel int64                       `yaml:"pending_rows_before_cancel"`
	ReuseRows               int                         `yaml:"reuse_rows"`
	DecodedRow              sourceDecodedRowExpectation `yaml:"decoded_row"`
}

type sourceDecodedRowExpectation struct {
	ID          string `yaml:"id"`
	SessionID   string `yaml:"session_id"`
	Type        string `yaml:"type"`
	TimeCreated int64  `yaml:"time_created"`
	TimeUpdated int64  `yaml:"time_updated"`
	Data        string `yaml:"data"`
	Seq         int64  `yaml:"seq"`
}

type deniedStatement struct {
	Name                 string `yaml:"name"`
	Statement            string `yaml:"statement"`
	ErrorContains        string `yaml:"error_contains"`
	MutationProof        bool   `yaml:"mutation_proof"`
	ExpectedSessionCount int64  `yaml:"expected_session_count"`
}

type sourceLifecycleCase struct {
	Name               string   `yaml:"name"`
	Fixture            string   `yaml:"fixture"`
	PageSize           int      `yaml:"page_size"`
	ExpectedSessionIDs []string `yaml:"expected_session_ids"`
	ExpectedMessageIDs []string `yaml:"expected_message_ids"`
}

func TestPrivateCatalogExecutorDeniesEveryForbiddenOperationClass(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	for _, denied := range fixture.DeniedStatements {
		denied := denied
		t.Run(denied.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixture.DeniedFixture)
			writer := appendCommittedSyntheticWALTransaction(t, materialized.Path)
			defer closeSyntheticWriter(t, writer)
			mainBefore := readTestOwnedSourceFile(t, materialized.Path)
			walBefore := readTestOwnedSourceFile(t, materialized.Path+"-wal")
			source := openConcreteSyntheticSource(t, materialized)
			before := observePrivateSourceState(t, source, 1)

			err := executePrivateCatalogStatement(t, source, denied.Statement)
			if err == nil {
				t.Fatalf("private catalog executor accepted forbidden statement %q", denied.Statement)
			}
			if !strings.Contains(err.Error(), denied.ErrorContains) || !strings.Contains(err.Error(), "query_only remains enabled") {
				t.Errorf("forbidden statement error = %q, want %q and query_only enforcement", err, denied.ErrorContains)
			}
			if !reflect.DeepEqual(before, observePrivateSourceState(t, source, 1)) {
				t.Fatal("production source observable logical content or schema changed after denied statement")
			}
			if err := source.Close(t.Context()); err != nil {
				t.Fatalf("close source after denied statement: %v", err)
			}
			assertTestOwnedSourceFileEqual(t, materialized.Path, mainBefore, "main database")
			assertTestOwnedSourceFileEqual(t, materialized.Path+"-wal", walBefore, "committed WAL transaction")
		})
	}
}

func TestProductionSourceLifecycleUsesOneConnectionForCatalogAndPagedReads(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	for _, fixtureCase := range fixture.SourceLifecycleCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
			if err != nil {
				t.Fatalf("validate synthetic source path: %v", err)
			}
			options := DefaultOpenCodeSQLiteSourceOptions()
			var opens atomic.Int64
			options.openConnection = func(uri string, flags ...sqlite.OpenFlags) (*sqlite.Conn, error) {
				opens.Add(1)
				return sqlite.OpenConn(uri, flags...)
			}
			opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, options)
			if err != nil {
				t.Fatalf("open source through production lifecycle: %v", err)
			}
			source := opened.(*zombiezenOpenCodeSQLiteSource)
			state := observePrivateSourceState(t, source, fixtureCase.PageSize)
			if !reflect.DeepEqual(state.sessionIDs, fixtureCase.ExpectedSessionIDs) || !reflect.DeepEqual(state.messageIDs, fixtureCase.ExpectedMessageIDs) {
				t.Fatalf("production catalog and paged reads = sessions %v messages %v, want fixture sessions %v messages %v", state.sessionIDs, state.messageIDs, fixtureCase.ExpectedSessionIDs, fixtureCase.ExpectedMessageIDs)
			}
			if got := opens.Load(); got != 1 {
				t.Fatalf("production source opened %d SQLite connections across Catalog plus paged reads, want exactly one", got)
			}
			if err := source.Close(t.Context()); err != nil {
				t.Fatalf("close one-connection production source: %v", err)
			}
			if got := opens.Load(); got != 1 {
				t.Fatalf("production source opened %d SQLite connections after Close, want exactly one lifecycle connection", got)
			}
		})
	}
}

func TestDeniedStatementFixtureIsNonVacuous(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	var proof *deniedStatement
	for index := range fixture.DeniedStatements {
		if fixture.DeniedStatements[index].MutationProof {
			if proof != nil {
				t.Fatal("source safety fixture declares more than one denial mutation proof")
			}
			proof = &fixture.DeniedStatements[index]
		}
	}
	if proof == nil {
		t.Fatal("source safety fixture declares no denial mutation proof")
	}
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open writable synthetic control: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, proof.Statement, nil); err != nil {
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
	if sessions != proof.ExpectedSessionCount {
		t.Fatalf("writable synthetic control session count = %d, want fixture-pinned %d proving the denied statement is effective", sessions, proof.ExpectedSessionCount)
	}
}

func TestSQLiteSourceFilesContainNoDirectMutationEscape(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source guard location")
	}
	directory := filepath.Dir(currentFile)
	sourceBoundary := []string{
		filepath.Join(directory, "opencode_sqlite_source.go"),
		filepath.Join(directory, "opencode_sqlite_source_zombiezen.go"),
	}
	assertProductionFilesExcludeTokens(t, sourceBoundary, fixture.ForbiddenSourceBoundaryTokens, "source-boundary")
	production, err := ingestSourceGuardProductionFiles(directory)
	if err != nil {
		t.Fatalf("discover complete ingest production scope for SQLite token guard: %v", err)
	}
	assertProductionFilesExcludeTokens(t, production, fixture.ForbiddenIngestSQLiteTokens, "package-wide SQLite")
}

func assertProductionFilesExcludeTokens(t testing.TB, production, tokens []string, scope string) {
	t.Helper()
	for _, filename := range production {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read source-boundary production file %q: %v", filename, err)
		}
		for _, token := range tokens {
			if bytes.Contains(data, []byte(token)) {
				t.Errorf("%s production file %q contains forbidden mutation/data escape token %q", scope, filepath.Base(filename), token)
			}
		}
	}
}

func ingestSourceGuardProductionFiles(directory string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("resolve ingest production source pattern: %w", err)
	}
	production := make([]string, 0, len(matches))
	for _, filename := range matches {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		info, err := os.Stat(filename)
		if err != nil {
			return nil, fmt.Errorf("inspect ingest production source %q: %w", filename, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("ingest production source %q is not a regular file", filename)
		}
		production = append(production, filename)
	}
	if len(production) == 0 {
		return nil, fmt.Errorf("no regular non-test ingest production Go files found in %q", directory)
	}
	sort.Strings(production)
	return production, nil
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

func TestCurrentMessageCancellationWhileLockedReturnsNoPartialPageAndReusesSource(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-reader-pages")
	source := openConcreteSyntheticSource(t, materialized)
	writer, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic exclusive current writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "BEGIN EXCLUSIVE", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("begin synthetic exclusive current transaction: %v", err)
	}
	sessionID, err := NewOpenCodeCurrentSessionID("ses_current_reader_a")
	if err != nil {
		t.Fatalf("construct locked current session identifier: %v", err)
	}
	pageSize, err := NewOpenCodeCurrentPageSize(2)
	if err != nil {
		t.Fatalf("construct locked current page size: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan struct {
		page OpenCodeCurrentPage
		err  error
	}, 1)
	go func() {
		page, readErr := source.CurrentMessages(ctx, OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize})
		readDone <- struct {
			page OpenCodeCurrentPage
			err  error
		}{page: page, err: readErr}
	}()
	waitForActiveCatalog(t, source)
	cancel()
	result := <-readDone
	if !errors.Is(result.err, context.Canceled) || len(result.page.Messages) != 0 || result.page.Next != nil {
		t.Errorf("locked canceled current page = %+v error=%v, want zero page and cancellation cause", result.page, result.err)
	}
	rollbackErr := sqlitex.ExecuteTransient(writer, "ROLLBACK", nil)
	closeWriterErr := writer.Close()
	if rollbackErr != nil || closeWriterErr != nil {
		t.Fatalf("release synthetic exclusive current writer: %v", errors.Join(rollbackErr, closeWriterErr))
	}
	reused, reuseErr := source.CurrentMessages(t.Context(), OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize})
	if reuseErr != nil || len(reused.Messages) != 2 {
		t.Fatalf("reuse current source after cancellation = %+v error=%v, want complete page", reused, reuseErr)
	}
	if err := source.Close(t.Context()); err != nil {
		t.Fatalf("close source after current cancellation reuse: %v", err)
	}
}

type pendingRowCancellationCheckpoint struct {
	reached chan openCodeCurrentPendingPageState
	calls   atomic.Int64
}

func (checkpoint *pendingRowCancellationCheckpoint) AfterPendingRow(ctx context.Context, pending openCodeCurrentPendingPageState) error {
	if checkpoint.calls.Add(1) == 1 {
		select {
		case checkpoint.reached <- pending:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return ctx.Err()
}

func TestCurrentMessageCancellationAfterPendingRowReturnsNoPartialPageAndReusesSource(t *testing.T) {
	fixture := loadSourceSafetyFixture(t)
	for _, fixtureCase := range fixture.CancellationCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
			if err != nil {
				t.Fatalf("validate synthetic post-decode cancellation source: %v", err)
			}
			checkpoint := &pendingRowCancellationCheckpoint{reached: make(chan openCodeCurrentPendingPageState, 1)}
			options := DefaultOpenCodeSQLiteSourceOptions()
			options.cancellationCheckpoint = checkpoint
			var source OpenCodeSQLiteSource
			source, err = OpenOpenCodeSQLiteSource(t.Context(), path, options)
			if err != nil {
				t.Fatalf("open synthetic source with private cancellation checkpoint: %v", err)
			}
			sessionID, err := NewOpenCodeCurrentSessionID(fixtureCase.SessionID)
			if err != nil {
				t.Fatalf("construct post-decode cancellation session: %v", err)
			}
			pageSize, err := NewOpenCodeCurrentPageSize(fixtureCase.PageSize)
			if err != nil {
				t.Fatalf("construct post-decode cancellation page size: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan struct {
				page OpenCodeCurrentPage
				err  error
			}, 1)
			go func() {
				page, readErr := source.CurrentMessages(ctx, OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize})
				result <- struct {
					page OpenCodeCurrentPage
					err  error
				}{page: page, err: readErr}
			}()
			var pending openCodeCurrentPendingPageState
			select {
			case pending = <-checkpoint.reached:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("production current-row decoder did not reach the private cancellation checkpoint within one second; verify CurrentMessages invokes the checkpoint after collecting a decoded row")
			}
			if calls := checkpoint.calls.Load(); calls != 1 {
				t.Fatalf("production pending-page checkpoint was called %d times before cancellation, want exactly one", calls)
			}
			if int64(pending.count) != fixtureCase.PendingRowsBeforeCancel {
				t.Fatalf("production pending-page checkpoint observed %d collected rows before cancellation, want fixture-pinned %d", pending.count, fixtureCase.PendingRowsBeforeCancel)
			}
			assertDecodedCurrentRow(t, pending.row, fixtureCase.DecodedRow)
			cancel()
			var read struct {
				page OpenCodeCurrentPage
				err  error
			}
			select {
			case read = <-result:
			case <-time.After(time.Second):
				t.Fatal("post-decode canceled current read did not release its source permit within one second; ensure the checkpoint observes cancellation and the atomic page unwinds")
			}
			if !errors.Is(read.err, context.Canceled) || len(read.page.Messages) != 0 || read.page.Next != nil {
				t.Fatalf("post-decode canceled current page = %+v error=%v, want context.Canceled and zero atomic page", read.page, read.err)
			}
			reuseCtx, reuseCancel := context.WithTimeout(t.Context(), time.Second)
			defer reuseCancel()
			reused, reuseErr := source.CurrentMessages(reuseCtx, OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize})
			if reuseErr != nil || len(reused.Messages) != fixtureCase.ReuseRows {
				t.Fatalf("reuse after post-decode cancellation = %+v error=%v, want %d complete rows", reused, reuseErr, fixtureCase.ReuseRows)
			}
			if err := source.Close(t.Context()); err != nil {
				t.Fatalf("close source after post-decode cancellation: %v", err)
			}
		})
	}
}

func TestCurrentMessagePendingCheckpointFollowsPageAppend(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current reader source position guard")
	}
	filename := filepath.Join(filepath.Dir(currentFile), "opencode_sqlite_source_zombiezen.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, filename, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse current reader source for pending-page placement guard: %v", err)
	}
	var appendPosition, checkpointPosition token.Pos
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "CurrentMessages" || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.AssignStmt:
				if len(value.Rhs) == 1 {
					if call, ok := value.Rhs[0].(*ast.CallExpr); ok {
						if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "append" {
							appendPosition = value.Pos()
						}
					}
				}
			case *ast.CallExpr:
				if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "AfterPendingRow" {
					checkpointPosition = value.Pos()
				}
			}
			return true
		})
	}
	if appendPosition == token.NoPos || checkpointPosition == token.NoPos || appendPosition >= checkpointPosition {
		t.Fatalf("current pending-page placement = append %s checkpoint %s, want exactly one page append before AfterPendingRow; collect the decoded row before exposing the cancellation checkpoint", fileSet.Position(appendPosition), fileSet.Position(checkpointPosition))
	}
}

func assertDecodedCurrentRow(t testing.TB, row OpenCodeCurrentMessageRow, expected sourceDecodedRowExpectation) {
	t.Helper()
	if row.ID.String() != expected.ID || row.SessionID.String() != expected.SessionID || row.Type.String() != expected.Type ||
		row.TimeCreated != expected.TimeCreated || row.TimeUpdated != expected.TimeUpdated || row.Data != expected.Data || row.Seq.Value() != expected.Seq {
		t.Fatalf("decoded current row at cancellation checkpoint = id=%q session=%q type=%q created=%d updated=%d data=%q seq=%d, want fixture-pinned %+v", row.ID.String(), row.SessionID.String(), row.Type.String(), row.TimeCreated, row.TimeUpdated, row.Data, row.Seq.Value(), expected)
	}
}

func TestCurrentMessageDecoderRejectsNonTextProjectedSession(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-reader-pages")
	source := openConcreteSyntheticSource(t, materialized)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("wait for private current decoder permit: context ended")
	case <-source.permit:
	}
	oldInterrupt := source.conn.SetInterrupt(ctx.Done())
	err := source.executeRowsLocked(ctx, "SELECT id, CAST(session_id AS BLOB), type, time_created, time_updated, data, seq FROM session_message WHERE session_id = 'ses_current_reader_a' ORDER BY seq LIMIT 1", nil, func(stmt *sqlite.Stmt) error {
		_, decodeErr := decodeCurrentMessageRow(stmt)
		return decodeErr
	})
	source.conn.SetInterrupt(oldInterrupt)
	source.permit <- struct{}{}
	if err == nil || !strings.Contains(err.Error(), "column 1") {
		t.Fatalf("non-text projected current session decode error = %v, want strict column 1 rejection", err)
	}
	if err := source.Close(t.Context()); err != nil {
		t.Fatalf("close source after current session decoder check: %v", err)
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

type privateSourceState struct {
	catalog    OpenCodeSchemaEvidence
	sessionIDs []string
	messageIDs []string
}

// observePrivateSourceState reaches the public source operations only. The
// private statement executor is used separately solely to prove the installed
// authorizer denies fixture-owned mutation attempts.
func observePrivateSourceState(t *testing.T, source OpenCodeSQLiteSource, pageSize int) privateSourceState {
	t.Helper()
	size, err := NewOpenCodeCurrentPageSize(pageSize)
	if err != nil {
		t.Fatalf("construct bounded source observation page size: %v", err)
	}
	state := privateSourceState{}
	state.catalog, err = source.Catalog(t.Context())
	if err != nil {
		t.Fatalf("observe production source catalog: %v", err)
	}
	var sessionCursor *OpenCodeCurrentSessionCursor
	for {
		page, pageErr := source.CurrentSessionIDs(t.Context(), OpenCodeCurrentSessionPageRequest{PageSize: size, After: sessionCursor})
		if pageErr != nil {
			t.Fatalf("observe production source session page: %v", pageErr)
		}
		for _, sessionID := range page.SessionIDs {
			state.sessionIDs = append(state.sessionIDs, sessionID.String())
			var messageCursor *OpenCodeCurrentCursor
			for {
				messages, messagesErr := source.CurrentMessages(t.Context(), OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: size, After: messageCursor})
				if messagesErr != nil {
					t.Fatalf("observe production source current message page: %v", messagesErr)
				}
				for _, message := range messages.Messages {
					state.messageIDs = append(state.messageIDs, message.ID.String())
				}
				if messages.Next == nil {
					break
				}
				messageCursor = messages.Next
			}
		}
		if page.Next == nil {
			break
		}
		sessionCursor = page.Next
	}
	return state
}

func appendCommittedSyntheticWALTransaction(t *testing.T, path string) *sqlite.Conn {
	t.Helper()
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open test-owned WAL writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "PRAGMA wal_autocheckpoint=0", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("disable test-owned WAL autocheckpoint: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "INSERT INTO session (id) VALUES ('ses_source_safety_wal')", nil); err != nil {
		_ = writer.Close()
		t.Fatalf("append test-owned WAL session: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, `INSERT INTO session_message
		(id, session_id, type, time_created, time_updated, data, seq)
		VALUES ('sm_source_safety_wal', 'ses_source_safety_wal', 'message', 1, 1, '{"role":"assistant"}', 1)`, nil); err != nil {
		_ = writer.Close()
		t.Fatalf("append test-owned committed WAL message: %v", err)
	}
	return writer
}

func closeSyntheticWriter(t *testing.T, writer *sqlite.Conn) {
	t.Helper()
	if err := writer.Close(); err != nil {
		t.Errorf("close test-owned WAL writer: %v", err)
	}
}

func readTestOwnedSourceFile(t *testing.T, filename string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read test-owned source file %q: %v", filename, err)
	}
	return contents
}

func assertTestOwnedSourceFileEqual(t *testing.T, filename string, before []byte, label string) {
	t.Helper()
	if after := readTestOwnedSourceFile(t, filename); !bytes.Equal(before, after) {
		t.Errorf("%s changed while the production source denied a mutation", label)
	}
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
	if fixture.DeclaredDenialMutationProofs != expectedDenialMutationProofs {
		t.Fatalf("source denial mutation proof row guard: declared=%d required=%d", fixture.DeclaredDenialMutationProofs, expectedDenialMutationProofs)
	}
	if fixture.DeclaredCancellationCases != expectedSourceCancellationCases || len(fixture.CancellationCases) != expectedSourceCancellationCases {
		t.Fatalf("source cancellation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredCancellationCases, len(fixture.CancellationCases), expectedSourceCancellationCases)
	}
	if fixture.DeclaredSourceLifecycleCases != expectedSourceLifecycleCases || len(fixture.SourceLifecycleCases) != expectedSourceLifecycleCases {
		t.Fatalf("source lifecycle fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredSourceLifecycleCases, len(fixture.SourceLifecycleCases), expectedSourceLifecycleCases)
	}
	if strings.TrimSpace(fixture.DeniedFixture) == "" {
		t.Fatal("source safety fixture must name the test-owned WAL source used for every denied operation")
	}
	if len(fixture.ForbiddenSourceBoundaryTokens) == 0 || len(fixture.ForbiddenIngestSQLiteTokens) == 0 {
		t.Fatal("source safety fixture must declare source-boundary and package-wide SQLite structural tokens")
	}
	names := make(map[string]struct{}, len(fixture.DeniedStatements))
	denialMutationProofs := 0
	for _, denied := range fixture.DeniedStatements {
		if strings.TrimSpace(denied.Name) == "" || strings.TrimSpace(denied.Statement) == "" || strings.TrimSpace(denied.ErrorContains) == "" {
			t.Fatalf("source safety fixture has missing required fields: %+v", denied)
		}
		if _, duplicate := names[denied.Name]; duplicate {
			t.Fatalf("source safety fixture has duplicate name %q", denied.Name)
		}
		names[denied.Name] = struct{}{}
		if denied.MutationProof {
			denialMutationProofs++
			if denied.ExpectedSessionCount <= 0 {
				t.Fatalf("source safety denial mutation proof %q has invalid expected session count %d", denied.Name, denied.ExpectedSessionCount)
			}
		} else if denied.ExpectedSessionCount != 0 {
			t.Fatalf("source safety denial %q has an expected session count without a mutation proof", denied.Name)
		}
	}
	if denialMutationProofs != fixture.DeclaredDenialMutationProofs {
		t.Fatalf("source denial mutation proof actual count=%d, want declared=%d", denialMutationProofs, fixture.DeclaredDenialMutationProofs)
	}
	for _, fixtureCase := range fixture.CancellationCases {
		if strings.TrimSpace(fixtureCase.Name) == "" || strings.TrimSpace(fixtureCase.Fixture) == "" || strings.TrimSpace(fixtureCase.SessionID) == "" || fixtureCase.PageSize <= 0 || fixtureCase.PendingRowsBeforeCancel <= 0 || fixtureCase.ReuseRows <= 0 || strings.TrimSpace(fixtureCase.DecodedRow.ID) == "" || strings.TrimSpace(fixtureCase.DecodedRow.SessionID) == "" || strings.TrimSpace(fixtureCase.DecodedRow.Type) == "" || !json.Valid([]byte(fixtureCase.DecodedRow.Data)) || fixtureCase.DecodedRow.Seq < 0 {
			t.Fatalf("source cancellation fixture has missing or invalid fields: %+v", fixtureCase)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			t.Fatalf("source safety fixture has duplicate name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
	}
	for _, fixtureCase := range fixture.SourceLifecycleCases {
		if strings.TrimSpace(fixtureCase.Name) == "" || strings.TrimSpace(fixtureCase.Fixture) == "" || fixtureCase.PageSize <= 0 || len(fixtureCase.ExpectedSessionIDs) == 0 || len(fixtureCase.ExpectedMessageIDs) == 0 {
			t.Fatalf("source lifecycle fixture has missing required fields: %+v", fixtureCase)
		}
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			t.Fatalf("source safety fixture has duplicate name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
		validateDistinctSourceFixtureIDs(t, fixtureCase.Name, "session", fixtureCase.ExpectedSessionIDs)
		validateDistinctSourceFixtureIDs(t, fixtureCase.Name, "message", fixtureCase.ExpectedMessageIDs)
	}
	validateSourceTokens(t, "source-boundary", fixture.ForbiddenSourceBoundaryTokens)
	validateSourceTokens(t, "package-wide SQLite", fixture.ForbiddenIngestSQLiteTokens)
	return fixture
}

func validateDistinctSourceFixtureIDs(t testing.TB, caseName, kind string, values []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("source lifecycle fixture %q has empty expected %s identifier", caseName, kind)
		}
		if _, duplicate := seen[value]; duplicate {
			t.Fatalf("source lifecycle fixture %q has duplicate expected %s identifier %q", caseName, kind, value)
		}
		seen[value] = struct{}{}
	}
}

func validateSourceTokens(t testing.TB, scope string, values []string) {
	t.Helper()
	tokens := make(map[string]struct{}, len(values))
	for _, token := range values {
		if strings.TrimSpace(token) == "" {
			t.Fatalf("source safety fixture has empty %s structural token", scope)
		}
		if _, duplicate := tokens[token]; duplicate {
			t.Fatalf("source safety fixture has duplicate %s structural token %q", scope, token)
		}
		tokens[token] = struct{}{}
	}
}
