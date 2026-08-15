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

const expectedLoaderValidationCases = 9

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
	mutationUnknownField    loaderMutation = "unknown_field"
	mutationTrailingDoc     loaderMutation = "trailing_document"
	mutationDeclaredCount   loaderMutation = "declared_count"
	mutationDuplicateName   loaderMutation = "duplicate_name"
	mutationMissingName     loaderMutation = "missing_name"
	mutationAbsolutePath    loaderMutation = "absolute_path"
	mutationTraversingPath  loaderMutation = "traversing_path"
	mutationDeclaredRows    loaderMutation = "declared_rows"
	mutationMissingRowField loaderMutation = "missing_row_field"
)

func TestEmbeddedCorpusIsStrictAndNonVacuous(t *testing.T) {
	fixtures, err := loadCorpus(fixtureYAML)
	if err != nil {
		t.Fatalf("load embedded fixture corpus: %v", err)
	}
	if fixtures.DeclaredCases != expectedCaseCount || len(fixtures.Cases) != expectedCaseCount {
		t.Fatalf("embedded fixture count = declared %d actual %d, want %d", fixtures.DeclaredCases, len(fixtures.Cases), expectedCaseCount)
	}

	seenSchemas := make(map[SchemaKind]bool)
	seenFormats := make(map[SourceFormat]bool, 2)
	seenWAL := false
	for _, fixtureCase := range fixtures.Cases {
		seenSchemas[fixtureCase.Schema] = true
		seenFormats[fixtureCase.Format] = true
		seenWAL = seenWAL || fixtureCase.JournalMode == JournalWAL
	}
	assertSchemaCovered(t, seenSchemas, SchemaEmpty)
	assertSchemaCovered(t, seenSchemas, SchemaLegacy)
	assertSchemaCovered(t, seenSchemas, SchemaCurrent)
	assertSchemaCovered(t, seenSchemas, SchemaHybrid)
	assertSchemaCovered(t, seenSchemas, SchemaCurrentMissingSeq)
	assertSchemaCovered(t, seenSchemas, SchemaCurrentNullableSeq)
	assertSchemaCovered(t, seenSchemas, SchemaUnsupported)
	if !seenFormats[SourceFormatSQLite] || !seenFormats[SourceFormatCorrupt] || !seenWAL {
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
			source := Materialize(t, fixtureCase)
			if fixtureCase.Format == SourceFormatCorrupt {
				data, readErr := os.ReadFile(source.Path)
				if readErr != nil {
					t.Fatalf("read corrupt fixture: %v", readErr)
				}
				if string(data) != fixtureCase.CorruptContent {
					t.Fatalf("corrupt fixture bytes = %q, want %q", data, fixtureCase.CorruptContent)
				}
				return
			}

			catalog := readCatalog(t, source.Path)
			assertCatalog(t, fixtureCase.ExpectedCatalog, catalog)
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

	source := Materialize(t, CaseByName(t, "empty-valid"))
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
	source := Materialize(t, CaseByName(t, "empty-valid"))
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
	source := Materialize(t, CaseByName(t, "wal-capable"))

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
	if journalMode != string(JournalWAL) {
		t.Fatalf("journal mode = %q, want %q", journalMode, JournalWAL)
	}
}

func assertSchemaCovered(t testing.TB, seen map[SchemaKind]bool, schema SchemaKind) {
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

func assertCatalog(t testing.TB, expected ExpectedCatalog, catalog catalogSnapshot) {
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
	wantTables := append([]string(nil), expected.Tables...)
	wantIndexes := append([]string(nil), expected.Indexes...)
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
	switch expected.Seq {
	case SeqAbsent:
		if exists {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want absent", exists, notNull)
		}
	case SeqNotNull:
		if !exists || !notNull {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want both true", exists, notNull)
		}
	case SeqNullable:
		if !exists || notNull {
			t.Errorf("session_message seq metadata = exists %t not-null %t, want present and nullable", exists, notNull)
		}
	}
}

func assertRows(t testing.TB, fixtureCase Case, databasePath string) {
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

func schemaHasTable(schema SchemaKind, table string) bool {
	switch table {
	case "message", "part":
		return schema == SchemaLegacy || schema == SchemaHybrid
	case "session_message":
		return schema == SchemaCurrent || schema == SchemaHybrid || schema == SchemaCurrentMissingSeq || schema == SchemaCurrentNullableSeq
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
	case mutationUnknownField, mutationTrailingDoc, mutationDeclaredCount, mutationDuplicateName, mutationMissingName, mutationAbsolutePath, mutationTraversingPath, mutationDeclaredRows, mutationMissingRowField:
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
		return replaceOnce("declared_cases: 9", "declared_cases: 8")
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
	default:
		return nil, fmt.Errorf("unknown fixture mutation %q", mutation)
	}
}

func pathIsWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
