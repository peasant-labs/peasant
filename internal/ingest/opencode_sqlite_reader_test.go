package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testutil"
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
	var fixture currentReaderFixture
	if err := testutil.DecodeFixtureYAML(data, &fixture); err != nil {
		return currentReaderFixture{}, fmt.Errorf("decode strict current reader fixture: %w", err)
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
	parts, err := source.LegacySessionParts(t.Context(), ingest.OpenCodeLegacySessionPartPageRequest{SessionID: sessionID, PageSize: mustLegacyPageSize(t, 4)})
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
	parts, err := source.LegacySessionParts(t.Context(), ingest.OpenCodeLegacySessionPartPageRequest{SessionID: sessionID, PageSize: mustLegacyPageSize(t, 4)})
	if err != nil || !equalStrings(legacyPartStrings(parts.Parts), []string{"part_wal_base", "part_wal_only"}) {
		t.Fatalf("WAL-aware session part page = %+v error=%v, want the base part and the WAL-only part", parts, err)
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

//go:embed testdata/opencode_sqlite_reader.yaml
var openCodeSQLiteReaderYAML []byte

type readerPageKind string

const (
	readerPageSessions readerPageKind = "sessions"
	readerPageMessages readerPageKind = "messages"
	readerPageParts    readerPageKind = "parts"
)

type readerInvalidIdentifierKind string

const readerInvalidPartID readerInvalidIdentifierKind = "part"

type readerSignatureRuleKind string

const (
	readerRuleContains     readerSignatureRuleKind = "contains"
	readerRuleNestedFunc   readerSignatureRuleKind = "nested_func"
	readerRuleDirectString readerSignatureRuleKind = "direct_string"
	readerRuleStringID     readerSignatureRuleKind = "string_id"
)

type readerGuardMutationKind string

const (
	readerMutationRawHandle        readerGuardMutationKind = "raw_handle"
	readerMutationGenericArguments readerGuardMutationKind = "generic_arguments"
	readerMutationCallback         readerGuardMutationKind = "callback"
	readerMutationBareString       readerGuardMutationKind = "bare_string"
	readerMutationStringID         readerGuardMutationKind = "string_id"
)

type readerLoaderMutationKind string

const (
	readerLoaderUnknownField    readerLoaderMutationKind = "unknown_field"
	readerLoaderTrailingDoc     readerLoaderMutationKind = "trailing_document"
	readerLoaderDeclaredCount   readerLoaderMutationKind = "declared_count"
	readerLoaderDuplicateMethod readerLoaderMutationKind = "duplicate_method"
)

type readerContractFixture struct {
	RequiredPageCases          []string                      `yaml:"required_page_cases"`
	PageCases                  []readerPageCase              `yaml:"page_cases"`
	RequiredInvalidIdentifiers []string                      `yaml:"required_invalid_identifiers"`
	InvalidIdentifiers         []readerInvalidIdentifierCase `yaml:"invalid_identifiers"`
	RequiredMethods            []string                      `yaml:"required_methods"`
	Methods                    []readerMethod                `yaml:"methods"`
	RequiredSignatureRules     []string                      `yaml:"required_signature_rules"`
	SignatureRules             []readerSignatureRule         `yaml:"signature_rules"`
	RequiredGuardMutations     []string                      `yaml:"required_guard_mutations"`
	GuardMutations             []readerGuardMutation         `yaml:"guard_mutations"`
	RequiredLoaderMutations    []string                      `yaml:"required_loader_mutations"`
	LoaderMutations            []readerLoaderMutation        `yaml:"loader_mutations"`
}

type readerPageCase struct {
	Name      string               `yaml:"name"`
	Kind      readerPageKind       `yaml:"kind"`
	Fixture   string               `yaml:"fixture"`
	SessionID string               `yaml:"session_id"`
	MessageID string               `yaml:"message_id"`
	PageSize  int                  `yaml:"page_size"`
	Pages     []readerExpectedPage `yaml:"pages"`
}

type readerExpectedPage struct {
	IDs     []string `yaml:"ids"`
	HasNext bool     `yaml:"has_next"`
}

type readerInvalidIdentifierCase struct {
	Name          string                      `yaml:"name"`
	Kind          readerInvalidIdentifierKind `yaml:"kind"`
	Value         string                      `yaml:"value"`
	ErrorContains string                      `yaml:"error_contains"`
}

type readerMethod struct {
	Name      string `yaml:"name"`
	Signature string `yaml:"signature"`
}

type readerSignatureRule struct {
	Name  string                  `yaml:"name"`
	Kind  readerSignatureRuleKind `yaml:"kind"`
	Token string                  `yaml:"token"`
}

type readerGuardMutation struct {
	Name          string                  `yaml:"name"`
	Kind          readerGuardMutationKind `yaml:"kind"`
	ErrorContains string                  `yaml:"error_contains"`
}

type readerLoaderMutation struct {
	Name          string                   `yaml:"name"`
	Kind          readerLoaderMutationKind `yaml:"kind"`
	ErrorContains string                   `yaml:"error_contains"`
}

func TestOpenCodeLegacyReaderPagesMatchStrictFixture(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, fixtureCase := range fixture.PageCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			defer closeSyntheticSource(t, source)
			pageSize := mustLegacyPageSize(t, fixtureCase.PageSize)
			switch fixtureCase.Kind {
			case readerPageSessions:
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacySessionCursor) (readerFetchedPage[ingest.OpenCodeLegacySessionCursor], error) {
					page, err := source.LegacySessionIDs(t.Context(), ingest.OpenCodeLegacySessionPageRequest{PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacySessionCursor]{IDs: legacySessionStrings(page.SessionIDs), Capacity: cap(page.SessionIDs), Next: page.Next}, err
				})
			case readerPageMessages:
				sessionID := mustLegacySessionID(t, fixtureCase.SessionID)
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacyMessageCursor) (readerFetchedPage[ingest.OpenCodeLegacyMessageCursor], error) {
					page, err := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacyMessageCursor]{IDs: legacyMessageStrings(page.Messages), Capacity: cap(page.Messages), Next: page.Next}, err
				})
			case readerPageParts:
				sessionID := mustLegacySessionID(t, fixtureCase.SessionID)
				assertFixturePages(t, fixtureCase, func(cursor *ingest.OpenCodeLegacyPartCursor) (readerFetchedPage[ingest.OpenCodeLegacyPartCursor], error) {
					page, err := source.LegacySessionParts(t.Context(), ingest.OpenCodeLegacySessionPartPageRequest{SessionID: sessionID, PageSize: pageSize, After: cursor})
					return readerFetchedPage[ingest.OpenCodeLegacyPartCursor]{IDs: legacyPartStrings(page.Parts), Capacity: cap(page.Parts), Next: page.Next}, err
				})
			default:
				t.Fatalf("strict reader fixture %q has unsupported page kind %q", fixtureCase.Name, fixtureCase.Kind)
			}
		})
	}
}

