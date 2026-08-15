package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedCurrentPageCases       = 1
	expectedCurrentMalformedCases  = 8
	expectedCurrentValidationCases = 4
	expectedCurrentQueryPlanCases  = 2
	expectedCurrentPagingHazards   = 1
	expectedCurrentLoaderMutations = 4
)

//go:embed testdata/opencode_sqlite_current_reader.yaml
var currentReaderYAML []byte

type currentMalformedMutation string

const (
	currentMutationIDType      currentMalformedMutation = "id_type"
	currentMutationMessageType currentMalformedMutation = "message_type"
	currentMutationCreatedType currentMalformedMutation = "created_type"
	currentMutationUpdatedType currentMalformedMutation = "updated_type"
	currentMutationDataType    currentMalformedMutation = "data_type"
	currentMutationSeqType     currentMalformedMutation = "seq_type"
	currentMutationNegativeSeq currentMalformedMutation = "negative_seq"
	currentMutationInvalidJSON currentMalformedMutation = "invalid_json"
)

type currentValidationKind string

const (
	currentValidationPageSize  currentValidationKind = "page_size"
	currentValidationSessionID currentValidationKind = "session_id"
	currentValidationSeq       currentValidationKind = "seq"
)

type currentLoaderMutation string

const (
	currentLoaderUnknownField currentLoaderMutation = "unknown_field"
	currentLoaderTrailingDoc  currentLoaderMutation = "trailing_document"
	currentLoaderDeclared     currentLoaderMutation = "declared_count"
	currentLoaderDuplicate    currentLoaderMutation = "duplicate_name"
)

type currentReaderFixture struct {
	DeclaredPageCases       int                     `yaml:"declared_page_cases"`
	PageCases               []currentPageCase       `yaml:"page_cases"`
	DeclaredMalformedCases  int                     `yaml:"declared_malformed_cases"`
	MalformedCases          []currentMalformedCase  `yaml:"malformed_cases"`
	DeclaredValidationCases int                     `yaml:"declared_validation_cases"`
	ValidationCases         []currentValidationCase `yaml:"validation_cases"`
	DeclaredQueryPlanCases  int                     `yaml:"declared_query_plan_cases"`
	QueryPlanCases          []currentQueryPlanCase  `yaml:"query_plan_cases"`
	DeclaredPagingHazards   int                     `yaml:"declared_pagination_hazards"`
	PagingHazards           []currentPagingHazard   `yaml:"pagination_hazards"`
	DeclaredLoaderMutations int                     `yaml:"declared_loader_mutations"`
	LoaderMutations         []currentLoaderCase     `yaml:"loader_mutations"`
}

type currentPageCase struct {
	Name      string                `yaml:"name"`
	Fixture   string                `yaml:"fixture"`
	SessionID string                `yaml:"session_id"`
	PageSize  int                   `yaml:"page_size"`
	Pages     []currentExpectedPage `yaml:"pages"`
}

type currentExpectedPage struct {
	IDs     []string `yaml:"ids"`
	Seqs    []int64  `yaml:"seqs"`
	HasNext bool     `yaml:"has_next"`
}

type currentMalformedCase struct {
	Name          string                   `yaml:"name"`
	Mutation      currentMalformedMutation `yaml:"mutation"`
	ErrorContains string                   `yaml:"error_contains"`
}

type currentValidationCase struct {
	Name          string                `yaml:"name"`
	Kind          currentValidationKind `yaml:"kind"`
	Value         int64                 `yaml:"value"`
	Text          string                `yaml:"text"`
	ErrorContains string                `yaml:"error_contains"`
}

type currentLoaderCase struct {
	Name          string                `yaml:"name"`
	Kind          currentLoaderMutation `yaml:"kind"`
	ErrorContains string                `yaml:"error_contains"`
}

type currentQueryPlanCase struct {
	Name              string   `yaml:"name"`
	Fixture           string   `yaml:"fixture"`
	SessionID         string   `yaml:"session_id"`
	AfterSeq          *int64   `yaml:"after_seq"`
	PageSize          int      `yaml:"page_size"`
	Statement         string   `yaml:"statement"`
	RequiredContains  string   `yaml:"required_contains"`
	ForbiddenContains []string `yaml:"forbidden_contains"`
}

