package testfixture

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const expectedLoaderValidationCases = 13

//go:embed testdata/loader_validation.yaml
var loaderValidationYAML []byte

type loaderValidationCorpus struct {
	DeclaredCases int                    `yaml:"declared_cases"`
	Cases         []loaderValidationCase `yaml:"cases"`
}

type loaderValidationCase struct {
	Name          string         `yaml:"name"`
	Mutation      loaderMutation `yaml:"mutation"`
	ErrorContains string         `yaml:"error_contains"`
}

type loaderMutation string

const (
	mutationUnknownField     loaderMutation = "unknown_field"
	mutationTrailingDoc      loaderMutation = "trailing_document"
	mutationDeclaredCount    loaderMutation = "declared_count"
	mutationDuplicateName    loaderMutation = "duplicate_name"
	mutationMissingName      loaderMutation = "missing_name"
	mutationAbsolutePath     loaderMutation = "absolute_path"
	mutationTraversingPath   loaderMutation = "traversing_path"
	mutationDeclaredRows     loaderMutation = "declared_rows"
	mutationMissingRowField  loaderMutation = "missing_row_field"
	mutationHistoryRows      loaderMutation = "history_rows"
	mutationDuplicateHistory loaderMutation = "duplicate_history"
	mutationUnknownHistory   loaderMutation = "unknown_history"
	mutationPartialDuplicate loaderMutation = "partial_duplicate"
)

func TestEmbeddedCorpusIsStrictAndNonVacuous(t *testing.T) {
	fixtures, err := loadCorpus(fixtureYAML)
	if err != nil {
		t.Fatalf("load embedded fixture corpus: %v", err)
	}
	if fixtures.DeclaredCases != expectedCaseCount || len(fixtures.Cases) != expectedCaseCount {
		t.Fatalf("embedded fixture count = declared %d actual %d, want %d", fixtures.DeclaredCases, len(fixtures.Cases), expectedCaseCount)
	}

	seenSchemas := make(map[schemaKind]bool)
	seenFormats := make(map[sourceFormat]bool, 2)
	seenWAL := false
	for _, fixtureCase := range fixtures.Cases {
		seenSchemas[fixtureCase.Schema] = true
		seenFormats[fixtureCase.Format] = true
		seenWAL = seenWAL || fixtureCase.JournalMode == journalWAL
	}
	assertSchemaCovered(t, seenSchemas, schemaEmpty)
	assertSchemaCovered(t, seenSchemas, schemaLegacy)
	assertSchemaCovered(t, seenSchemas, schemaCurrent)
	assertSchemaCovered(t, seenSchemas, schemaHybrid)
	assertSchemaCovered(t, seenSchemas, schemaCurrentMissingSeq)
	assertSchemaCovered(t, seenSchemas, schemaCurrentNullableSeq)
	assertSchemaCovered(t, seenSchemas, schemaCurrentPartialSeq)
	assertSchemaCovered(t, seenSchemas, schemaUnsupported)
	if !seenFormats[sourceFormatSQLite] || !seenFormats[sourceFormatCorrupt] || !seenWAL {
		t.Errorf("embedded fixture corpus coverage = formats %v WAL %t; want SQLite, corrupt, and WAL", seenFormats, seenWAL)
	}
}

func TestLoaderRejectsInvalidCorpusMutations(t *testing.T) {
	for _, validationCase := range loadValidationCases(t) {
		validationCase := validationCase
		t.Run(validationCase.Name, func(t *testing.T) {
			mutated, err := applyMutation(fixtureYAML, validationCase.Mutation)
			if err != nil {
				t.Fatalf("apply fixture mutation %q: %v", validationCase.Mutation, err)
			}
			_, err = loadCorpus(mutated)
			if err == nil {
				t.Fatalf("load mutated fixture corpus: expected error containing %q", validationCase.ErrorContains)
			}
			if !strings.Contains(err.Error(), validationCase.ErrorContains) {
				t.Fatalf("load mutated fixture corpus error = %q, want substring %q", err, validationCase.ErrorContains)
			}
		})
	}
}

func TestMaterializeExpectedCatalogs(t *testing.T) {
	fixtures, err := loadCorpus(fixtureYAML)
	if err != nil {
		t.Fatalf("load embedded fixture corpus: %v", err)
	}
	for _, fixtureCase := range fixtures.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			source := materialize(t, fixtureCase)
			if fixtureCase.Format == sourceFormatCorrupt {
				data, readErr := os.ReadFile(source.Path)
				if readErr != nil {
					t.Fatalf("read corrupt fixture: %v", readErr)
				}
				if !bytes.Equal(data, corruptBytes(fixtureCase.Corruption)) {
					t.Fatalf("corrupt fixture bytes = %q, want fixed %q evidence", data, fixtureCase.Corruption)
				}
				return
			}

			catalog := readCatalog(t, source.Path)
			assertCatalog(t, source.ExpectedCatalog(), catalog)
			assertRows(t, fixtureCase, source.Path)
		})
	}
}

