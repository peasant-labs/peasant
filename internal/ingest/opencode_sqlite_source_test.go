package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const expectedDeniedStatementCount = 8

//go:embed testdata/opencode_sqlite_source.yaml
var sourceSafetyYAML []byte

type sourceSafetyCorpus struct {
	DeclaredDeniedStatements int               `yaml:"declared_denied_statements"`
	DeniedStatements         []deniedStatement `yaml:"denied_statements"`
}

type deniedStatement struct {
	Name          string `yaml:"name"`
	Statement     string `yaml:"statement"`
	ErrorContains string `yaml:"error_contains"`
}

func TestOpenCodeSQLiteSourceReadsCatalogWithQueryOnlyEnabled(t *testing.T) {
	fixtureCase := testfixture.CaseByName(t, "hybrid-catalog")
	materialized := testfixture.Materialize(t, fixtureCase)
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	queryOnly := readSingleInteger(t, source, "PRAGMA query_only")
	if queryOnly != 1 {
		t.Fatalf("query_only = %d, want 1", queryOnly)
	}

	gotTables := readCatalogNames(t, source, "table")
	gotIndexes := readCatalogNames(t, source, "index")
	wantTables := append([]string(nil), fixtureCase.ExpectedCatalog.Tables...)
	wantIndexes := append([]string(nil), fixtureCase.ExpectedCatalog.Indexes...)
	sort.Strings(wantTables)
	sort.Strings(wantIndexes)
	if !equalStrings(gotTables, wantTables) {
		t.Errorf("catalog tables = %v, want %v", gotTables, wantTables)
	}
	if !equalStrings(gotIndexes, wantIndexes) {
		t.Errorf("catalog indexes = %v, want %v", gotIndexes, wantIndexes)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceRejectsMutatingStatements(t *testing.T) {
	for _, denied := range loadDeniedStatements(t) {
		denied := denied
		t.Run(denied.Name, func(t *testing.T) {
			materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "current-session-message"))
			before := testfixture.SnapshotSource(t, materialized)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

			err := source.Read(context.Background(), denied.Statement, nil, nil)
			if err == nil {
				t.Fatalf("read mutating statement %q: expected rejection", denied.Statement)
			}
			if !strings.Contains(err.Error(), denied.ErrorContains) {
				t.Errorf("read mutating statement error = %q, want substring %q", err, denied.ErrorContains)
			}
			if !strings.Contains(err.Error(), materialized.Path) || !strings.Contains(err.Error(), "use exactly one SELECT") {
				t.Errorf("read mutating statement error is not actionable: %q", err)
			}

			closeSyntheticSource(t, source)
			testfixture.AssertUnchanged(t, materialized, before)
		})
	}
}

