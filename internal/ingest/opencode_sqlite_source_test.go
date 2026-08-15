package ingest_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestOpenCodeSQLiteSourceReturnsBoundedDetachedCatalog(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "hybrid-catalog")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	evidence, err := source.Catalog(t.Context())
	if err != nil {
		t.Fatalf("inspect synthetic catalog through typed production boundary: %v", err)
	}
	wantTables := materialized.ExpectedCatalog().Tables()
	sort.Strings(wantTables)
	if !equalStrings(evidence.Tables, wantTables) {
		t.Errorf("catalog tables = %v, want %v", evidence.Tables, wantTables)
	}
	if len(evidence.LegacyMessageColumns) == 0 || len(evidence.LegacyPartColumns) == 0 || len(evidence.CurrentMessageColumns) == 0 || len(evidence.CurrentIndexes) == 0 {
		t.Errorf("typed catalog omitted required bounded structural evidence: %+v", evidence)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceHonorsCancellationBeforeCatalog(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "empty-valid")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := source.Catalog(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("catalog with canceled context error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), materialized.Path) || !strings.Contains(err.Error(), "live bounded context") {
		t.Errorf("canceled catalog error is not actionable: %q", err)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceReportsCorruptCatalogActionably(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "corrupt-non-sqlite")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	_, err := source.Catalog(t.Context())
	if err == nil {
		t.Fatal("inspect corrupt synthetic source: expected actionable error")
	}
	errorText := err.Error()
	if !strings.Contains(errorText, materialized.Path) ||
		!strings.Contains(errorText, "bounded explicit-column schema collection") ||
		!strings.Contains(errorText, "no transcript rows were exposed") ||
		!strings.Contains(errorText, "verify the OpenCode schema") {
		t.Errorf("corrupt catalog error is not actionable: %q", err)
	}
	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceHonorsInjectedCatalogDeadline(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "empty-valid")
	before := testfixture.SnapshotSource(t, materialized)
	clock := &cancelCatalogClock{}
	options, err := ingest.NewOpenCodeSQLiteSourceOptions(time.Millisecond, time.Second, clock)
	if err != nil {
		t.Fatalf("create controlled source options: %v", err)
	}
	source := openSyntheticSource(t, materialized, options)

	_, err = source.Catalog(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("catalog with injected deadline error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "deadline ended") ||
		!strings.Contains(err.Error(), "source remains untouched") && !strings.Contains(err.Error(), "no source write was attempted") {
		t.Errorf("deadline catalog error is not actionable: %q", err)
	}
	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceBlockedConsumerCannotHoldConnectionOrClose(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	evidence, err := source.Catalog(t.Context())
	if err != nil {
		t.Fatalf("materialize detached catalog before blocking consumer: %v", err)
	}

	consumerStarted := make(chan struct{})
	releaseConsumer := make(chan struct{})
	consumerDone := make(chan struct{})
	go func(detached ingest.OpenCodeSchemaEvidence) {
		close(consumerStarted)
		<-releaseConsumer
		_ = len(detached.CurrentMessageColumns)
		close(consumerDone)
	}(evidence)
	<-consumerStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- source.Close(context.Background()) }()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("close while detached consumer is blocked: %v", closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("detached consumer held the source connection or cleanup")
	}
	close(releaseConsumer)
	<-consumerDone
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeSQLiteSourceCloseUsesInjectedDeadlinePolicy(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "empty-valid")
	clock := &deadlineRecordingClock{}
	const queryTimeout = 75 * time.Millisecond
	options, err := ingest.NewOpenCodeSQLiteSourceOptions(5*time.Millisecond, queryTimeout, clock)
	if err != nil {
		t.Fatalf("create source options with recorded cleanup policy: %v", err)
	}
	source := openSyntheticSource(t, materialized, options)
	closeSyntheticSource(t, source)
	durations := clock.Durations()
	if len(durations) != 2 {
		t.Fatalf("deadline policy calls = %d, want initialization and Close", len(durations))
	}
	if durations[0] != queryTimeout || durations[1] != queryTimeout {
		t.Errorf("deadline policy durations = %v, want both %s", durations, queryTimeout)
	}
}

func TestOpenCodeSQLiteSourceReadsWALOnlyCatalogWithoutChangingContent(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "wal-capable")
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	appendWALCatalogIndex(t, writer, "wal_pending_catalog_idx")

	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	evidence, err := source.Catalog(t.Context())
	if err != nil {
		t.Fatalf("read WAL-only catalog evidence: %v", err)
	}
	if !hasIndex(evidence.CurrentIndexes, "wal_pending_catalog_idx") {
		t.Errorf("WAL-aware catalog indexes = %+v, want WAL-only index", evidence.CurrentIndexes)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeSQLiteSourceRepeatedWALCatalogDoesNotCheckpointOrTruncate(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "wal-capable")
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	appendWALCatalogIndex(t, writer, "wal_repeat_catalog_idx")

	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	for iteration := 0; iteration < 5; iteration++ {
		source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
		evidence, err := source.Catalog(t.Context())
		if err != nil {
			t.Fatalf("repeated WAL catalog read %d: %v", iteration, err)
		}
		if !hasIndex(evidence.CurrentIndexes, "wal_repeat_catalog_idx") {
			t.Errorf("repeated WAL catalog read %d omitted WAL-only index", iteration)
		}
		closeSyntheticSource(t, source)
	}
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeSQLiteSourceConcurrentWALWriterRemainsHealthy(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "wal-capable")
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic WAL writer")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)

	writerDone := make(chan error, 1)
	readerDone := make(chan error, 1)
	go func() {
		var err error
		for sequence := int64(1); sequence <= 10 && err == nil; sequence++ {
			err = appendWALSessionResult(writer, fmtSyntheticID("ses_concurrent", sequence), fmtSyntheticID("msg_concurrent", sequence), sequence)
		}
		writerDone <- err
	}()
	go func() {
		var err error
		for iteration := 0; iteration < 10 && err == nil; iteration++ {
			_, err = source.Catalog(context.Background())
		}
		readerDone <- err
	}()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("concurrent synthetic WAL writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent synthetic WAL writer did not finish within the bound")
	}
	select {
	case err := <-readerDone:
		if err != nil {
			t.Fatalf("concurrent bounded catalog reader: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent bounded catalog reader did not finish within the bound")
	}
}

func TestOpenCodeSQLiteSourceLeavesOnlyBenignWALCoordinationResidue(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "wal-capable")
	databaseBefore := readSyntheticFile(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if _, err := source.Catalog(t.Context()); err != nil {
		t.Fatalf("read clean WAL catalog: %v", err)
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
	if readErr != nil || closeErr != nil {
		t.Fatalf("plain read/close after WAL coordination residue: %v", errors.Join(readErr, closeErr))
	}
	if count != 1 {
		t.Errorf("plain read count after WAL coordination residue = %d, want 1", count)
	}
}

func TestOpenCodeSQLiteSourceClosePreventsReuse(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "empty-valid")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())

	closeSyntheticSource(t, source)
	if err := source.Close(t.Context()); err != nil {
		t.Fatalf("close source a second time: %v", err)
	}
	_, err := source.Catalog(t.Context())
	if err == nil || !strings.Contains(err.Error(), "closing or closed") {
		t.Fatalf("inspect closed source error = %v, want actionable closed-source error", err)
	}
	testfixture.AssertUnchanged(t, materialized, before)
}

type cancelCatalogClock struct {
	calls atomic.Int64
}

type deadlineRecordingClock struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (c *deadlineRecordingClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	c.mu.Lock()
	c.durations = append(c.durations, timeout)
	c.mu.Unlock()
	return context.WithTimeout(parent, timeout)
}

func (c *deadlineRecordingClock) Durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.durations...)
}

func (c *cancelCatalogClock) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	switch c.calls.Add(1) {
	case 1, 3:
		return context.WithTimeout(parent, timeout)
	default:
		ctx, cancel := context.WithCancelCause(parent)
		cancel(context.DeadlineExceeded)
		return ctx, func() {}
	}
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
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close synthetic source through production boundary: %v", err)
	}
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

func appendWALCatalogIndex(t *testing.T, writer *sqlite.Conn, name string) {
	t.Helper()
	statement := "CREATE INDEX " + name + " ON session_message(type)"
	if err := sqlitex.ExecuteTransient(writer, statement, nil); err != nil {
		t.Fatalf("append synthetic WAL-only catalog index %q: %v", name, err)
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

func fmtSyntheticID(prefix string, sequence int64) string {
	return fmt.Sprintf("%s_%d", prefix, sequence)
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
		t.Errorf("WAL frame count changed from %d to %d; catalog reads must not append, truncate, or checkpoint committed transactions", before.frameCount, after.frameCount)
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
		t.Errorf("%s bytes changed during read-only catalog operation", label)
	}
}

func hasIndex(indexes []ingest.OpenCodeIndexEvidence, name string) bool {
	for _, index := range indexes {
		if index.Name == name {
			return true
		}
	}
	return false
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