func TestMaterializeIsConfinedAndIgnoresEnvironmentDefaults(t *testing.T) {
	externalRoot := t.TempDir()
	externalPath := filepath.Join(externalRoot, "must-not-open.db")
	want := []byte("external sentinel")
	if err := os.WriteFile(externalPath, want, 0o600); err != nil {
		t.Fatalf("write external sentinel: %v", err)
	}
	t.Setenv("HOME", externalRoot)
	t.Setenv(defaults.EnvXDGDataHome.String(), externalRoot)
	t.Setenv("OPENCODE_DB", externalPath)

	source := MaterializeByName(t, "empty-valid")
	if source.Path == externalPath || !pathIsWithin(source.root, source.Path) {
		t.Fatalf("materialized path %q is not confined to helper root %q", source.Path, source.root)
	}
	got, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatalf("read external sentinel after materialization: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("external sentinel changed from %q to %q", want, got)
	}

	if _, err := snapshotSource(MaterializedSource{Path: externalPath, root: source.root}); err == nil {
		t.Fatal("snapshotSource accepted an external path")
	}
	if _, err := snapshotSource(MaterializedSource{Path: externalPath}); err == nil {
		t.Fatal("snapshotSource accepted a source without a materializer-owned root")
	}
	if err := os.Symlink(externalPath, source.Path+"-wal"); err != nil {
		t.Fatalf("create external sidecar symlink: %v", err)
	}
	if _, err := snapshotSource(source); err == nil {
		t.Fatal("snapshotSource followed a sidecar symlink outside the materializer-owned root")
	}
}

func TestSnapshotCoversDatabaseAndSidecars(t *testing.T) {
	source := MaterializeByName(t, "empty-valid")
	if err := os.WriteFile(source.Path+"-wal", []byte("synthetic WAL snapshot sentinel"), 0o600); err != nil {
		t.Fatalf("write synthetic WAL sidecar sentinel: %v", err)
	}
	if err := os.WriteFile(source.Path+"-shm", []byte("synthetic SHM snapshot sentinel"), 0o600); err != nil {
		t.Fatalf("write synthetic SHM sidecar sentinel: %v", err)
	}

	before := SnapshotSource(t, source)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if !before.files[suffix].present {
			t.Errorf("snapshot does not contain present %s", snapshotLabel(suffix))
		}
		if suffix != "" && len(before.files[suffix].data) == 0 {
			t.Errorf("snapshot does not contain nonempty %s", snapshotLabel(suffix))
		}
	}
	AssertUnchanged(t, source, before)

	if err := os.WriteFile(source.Path+"-wal", []byte("changed"), 0o600); err != nil {
		t.Fatalf("change synthetic WAL sidecar sentinel: %v", err)
	}
	after, err := snapshotSource(source)
	if err != nil {
		t.Fatalf("snapshot changed source: %v", err)
	}
	if bytes.Equal(before.files["-wal"].data, after.files["-wal"].data) {
		t.Fatal("WAL snapshot comparison is vacuous")
	}
}

func TestWALCapableSourcePersistsJournalMode(t *testing.T) {
	source := MaterializeByName(t, "wal-capable")

	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open WAL-capable source read-only: %v", err)
	}
	var journalMode string
	err = sqlitex.Execute(conn, "PRAGMA journal_mode;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			journalMode = stmt.ColumnText(0)
			return nil
		},
	})
	closeErr := conn.Close()
	if err != nil {
		t.Fatalf("read WAL-capable source journal mode: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close WAL-capable source reader: %v", closeErr)
	}
	if journalMode != string(journalWAL) {
		t.Fatalf("journal mode = %q, want %q", journalMode, journalWAL)
	}
}

func TestHistoryFixtureSeparatesLatestMaterializedRowsFromIgnoredHistory(t *testing.T) {
	source := MaterializeByName(t, "repeated-history-distractors")
	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open repeated-history synthetic source: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close repeated-history synthetic source: %v", err)
		}
	}()

	messageData := querySingleText(t, conn, `SELECT data FROM message WHERE id = 'msg_latest';`)
	if messageData != `{"role":"assistant","version":"latest"}` {
		t.Errorf("latest materialized message data = %q", messageData)
	}
	partData := querySingleText(t, conn, `SELECT data FROM part WHERE id = 'part_latest';`)
	if partData != `{"type":"text","text":"latest materialized part"}` {
		t.Errorf("latest materialized part data = %q", partData)
	}
	assertRepeatedStableIdentity(t, conn, historyEvent, "msg_latest", 2)
	assertRepeatedStableIdentity(t, conn, historyDelta, "part_latest", 2)
}

