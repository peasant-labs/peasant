package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestOpenCodeLegacyReaderReturnsDetachedRows(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	before := testfixture.SnapshotSource(t, materialized)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	pageSize := mustLegacyPageSize(t, 2)
	sessionID := mustLegacySessionID(t, "ses_reader_a")
	firstMessages, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize})
	if err != nil {
		t.Fatalf("read synthetic legacy message page before detached mutation: %v", err)
	}

	firstMessages.Messages[0].Data = "mutated detached copy"
	repeated, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize})
	if err != nil {
		t.Fatalf("repeat identical message cursor: %v", err)
	}
	if repeated.Messages[0].Data == "mutated detached copy" || repeated.Messages[0].ID != firstMessages.Messages[0].ID {
		t.Errorf("repeated cursor page was not deterministic and detached: %+v", repeated)
	}

	closeSyntheticSource(t, source)
	testfixture.AssertUnchanged(t, materialized, before)
}

func TestOpenCodeLegacyReaderReturnsFixtureOwnedLargeInlinePayload(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	page, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_reader_a"), PageSize: mustLegacyPageSize(t, 4)})
	if err != nil {
		t.Fatalf("read synthetic legacy page containing large inline payload marker: %v", err)
	}
	if len(page.Messages) != 4 || !strings.Contains(page.Messages[3].Data, "LARGE_INLINE_PAYLOAD_MARKER") {
		t.Fatalf("bounded synthetic legacy page omitted fixture-owned large inline payload marker: %+v", page)
	}
}

func TestOpenCodeLegacyReaderReturnsEmptyPagesForUnknownSession(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	request := ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_empty"), PageSize: mustLegacyPageSize(t, 2)}
	page, err := source.LegacyMessages(t.Context(), request)
	if err != nil {
		t.Fatalf("read empty synthetic legacy session: %v", err)
	}
	if len(page.Messages) != 0 || cap(page.Messages) != 0 || page.Next != nil {
		t.Fatalf("empty session page = %+v length/capacity=%d/%d, want no rows, no retained capacity, and no cursor", page, len(page.Messages), cap(page.Messages))
	}
}

func TestOpenCodeLegacyReaderUsesOnlyMaterializedTables(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "repeated-history-distractors")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	sessionID := mustLegacySessionID(t, "ses_history")
	messages, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: mustLegacyPageSize(t, 4)})
	if err != nil {
		t.Fatalf("read materialized message with history distractors: %v", err)
	}
	if len(messages.Messages) != 1 || messages.Messages[0].ID.String() != "msg_latest" || !strings.Contains(messages.Messages[0].Data, `"version":"latest"`) {
		t.Fatalf("materialized message page = %+v, want only latest primary-table row", messages)
	}
	parts, err := source.LegacyParts(t.Context(), ingest.OpenCodeLegacyPartPageRequest{SessionID: sessionID, MessageID: messages.Messages[0].ID, PageSize: mustLegacyPageSize(t, 4)})
	if err != nil {
		t.Fatalf("read materialized part with history distractors: %v", err)
	}
	if len(parts.Parts) != 1 || parts.Parts[0].ID.String() != "part_latest" || !strings.Contains(parts.Parts[0].Data, "latest materialized part") {
		t.Fatalf("materialized part page = %+v, want only latest primary-table row", parts)
	}
}

func TestOpenCodeLegacyReaderRejectsInvalidTypedInputsBeforeSourceAccess(t *testing.T) {
	if _, err := ingest.NewOpenCodeLegacyPageSize(0); err == nil || !strings.Contains(err.Error(), "fixed maximum") {
		t.Fatalf("zero page size error = %v, want actionable fixed-bound rejection", err)
	}
	if _, err := ingest.NewOpenCodeLegacyPageSize(ingest.MaxOpenCodeLegacyPageSize + 1); err == nil {
		t.Fatal("page-size constructor accepted a value above the fixed maximum")
	}
	if _, err := ingest.NewOpenCodeLegacySessionID(" bad "); err == nil || !strings.Contains(err.Error(), "before source access") {
		t.Fatalf("invalid session identifier error = %v, want actionable boundary rejection", err)
	}

	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	if page, err := source.LegacySessionIDs(nil, ingest.OpenCodeLegacySessionPageRequest{PageSize: mustLegacyPageSize(t, 1)}); err == nil || page.Next != nil || len(page.SessionIDs) != 0 {
		t.Fatalf("nil-context session page = %+v error=%v, want zero page and actionable error", page, err)
	}
	if page, err := source.LegacySessionIDs(t.Context(), ingest.OpenCodeLegacySessionPageRequest{}); err == nil || page.Next != nil || len(page.SessionIDs) != 0 {
		t.Fatalf("zero-value page request = %+v error=%v, want zero page and validation error", page, err)
	}
	invalidCursor := &ingest.OpenCodeLegacyMessageCursor{}
	page, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_reader_a"), PageSize: mustLegacyPageSize(t, 1), After: invalidCursor})
	if err == nil || len(page.Messages) != 0 || page.Next != nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("invalid message cursor page = %+v error=%v, want zero page and cursor error", page, err)
	}
}