type currentPagingHazard struct {
	Name        string `yaml:"name"`
	Fixture     string `yaml:"fixture"`
	SessionID   string `yaml:"session_id"`
	PageSize    int    `yaml:"page_size"`
	SourceRows  int    `yaml:"source_rows"`
	VisibleRows int    `yaml:"visible_rows"`
}

func TestOpenCodeCurrentMessagesMatchStrictPages(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.PageCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			before := testfixture.SnapshotSource(t, materialized)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			request := ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, fixtureCase.SessionID), PageSize: mustCurrentPageSize(t, fixtureCase.PageSize)}
			seen := make(map[string]struct{})
			for index, expected := range fixtureCase.Pages {
				requestCursor := request.After
				page, err := source.CurrentMessages(t.Context(), request)
				if err != nil {
					t.Fatalf("read strict current page %d: %v", index, err)
				}
				assertCurrentPage(t, fixtureCase, index, page, expected, seen)
				repeated, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{SessionID: request.SessionID, PageSize: request.PageSize, After: requestCursor})
				if err != nil || !equalCurrentPages(page, repeated) {
					t.Fatalf("repeat strict current page %d = %+v error=%v, want deterministic %+v", index, repeated, err, page)
				}
				request.After = page.Next
			}
			if request.After != nil {
				t.Fatal("strict current pages ended with a continuation cursor")
			}
			page, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, "ses_current_reader_missing"), PageSize: request.PageSize})
			if err != nil || len(page.Messages) != 0 || cap(page.Messages) != 0 || page.Next != nil {
				t.Fatalf("unknown current session page = %+v error=%v, want exact empty detached page", page, err)
			}
			closeSyntheticSource(t, source)
			testfixture.AssertUnchanged(t, materialized, before)
		})
	}
}

func TestOpenCodeCurrentMessagesAreDetached(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-reader-pages")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	request := currentReaderRequest(t, 2)
	page, err := source.CurrentMessages(t.Context(), request)
	if err != nil {
		t.Fatalf("read current page before detached mutation: %v", err)
	}
	page.Messages[0].Data = "mutated detached copy"
	repeated, err := source.CurrentMessages(t.Context(), request)
	if err != nil || repeated.Messages[0].Data == "mutated detached copy" {
		t.Fatalf("repeated current page was not detached: %+v error=%v", repeated, err)
	}
}

func TestOpenCodeCurrentMessagesRejectMalformedRowsAtomically(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.MalformedCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, "current-reader-pages")
			mutateCurrentRow(t, materialized.Path, fixtureCase.Mutation)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			page, err := source.CurrentMessages(t.Context(), currentReaderRequest(t, ingest.MaxOpenCodeCurrentPageSize))
			if err == nil || len(page.Messages) != 0 || page.Next != nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) || !strings.Contains(err.Error(), "no partial page") {
				t.Fatalf("malformed current page = %+v error=%v, want atomic actionable rejection containing %q", page, err, fixtureCase.ErrorContains)
			}
			other, reuseErr := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, "ses_current_reader_b"), PageSize: mustCurrentPageSize(t, 2)})
			if reuseErr != nil || len(other.Messages) != 2 {
				t.Fatalf("source after malformed current page was not reusable: %+v error=%v", other, reuseErr)
			}
			closeSyntheticSource(t, source)
		})
	}
}

func TestOpenCodeCurrentContractsRejectStrictValidationCases(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.ValidationCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			var err error
			switch fixtureCase.Kind {
			case currentValidationPageSize:
				_, err = ingest.NewOpenCodeCurrentPageSize(int(fixtureCase.Value))
			case currentValidationSessionID:
				_, err = ingest.NewOpenCodeCurrentSessionID(fixtureCase.Text)
			case currentValidationSeq:
				_, err = ingest.NewOpenCodeCurrentSeq(fixtureCase.Value)
			default:
				t.Fatalf("strict current validation case has unknown kind %q", fixtureCase.Kind)
			}
			if err == nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) {
				t.Fatalf("strict current validation error = %v, want substring %q", err, fixtureCase.ErrorContains)
			}
		})
	}

	materialized := testfixture.MaterializeByName(t, "current-reader-pages")
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	defer closeSyntheticSource(t, source)
	page, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{})
	if err == nil || len(page.Messages) != 0 || page.Next != nil {
		t.Fatalf("zero current request = %+v error=%v, want boundary rejection and zero page", page, err)
	}
}