type readerFetchedPage[C any] struct {
	IDs      []string
	Capacity int
	Next     *C
}

func assertFixturePages[C any](t testing.TB, fixtureCase readerPageCase, fetch func(*C) (readerFetchedPage[C], error)) {
	t.Helper()
	var cursor *C
	seen := make(map[string]struct{})
	for index, expected := range fixtureCase.Pages {
		requestCursor := cursor
		page, err := fetch(requestCursor)
		if err != nil {
			t.Fatalf("read strict fixture page %d: %v", index, err)
		}
		assertFixturePage(t, fixtureCase, index, page.IDs, page.Capacity, page.Next != nil, expected, seen)
		repeated, err := fetch(requestCursor)
		if err != nil {
			t.Fatalf("repeat strict fixture cursor for page %d: %v", index, err)
		}
		if !equalStrings(repeated.IDs, page.IDs) || repeated.Capacity != page.Capacity || (repeated.Next != nil) != (page.Next != nil) {
			t.Fatalf("repeated strict fixture page %d = ids %v cap %d next %t, want deterministic ids %v cap %d next %t", index, repeated.IDs, repeated.Capacity, repeated.Next != nil, page.IDs, page.Capacity, page.Next != nil)
		}
		cursor = page.Next
	}
	if cursor != nil {
		t.Fatalf("strict fixture %q ended with continuation cursor after final page", fixtureCase.Name)
	}
}