func TestOpenCodeLegacyReaderCancellationReturnsNoPartialPage(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	clock := &cancelCatalogClock{}
	options, err := ingest.NewOpenCodeSQLiteSourceOptions(time.Millisecond, time.Second, clock)
	if err != nil {
		t.Fatalf("create canceling legacy-reader options: %v", err)
	}
	source := openSyntheticSource(t, materialized, options)
	page, err := source.LegacyMessages(context.Background(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_reader_a"), PageSize: mustLegacyPageSize(t, 2)})
	if !errors.Is(err, context.DeadlineExceeded) || len(page.Messages) != 0 || page.Next != nil {
		t.Fatalf("canceled message page = %+v error=%v, want zero page, no cursor, and deadline cause", page, err)
	}
	closeSyntheticSource(t, source)
}

func TestOpenCodeLegacyReaderReportsSchemaAndRowDecodeFailuresActionably(t *testing.T) {
	current := testfixture.MaterializeByName(t, "current-session-message")
	currentSource := openSyntheticSource(t, current, ingest.DefaultOpenCodeSQLiteSourceOptions())
	_, err := currentSource.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_current_1"), PageSize: mustLegacyPageSize(t, 1)})
	if err == nil || !strings.Contains(err.Error(), "supported legacy message/part schema") || !strings.Contains(err.Error(), "no partial page") {
		t.Fatalf("schema mismatch error = %v, want actionable legacy-schema diagnostic", err)
	}
	closeSyntheticSource(t, currentSource)

	malformed := testfixture.MaterializeByName(t, "legacy-reader-pages")
	writer, err := sqlite.OpenConn(malformed.Path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic row-decode control: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(writer, "UPDATE message SET data = 'not-json' WHERE id = 'msg_tie_a'", nil)
	closeErr := writer.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("prepare synthetic row-decode control: %v", errors.Join(updateErr, closeErr))
	}
	malformedSource := openSyntheticSource(t, malformed, ingest.DefaultOpenCodeSQLiteSourceOptions())
	page, err := malformedSource.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_reader_a"), PageSize: mustLegacyPageSize(t, 2)})
	if err == nil || len(page.Messages) != 0 || page.Next != nil || !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), "no partial page") {
		t.Fatalf("row-decode failure page = %+v error=%v, want zero page and actionable decode diagnostic", page, err)
	}
	closeSyntheticSource(t, malformedSource)
}