func TestOpenCodeSQLiteSourceHonorsCancellationBeforeRead(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "empty-valid"))
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := source.Read(ctx, "SELECT 1", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read with canceled context error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), materialized.Path) || !strings.Contains(err.Error(), "retry with a live bounded context") {
		t.Errorf("canceled read error is not actionable: %q", err)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceReportsCorruptSourceActionably(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "corrupt-non-sqlite"))
	before := testfixture.SnapshotSource(t, materialized)
	path, err := ingest.NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate corrupt synthetic source path: %v", err)
	}
	source, err := ingest.OpenOpenCodeSQLiteSource(context.Background(), path, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open corrupt synthetic source before its first data read: %v", err)
	}
	err = source.Read(context.Background(), "SELECT name FROM sqlite_master", nil, nil)
	if err == nil {
		t.Fatal("read corrupt synthetic source: expected actionable error")
	}
	errorText := err.Error()
	if !strings.Contains(errorText, materialized.Path) ||
		!strings.Contains(errorText, "preparing the bounded read statement") ||
		!strings.Contains(errorText, "query execution did not start") ||
		!strings.Contains(errorText, "verify the expected OpenCode schema") {
		t.Errorf("corrupt source error is not actionable: %q", err)
	}
	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceHonorsInjectedQueryDeadline(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "empty-valid"))
	before := testfixture.SnapshotSource(t, materialized)
	clock := &cancelAfterInitializationClock{}
	options, err := ingest.NewOpenCodeSQLiteSourceOptions(time.Millisecond, time.Second, clock)
	if err != nil {
		t.Fatalf("create controlled source options: %v", err)
	}
	source := openSyntheticSource(t, materialized, options)

	err = source.Read(context.Background(), "SELECT 1", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read with injected deadline error = %v, want context.DeadlineExceeded", err)
	}
	errorText := err.Error()
	if !strings.Contains(errorText, "deadline ended") ||
		!strings.Contains(errorText, "source remains untouched") && !strings.Contains(errorText, "no source write was attempted") {
		t.Errorf("deadline read error is not actionable: %q", err)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceBoundsSingleConnectionWait(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "empty-valid"))
	before := testfixture.SnapshotSource(t, materialized)
	options, err := ingest.NewOpenCodeSQLiteSourceOptions(5*time.Millisecond, 40*time.Millisecond, systemTestDeadlineClock{})
	if err != nil {
		t.Fatalf("create short source options: %v", err)
	}
	source := openSyntheticSource(t, materialized, options)

	visitorEntered := make(chan struct{})
	releaseVisitor := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- source.Read(context.Background(), "SELECT 1", nil, func(ingest.OpenCodeSQLiteRow) error {
			close(visitorEntered)
			<-releaseVisitor
			return nil
		})
	}()
	select {
	case <-visitorEntered:
	case <-time.After(time.Second):
		t.Fatal("first read did not enter its visitor")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- source.Read(context.Background(), "SELECT 2", nil, nil)
	}()
	select {
	case secondErr := <-secondDone:
		if !errors.Is(secondErr, context.DeadlineExceeded) {
			t.Errorf("second read while connection occupied error = %v, want context.DeadlineExceeded", secondErr)
		}
	case <-time.After(time.Second):
		t.Error("second read did not honor the bounded single-connection wait")
	}
	close(releaseVisitor)
	if firstErr := <-firstDone; !errors.Is(firstErr, context.DeadlineExceeded) {
		t.Errorf("first serialized read error = %v, want context.DeadlineExceeded after the deliberately blocked visitor", firstErr)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceReadsWALOnlyRowsWithoutChangingContent(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "wal-capable"))
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	appendWALSession(t, writer, "ses_wal_pending", "sm_wal_pending", 1)

	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	got := readSingleInteger(t, source, "SELECT count(*) FROM session_message")
	if got != 2 {
		t.Errorf("WAL-aware session_message count = %d, want 2 including the WAL-only row", got)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeSQLiteSourceRepeatedWALReadsDoNotCheckpointOrTruncate(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "wal-capable"))
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	appendWALSession(t, writer, "ses_wal_repeat", "sm_wal_repeat", 1)

	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	for iteration := 0; iteration < 5; iteration++ {
		source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
		if got := readSingleInteger(t, source, "SELECT count(*) FROM session_message"); got != 2 {
			t.Errorf("WAL-aware repeated read %d count = %d, want 2", iteration, got)
		}
		closeSyntheticSource(t, source)
	}
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeSQLiteSourceDeniedWALWriteLeavesTransactionContentUnchanged(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "wal-capable"))
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	appendWALSession(t, writer, "ses_wal_denied_seed", "sm_wal_denied_seed", 1)

	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	err := source.Read(context.Background(), "DELETE FROM session_message", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "forbidden SQLite action") || !strings.Contains(err.Error(), "use exactly one SELECT") {
		t.Fatalf("denied WAL write error = %v, want actionable authorizer rejection", err)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeSQLiteSourceConcurrentWALWriterRemainsHealthy(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "wal-capable"))
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)

	visitorEntered := make(chan struct{})
	releaseVisitor := make(chan struct{})
	readDone := make(chan error, 1)
	var snapshotIDs []string
	go func() {
		readDone <- source.Read(context.Background(), "SELECT id FROM session_message ORDER BY id", nil, func(row ingest.OpenCodeSQLiteRow) error {
			columns := row.Columns()
			if len(columns) != 1 || columns[0].Value.Kind != ingest.OpenCodeSQLiteValueText {
				return errors.New("WAL snapshot query did not return exactly one text column")
			}
			snapshotIDs = append(snapshotIDs, columns[0].Value.Text)
			if len(snapshotIDs) == 1 {
				close(visitorEntered)
				<-releaseVisitor
			}
			return nil
		})
	}()
	select {
	case <-visitorEntered:
	case <-time.After(time.Second):
		t.Fatal("WAL reader did not establish its snapshot")
	}

	writerDone := make(chan error, 1)
	go func() {
		writerDone <- appendWALSessionResult(writer, "ses_wal_concurrent", "sm_wal_concurrent", 1)
	}()
	var writerErr error
	select {
	case writerErr = <-writerDone:
	case <-time.After(time.Second):
		writerErr = errors.New("concurrent synthetic WAL writer did not commit within the bound")
	}
	close(releaseVisitor)
	if err := <-readDone; err != nil {
		t.Fatalf("read consistent WAL snapshot: %v", err)
	}
	if writerErr != nil {
		t.Fatalf("concurrent synthetic WAL writer: %v", writerErr)
	}
	if len(snapshotIDs) != 1 || snapshotIDs[0] != "sm_wal_1" {
		t.Errorf("WAL snapshot ids = %v, want only the pre-commit row", snapshotIDs)
	}
	if got := readSingleInteger(t, source, "SELECT count(*) FROM session_message"); got != 2 {
		t.Errorf("post-commit WAL-aware count = %d, want 2", got)
	}
}