func TestOpenCodeCurrentReaderFixtureLoaderRejectsStrictMutations(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.LoaderMutations {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			mutated, err := mutateCurrentReaderFixture(currentReaderYAML, fixtureCase.Kind)
			if err != nil {
				t.Fatalf("mutate strict current reader fixture: %v", err)
			}
			_, err = parseCurrentReaderFixture(mutated)
			if err == nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) {
				t.Fatalf("strict current fixture mutation error = %v, want substring %q", err, fixtureCase.ErrorContains)
			}
		})
	}
}

func TestOpenCodeCurrentMessagesReadCommittedWALRowsWithoutChangingTransactionContent(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "current-reader-wal")
	writer := openWALWriter(t, materialized.Path)
	defer closeSQLiteConnection(t, writer, "synthetic current WAL writer")
	if err := appendCurrentWALRow(writer); err != nil {
		t.Fatalf("append synthetic current WAL row: %v", err)
	}
	databaseBefore := readSyntheticFile(t, materialized.Path)
	walBefore := readWALState(t, materialized.Path)
	source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
	page, err := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, "ses_current_wal"), PageSize: mustCurrentPageSize(t, 4)})
	if err != nil || !equalStrings(currentMessageIDs(page.Messages), []string{"sm_current_wal_base", "sm_current_wal_only"}) {
		t.Fatalf("WAL-aware current page = %+v error=%v, want base and committed WAL rows", page, err)
	}
	closeSyntheticSource(t, source)
	assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database")
	assertWALStateEqual(t, materialized.Path, walBefore)
}

func TestSupportedCurrentQueryUsesFixturePinnedOrderingPlan(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.QueryPlanCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			conn, err := sqlite.OpenConn(materialized.Path, sqlite.OpenReadOnly)
			if err != nil {
				t.Fatalf("open synthetic source for query-plan proof: %v", err)
			}
			var details []string
			args := []any{fixtureCase.SessionID, fixtureCase.PageSize + 1}
			if fixtureCase.AfterSeq != nil {
				args = []any{fixtureCase.SessionID, *fixtureCase.AfterSeq, fixtureCase.PageSize + 1}
			}
			planErr := sqlitex.ExecuteTransient(conn, fixtureCase.Statement, &sqlitex.ExecOptions{
				Args: args,
				ResultFunc: func(stmt *sqlite.Stmt) error {
					details = append(details, stmt.ColumnText(3))
					return nil
				},
			})
			closeErr := conn.Close()
			if planErr != nil || closeErr != nil {
				t.Fatalf("inspect and close synthetic current query plan: %v", errors.Join(planErr, closeErr))
			}
			joined := strings.Join(details, "\n")
			if !strings.Contains(joined, fixtureCase.RequiredContains) {
				t.Fatalf("current query plan %q does not contain required ordering index %q", joined, fixtureCase.RequiredContains)
			}
			for _, forbidden := range fixtureCase.ForbiddenContains {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("current query plan %q contains forbidden scan/sort operation %q", joined, forbidden)
				}
			}
		})
	}
}

func TestPartialOrderingFixtureProvesDuplicateCursorHazard(t *testing.T) {
	fixture := loadCurrentReaderFixture(t)
	for _, fixtureCase := range fixture.PagingHazards {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			sourceRows := countMaterializedCurrentRows(t, materialized.Path, fixtureCase.SessionID)
			if sourceRows != fixtureCase.SourceRows {
				t.Fatalf("materialized partial-index source contains %d rows for session %q, want fixture-pinned %d before paging; restore the duplicate cursor hazard or update both fixture corpora intentionally", sourceRows, fixtureCase.SessionID, fixtureCase.SourceRows)
			}
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			defer closeSyntheticSource(t, source)
			request := ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, fixtureCase.SessionID), PageSize: mustCurrentPageSize(t, fixtureCase.PageSize)}
			visible := 0
			for {
				page, err := source.CurrentMessages(t.Context(), request)
				if err != nil {
					t.Fatalf("read synthetic partial-index pagination hazard: %v", err)
				}
				visible += len(page.Messages)
				if page.Next == nil {
					break
				}
				request.After = page.Next
			}
			if visible != fixtureCase.VisibleRows || visible >= fixtureCase.SourceRows {
				t.Fatalf("partial-index pagination exposed %d of %d rows, want fixture-pinned %d-row loss control", visible, fixtureCase.SourceRows, fixtureCase.VisibleRows)
			}
		})
	}
}