func assertFixturePage(t testing.TB, fixtureCase readerPageCase, index int, ids []string, capacity int, hasNext bool, expected readerExpectedPage, seen map[string]struct{}) {
	t.Helper()
	if !equalStrings(ids, expected.IDs) {
		t.Fatalf("strict fixture page %d IDs = %v, want %v", index, ids, expected.IDs)
	}
	if len(ids) != len(expected.IDs) || capacity != len(ids) {
		t.Fatalf("strict fixture page %d length/capacity = %d/%d, want exact returned bound %d/%d so no sentinel can be resliced", index, len(ids), capacity, len(expected.IDs), len(expected.IDs))
	}
	if len(ids) > fixtureCase.PageSize {
		t.Fatalf("strict fixture page %d returned %d rows above requested size %d", index, len(ids), fixtureCase.PageSize)
	}
	if hasNext != expected.HasNext {
		t.Fatalf("strict fixture page %d continuation = %t, want %t", index, hasNext, expected.HasNext)
	}
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("strict fixture page %d repeated sentinel or previously returned identity %q", index, id)
		}
		seen[id] = struct{}{}
	}
}

func TestOpenCodeLegacyPartIDRejectsStrictFixtureCases(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, fixtureCase := range fixture.InvalidIdentifiers {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			if fixtureCase.Kind != readerInvalidPartID {
				t.Fatalf("strict reader fixture %q has unsupported invalid identifier kind %q", fixtureCase.Name, fixtureCase.Kind)
			}
			_, err := ingest.NewOpenCodeLegacyPartID(fixtureCase.Value)
			if err == nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) {
				t.Fatalf("invalid part identifier error = %v, want substring %q", err, fixtureCase.ErrorContains)
			}
		})
	}
}

func TestOpenCodeSQLiteSourceSurfaceMatchesStrictFixture(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	interfaceType := reflect.TypeOf((*ingest.OpenCodeSQLiteSource)(nil)).Elem()
	if err := validateReaderMethodInventory(interfaceType, fixture.Methods); err != nil {
		t.Fatal(err)
	}
	if err := validateReaderSurfaceTypes(interfaceType, fixture.SignatureRules); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeSQLiteSourceSurfaceGuardMutationsAreNonVacuous(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, mutation := range fixture.GuardMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			interfaceType := readerMutationInterface(t, mutation.Kind)
			err := validateReaderSurfaceTypes(interfaceType, fixture.SignatureRules)
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("surface guard mutation error = %v, want rule %q", err, mutation.ErrorContains)
			}
		})
	}
}

func TestOpenCodeSQLiteReaderFixtureLoaderMutationsAreRejected(t *testing.T) {
	fixture := loadReaderContractFixture(t)
	for _, mutation := range fixture.LoaderMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			mutated, err := mutateReaderFixture(openCodeSQLiteReaderYAML, mutation.Kind)
			if err != nil {
				t.Fatalf("apply strict reader fixture mutation: %v", err)
			}
			_, err = parseReaderContractFixture(mutated)
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("strict reader fixture mutation error = %v, want substring %q", err, mutation.ErrorContains)
			}
		})
	}
}

func validateReaderMethodInventory(interfaceType reflect.Type, expected []readerMethod) error {
	if interfaceType.Kind() != reflect.Interface {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production type %s is not an interface; callers cannot rely on the restrictive contract; restore OpenCodeSQLiteSource as an interface", interfaceType)
	}
	if interfaceType.NumMethod() != len(expected) {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production exposes %d methods but strict fixture declares %d; the boundary may have drifted; update the typed interface and reviewed fixture together", interfaceType.NumMethod(), len(expected))
	}
	expectedByName := make(map[string]string, len(expected))
	for _, method := range expected {
		expectedByName[method.Name] = method.Signature
	}
	for index := 0; index < interfaceType.NumMethod(); index++ {
		method := interfaceType.Method(index)
		want, ok := expectedByName[method.Name]
		if !ok {
			return fmt.Errorf("validate OpenCode SQLite source method inventory failed: production method %q is absent from the strict fixture; an unreviewed operation could escape the bounded boundary; add only a typed reviewed method and fixture entry", method.Name)
		}
		if got := method.Type.String(); got != want {
			return fmt.Errorf("validate OpenCode SQLite source method inventory failed for %q: signature is %q, want strict fixture signature %q; raw or generic types may have escaped; restore the typed signature or update the reviewed fixture", method.Name, got, want)
		}
		delete(expectedByName, method.Name)
	}
	if len(expectedByName) != 0 {
		return fmt.Errorf("validate OpenCode SQLite source method inventory failed: strict fixture methods %v are missing from production; mounted consumers cannot use the promised contract; restore the methods", expectedByName)
	}
	return nil
}