func TestMissingParentFixtureMaterializesOrphanWithoutInventingParent(t *testing.T) {
	source := MaterializeByName(t, "missing-parent")
	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open missing-parent synthetic source: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close missing-parent synthetic source: %v", err)
		}
	}()

	var orphanCount int
	if err := sqlitex.Execute(conn, `SELECT count(*) FROM part LEFT JOIN message ON message.id = part.message_id WHERE message.id IS NULL;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			orphanCount = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("inspect synthetic orphan relationship: %v", err)
	}
	if orphanCount != 1 {
		t.Errorf("synthetic orphan count = %d, want 1", orphanCount)
	}
}

func TestTruncatedSQLiteHeaderFixtureIsHeaderLikeButMalformed(t *testing.T) {
	source := MaterializeByName(t, "truncated-sqlite-header")
	data, err := os.ReadFile(source.Path)
	if err != nil {
		t.Fatalf("read truncated SQLite-header fixture: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) || len(data) >= 100 {
		t.Fatalf("truncated SQLite-header bytes = %q, want a valid header prefix shorter than one SQLite header", data)
	}
	conn, err := sqlite.OpenConn(source.Path, sqlite.OpenReadOnly)
	if err == nil {
		queryErr := sqlitex.Execute(conn, `SELECT name FROM sqlite_master;`, nil)
		closeErr := conn.Close()
		if queryErr == nil {
			t.Fatal("truncated SQLite-header fixture unexpectedly allowed a catalog read")
		}
		if closeErr != nil {
			t.Fatalf("close truncated SQLite-header fixture after rejected read: %v", closeErr)
		}
	}
}

func querySingleText(t testing.TB, conn *sqlite.Conn, query string) string {
	t.Helper()
	var value string
	rows := 0
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows++
			value = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query fixed synthetic fixture content: %v", err)
	}
	if rows != 1 {
		t.Fatalf("fixed synthetic fixture query returned %d rows, want 1", rows)
	}
	return value
}

func assertRepeatedStableIdentity(t testing.TB, conn *sqlite.Conn, kind historyKind, stableID string, want int) {
	t.Helper()
	var count int
	table := historyTableName(kind)
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE stable_id = ?1;", table)
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: []any{stableID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count repeated stable identity %q in synthetic %s history: %v", stableID, table, err)
	}
	if count != want {
		t.Errorf("synthetic %s history count for stable identity %q = %d, want %d", table, stableID, count, want)
	}
}

func assertSchemaCovered(t testing.TB, seen map[schemaKind]bool, schema schemaKind) {
	t.Helper()
	if !seen[schema] {
		t.Errorf("embedded fixture corpus does not cover schema %q", schema)
	}
}

type catalogEntry struct {
	Name string
	Type string
}

type catalogSnapshot struct {
	Entries            []catalogEntry
	SessionMessageCols map[string]bool
}

func readCatalog(t testing.TB, databasePath string) catalogSnapshot {
	t.Helper()
	conn, err := sqlite.OpenConn(databasePath, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open synthetic catalog: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close synthetic catalog: %v", err)
		}
	}()

	result := catalogSnapshot{SessionMessageCols: make(map[string]bool)}
	if err := sqlitex.Execute(conn, `SELECT type, name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%' ORDER BY type, name;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result.Entries = append(result.Entries, catalogEntry{Type: stmt.ColumnText(0), Name: stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		t.Fatalf("read synthetic catalog entries: %v", err)
	}
	if err := sqlitex.Execute(conn, `SELECT name, "notnull" FROM pragma_table_info('session_message') ORDER BY cid;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result.SessionMessageCols[stmt.ColumnText(0)] = stmt.ColumnInt64(1) == 1
			return nil
		},
	}); err != nil {
		t.Fatalf("read synthetic session_message columns: %v", err)
	}
	return result
}

func assertCatalog(t testing.TB, expected CatalogExpectation, catalog catalogSnapshot) {
	t.Helper()
	gotTables := make([]string, 0, len(catalog.Entries))
	gotIndexes := make([]string, 0, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		switch entry.Type {
		case "table":
			gotTables = append(gotTables, entry.Name)
		case "index":
			gotIndexes = append(gotIndexes, entry.Name)
		default:
			t.Errorf("catalog contains unexpected object type %q for %q", entry.Type, entry.Name)
		}
	}
	wantTables := append([]string(nil), expected.tables...)
	wantIndexes := append([]string(nil), expected.indexes...)
	sort.Strings(gotTables)
	sort.Strings(gotIndexes)
	sort.Strings(wantTables)
	sort.Strings(wantIndexes)
	if !equalStrings(gotTables, wantTables) {
		t.Errorf("catalog tables = %v, want %v", gotTables, wantTables)
	}
	if !equalStrings(gotIndexes, wantIndexes) {
		t.Errorf("catalog indexes = %v, want %v", gotIndexes, wantIndexes)
	}
	notNull, exists := catalog.SessionMessageCols["seq"]
	switch expected.seq {
	case seqAbsent:
		if exists {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want absent", exists, notNull)
		}
	case seqNotNull:
		if !exists || !notNull {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want both true", exists, notNull)
		}
	case seqNullable:
		if !exists || notNull {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want present and nullable", exists, notNull)
		}
	}
}

func assertRows(t testing.TB, fixtureCase caseSpec, databasePath string) {
	t.Helper()
	conn, err := sqlite.OpenConn(databasePath, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open synthetic rows: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close synthetic rows: %v", err)
		}
	}()

	if schemaHasTable(fixtureCase.Schema, "message") {
		assertTableRowCount(t, conn, "message", len(fixtureCase.LegacyMessages))
	}
	if schemaHasTable(fixtureCase.Schema, "part") {
		assertTableRowCount(t, conn, "part", len(fixtureCase.LegacyParts))
	}
	if schemaHasTable(fixtureCase.Schema, "session_message") {
		assertTableRowCount(t, conn, "session_message", len(fixtureCase.CurrentMessages))
	}
	assertHistoryRows(t, conn, fixtureCase)
}

func assertHistoryRows(t testing.TB, conn *sqlite.Conn, fixtureCase caseSpec) {
	t.Helper()
	if hasHistoryKind(fixtureCase.IgnoredHistory, historyEvent) {
		assertTableRowCount(t, conn, "event", countHistoryKind(fixtureCase.IgnoredHistory, historyEvent))
	}
	if hasHistoryKind(fixtureCase.IgnoredHistory, historyDelta) {
		assertTableRowCount(t, conn, "delta", countHistoryKind(fixtureCase.IgnoredHistory, historyDelta))
	}
	if hasHistoryKind(fixtureCase.IgnoredHistory, historyInput) {
		assertTableRowCount(t, conn, "input", countHistoryKind(fixtureCase.IgnoredHistory, historyInput))
	}
	if hasHistoryKind(fixtureCase.IgnoredHistory, historyContext) {
		assertTableRowCount(t, conn, "context", countHistoryKind(fixtureCase.IgnoredHistory, historyContext))
	}
	if hasHistoryKind(fixtureCase.IgnoredHistory, historyMigration) {
		assertTableRowCount(t, conn, "migration", countHistoryKind(fixtureCase.IgnoredHistory, historyMigration))
	}
}

func countHistoryKind(rows []historyRow, kind historyKind) int {
	count := 0
	for _, row := range rows {
		if row.Kind == kind {
			count++
		}
	}
	return count
}

func assertTableRowCount(t testing.TB, conn *sqlite.Conn, table string, want int) {
	t.Helper()
	var got int
	query := fmt.Sprintf("SELECT count(*) FROM %s;", table)
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		got = stmt.ColumnInt(0)
		return nil
	}}); err != nil {
		t.Fatalf("count explicit fixture table %q: %v", table, err)
	}
	if got != want {
		t.Errorf("table %s row count = %d, want %d", table, got, want)
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

func schemaHasTable(schema schemaKind, table string) bool {
	switch table {
	case "message", "part":
		return schema == schemaLegacy || schema == schemaHybrid
	case "session_message":
		return schema == schemaCurrent || schema == schemaHybrid || schema == schemaCurrentMissingSeq || schema == schemaCurrentNullableSeq || schema == schemaCurrentPartialSeq
	default:
		return false
	}
}

func loadValidationCases(t testing.TB) []loaderValidationCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(loaderValidationYAML))
	decoder.KnownFields(true)
	var fixtures loaderValidationCorpus
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode loader validation fixtures: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode loader validation fixtures: expected exactly one YAML document: %v", err)
	}
	if fixtures.DeclaredCases != expectedLoaderValidationCases || len(fixtures.Cases) != expectedLoaderValidationCases {
		t.Fatalf("loader validation fixture row guard: declared=%d actual=%d required=%d", fixtures.DeclaredCases, len(fixtures.Cases), expectedLoaderValidationCases)
	}
	names := make(map[string]struct{}, len(fixtures.Cases))
	for _, validationCase := range fixtures.Cases {
		if strings.TrimSpace(validationCase.Name) == "" || strings.TrimSpace(validationCase.ErrorContains) == "" {
			t.Fatalf("loader validation fixture has missing required fields: %+v", validationCase)
		}
		if _, duplicate := names[validationCase.Name]; duplicate {
			t.Fatalf("loader validation fixture has duplicate name %q", validationCase.Name)
		}
		names[validationCase.Name] = struct{}{}
		if !knownMutation(validationCase.Mutation) {
			t.Fatalf("loader validation fixture %q has unknown mutation %q", validationCase.Name, validationCase.Mutation)
		}
	}
	return fixtures.Cases
}

func knownMutation(mutation loaderMutation) bool {
	switch mutation {
	case mutationUnknownField, mutationTrailingDoc, mutationDeclaredCount, mutationDuplicateName, mutationMissingName, mutationAbsolutePath, mutationTraversingPath, mutationDeclaredRows, mutationMissingRowField, mutationHistoryRows, mutationDuplicateHistory, mutationUnknownHistory, mutationPartialDuplicate:
		return true
	default:
		return false
	}
}

func applyMutation(source []byte, mutation loaderMutation) ([]byte, error) {
	replaceOnce := func(old, replacement string) ([]byte, error) {
		if !bytes.Contains(source, []byte(old)) {
			return nil, fmt.Errorf("mutation anchor %q is absent", old)
		}
		return bytes.Replace(source, []byte(old), []byte(replacement), 1), nil
	}
	switch mutation {
	case mutationUnknownField:
		return append(append([]byte(nil), source...), []byte("unexpected: true\n")...), nil
	case mutationTrailingDoc:
		return append(append([]byte(nil), source...), []byte("---\ndeclared_cases: 0\ncases: []\n")...), nil
	case mutationDeclaredCount:
		return replaceOnce("declared_cases: 36", "declared_cases: 31")
	case mutationDuplicateName:
		return replaceOnce("name: legacy-message-part", "name: empty-valid")
	case mutationMissingName:
		return replaceOnce("name: empty-valid", "name: ''")
	case mutationAbsolutePath:
		return replaceOnce("logical_path: stable/opencode.db", "logical_path: /tmp/opencode.db")
	case mutationTraversingPath:
		return replaceOnce("logical_path: stable/opencode.db", "logical_path: ../opencode.db")
	case mutationDeclaredRows:
		return replaceOnce("legacy_messages: 1\n      legacy_parts: 2", "legacy_messages: 2\n      legacy_parts: 2")
	case mutationMissingRowField:
		return replaceOnce("session_id: ses_legacy_1", "session_id: ''")
	case mutationHistoryRows:
		return replaceOnce("ignored_history: 7", "ignored_history: 6")
	case mutationDuplicateHistory:
		return replaceOnce("id: event_2", "id: event_1")
	case mutationUnknownHistory:
		return replaceOnce("kind: migration", "kind: replay")
	case mutationPartialDuplicate:
		mutated, err := replaceOnce(
			"name: current-partial-ordering-index\n    logical_path: current/partial-ordering.db\n    format: sqlite\n    schema: current_partial_seq\n    journal_mode: delete\n    corruption: \"\"\n    declared_rows: {legacy_messages: 0, legacy_parts: 0, current_messages: 3, ignored_history: 0}",
			"name: current-partial-ordering-index\n    logical_path: current/partial-ordering.db\n    format: sqlite\n    schema: current_partial_seq\n    journal_mode: delete\n    corruption: \"\"\n    declared_rows: {legacy_messages: 0, legacy_parts: 0, current_messages: 2, ignored_history: 0}",
		)
		if err != nil {
			return nil, err
		}
		duplicate := "      - {id: sm_partial_b, session_id: ses_partial, type: part, time_created: 9401, time_updated: 9401, data: '{\"marker\":\"duplicate-b\"}', seq: 1}\n"
		if !bytes.Contains(mutated, []byte(duplicate)) {
			return nil, fmt.Errorf("mutation anchor for the partial-index duplicate row is absent")
		}
		return bytes.Replace(mutated, []byte(duplicate), nil, 1), nil
	default:
		return nil, fmt.Errorf("unknown fixture mutation %q", mutation)
	}
}

func pathIsWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