func countMaterializedCurrentRows(t testing.TB, path, sessionID string) int {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open materialized current source for row-count proof: %v", err)
	}
	var count int
	queryErr := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM session_message WHERE session_id = ?1`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	})
	closeErr := conn.Close()
	if queryErr != nil || closeErr != nil {
		t.Fatalf("count and close materialized current source rows: %v", errors.Join(queryErr, closeErr))
	}
	return count
}

func assertCurrentPage(t testing.TB, fixtureCase currentPageCase, index int, page ingest.OpenCodeCurrentPage, expected currentExpectedPage, seen map[string]struct{}) {
	t.Helper()
	ids := currentMessageIDs(page.Messages)
	seqs := currentMessageSeqs(page.Messages)
	if !equalStrings(ids, expected.IDs) || !equalInt64s(seqs, expected.Seqs) {
		t.Fatalf("strict current page %d = IDs %v seqs %v, want %v/%v", index, ids, seqs, expected.IDs, expected.Seqs)
	}
	if len(page.Messages) > fixtureCase.PageSize || cap(page.Messages) != len(page.Messages) {
		t.Fatalf("strict current page %d length/capacity = %d/%d above or retaining requested bound %d", index, len(page.Messages), cap(page.Messages), fixtureCase.PageSize)
	}
	if (page.Next != nil) != expected.HasNext {
		t.Fatalf("strict current page %d continuation = %t, want %t", index, page.Next != nil, expected.HasNext)
	}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("strict current page %d repeated identity or sentinel %q", index, id)
		}
		seen[id] = struct{}{}
	}
	if page.Next != nil && page.Next.Seq().Value() != expected.Seqs[len(expected.Seqs)-1] {
		t.Fatalf("strict current page %d cursor seq = %d, want %d", index, page.Next.Seq().Value(), expected.Seqs[len(expected.Seqs)-1])
	}
}

func loadCurrentReaderFixture(t testing.TB) currentReaderFixture {
	t.Helper()
	fixture, err := parseCurrentReaderFixture(currentReaderYAML)
	if err != nil {
		t.Fatalf("load strict current reader fixture: %v", err)
	}
	return fixture
}

func parseCurrentReaderFixture(data []byte) (currentReaderFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture currentReaderFixture
	if err := decoder.Decode(&fixture); err != nil {
		return currentReaderFixture{}, fmt.Errorf("decode strict current reader fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return currentReaderFixture{}, fmt.Errorf("decode strict current reader fixture: expected exactly one YAML document: %w", err)
	}
	if fixture.DeclaredPageCases != expectedCurrentPageCases || len(fixture.PageCases) != expectedCurrentPageCases ||
		fixture.DeclaredMalformedCases != expectedCurrentMalformedCases || len(fixture.MalformedCases) != expectedCurrentMalformedCases ||
		fixture.DeclaredValidationCases != expectedCurrentValidationCases || len(fixture.ValidationCases) != expectedCurrentValidationCases ||
		fixture.DeclaredQueryPlanCases != expectedCurrentQueryPlanCases || len(fixture.QueryPlanCases) != expectedCurrentQueryPlanCases ||
		fixture.DeclaredPagingHazards != expectedCurrentPagingHazards || len(fixture.PagingHazards) != expectedCurrentPagingHazards ||
		fixture.DeclaredLoaderMutations != expectedCurrentLoaderMutations || len(fixture.LoaderMutations) != expectedCurrentLoaderMutations {
		return currentReaderFixture{}, fmt.Errorf("strict current reader fixture row guard failed: pages=%d/%d malformed=%d/%d validation=%d/%d query_plans=%d/%d paging_hazards=%d/%d loader=%d/%d", fixture.DeclaredPageCases, len(fixture.PageCases), fixture.DeclaredMalformedCases, len(fixture.MalformedCases), fixture.DeclaredValidationCases, len(fixture.ValidationCases), fixture.DeclaredQueryPlanCases, len(fixture.QueryPlanCases), fixture.DeclaredPagingHazards, len(fixture.PagingHazards), fixture.DeclaredLoaderMutations, len(fixture.LoaderMutations))
	}
	names := make(map[string]struct{})
	addName := func(group, name string) error {
		key := group + "\x00" + name
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("strict current reader fixture %s has an empty name", group)
		}
		if _, duplicate := names[key]; duplicate {
			return fmt.Errorf("strict current reader fixture %s has duplicate name %q", group, name)
		}
		names[key] = struct{}{}
		return nil
	}
	for _, fixtureCase := range fixture.PageCases {
		if err := addName("page", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if fixtureCase.Fixture == "" || fixtureCase.SessionID == "" || fixtureCase.PageSize <= 0 || len(fixtureCase.Pages) < 2 {
			return currentReaderFixture{}, fmt.Errorf("strict current page case %q is incomplete", fixtureCase.Name)
		}
		for index, page := range fixtureCase.Pages {
			if len(page.IDs) == 0 || len(page.IDs) != len(page.Seqs) || len(page.IDs) > fixtureCase.PageSize || page.HasNext != (index < len(fixtureCase.Pages)-1) {
				return currentReaderFixture{}, fmt.Errorf("strict current page case %q page %d is incomplete", fixtureCase.Name, index)
			}
		}
	}
	for _, fixtureCase := range fixture.MalformedCases {
		if err := addName("malformed", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if !knownCurrentMutation(fixtureCase.Mutation) || fixtureCase.ErrorContains == "" {
			return currentReaderFixture{}, fmt.Errorf("strict malformed current case %q is incomplete", fixtureCase.Name)
		}
	}
	for _, fixtureCase := range fixture.ValidationCases {
		if err := addName("validation", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if !knownCurrentValidation(fixtureCase.Kind) || fixtureCase.ErrorContains == "" {
			return currentReaderFixture{}, fmt.Errorf("strict current validation case %q is incomplete", fixtureCase.Name)
		}
	}
	for _, fixtureCase := range fixture.LoaderMutations {
		if err := addName("loader", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if !knownCurrentLoaderMutation(fixtureCase.Kind) || fixtureCase.ErrorContains == "" {
			return currentReaderFixture{}, fmt.Errorf("strict current loader case %q is incomplete", fixtureCase.Name)
		}
	}
	for _, fixtureCase := range fixture.QueryPlanCases {
		if err := addName("query_plan", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if fixtureCase.Fixture == "" || fixtureCase.SessionID == "" || fixtureCase.PageSize <= 0 || fixtureCase.Statement == "" || fixtureCase.RequiredContains == "" || len(fixtureCase.ForbiddenContains) == 0 {
			return currentReaderFixture{}, fmt.Errorf("strict current query-plan case %q is incomplete", fixtureCase.Name)
		}
	}
	for _, fixtureCase := range fixture.PagingHazards {
		if err := addName("paging_hazard", fixtureCase.Name); err != nil {
			return currentReaderFixture{}, err
		}
		if fixtureCase.Fixture == "" || fixtureCase.SessionID == "" || fixtureCase.PageSize <= 0 || fixtureCase.SourceRows <= 0 || fixtureCase.VisibleRows <= 0 || fixtureCase.VisibleRows >= fixtureCase.SourceRows {
			return currentReaderFixture{}, fmt.Errorf("strict current pagination-hazard case %q is incomplete", fixtureCase.Name)
		}
	}
	return fixture, nil
}

func knownCurrentMutation(mutation currentMalformedMutation) bool {
	switch mutation {
	case currentMutationIDType, currentMutationMessageType, currentMutationCreatedType, currentMutationUpdatedType, currentMutationDataType, currentMutationSeqType, currentMutationNegativeSeq, currentMutationInvalidJSON:
		return true
	default:
		return false
	}
}

func knownCurrentValidation(kind currentValidationKind) bool {
	switch kind {
	case currentValidationPageSize, currentValidationSessionID, currentValidationSeq:
		return true
	default:
		return false
	}
}

func knownCurrentLoaderMutation(mutation currentLoaderMutation) bool {
	switch mutation {
	case currentLoaderUnknownField, currentLoaderTrailingDoc, currentLoaderDeclared, currentLoaderDuplicate:
		return true
	default:
		return false
	}
}

func mutateCurrentReaderFixture(source []byte, mutation currentLoaderMutation) ([]byte, error) {
	replace := func(old, replacement string) ([]byte, error) {
		if !bytes.Contains(source, []byte(old)) {
			return nil, fmt.Errorf("strict current reader mutation anchor %q is absent", old)
		}
		return bytes.Replace(source, []byte(old), []byte(replacement), 1), nil
	}
	switch mutation {
	case currentLoaderUnknownField:
		return append(append([]byte(nil), source...), []byte("unexpected: true\n")...), nil
	case currentLoaderTrailingDoc:
		return append(append([]byte(nil), source...), []byte("---\ndeclared_page_cases: 0\n")...), nil
	case currentLoaderDeclared:
		return replace("declared_malformed_cases: 8", "declared_malformed_cases: 7")
	case currentLoaderDuplicate:
		return replace("name: reject-oversized-page", "name: reject-zero-page-size")
	default:
		return nil, fmt.Errorf("unknown strict current reader loader mutation %q", mutation)
	}
}

func mutateCurrentRow(t testing.TB, path string, mutation currentMalformedMutation) {
	t.Helper()
	statement := ""
	switch mutation {
	case currentMutationIDType:
		statement = "UPDATE session_message SET id = X'01' WHERE seq = 7"
	case currentMutationMessageType:
		statement = "UPDATE session_message SET type = X'02' WHERE seq = 7"
	case currentMutationCreatedType:
		statement = "UPDATE session_message SET time_created = X'03' WHERE seq = 7"
	case currentMutationUpdatedType:
		statement = "UPDATE session_message SET time_updated = X'04' WHERE seq = 7"
	case currentMutationDataType:
		statement = "UPDATE session_message SET data = X'05' WHERE seq = 7"
	case currentMutationSeqType:
		statement = "UPDATE session_message SET seq = X'06' WHERE seq = 7"
	case currentMutationNegativeSeq:
		statement = "UPDATE session_message SET seq = -7 WHERE seq = 7"
	case currentMutationInvalidJSON:
		statement = "UPDATE session_message SET data = 'not-json' WHERE seq = 7"
	default:
		t.Fatalf("unknown current row mutation %q", mutation)
	}
	writer, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open synthetic current mutation writer: %v", err)
	}
	updateErr := sqlitex.ExecuteTransient(writer, statement, nil)
	closeErr := writer.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("apply synthetic current row mutation %q: %v", mutation, errors.Join(updateErr, closeErr))
	}
}

func appendCurrentWALRow(writer *sqlite.Conn) error {
	return sqlitex.ExecuteTransient(writer, `INSERT INTO session_message
		(id, session_id, type, time_created, time_updated, data, seq)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`, &sqlitex.ExecOptions{Args: []any{"sm_current_wal_only", "ses_current_wal", "message", int64(9301), int64(9301), `{"role":"assistant","marker":"wal-only"}`, int64(11)}})
}

func currentReaderRequest(t testing.TB, pageSize int) ingest.OpenCodeCurrentPageRequest {
	t.Helper()
	return ingest.OpenCodeCurrentPageRequest{SessionID: mustCurrentSessionID(t, "ses_current_reader_a"), PageSize: mustCurrentPageSize(t, pageSize)}
}

func mustCurrentSessionID(t testing.TB, value string) ingest.OpenCodeCurrentSessionID {
	t.Helper()
	id, err := ingest.NewOpenCodeCurrentSessionID(value)
	if err != nil {
		t.Fatalf("construct synthetic current session identifier: %v", err)
	}
	return id
}

func mustCurrentPageSize(t testing.TB, value int) ingest.OpenCodeCurrentPageSize {
	t.Helper()
	size, err := ingest.NewOpenCodeCurrentPageSize(value)
	if err != nil {
		t.Fatalf("construct synthetic current page size: %v", err)
	}
	return size
}

func currentMessageIDs(rows []ingest.OpenCodeCurrentMessageRow) []string {
	result := make([]string, len(rows))
	for index, row := range rows {
		result[index] = row.ID.String()
	}
	return result
}

func currentMessageSeqs(rows []ingest.OpenCodeCurrentMessageRow) []int64 {
	result := make([]int64, len(rows))
	for index, row := range rows {
		result[index] = row.Seq.Value()
	}
	return result
}

func equalInt64s(left, right []int64) bool {
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

func equalCurrentPages(left, right ingest.OpenCodeCurrentPage) bool {
	if !equalStrings(currentMessageIDs(left.Messages), currentMessageIDs(right.Messages)) || !equalInt64s(currentMessageSeqs(left.Messages), currentMessageSeqs(right.Messages)) || (left.Next == nil) != (right.Next == nil) {
		return false
	}
	if left.Next != nil && left.Next.Seq().Value() != right.Next.Seq().Value() {
		return false
	}
	return true
}