func TestOpenCodeSQLiteSourceLeavesOnlyBenignWALCoordinationResidue(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "wal-capable"))
	databaseBefore := readSyntheticFile(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if got := readSingleInteger(t, source, "SELECT count(*) FROM session_message"); got != 1 {
		t.Errorf("clean WAL source count = %d, want 1", got)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")

	walBytes, walPresent := readOptionalSyntheticFile(t, materialized.Path+"-wal")
	if walPresent && len(walBytes) != 0 {
		t.Errorf("reader-created WAL residue has %d bytes, want empty coordination residue", len(walBytes))
	}
	plain, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("plain open after WAL coordination residue: %v", err)
	}
	var count int64
	readErr := sqlitex.ExecuteTransient(plain, "SELECT count(*) FROM session_message", &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		count = stmt.ColumnInt64(0)
		return nil
	}})
	closeErr := plain.Close()
	if readErr != nil {
		t.Fatalf("plain read after WAL coordination residue: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close plain reader after WAL coordination residue: %v", closeErr)
	}
	if count != 1 {
		t.Errorf("plain read count after WAL coordination residue = %d, want 1", count)
	}
}

func TestOpenCodeSQLiteSourceClosePreventsReuse(t *testing.T) {
	materialized := testfixture.Materialize(t, testfixture.CaseByName(t, "empty-valid"))
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	closeSyntheticSource(t, source)
	if err := source.Close(); err != nil {
		t.Fatalf("close source a second time: %v", err)
	}
	err := source.Read(context.Background(), "SELECT 1", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "source is closed") {
		t.Fatalf("read closed source error = %v, want actionable closed-source error", err)
	}
	testfixture.AssertUnchanged(t, materialized, before)
}

type cancelAfterInitializationClock struct {
	calls atomic.Int64
}

func (c *cancelAfterInitializationClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if c.calls.Add(1) == 1 {
		return context.WithTimeout(parent, timeout)
	}
	ctx, cancel := context.WithCancelCause(parent)
	cancel(context.DeadlineExceeded)
	return ctx, func() {}
}

type systemTestDeadlineClock struct{}