func validateReaderSurfaceTypes(interfaceType reflect.Type, rules []readerSignatureRule) error {
	for index := 0; index < interfaceType.NumMethod(); index++ {
		method := interfaceType.Method(index)
		for _, rule := range rules {
			switch rule.Kind {
			case readerRuleContains:
				if path, found := findReaderTypeToken(method.Type, method.Name, rule.Token, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because type path %s contains forbidden token %q", rule.Name, method.Name, path, rule.Token)
				}
			case readerRuleNestedFunc:
				if path, found := findReaderCallback(method.Type, method.Name, rule.Token, true, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because type path %s contains a callback", rule.Name, method.Name, path)
				}
			case readerRuleDirectString:
				for argument := 0; argument < method.Type.NumIn(); argument++ {
					if path, found := findReaderBareInputString(method.Type.In(argument), fmt.Sprintf("%s.in%d", method.Name, argument), rule.Token, make(map[reflect.Type]bool)); found {
						return fmt.Errorf("reader surface rule %s rejected method %s because input path %s is a bare string that could carry SQL or a table name", rule.Name, method.Name, path)
					}
				}
			case readerRuleStringID:
				if path, found := findStringlyIdentity(method.Type, method.Name, rule.Token, make(map[reflect.Type]bool)); found {
					return fmt.Errorf("reader surface rule %s rejected method %s because identity path %s is a bare string", rule.Name, method.Name, path)
				}
			default:
				return fmt.Errorf("reader surface validation encountered unknown strict rule kind %q after fixture validation", rule.Kind)
			}
		}
	}
	return nil
}