func TestOpenCodeLegacyReaderReadsCommittedWALRowsWithoutChangingTransactionContent(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-wal")
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic legacy WAL writer")
	if err := appendLegacyWALRows(writer); err != nil {
		t.Fatalf("append synthetic legacy WAL-only rows: %v", err)
	}
	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	sessionID := mustLegacySessionID(t, "ses_wal_legacy")
	messages, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: mustLegacyPageSize(t, 4)})
	if err != nil {
		t.Fatalf("read committed WAL-resident legacy messages: %v", err)
	}
	if !equalStrings(legacyMessageStrings(messages.Messages), []string{"msg_wal_base", "msg_wal_only"}) {
		t.Fatalf("WAL-aware message IDs = %v, want base and WAL-only rows", legacyMessageStrings(messages.Messages))
	}
	parts, err := source.LegacyParts(t.Context(), ingest.OpenCodeLegacyPartPageRequest{SessionID: sessionID, MessageID: mustLegacyMessageID(t, "msg_wal_only"), PageSize: mustLegacyPageSize(t, 2)})
	if err != nil || !equalStrings(legacyPartStrings(parts.Parts), []string{"part_wal_only"}) {
		t.Fatalf("WAL-aware part page = %+v error=%v, want WAL-only part", parts, err)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestOpenCodeLegacyReaderNeverUsesEnvironmentOrExternalOutputFiles(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	externalRoot := t.TempDir()
	externalDatabase := filepath.Join(externalRoot, "must-not-open.db")
	externalOutput := filepath.Join(externalRoot, "full-output.txt")
	databaseSentinel := []byte("not a SQLite source")
	outputSentinel := []byte("external full output must remain untouched")
	if err := os.WriteFile(externalDatabase, databaseSentinel, 0o600); err != nil {
		t.Fatalf("write external database sentinel: %v", err)
	}
	if err := os.WriteFile(externalOutput, outputSentinel, 0o600); err != nil {
		t.Fatalf("write external output sentinel: %v", err)
	}
	t.Setenv("HOME", externalRoot)
	t.Setenv(defaults.EnvXDGDataHome.String(), externalRoot)
	t.Setenv("OPENCODE_DB", externalDatabase)
	t.Setenv("OPENCODE_FULL_OUTPUT", externalOutput)

	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	_, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: mustLegacySessionID(t, "ses_reader_a"), PageSize: mustLegacyPageSize(t, 1)})
	if err != nil {
		t.Fatalf("read explicitly selected synthetic source with hostile environment defaults: %v", err)
	}
	closeSyntheticSource(t, source)
	if got, err := os.ReadFile(externalDatabase); err != nil || !bytes.Equal(got, databaseSentinel) {
		t.Fatalf("external database sentinel changed or became unreadable: bytes=%q error=%v", got, err)
	}
	if got, err := os.ReadFile(externalOutput); err != nil || !bytes.Equal(got, outputSentinel) {
		t.Fatalf("external output sentinel changed or became unreadable: bytes=%q error=%v", got, err)
	}
}

func appendLegacyWALRows(writer *sqlite.Conn) (err error) {
	endTransaction, err := sqlitex.ImmediateTransaction(writer)
	if err != nil {
		return err
	}
	defer endTransaction(&err)
	if err = sqlitex.ExecuteTransient(writer, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{"msg_wal_only", "ses_wal_legacy", int64(8002), int64(8002), `{"role":"assistant","marker":"wal-only"}`}}); err != nil {
		return err
	}
	return sqlitex.ExecuteTransient(writer, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?1, ?2, ?3, ?4, ?5, ?6)`, &sqlitex.ExecOptions{Args: []any{"part_wal_only", "msg_wal_only", "ses_wal_legacy", int64(8003), int64(8003), `{"type":"text","text":"wal-only"}`}})
}

func mustLegacyPageSize(t testing.TB, value int) ingest.OpenCodeLegacyPageSize {
	t.Helper()
	size, err := ingest.NewOpenCodeLegacyPageSize(value)
	if err != nil {
		t.Fatalf("construct synthetic legacy page size: %v", err)
	}
	return size
}

func mustLegacySessionID(t testing.TB, value string) ingest.OpenCodeLegacySessionID {
	t.Helper()
	id, err := ingest.NewOpenCodeLegacySessionID(value)
	if err != nil {
		t.Fatalf("construct synthetic legacy session identifier: %v", err)
	}
	return id
}

func mustLegacyMessageID(t testing.TB, value string) ingest.OpenCodeLegacyMessageID {
	t.Helper()
	id, err := ingest.NewOpenCodeLegacyMessageID(value)
	if err != nil {
		t.Fatalf("construct synthetic legacy message identifier: %v", err)
	}
	return id
}

func mustLegacyPartID(t testing.TB, value string) ingest.OpenCodeLegacyPartID {
	t.Helper()
	id, err := ingest.NewOpenCodeLegacyPartID(value)
	if err != nil {
		t.Fatalf("construct synthetic legacy part identifier: %v", err)
	}
	return id
}

func legacySessionStrings(ids []ingest.OpenCodeLegacySessionID) []string {
	result := make([]string, len(ids))
	for index, id := range ids {
		result[index] = id.String()
	}
	return result
}

func legacyMessageStrings(rows []ingest.OpenCodeLegacyMessageRow) []string {
	result := make([]string, len(rows))
	for index, row := range rows {
		result[index] = row.ID.String()
	}
	return result
}

func legacyPartStrings(rows []ingest.OpenCodeLegacyPartRow) []string {
	result := make([]string, len(rows))
	for index, row := range rows {
		result[index] = row.ID.String()
	}
	return result
}