func (systemTestDeadlineClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

func openSyntheticSource(t *testing.T, materialized testfixture.MaterializedSource, options ingest.OpenCodeSQLiteSourceOptions) ingest.OpenCodeSQLiteSource {
	t.Helper()
	path, err := ingest.NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	source, err := ingest.OpenOpenCodeSQLiteSource(context.Background(), path, options)
	if err != nil {
		t.Fatalf("open synthetic source through production boundary: %v", err)
	}
	return source
}

func closeSyntheticSource(t *testing.T, source ingest.OpenCodeSQLiteSource) {
	t.Helper()
	if err := source.Close(); err != nil {
		t.Fatalf("close synthetic source through production boundary: %v", err)
	}
}

func readSingleInteger(t *testing.T, source ingest.OpenCodeSQLiteSource, statement string) int64 {
	t.Helper()
	var values []int64
	err := source.Read(context.Background(), statement, nil, func(row ingest.OpenCodeSQLiteRow) error {
		columns := row.Columns()
		if len(columns) != 1 || columns[0].Value.Kind != ingest.OpenCodeSQLiteValueInteger {
			return errors.New("query did not return exactly one integer column")
		}
		values = append(values, columns[0].Value.Integer)
		return nil
	})
	if err != nil {
		t.Fatalf("read one integer through production source: %v", err)
	}
	if len(values) != 1 {
		t.Fatalf("integer result row count = %d, want 1", len(values))
	}
	return values[0]
}

func readCatalogNames(t *testing.T, source ingest.OpenCodeSQLiteSource, objectType string) []string {
	t.Helper()
	var names []string
	err := source.Read(context.Background(), `SELECT name FROM sqlite_master
		WHERE type = ?1 AND name NOT LIKE 'sqlite_%' ORDER BY name`, []any{objectType}, func(row ingest.OpenCodeSQLiteRow) error {
		columns := row.Columns()
		if len(columns) != 1 || columns[0].Value.Kind != ingest.OpenCodeSQLiteValueText {
			return errors.New("catalog query did not return exactly one text column")
		}
		names = append(names, columns[0].Value.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s catalog through production source: %v", objectType, err)
	}
	return names
}

type walContentState struct {
	bytes      []byte
	frameCount int
}

func openWALWriter(t *testing.T, path string) *sqlite.Conn {
	t.Helper()
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic WAL writer: %v", err)
	}
	if err := sqlitex.ExecuteTransient(writer, "PRAGMA wal_autocheckpoint=0", nil); err != nil {
		closeErr := writer.Close()
		t.Fatalf("disable synthetic WAL auto-checkpoint: %v (close error: %v)", err, closeErr)
	}
	return writer
}

func closeSQLiteConnection(t *testing.T, conn *sqlite.Conn, label string) {
	t.Helper()
	if err := conn.Close(); err != nil {
		t.Errorf("close %s: %v", label, err)
	}
}

func appendWALSession(t *testing.T, writer *sqlite.Conn, sessionID, messageID string, sequence int64) {
	t.Helper()
	if err := appendWALSessionResult(writer, sessionID, messageID, sequence); err != nil {
		t.Fatalf("append synthetic WAL session %q: %v", sessionID, err)
	}
}

func appendWALSessionResult(writer *sqlite.Conn, sessionID, messageID string, sequence int64) (err error) {
	endTransaction, err := sqlitex.ImmediateTransaction(writer)
	if err != nil {
		return err
	}
	defer endTransaction(&err)
	if err = sqlitex.ExecuteTransient(writer, "INSERT INTO session (id) VALUES (?1)", &sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		return err
	}
	return sqlitex.ExecuteTransient(writer, `INSERT INTO session_message
		(id, session_id, type, time_created, time_updated, data, seq)
		VALUES (?1, ?2, 'message', 5000, 5000, '{"role":"assistant"}', ?3)`, &sqlitex.ExecOptions{Args: []any{messageID, sessionID, sequence}})
}

func readWALState(t *testing.T, databasePath string) walContentState {
	t.Helper()
	walPath := databasePath + "-wal"
	data := readSyntheticFile(t, walPath)
	if len(data) < 32 {
		t.Fatalf("synthetic WAL %q has %d bytes, want at least the 32-byte header", walPath, len(data))
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		t.Fatalf("synthetic WAL %q has magic %#x, want a SQLite WAL header", walPath, magic)
	}
	pageSize := int(binary.BigEndian.Uint32(data[8:12]))
	if pageSize == 1 {
		pageSize = 65536
	}
	frameSize := 24 + pageSize
	payloadSize := len(data) - 32
	if pageSize <= 0 || payloadSize%frameSize != 0 {
		t.Fatalf("synthetic WAL %q size %d is not aligned to page size %d and frame header size 24", walPath, len(data), pageSize)
	}
	frameCount := payloadSize / frameSize
	if frameCount == 0 {
		t.Fatalf("synthetic WAL %q has no committed frames", walPath)
	}
	return walContentState{bytes: data, frameCount: frameCount}
}

func assertWALStateEqual(t *testing.T, databasePath string, before walContentState) {
	t.Helper()
	after := readWALState(t, databasePath)
	if after.frameCount != before.frameCount {
		t.Errorf("WAL frame count changed from %d to %d; source reads must not append, truncate, or checkpoint committed transactions", before.frameCount, after.frameCount)
	}
	if !bytes.Equal(after.bytes, before.bytes) {
		t.Errorf("WAL transaction bytes changed while frame count remained %d", before.frameCount)
	}
}

func readSyntheticFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synthetic source file %q: %v", path, err)
	}
	return data
}

func readOptionalSyntheticFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read optional synthetic coordination file %q: %v", path, err)
	}
	return data, true
}

func assertSyntheticFileEqual(t *testing.T, path string, before []byte, label string) {
	t.Helper()
	after := readSyntheticFile(t, path)
	if !bytes.Equal(after, before) {
		t.Errorf("%s bytes changed during read-only source operation", label)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func loadDeniedStatements(t *testing.T) []deniedStatement {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sourceSafetyYAML))
	decoder.KnownFields(true)
	var corpus sourceSafetyCorpus
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatalf("decode source safety fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode source safety fixtures: expected exactly one YAML document: %v", err)
	}
	if corpus.DeclaredDeniedStatements != expectedDeniedStatementCount || len(corpus.DeniedStatements) != expectedDeniedStatementCount {
		t.Fatalf("source safety fixture row guard: declared=%d actual=%d required=%d", corpus.DeclaredDeniedStatements, len(corpus.DeniedStatements), expectedDeniedStatementCount)
	}
	names := make(map[string]struct{}, len(corpus.DeniedStatements))
	for _, denied := range corpus.DeniedStatements {
		if strings.TrimSpace(denied.Name) == "" || strings.TrimSpace(denied.Statement) == "" || strings.TrimSpace(denied.ErrorContains) == "" {
			t.Fatalf("source safety fixture has missing required fields: %+v", denied)
		}
		if _, duplicate := names[denied.Name]; duplicate {
			t.Fatalf("source safety fixture has duplicate name %q", denied.Name)
		}
		names[denied.Name] = struct{}{}
	}
	return corpus.DeniedStatements
}