func findReaderTypeToken(value reflect.Type, path, token string, seen map[reflect.Type]bool) (string, bool) {
	if strings.Contains(value.String(), token) {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findReaderTypeToken(value.In(index), fmt.Sprintf("%s.in%d", path, index), token, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findReaderTypeToken(value.Out(index), fmt.Sprintf("%s.out%d", path, index), token, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderTypeToken(value.Elem(), path, token, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if foundPath, found := findReaderTypeToken(field.Type, path+"."+field.Name, token, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findReaderCallback(value reflect.Type, path, token string, root bool, seen map[reflect.Type]bool) (string, bool) {
	if value.Kind() == reflect.Func && !root && strings.Contains(value.String(), token) {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findReaderCallback(value.In(index), fmt.Sprintf("%s.in%d", path, index), token, false, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findReaderCallback(value.Out(index), fmt.Sprintf("%s.out%d", path, index), token, false, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderCallback(value.Elem(), path, token, false, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if foundPath, found := findReaderCallback(field.Type, path+"."+field.Name, token, false, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findReaderBareInputString(value reflect.Type, path, token string, seen map[reflect.Type]bool) (string, bool) {
	if value.Kind() == reflect.String && value.Kind().String() == token {
		return path, true
	}
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findReaderBareInputString(value.Elem(), path, token, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			if field.PkgPath != "" {
				continue
			}
			if foundPath, found := findReaderBareInputString(field.Type, path+"."+field.Name, token, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func findStringlyIdentity(value reflect.Type, path, identityToken string, seen map[reflect.Type]bool) (string, bool) {
	if seen[value] {
		return "", false
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Func:
		for index := 0; index < value.NumIn(); index++ {
			if foundPath, found := findStringlyIdentity(value.In(index), fmt.Sprintf("%s.in%d", path, index), identityToken, seen); found {
				return foundPath, true
			}
		}
		for index := 0; index < value.NumOut(); index++ {
			if foundPath, found := findStringlyIdentity(value.Out(index), fmt.Sprintf("%s.out%d", path, index), identityToken, seen); found {
				return foundPath, true
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findStringlyIdentity(value.Elem(), path, identityToken, seen)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Field(index)
			fieldPath := path + "." + field.Name
			if strings.HasSuffix(strings.ToLower(field.Name), strings.ToLower(identityToken)) && field.Type.Kind() == reflect.String {
				return fieldPath, true
			}
			if foundPath, found := findStringlyIdentity(field.Type, fieldPath, identityToken, seen); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

type readerRawHandleMutation interface {
	Escape(context.Context, *sqlite.Conn) error
}

type readerGenericArgumentsMutation interface {
	Escape(context.Context, []any) error
}

type readerCallbackMutation interface {
	Escape(context.Context, func()) error
}

type readerBareStringMutation interface {
	Escape(context.Context, string) error
}

type readerStringIDMutationRequest struct {
	PartID string
}

type readerStringIDMutation interface {
	Escape(context.Context, readerStringIDMutationRequest) error
}

func readerMutationInterface(t testing.TB, kind readerGuardMutationKind) reflect.Type {
	t.Helper()
	switch kind {
	case readerMutationRawHandle:
		return reflect.TypeOf((*readerRawHandleMutation)(nil)).Elem()
	case readerMutationGenericArguments:
		return reflect.TypeOf((*readerGenericArgumentsMutation)(nil)).Elem()
	case readerMutationCallback:
		return reflect.TypeOf((*readerCallbackMutation)(nil)).Elem()
	case readerMutationBareString:
		return reflect.TypeOf((*readerBareStringMutation)(nil)).Elem()
	case readerMutationStringID:
		return reflect.TypeOf((*readerStringIDMutation)(nil)).Elem()
	default:
		t.Fatalf("strict reader fixture has unsupported guard mutation kind %q", kind)
		return nil
	}
}

func loadReaderContractFixture(t testing.TB) readerContractFixture {
	t.Helper()
	fixture, err := parseReaderContractFixture(openCodeSQLiteReaderYAML)
	if err != nil {
		t.Fatalf("load strict OpenCode SQLite reader fixture: %v", err)
	}
	return fixture
}

func parseReaderContractFixture(data []byte) (readerContractFixture, error) {
	var fixture readerContractFixture
	if err := testutil.DecodeFixtureYAML(data, &fixture); err != nil {
		return readerContractFixture{}, fmt.Errorf("decode strict OpenCode SQLite reader fixture: %w", err)
	}
	if len(fixture.RequiredPageCases) == 0 || len(fixture.RequiredInvalidIdentifiers) == 0 || len(fixture.RequiredMethods) == 0 || len(fixture.RequiredSignatureRules) == 0 || len(fixture.RequiredGuardMutations) == 0 || len(fixture.RequiredLoaderMutations) == 0 {
		return readerContractFixture{}, fmt.Errorf("strict OpenCode SQLite reader fixture declares an empty required manifest")
	}
	if err := validateReaderFixture(fixture); err != nil {
		return readerContractFixture{}, err
	}
	return fixture, nil
}

func validateReaderFixture(fixture readerContractFixture) error {
	names := make(map[string]string)
	for _, fixtureCase := range fixture.PageCases {
		if err := addReaderFixtureName(names, "page", fixtureCase.Name); err != nil {
			return err
		}
		if fixtureCase.Fixture == "" || fixtureCase.PageSize <= 0 || len(fixtureCase.Pages) < 2 {
			return fmt.Errorf("strict reader page fixture is incomplete: %+v", fixtureCase)
		}
		for index, page := range fixtureCase.Pages {
			if len(page.IDs) == 0 || len(page.IDs) > fixtureCase.PageSize || page.HasNext != (index < len(fixtureCase.Pages)-1) {
				return fmt.Errorf("strict reader page fixture %q page %d has invalid IDs or continuation: %+v", fixtureCase.Name, index, page)
			}
		}
	}
	for _, fixtureCase := range fixture.InvalidIdentifiers {
		if err := addReaderFixtureName(names, "invalid_identifier", fixtureCase.Name); err != nil {
			return err
		}
		if fixtureCase.Kind != readerInvalidPartID || fixtureCase.Value == "" || fixtureCase.ErrorContains == "" {
			return fmt.Errorf("strict invalid identifier fixture is incomplete: %+v", fixtureCase)
		}
	}
	for _, method := range fixture.Methods {
		if err := addReaderFixtureName(names, "method", method.Name); err != nil {
			return err
		}
		if method.Signature == "" {
			return fmt.Errorf("strict reader method %q has an empty signature", method.Name)
		}
	}
	for _, rule := range fixture.SignatureRules {
		if err := addReaderFixtureName(names, "rule", rule.Name); err != nil {
			return err
		}
		if rule.Token == "" || !knownReaderRule(rule.Kind) {
			return fmt.Errorf("strict reader signature rule is incomplete: %+v", rule)
		}
	}
	for _, mutation := range fixture.GuardMutations {
		if err := addReaderFixtureName(names, "guard_mutation", mutation.Name); err != nil {
			return err
		}
		if mutation.ErrorContains == "" || !knownReaderMutation(mutation.Kind) {
			return fmt.Errorf("strict reader guard mutation is incomplete: %+v", mutation)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if err := addReaderFixtureName(names, "loader_mutation", mutation.Name); err != nil {
			return err
		}
		if mutation.ErrorContains == "" || !knownReaderLoaderMutation(mutation.Kind) {
			return fmt.Errorf("strict reader loader mutation is incomplete: %+v", mutation)
		}
	}
	requiredGroups := []struct {
		group    string
		required []string
	}{
		{"page", fixture.RequiredPageCases},
		{"invalid_identifier", fixture.RequiredInvalidIdentifiers},
		{"method", fixture.RequiredMethods},
		{"rule", fixture.RequiredSignatureRules},
		{"guard_mutation", fixture.RequiredGuardMutations},
		{"loader_mutation", fixture.RequiredLoaderMutations},
	}
	for _, entry := range requiredGroups {
		for _, name := range entry.required {
			if _, ok := names[entry.group+"\x00"+name]; !ok {
				return fmt.Errorf("strict OpenCode SQLite reader fixture is missing required %s %q", entry.group, name)
			}
		}
	}
	return nil
}

func addReaderFixtureName(names map[string]string, group, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("strict OpenCode SQLite reader fixture %s has an empty name", group)
	}
	key := group + "\x00" + name
	if prior, duplicate := names[key]; duplicate {
		return fmt.Errorf("strict OpenCode SQLite reader fixture %s duplicates name %q previously recorded in %s", group, name, prior)
	}
	names[key] = group
	return nil
}

func knownReaderRule(kind readerSignatureRuleKind) bool {
	switch kind {
	case readerRuleContains, readerRuleNestedFunc, readerRuleDirectString, readerRuleStringID:
		return true
	default:
		return false
	}
}

func knownReaderMutation(kind readerGuardMutationKind) bool {
	switch kind {
	case readerMutationRawHandle, readerMutationGenericArguments, readerMutationCallback, readerMutationBareString, readerMutationStringID:
		return true
	default:
		return false
	}
}

func knownReaderLoaderMutation(kind readerLoaderMutationKind) bool {
	switch kind {
	case readerLoaderUnknownField, readerLoaderTrailingDoc, readerLoaderDeclaredCount, readerLoaderDuplicateMethod:
		return true
	default:
		return false
	}
}

func mutateReaderFixture(source []byte, kind readerLoaderMutationKind) ([]byte, error) {
	replaceOnce := func(old, replacement string) ([]byte, error) {
		if !bytes.Contains(source, []byte(old)) {
			return nil, fmt.Errorf("strict reader fixture mutation anchor %q is absent", old)
		}
		return bytes.Replace(source, []byte(old), []byte(replacement), 1), nil
	}
	switch kind {
	case readerLoaderUnknownField:
		return append(append([]byte(nil), source...), []byte("unexpected: true\n")...), nil
	case readerLoaderTrailingDoc:
		return append(append([]byte(nil), source...), []byte("---\nrequired_page_cases: []\n")...), nil
	case readerLoaderDeclaredCount:
		return replaceOnce("\n  - MaxEventSeq\n", "\n  - MaxEventSeqRenamedAway\n")
	case readerLoaderDuplicateMethod:
		return replaceOnce("  - name: Close\n", "  - name: Catalog\n")
	default:
		return nil, fmt.Errorf("unknown strict reader fixture loader mutation %q", kind)
	}
}

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
